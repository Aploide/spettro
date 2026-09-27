package acp

// Approvals over ACP: the runtime's approval callback (agent.
// ShellApprovalRequest, used for shell commands, file writes and network
// access under ask-first or restricted) becomes a session/request_permission
// request, so the editor shows its native approval prompt on the tool call
// card it is already rendering.
//
// Which requests reach here. The runtime asks only when its own rules leave
// the decision to the user: permission rules, hooks and the persisted
// allow-lists decide first (see authorizeShellCommand, authorizeWrite and
// authorizeNetworkAccess in internal/agent), and lsp-op rules deny an lsp
// operation outright rather than asking. Under yolo the bridge answers
// "allow once" itself without a round trip.
//
// Which card. The request is shown on the tool call card of the call that
// asked (approvalToolCallID). The main agent and its sub-agents run tools in
// parallel, so the runtime names the asking agent and its working directory
// on the request (ShellApprovalRequest.AgentID and CWD), and only that
// agent's cards are candidates. Among them, the card whose arguments name
// what the approval is about wins; a card already showing a prompt never
// does, because its call is blocked on that prompt and cannot be asking
// again. With no candidate left the request describes the call on a card of
// its own.
//
// Options. Every request offers "Allow once" and "Deny". "Always allow" is
// offered only where the runtime remembers the answer: for shell commands
// (the approved command segments are persisted to the project's allow-list)
// and network targets (persisted likewise). A write approval is never
// remembered by the runtime, so offering "always" there would be a lie. The
// label names what is remembered, which for a network approval depends on
// the tool (see networkTargets): it is never a whole site.
//
// Content. What the prompt shows is what is approved, in full: the proposed
// change as a real diff (or the whole unified diff as text when the file is
// too large for one), the whole command, or the whole network target. Tool
// call titles and rawInput are clipped for every card (see tools.go); an
// approval's content is not, beyond maxApprovalTextBytes, which no command or
// diff a person reads comes near. Past it the text ends with an explicit
// "[truncated: ...]" line (remote.ClipApprovalText), so a cut is never shown
// as the whole: a person must be able to trust what they approve.
//
// The editor draws that text, not a terminal, but the same characters can
// hide in it: a carriage return, an escape sequence, a bidi override that
// reorders the line, zero-width characters and variation selectors that
// carry a payload inside what reads as "". So the command, the network
// target, the text diff and the card title are escaped the way the TUI
// escapes them (termtext.EscapeExact, line by line). A structured diff
// cannot be: the editor shows the file's own text. When that text holds
// such a character, the escaped unified diff follows it, under a line that
// says why.
//
// Timeouts. The request is bound to the tool's own deadline (timeout_sec in
// the manifest, e.g. 120 s for bash and 60 s for file edits): when it passes
// unanswered, or the turn is cancelled, the SDK withdraws the request with
// $/cancel_request and the call fails as not approved. The permission
// request never outlives the turn.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/agent"
	"spettro/internal/remote"
	"spettro/internal/termtext"
)

// Permission option IDs. They are echoed back by the client, so they must
// not change casually.
const (
	permAllowOnce   = "allow-once"
	permAllowAlways = "allow-always"
	permDeny        = "deny"
)

// networkTarget describes the target a network tool asks approval for.
type networkTarget struct {
	// noun names the target in the "Always allow this <noun>" option.
	noun string
	// fromArgs reads the target from a call's arguments, the way the
	// runtime builds it before calling authorizeNetworkAccess.
	fromArgs func(toolArgs) string
}

