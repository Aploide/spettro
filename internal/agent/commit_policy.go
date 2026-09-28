package agent

import (
	"strings"
)

// spettroCommitName/Email is Spettro's git identity: the author of commits it
// makes autonomously (subagent workspace fallback commits) and the co-author
// credited on commits it makes on the user's behalf.
//
// Keep the email in sync with internal/checkpoint/checkpoint.go (shadow-repo
// commits) — that package cannot import this one.
const (
	spettroCommitName  = "Spettro"
	spettroCommitEmail = "spettro@eyed.to"
)

// spettroCoAuthorTrailer is the mandatory Co-Authored-By trailer that Spettro
// guarantees on every commit it makes — directly via LLMCommitter, or
// indirectly when an LLM agent issues `git commit` through the bash tool.
//
// Keep this string in sync with internal/agent/committer.go and
// internal/tui/model.go.
const spettroCoAuthorTrailer = "Co-Authored-By: " + spettroCommitName + " <" + spettroCommitEmail + ">"

// commitTrailerFlag is the formatted `--trailer "..."` token we inject into
// rewritten git commit invocations. Single quotes keep the angle brackets safe
// from shell expansion inside `bash -lc`.
var commitTrailerFlag = " --trailer '" + spettroCoAuthorTrailer + "'"

// EnforceCommitCoAuthor rewrites a shell command so that every `git commit`
// invocation inside it carries the mandatory Spettro Co-Authored-By trailer.
//
// The transformation is:
//
//   - idempotent — segments that already mention `Co-Authored-By: Spettro` are
//     left alone, so chaining the rewriter twice (or a user-provided trailer
//     plus an auto-injected one) does not duplicate the line.
//
//   - segment-aware — multi-command strings like
//     `git status && git commit -m 'x' && git push`
//     get the trailer appended to the `git commit` segment only.
//
//   - quote-, subshell-, and heredoc-safe — `;`, `|`, `&&`, `||`, and
//     newlines inside `'...'`, `"..."`, `$(...)`, or a heredoc body are NOT
//     treated as separators, and heredoc bodies are fully opaque: quote
//     characters in the commit-message text never leak into the parser state.
//
//   - heredoc-placement-aware — for `git commit -F - <<'EOF' ...` the flag is
//     inserted on the command line (before the body starts), while for the
//     `git commit -m "$(cat <<'EOF' ... EOF\n)"` idiom it lands after the
//     closing `)"` — both spots git actually parses as arguments.
//
//   - tolerant of common git option prefixes (`-C dir`, `--git-dir=...`,
//     `-c key=val`, leading `env VAR=value`) and excludes plumbing variants
//     like `git commit-tree` and `git commit-graph`.
//
// The function is intentionally conservative: when the command shape is
// ambiguous (e.g. dynamic `$(...)` invocations, sub-shells, piping git
// output through another tool, or an unterminated heredoc), it falls back to
// leaving the command as-is. The accompanying prompt rules in agents/git.md
// still require the trailer explicitly, so the LLM is the second line of
// defence.
func EnforceCommitCoAuthor(command string) string {
	if command == "" {
		return command
	}
	segments, ok := splitShellSegmentsWithRanges(command)
	if !ok || len(segments) == 0 {
		return command
	}
	var out strings.Builder
	out.Grow(len(command) + len(commitTrailerFlag))
	cursor := 0
	for _, seg := range segments {
		out.WriteString(command[cursor:seg.start])
		body := command[seg.start:seg.end]
		if commitSegmentNeedsTrailer(body) {
			pos := len(body)
			if seg.injectAt >= seg.start && seg.injectAt < seg.end {
				pos = seg.injectAt - seg.start
			}
			leading := strings.TrimRight(body[:pos], " \t")
			out.WriteString(leading)
			out.WriteString(commitTrailerFlag)
			out.WriteString(body[len(leading):])
		} else {
			out.WriteString(body)
		}
		cursor = seg.end
	}
	out.WriteString(command[cursor:])
	return out.String()
}

// shellSegmentRange records the [start, end) bounds of one top-level shell
// segment inside the original command string. End points to the first index
// of the separator (or len(command) for the trailing segment). injectAt,
// when >= 0, is the absolute index where injected flags must go instead of
// the segment end: the newline that opens a top-level heredoc body. Anything
// appended after that newline would become literal heredoc text.
type shellSegmentRange struct {
	start    int
	end      int
	injectAt int
}

// pendingHeredoc is a heredoc operator whose body has not started yet:
// `<<DELIM` (or `<<-DELIM`) was seen, and the body begins at the next
// unquoted newline.
type pendingHeredoc struct {
	delim     string
	stripTabs bool
}