// networkTargets lists, for every tool that asks for network access, what
// authorizeNetworkAccess (internal/agent) receives as the target. That
// exact string is what "always allow" persists, so a web-fetch approval
// covers one URL, not its site, and a web-search approval one query.
var networkTargets = map[string]networkTarget{
	"web-fetch":          {noun: "URL", fromArgs: func(a toolArgs) string { return a.str("url") }},
	"download":           {noun: "URL", fromArgs: func(a toolArgs) string { return a.str("url") }},
	"web-search":         {noun: "search", fromArgs: func(a toolArgs) string { return a.str("query") }},
	"mcp-list-resources": {noun: "MCP server", fromArgs: func(a toolArgs) string { return a.str("server_id") }},
	"mcp-auth":           {noun: "MCP server", fromArgs: func(a toolArgs) string { return a.str("server_id") }},
	"mcp-read-resource": {noun: "MCP resource", fromArgs: func(a toolArgs) string {
		return a.str("server_id") + ":" + a.str("resource_id")
	}},
}

// approvalSubject is what an approval request is about, derived from the
// runtime's request.
type approvalSubject struct {
	// tool is the canonical name of the tool asking.
	tool string
	// command is set for a command approval (bash, pty-start): the runtime
	// lists the command segments still needing approval, and persists them
	// when the answer is "always".
	command bool
	// network is set for a network access approval, whose target is the URL
	// or host being contacted.
	network bool
	target  string
}

// subjectOf reads an approval request. A network approval arrives with no
// ToolID and the command "network <tool> <target>" (authorizeNetworkAccess);
// a command approval carries the segments needing approval
// (authorizeShellCommand); a write approval carries neither
// (authorizeWrite).
func subjectOf(ar agent.ShellApprovalRequest) approvalSubject {
	if ar.ToolID == "" {
		if fields := strings.Fields(ar.Command); len(fields) >= 2 && fields[0] == "network" {
			target := ""
			if len(fields) >= 3 {
				target = strings.Join(fields[2:], " ")
			}
			return approvalSubject{tool: agent.CanonicalToolName(fields[1]), network: true, target: target}
		}
	}
	return approvalSubject{tool: agent.CanonicalToolName(ar.ToolID), command: len(ar.Segments) > 0}
}

// requestApproval bridges one runtime approval to session/request_permission
// and maps the answer back. A request the client fails, cancels or leaves
// unanswered past the deadline is a denial; an error is returned alongside
// so the runtime reports why.
func (t *turnState) requestApproval(ctx context.Context, ar agent.ShellApprovalRequest) (agent.ShellApprovalDecision, error) {
	subject := subjectOf(ar)
	id, attached := t.approvalToolCallID(subject, ar)

	update := acpsdk.ToolCallUpdate{
		ToolCallId: id,
		Status:     acpsdk.Ptr(acpsdk.ToolCallStatusPending),
		Content:    approvalContent(ar),
	}
	if !attached {
		// No card is open for this call (the runtime asked before announcing
		// it), so the request must describe it in full.
		update.Title = acpsdk.Ptr(approvalTitle(subject, ar))
		update.Kind = acpsdk.Ptr(approvalKind(subject))
		update.RawInput = map[string]any{"command": clipBytes(ar.Command, maxRawInputString), "reason": ar.Reason}
		if ar.Change != nil {
			update.Locations = []acpsdk.ToolCallLocation{{Path: ar.Change.Path}}
		}
	}

	options := []acpsdk.PermissionOption{
		{OptionId: permAllowOnce, Name: "Allow once", Kind: acpsdk.PermissionOptionKindAllowOnce},
	}
	if remembersApproval(subject) {
		options = append(options, acpsdk.PermissionOption{OptionId: permAllowAlways, Name: alwaysAllowLabel(subject, ar), Kind: acpsdk.PermissionOptionKindAllowAlways})
	}
	options = append(options, acpsdk.PermissionOption{OptionId: permDeny, Name: "Deny", Kind: acpsdk.PermissionOptionKindRejectOnce})

	resp, err := t.bridge.conn.RequestPermission(ctx, acpsdk.RequestPermissionRequest{
		SessionId: t.sessionID,
		ToolCall:  update,
		Options:   options,
	})
	decision := agent.ShellApprovalDeny
	switch {
	case err != nil:
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("no answer before the tool's timeout")
		}
	case resp.Outcome.Selected != nil:
		switch string(resp.Outcome.Selected.OptionId) {
		case permAllowOnce:
			decision = agent.ShellApprovalAllowOnce
		case permAllowAlways:
			if remembersApproval(subject) {
				decision = agent.ShellApprovalAllowAlways
			} else {
				decision = agent.ShellApprovalAllowOnce
			}
		}
	}
	t.settleApprovalCard(id, attached, decision)
	return decision, err
}

// settleApprovalCard moves the card out of "pending" once the user answered.
// An attached card goes back to in_progress when allowed (the tool now runs
// and its completion trace finishes the card) and is left for the failing
// completion otherwise. A card the request itself created has no
// completion trace coming, so it is finished here. Either way the card no
// longer awaits an answer, so a later request of the same call can use it.
func (t *turnState) settleApprovalCard(id acpsdk.ToolCallId, attached bool, decision agent.ShellApprovalDecision) {
	t.mu.Lock()
	delete(t.awaiting, id)
	t.mu.Unlock()
	allowed := decision == agent.ShellApprovalAllowOnce || decision == agent.ShellApprovalAllowAlways
	switch {
	case attached && allowed:
		t.sessionUpdate(acpsdk.UpdateToolCall(id, acpsdk.WithUpdateStatus(acpsdk.ToolCallStatusInProgress)))
	case !attached && allowed:
		t.sessionUpdate(acpsdk.UpdateToolCall(id, acpsdk.WithUpdateStatus(acpsdk.ToolCallStatusCompleted)))
	case !attached:
		t.sessionUpdate(acpsdk.UpdateToolCall(id, acpsdk.WithUpdateStatus(acpsdk.ToolCallStatusFailed)))
	}
}

// approvalToolCallID finds the open tool call an approval belongs to, so the
// editor shows the prompt on the card it is already rendering. Candidates
// are the open calls of the asking tool, made by the asking agent (when the
// request names one), that are not already showing a prompt. Among them a
// call whose arguments name the approval's subject (the command, the
// changed file, the network target) wins, then the most recently announced
// one. The chosen card is marked as awaiting an answer until
// settleApprovalCard. attached is false when there is no candidate, and a
// fresh ID is returned.
func (t *turnState) approvalToolCallID(subject approvalSubject, ar agent.ShellApprovalRequest) (id acpsdk.ToolCallId, attached bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var best openToolCall
	bestMatches := false
	found := false
	for _, queue := range t.open {
		for _, call := range queue {
			if call.name != subject.tool {
				continue
			}
			if ar.AgentID != "" && call.agentID != ar.AgentID {
				continue
			}
			if t.awaiting[call.id] {
				continue
			}
			matches := t.callMatchesApproval(call, subject, ar)
			better := !found ||
				(matches && !bestMatches) ||
				(matches == bestMatches && call.seq > best.seq)
			if better {
				best, bestMatches, found = call, matches, true
			}
		}
	}
	if !found {
		return t.nextToolCallIDLocked("perm"), false
	}
	if t.awaiting == nil {
		t.awaiting = map[acpsdk.ToolCallId]bool{}
	}
	t.awaiting[best.id] = true
	return best.id, true
}

// callMatchesApproval reports whether an open call's arguments name what the
// approval is about. Arguments are read under every name the runtime
// accepts (commandArg, pathArg), and a relative path is resolved against
// the asking agent's directory, which for a sub-agent in a worktree is not
// the session's. Caller holds t.mu.
func (t *turnState) callMatchesApproval(call openToolCall, subject approvalSubject, ar agent.ShellApprovalRequest) bool {
	switch {
	case subject.network:
		spec, ok := networkTargets[subject.tool]
		if !ok || subject.target == "" {
			return false
		}
		return collapseSpaces(spec.fromArgs(call.args)) == collapseSpaces(subject.target)
	case subject.command:
		command := call.args.commandArg()
		return command != "" && collapseSpaces(command) == collapseSpaces(ar.Command)
	case ar.Change != nil:
		p := call.args.pathArg()
		if p == "" {
			return false
		}
		if !filepath.IsAbs(p) {
			base := ar.CWD
			if base == "" {
				base = t.cwd
			}
			p = filepath.Join(base, p)
		}
		return filepath.Clean(p) == filepath.Clean(ar.Change.Path)
	}
	return false
}