// splitShellSegmentsWithRanges is a position-preserving variant of
// splitShellCommandSegments. It walks the command character by character,
// tracks quoting, `$(...)` nesting, and heredocs, and emits the indices of
// each top-level segment. Unlike the existing splitter (which returns trimmed
// segment text) this keeps original whitespace and lets callers patch
// segments back into the source string without losing operators.
//
// Two constructs get real modeling rather than best-effort skipping, because
// commit messages routinely contain quote characters that would otherwise
// corrupt the parser state:
//
//   - `$(...)` opens a fresh quoting context even inside `"..."` (bash
//     semantics), restored when the subshell closes.
//   - `<<DELIM` / `<<-DELIM` heredocs: the body — every line up to the
//     delimiter line — is consumed opaquely. `<<<` here-strings are not
//     heredocs and are left to normal lexing.
//
// ok is false when the command could not be confidently parsed (unterminated
// heredoc or malformed heredoc delimiter); callers must then leave the
// command untouched.
func splitShellSegmentsWithRanges(command string) (segments []shellSegmentRange, ok bool) {
	var (
		inSingle, inDouble, esc bool
		// doubleStack saves inDouble at each `$(` so the enclosing quote
		// state survives nested substitutions; its length is the depth.
		doubleStack []bool
		pending     []pendingHeredoc
		start       = 0
		injectAt    = -1
	)
	flush := func(endExclusive int) {
		segments = append(segments, shellSegmentRange{start: start, end: endExclusive, injectAt: injectAt})
		injectAt = -1
	}
	for i := 0; i < len(command); i++ {
		ch := command[i]
		if esc {
			esc = false
			continue
		}
		switch ch {
		case '\\':
			esc = true
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '$':
			// Forward-looking `$(` detection: an escaped `\$` is consumed by
			// the esc branch above and never reaches this case, so a literal
			// `\$(cat ...` inside a quoted commit message cannot open a
			// phantom subshell (which would then misread message text like
			// `<<'EOF'` as a live heredoc and poison the whole parse).
			if !inSingle && i+1 < len(command) && command[i+1] == '(' {
				doubleStack = append(doubleStack, inDouble)
				inDouble = false
				i++
			}
		case ')':
			if !inSingle && !inDouble && len(doubleStack) > 0 {
				inDouble = doubleStack[len(doubleStack)-1]
				doubleStack = doubleStack[:len(doubleStack)-1]
			}
		case '<':
			if inSingle || inDouble || i+1 >= len(command) || command[i+1] != '<' {
				break
			}
			if i+2 < len(command) && command[i+2] == '<' {
				// `<<<` here-string: no body to skip.
				i += 2
				break
			}
			j := i + 2
			stripTabs := false
			if j < len(command) && command[j] == '-' {
				stripTabs = true
				j++
			}
			for j < len(command) && (command[j] == ' ' || command[j] == '\t') {
				j++
			}
			delim, n, wordOK := lexHeredocDelimiter(command[j:])
			if !wordOK {
				return nil, false
			}
			pending = append(pending, pendingHeredoc{delim: delim, stripTabs: stripTabs})
			i = j + n - 1
		case ';':
			if !inSingle && !inDouble && len(doubleStack) == 0 {
				flush(i)
				start = i + 1
			}
		case '\n':
			if inSingle || inDouble {
				break
			}
			if len(pending) > 0 {
				// This newline opens the queued heredoc bodies. For a
				// top-level heredoc it is also the last place a flag can
				// legally be inserted into the command line.
				if len(doubleStack) == 0 && injectAt < 0 {
					injectAt = i
				}
				pos := i
				for len(pending) > 0 && pos < len(command) {
					lineStart := pos + 1
					lineEnd := lineStart + strings.IndexByte(command[lineStart:], '\n')
					if lineEnd < lineStart {
						lineEnd = len(command)
					}
					line := command[lineStart:lineEnd]
					if pending[0].stripTabs {
						line = strings.TrimLeft(line, "\t")
					}
					if line == pending[0].delim {
						pending = pending[1:]
					}
					pos = lineEnd
				}
				if len(pending) > 0 {
					return nil, false
				}
				// Reprocess the newline that terminates the delimiter line
				// (if any): it separates segments like a normal newline.
				i = pos - 1
				break
			}
			if len(doubleStack) == 0 {
				flush(i)
				start = i + 1
			}
		case '|':
			if !inSingle && !inDouble && len(doubleStack) == 0 {
				flush(i)
				if i+1 < len(command) && command[i+1] == '|' {
					i++
				}
				start = i + 1
			}
		case '&':
			if !inSingle && !inDouble && len(doubleStack) == 0 && i+1 < len(command) && command[i+1] == '&' {
				flush(i)
				i++
				start = i + 1
			}
		}
	}
	if len(pending) > 0 {
		// `<<EOF` seen but its body never started (no newline follows).
		return nil, false
	}
	if start <= len(command) {
		segments = append(segments, shellSegmentRange{start: start, end: len(command), injectAt: injectAt})
	}
	return segments, true
}