// collapseSpaces trims s and turns every run of whitespace into one space,
// the normalization the runtime applies to commands and network targets
// before asking (normalizeCommand in internal/agent).
func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// remembersApproval reports whether the runtime persists an "always allow"
// answer for this kind of approval (see the package comment above).
func remembersApproval(subject approvalSubject) bool {
	return subject.network || subject.command
}

// alwaysAllowLabel names the "always allow" option after what the runtime
// will remember: the command's segments, or the network tool's exact target
// (see networkTargets). Segments that are not simply the whole command (each
// side of a pipe, each line of a heredoc's body) are listed in the prompt's
// text (approvalContent), and the label points there.
func alwaysAllowLabel(subject approvalSubject, ar agent.ShellApprovalRequest) string {
	if !subject.network {
		switch {
		case !remembersOtherThanCommand(ar):
			return "Always allow this command"
		case len(ar.Segments) == 1:
			return "Always allow the command listed"
		default:
			return fmt.Sprintf("Always allow the %d commands listed", len(ar.Segments))
		}
	}
	if spec, ok := networkTargets[subject.tool]; ok {
		return "Always allow this " + spec.noun
	}
	return "Always allow this target"
}

// remembersOtherThanCommand reports whether "always allow" on a command
// approval remembers something other than exactly that command: more than
// one segment, or one that is not the whole command. Allowing
// "cat > notes.md <<'EOF'" for always remembers every line of the heredoc's
// body as a command of its own.
func remembersOtherThanCommand(ar agent.ShellApprovalRequest) bool {
	switch len(ar.Segments) {
	case 0:
		return false
	case 1:
		return ar.Segments[0] != agent.RememberedCommandKey(ar.Command)
	}
	return true
}

// approvalKind is the card kind for an approval with no open card.
func approvalKind(subject approvalSubject) acpsdk.ToolKind {
	if subject.network {
		return acpsdk.ToolKindFetch
	}
	return toolKind(subject.tool)
}

// approvalTitle is the card title for an approval with no open card.
// Every piece of text it takes from the request is escaped first
// (termtext.EscapeLines): clipLine folds whitespace, and would turn a no-break
// space, which a shell keeps inside a word, into the space that separates two.
func approvalTitle(subject approvalSubject, ar agent.ShellApprovalRequest) string {
	switch {
	case subject.network:
		return clipLine("Access the network: "+termtext.EscapeLines(subject.target), maxTitleRunes)
	case ar.Change != nil:
		verb := "Edit "
		if ar.Change.Created {
			verb = "Create "
		}
		return clipLine(verb+termtext.EscapeLines(ar.Change.Path), maxTitleRunes)
	case subject.command:
		return clipLine("Run "+termtext.EscapeLines(ar.Command), maxTitleRunes)
	}
	return clipLine(termtext.EscapeLines(ar.Command), maxTitleRunes)
}

// maxApprovalTextBytes bounds the command or diff text of one approval
// prompt (see the package comment: Content).
const maxApprovalTextBytes = remote.MaxApprovalFieldBytes