// lexHeredocDelimiter reads the heredoc delimiter word at the start of s,
// stripping the shell quoting bash allows there (`'EOF'`, `"EOF"`, `\EOF`,
// or bare, in any mix). It returns the unquoted delimiter, the number of
// bytes consumed, and ok=false when the word is empty or a quote is left
// unterminated.
func lexHeredocDelimiter(s string) (delim string, n int, ok bool) {
	var b strings.Builder
	i := 0
scan:
	for i < len(s) {
		switch ch := s[i]; ch {
		case '\'', '"':
			close := strings.IndexByte(s[i+1:], ch)
			if close < 0 {
				return "", 0, false
			}
			b.WriteString(s[i+1 : i+1+close])
			i += close + 2
		case '\\':
			if i+1 >= len(s) {
				return "", 0, false
			}
			b.WriteByte(s[i+1])
			i += 2
		case ' ', '\t', '\n', ';', '&', '|', '<', '>', '(', ')':
			break scan
		default:
			b.WriteByte(ch)
			i++
		}
	}
	if b.Len() == 0 {
		return "", 0, false
	}
	return b.String(), i, true
}

// commitSegmentNeedsTrailer returns true when `seg` invokes `git commit`
// without already mentioning the Spettro co-author trailer.
func commitSegmentNeedsTrailer(seg string) bool {
	if !isGitCommitInvocation(seg) {
		return false
	}
	// Idempotent: don't double-add if the user (or a previous pass) already
	// included the trailer somewhere in the segment text. We match a generous
	// "Co-Authored-By: Spettro" prefix so any variation of email/case still
	// counts.
	if strings.Contains(seg, "Co-Authored-By: Spettro") {
		return false
	}
	return true
}

// isGitCommitInvocation answers "does this segment run `git commit`?". We
// lex the segment shell-style (quotes are recognised) and skip leading env
// assignments + git's own multi-token global options before checking the
// subcommand. Plumbing variants like `commit-tree` / `commit-graph` do NOT
// match — only the porcelain `commit` does.
func isGitCommitInvocation(seg string) bool {
	tokens := lexShellTokens(seg)
	idx := 0
	for idx < len(tokens) {
		t := tokens[idx]
		if t == "" {
			idx++
			continue
		}
		// Strip a leading `env` wrapper. Anything that's not `git`/`/.../git`
		// after env assignments means this segment does not invoke git at all.
		if t == "env" {
			idx++
			continue
		}
		if !looksLikeEnvAssignment(t) {
			break
		}
		idx++
	}
	if idx >= len(tokens) {
		return false
	}
	cmd := tokens[idx]
	if cmd != "git" && !strings.HasSuffix(cmd, "/git") {
		return false
	}
	idx++
	for idx < len(tokens) {
		f := tokens[idx]
		if !strings.HasPrefix(f, "-") {
			break
		}
		// Skip git global options that take a separate value.
		switch f {
		case "-C", "--git-dir", "--work-tree", "-c", "--namespace", "--super-prefix", "--exec-path":
			idx += 2
			continue
		}
		// Inline `--key=val` style — only the flag token to skip.
		idx++
	}
	if idx >= len(tokens) {
		return false
	}
	return tokens[idx] == "commit"
}

// looksLikeEnvAssignment matches the `VAR=value` shape — i.e. an unquoted
// identifier followed by `=`. Anything containing slashes, quotes, or shell
// metacharacters is rejected so we don't mistake paths or strings for env
// assignments.
func looksLikeEnvAssignment(t string) bool {
	eq := strings.IndexByte(t, '=')
	if eq <= 0 {
		return false
	}
	for i := range eq {
		ch := t[i]
		switch {
		case ch >= 'A' && ch <= 'Z':
		case ch >= 'a' && ch <= 'z':
		case ch >= '0' && ch <= '9':
		case ch == '_':
		default:
			return false
		}
	}
	return true
}

// lexShellTokens splits seg into shell-style tokens respecting `'...'`,
// `"..."`, and backslash escapes. Quotes are preserved in the returned tokens
// only when they were already part of the inner text — we strip the outer
// quote characters. Subshell starts (`$(`) are treated as opaque single tokens
// so we never mistake their contents for the actual command.
func lexShellTokens(seg string) []string {
	var tokens []string
	var (
		cur                     strings.Builder
		inSingle, inDouble, esc bool
		subDepth                int
		started                 bool
	)
	flush := func() {
		if started {
			tokens = append(tokens, cur.String())
			cur.Reset()
			started = false
		}
	}
	for i := 0; i < len(seg); i++ {
		ch := seg[i]
		if esc {
			cur.WriteByte(ch)
			esc = false
			started = true
			continue
		}
		switch ch {
		case '\\':
			esc = true
		case '\'':
			if !inDouble {
				inSingle = !inSingle
				started = true
				continue
			}
			cur.WriteByte(ch)
			started = true
		case '"':
			if !inSingle {
				inDouble = !inDouble
				started = true
				continue
			}
			cur.WriteByte(ch)
			started = true
		case '$':
			if !inSingle && i+1 < len(seg) && seg[i+1] == '(' {
				subDepth++
				cur.WriteByte(ch)
				started = true
				continue
			}
			cur.WriteByte(ch)
			started = true
		case '(':
			cur.WriteByte(ch)
			started = true
		case ')':
			if subDepth > 0 {
				subDepth--
			}
			cur.WriteByte(ch)
			started = true
		case ' ', '\t':
			if inSingle || inDouble || subDepth > 0 {
				cur.WriteByte(ch)
				started = true
				continue
			}
			flush()
		default:
			cur.WriteByte(ch)
			started = true
		}
	}
	flush()
	return tokens
}