// approvalContent is what the prompt shows: the proposed change as a real
// diff when the runtime supplied one, else the whole unified diff as text;
// for anything that is not a file change, the whole command or network
// target; then the reason plus the command segments that still need
// approval. Text is escaped (see the package comment: Content).
func approvalContent(ar agent.ShellApprovalRequest) []acpsdk.ToolCallContent {
	var out []acpsdk.ToolCallContent
	switch {
	case ar.Change != nil:
		out = append(out, fileChangeContent([]agent.FileChange{*ar.Change})...)
		switch {
		case strings.TrimSpace(ar.Diff) == "":
		case fileChangeOmitted(*ar.Change):
			// The file is too large for a structured diff; the unified diff
			// the runtime computed still shows what changes.
			out = append(out, approvalTextBlock("diff", ar.Diff))
		case termtext.HasHidden(ar.Change.OldText) || termtext.HasHidden(ar.Change.NewText):
			// The editor draws the file's own text, invisible characters
			// and all; show the change once more with them named.
			out = append(out, acpsdk.ToolContent(acpsdk.TextBlock(hiddenCharsNote)))
			out = append(out, approvalTextBlock("diff", ar.Diff))
		}
	case strings.TrimSpace(ar.Diff) != "":
		out = append(out, approvalTextBlock("diff", ar.Diff))
	case subjectOf(ar).network:
		// The card's title and rawInput are clipped; this is the whole
		// target ("network <tool> <target>") the user is approving.
		out = append(out, approvalTextBlock("", ar.Command))
	case strings.TrimSpace(ar.Command) != "":
		// Likewise the whole command.
		out = append(out, approvalTextBlock("sh", ar.Command))
	}
	note := termtext.EscapeLines(strings.TrimSpace(ar.Reason))
	if len(ar.Segments) > 0 && !remembersOtherThanCommand(ar) {
		note = strings.TrimSpace(note + "\nneeds approval: " + termtext.EscapeLines(strings.Join(ar.Segments, " | ")))
	}
	if note != "" {
		out = append(out, acpsdk.ToolContent(acpsdk.TextBlock(clipBytes(note, maxToolTextBytes))))
	}
	if remembersOtherThanCommand(ar) {
		// What "Always allow" saves, one command per line and in full: the
		// choice approves each of them for good.
		out = append(out, acpsdk.ToolContent(acpsdk.TextBlock(fmt.Sprintf(
			"\"Always allow\" remembers these %d commands, and later runs them without asking:", len(ar.Segments)))))
		out = append(out, approvalTextBlock("", strings.Join(ar.Segments, "\n")))
	}
	return out
}

// hiddenCharsNote precedes the escaped copy of a file change whose text
// holds characters that do not show (approvalContent).
const hiddenCharsNote = "This change contains characters that do not show on screen " +
	"(control, bidi, zero-width or other invisible characters). " +
	"Here it is with each of them written out:"

// fileChangeOmitted reports whether fileChangeContent leaves change out of
// its structured diffs (texts dropped, or too large for one update).
func fileChangeOmitted(change agent.FileChange) bool {
	return change.TextOmitted || len(change.OldText)+len(change.NewText) > maxDiffBytesPerUpdate
}

// approvalTextBlock is text as a fenced code block of language lang, every
// character that would not show as itself written out
// (termtext.EscapeLines), cut to maxApprovalTextBytes with an explicit note
// after the fence when it is longer. The fence is longer than any run of
// backticks in text, so the text cannot close it early and show part of
// itself as prose.
func approvalTextBlock(lang, text string) acpsdk.ToolCallContent {
	body, cut := remote.ClipApprovalText(termtext.EscapeLines(strings.TrimRight(text, "\n")), maxApprovalTextBytes)
	note := ""
	if cut {
		// ClipApprovalText ends a cut text with its note; move the note out
		// of the code block so it reads as a statement, not as code.
		i := strings.LastIndex(body, "\n[truncated:")
		body, note = body[:i], body[i+1:]
	}
	fence := strings.Repeat("`", max(3, longestRun(body, '`')+1))
	block := fence + lang + "\n" + body + "\n" + fence
	if note != "" {
		block += "\n\n" + note
	}
	return acpsdk.ToolContent(acpsdk.TextBlock(block))
}

// longestRun is the length of the longest run of c in s.
func longestRun(s string, c byte) int {
	best, run := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			run++
			best = max(best, run)
		} else {
			run = 0
		}
	}
	return best
}
