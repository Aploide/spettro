package compact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"spettro/internal/provider"
)

// promptOf returns the summarizer prompt of a request: its single user
// message (the instructions ride in System).
func promptOf(req provider.Request) string {
	if len(req.Messages) != 1 || req.Messages[0].Role != provider.RoleUser || req.Prompt != "" {
		return ""
	}
	return req.Messages[0].Content
}

// session builds a realistic multi-turn, tool-heavy conversation: a first
// task (with its session snapshot), then for each turn a user request and a
// run of tool exchanges (some with several parallel calls, some with large
// spooled outputs, some failing), a steering message mid-run, and a final
// answer.
func session(turns, exchangesPerTurn int) []provider.Message {
	msgs := []provider.Message{{
		Role:           provider.RoleUser,
		Content:        "Task:\nAdd retry support to the HTTP client and make sure `go test ./...` passes.",
		SessionContext: "env: darwin, cwd /repo",
		LoadedTools:    []string{"save-memory"},
	}}
	id := 0
	for t := range turns {
		if t > 0 {
			msgs = append(msgs, provider.Message{Role: provider.RoleUser, Content: fmt.Sprintf("Task:\nfollow-up request number %d", t)})
		}
		for e := range exchangesPerTurn {
			calls := 1 + (e % 3)
			var tcs []provider.NativeTool
			var trs []provider.ToolResult
			for c := range calls {
				id++
				cid := fmt.Sprintf("call_%d", id)
				switch (id + c) % 4 {
				case 0:
					tcs = append(tcs, provider.NativeTool{ID: cid, Name: "file-edit", Args: json.RawMessage(fmt.Sprintf(`{"path":"internal/http/client%d.go","old_string":"a","new_string":%q}`, id%5, strings.Repeat("retry ", 600)))})
					trs = append(trs, provider.ToolResult{ID: cid, Name: "file-edit", Output: "edited"})
				case 1:
					tcs = append(tcs, provider.NativeTool{ID: cid, Name: "shell-exec", Args: json.RawMessage(`{"command":"go test ./..."}`)})
					trs = append(trs, provider.ToolResult{ID: cid, Name: "shell-exec", Output: strings.Repeat("ok line\n", 800) + "FAIL TestRetry (client_test.go:42)", IsErr: true, SpoolID: fmt.Sprintf("spool:%d", id)})
				case 2:
					tcs = append(tcs, provider.NativeTool{ID: cid, Name: "file-read", Args: json.RawMessage(`{"path":"internal/http/client.go"}`)})
					trs = append(trs, provider.ToolResult{ID: cid, Name: "file-read", Output: strings.Repeat("func x() {}\n", 400), SpoolID: fmt.Sprintf("spool:%d", id)})
				default:
					tcs = append(tcs, provider.NativeTool{ID: cid, Name: "grep", Args: json.RawMessage(`{"pattern":"Retry"}`)})
					trs = append(trs, provider.ToolResult{ID: cid, Name: "grep", Output: "client.go:12: Retry"})
				}
			}
			msgs = append(msgs,
				provider.Message{Role: provider.RoleAssistant, Content: "working", ToolCalls: tcs},
				provider.Message{Role: provider.RoleUser, ToolResults: trs, FileStamps: []provider.FileStamp{{Path: fmt.Sprintf("/repo/f%d.go", id), Seen: fmt.Sprintf("%064d", id)}}},
			)
			if e == exchangesPerTurn/2 {
				msgs = append(msgs, provider.Message{Role: provider.RoleUser, Content: fmt.Sprintf("[user steering] also cover the timeout case (turn %d)", t)})
			}
		}
		msgs = append(msgs, provider.Message{Role: provider.RoleAssistant, Content: fmt.Sprintf("Turn %d done.", t)})
	}
	return msgs
}

// TestCompactAlwaysProducesValidPairing sweeps histories, windows and modes:
// whatever compaction does, the result must be a history providers accept.
func TestCompactAlwaysProducesValidPairing(t *testing.T) {
	for _, turns := range []int{1, 2, 4} {
		for _, ex := range []int{1, 3, 7, 12} {
			msgs := session(turns, ex)
			// The final answer is cut off in some cases so the history ends
			// on a results turn, as it does mid-run.
			variants := [][]provider.Message{msgs, msgs[:len(msgs)-1]}
			for vi, h := range variants {
				if err := ValidatePairing(h); err != nil {
					t.Fatalf("fixture invalid: %v", err)
				}
				for _, window := range []int{2000, 8000, 30000, 200000} {
					for _, force := range []bool{false, true} {
						for _, keep := range []int{0, 1, 5} {
							name := fmt.Sprintf("turns=%d ex=%d v=%d window=%d force=%v keep=%d", turns, ex, vi, window, force, keep)
							res, err := Compact(context.Background(), fakeSend("## Goal\nretries"), h, Params{Window: window, Force: force, KeepToolExchanges: keep})
							if err != nil {
								t.Fatalf("%s: %v", name, err)
							}
							if err := ValidatePairing(res.Messages); err != nil {
								t.Fatalf("%s: invalid pairing: %v", name, err)
							}
							if !reflect.DeepEqual(res.Messages[0].Content, h[0].Content) || res.Messages[0].SessionContext != h[0].SessionContext {
								t.Fatalf("%s: the original task was not kept verbatim", name)
							}
						}
					}
				}
			}
		}
	}
}

// The original task, the latest user messages and the most recent tool
// exchanges all survive a summarizing compaction verbatim.
func TestCompactPreservesTaskUserMessagesAndRecentExchanges(t *testing.T) {
	msgs := session(3, 8)
	var req provider.Request
	send := func(_ context.Context, r provider.Request) (provider.Response, error) {
		req = r
		return provider.Response{Content: "## Goal\nretry support"}, nil
	}
	res, err := Compact(context.Background(), send, msgs, Params{Window: 1_000_000, Force: true, KeepToolExchanges: 3})
	if err != nil || !res.Summarized {
		t.Fatalf("expected a summary, res=%+v err=%v", res.Summarized, err)
	}
	out := res.Messages

	if out[0].Content != msgs[0].Content || out[0].SessionContext != msgs[0].SessionContext {
		t.Fatal("original task changed")
	}
	if !slices.Equal(out[0].LoadedTools, msgs[0].LoadedTools) {
		t.Fatalf("loaded tools record lost: %v", out[0].LoadedTools)
	}
	if !strings.HasPrefix(out[1].Content, SummaryHeader) || !strings.Contains(out[1].Content, "retry support") {
		t.Fatalf("summary turn missing: %q", out[1].Content)
	}

	// The latest user request ("follow-up request number 2") sits before the
	// kept tool exchanges, so it had to be lifted out of the summarized span.
	var lastUser string
	for _, m := range msgs {
		if isUserText(m) {
			lastUser = m.Content
		}
	}
	found := false
	for _, m := range out[2:] {
		if m.Content == lastUser && m.Role == provider.RoleUser {
			found = true
		}
	}
	if !found {
		t.Fatalf("latest user message %q not kept verbatim", lastUser)
	}
	if !strings.Contains(lastUser, "steering") && !strings.Contains(lastUser, "follow-up") {
		t.Fatalf("fixture: unexpected last user message %q", lastUser)
	}

	// The last three tool exchanges are intact, byte for byte.
	var wantEx, gotEx [][2]provider.Message
	collect := func(ms []provider.Message) [][2]provider.Message {
		var ex [][2]provider.Message
		for i := 0; i+1 < len(ms); i++ {
			if isToolCall(ms[i]) {
				ex = append(ex, [2]provider.Message{ms[i], ms[i+1]})
			}
		}
		return ex
	}
	wantEx, gotEx = collect(msgs), collect(out)
	if len(gotEx) < 3 {
		t.Fatalf("kept %d tool exchanges, want at least 3", len(gotEx))
	}
	for i := 1; i <= 3; i++ {
		if !reflect.DeepEqual(wantEx[len(wantEx)-i], gotEx[len(gotEx)-i]) {
			t.Fatalf("tool exchange -%d was altered", i)
		}
	}

	// The summarizer got the structured template and the full edit args
	// (the old renderer cut them at 200 chars).
	for _, section := range []string{"## Goal", "## Decisions and findings", "## Files modified", "## Current state", "## Next steps"} {
		if !strings.Contains(req.System, section) {
			t.Fatalf("summarizer instructions lack %q", section)
		}
	}
	if !strings.Contains(promptOf(req), strings.Repeat("retry ", 400)) {
		t.Fatal("edit arguments were truncated in the summarizer transcript")
	}
	if !strings.Contains(promptOf(req), "FAIL TestRetry (client_test.go:42)") {
		t.Fatal("the failing test's error text did not reach the summarizer")
	}
	if !strings.Contains(promptOf(req), "<files-modified-by-edit-tools>") || !strings.Contains(out[1].Content, "internal/http/client") {
		t.Fatal("the deterministic modified-files list is missing")
	}
	// Stamps from the summarized turns ride on the first message.
	if len(out[0].FileStamps) == 0 {
		t.Fatal("file stamps of the summarized turns were dropped")
	}
}

// Multi-turn: the carried history's first message is the first turn ever,
// but the current request is the latest user message, which must survive.
func TestCompactKeepsCurrentRequestInCarriedSession(t *testing.T) {
	msgs := session(2, 10)
	current := provider.Message{Role: provider.RoleUser, Content: "Task:\nnow rename Retry to Backoff everywhere"}
	msgs = append(msgs, current)
	// A long run on the current request pushes it far from the tail.
	msgs = append(msgs, session(1, 10)[1:]...)
	res, err := Compact(context.Background(), fakeSend("## Goal\nrename"), msgs, Params{Window: 20000})
	if err != nil || !res.Summarized {
		t.Fatalf("expected summarization, summarized=%v err=%v", res.Summarized, err)
	}
	var kept bool
	for _, m := range res.Messages[1:] {
		if m.Content == current.Content {
			kept = true
		}
	}
	if !kept {
		t.Fatal("the current request was summarized away")
	}
	if res.Messages[0].Content != msgs[0].Content {
		t.Fatal("the first task was not kept")
	}
}

// Pruning comes first: with a tool-heavy history whose bulk is old spooled
// outputs, the stubs alone get under the trigger and no summarizer call is
// made; the conversation keeps every turn.
func TestCompactPrunesBeforeSummarizing(t *testing.T) {
	msgs := session(1, 30)
	send := func(context.Context, provider.Request) (provider.Response, error) {
		t.Fatal("the summarizer must not run when pruning suffices")
		return provider.Response{}, nil
	}
	before := EstimateHistoryTokens("", msgs)
	// Window sized so the raw history crosses the auto threshold.
	window := before + 20000 - before/10
	res, err := Compact(context.Background(), send, msgs, Params{Window: window})
	if err != nil {
		t.Fatal(err)
	}
	if res.Summarized || res.Pruned == 0 {
		t.Fatalf("expected pruning only, got %+v", res)
	}
	if len(res.Messages) != len(msgs) {
		t.Fatal("pruning must not drop turns")
	}
	if err := ValidatePairing(res.Messages); err != nil {
		t.Fatal(err)
	}
	var stub string
	for _, m := range res.Messages {
		for _, tr := range m.ToolResults {
			if strings.HasPrefix(tr.Output, elidedPrefix) {
				stub = tr.Output
			}
		}
	}
	if !strings.HasPrefix(stub, "[output elided: ") || !strings.Contains(stub, "chars, spool:") {
		t.Fatalf("stub format: %q", stub)
	}
	// The newest outputs are spared.
	last := res.Messages[len(res.Messages)-2]
	for i, tr := range last.ToolResults {
		if tr.Output != msgs[len(msgs)-2].ToolResults[i].Output {
			t.Fatal("the most recent tool outputs were pruned")
		}
	}
	if after := EstimateHistoryTokens("", res.Messages); after >= before {
		t.Fatalf("pruning did not shrink the history: %d >= %d", after, before)
	}
}

// Old oversized tool-call arguments are elided to a marker but stay valid
// JSON objects with the same keys.
func TestPruneAllElidesLargeArgsAsValidJSON(t *testing.T) {
	msgs := session(1, 6)
	out, n := pruneToolOutputs(msgs, len(msgs)-2, pruneAll, 0)
	if n == 0 {
		t.Fatal("nothing pruned")
	}
	var saw bool
	for _, m := range out {
		for _, tc := range m.ToolCalls {
			if tc.Name != "file-edit" {
				continue
			}
			var a map[string]any
			if err := json.Unmarshal(tc.Args, &a); err != nil {
				t.Fatalf("args no longer valid JSON: %v", err)
			}
			if s, _ := a["new_string"].(string); strings.HasPrefix(s, "[elided from history:") {
				saw = true
				if a["path"] == nil {
					t.Fatal("elision dropped other keys")
				}
			}
		}
	}
	if !saw {
		t.Fatal("large file-edit args not elided")
	}
	if err := ValidatePairing(out); err != nil {
		t.Fatal(err)
	}
}

// A forced compaction for overflow recovery still shrinks the history when
// the summarizer fails, with a summary extracted from the transcript.
func TestCompactForcedFallsBackWhenSummarizerFails(t *testing.T) {
	msgs := session(2, 6)
	fail := func(context.Context, provider.Request) (provider.Response, error) {
		return provider.Response{}, errors.New("503 overloaded")
	}
	if _, err := Compact(context.Background(), fail, msgs, Params{Window: 1_000_000, Force: true}); err == nil {
		t.Fatal("without ExtractiveFallback (an explicit /compact) the failure must be reported")
	}
	res, err := Compact(context.Background(), fail, msgs, Params{Window: 1_000_000, Force: true, ExtractiveFallback: true})
	if err != nil || !res.Summarized || !res.Fallback {
		t.Fatalf("expected a fallback summary, res=%+v err=%v", res, err)
	}
	if len(res.Messages) >= len(msgs) {
		t.Fatal("history did not shrink")
	}
	sum := res.Messages[1].Content
	for _, want := range []string{"## Files modified", "internal/http/client", "## Last errors", "FAIL TestRetry", "go test ./..."} {
		if !strings.Contains(sum, want) {
			t.Fatalf("fallback summary lacks %q:\n%s", want, sum)
		}
	}
	if err := ValidatePairing(res.Messages); err != nil {
		t.Fatal(err)
	}
	// An automatic pass reports the failure instead (the caller counts it).
	if _, err := Compact(context.Background(), fail, msgs, Params{Window: 2000}); err == nil {
		t.Fatal("an automatic pass must surface a summarizer failure")
	}
}

// Re-compacting merges the previous summary instead of carrying it as a user
// message or dropping it.
func TestRecompactionMergesPreviousSummary(t *testing.T) {
	first, err := Compact(context.Background(), fakeSend("FIRST SUMMARY FACT"), session(2, 6), Params{Window: 1_000_000, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	msgs := append(first.Messages, session(1, 8)[1:]...)
	var prompt string
	send := func(_ context.Context, r provider.Request) (provider.Response, error) {
		prompt = promptOf(r)
		return provider.Response{Content: "SECOND SUMMARY"}, nil
	}
	second, err := Compact(context.Background(), send, msgs, Params{Window: 1_000_000, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "<previous-summary>") || !strings.Contains(prompt, "FIRST SUMMARY FACT") {
		t.Fatal("the previous summary was not handed to the summarizer")
	}
	n := 0
	for _, m := range second.Messages {
		if isSummary(m) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want exactly one summary turn, got %d", n)
	}
	if newestSummaryText(second.Messages) != "" && !strings.Contains(newestSummaryText(second.Messages), "SECOND SUMMARY") {
		t.Fatal("the newest summary is not the second one")
	}
}

// A forced pass on a history of huge reads keeps only the latest exchange
// verbatim once the tail would crowd the window; shrinkTail (the last
// resort after summarizing) stubs everything before the latest exchange.
func TestCompactForcedHugeTail(t *testing.T) {
	big := strings.Repeat("z", 50000)
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "task"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.NativeTool{{ID: "a", Name: "file-read"}}},
		{Role: provider.RoleUser, ToolResults: []provider.ToolResult{{ID: "a", Name: "file-read", Output: big, SpoolID: "spool:1"}}},
		{Role: provider.RoleAssistant, ToolCalls: []provider.NativeTool{{ID: "b", Name: "file-read"}}},
		{Role: provider.RoleUser, ToolResults: []provider.ToolResult{{ID: "b", Name: "file-read", Output: big, SpoolID: "spool:2"}}},
	}
	res, err := Compact(context.Background(), fakeSend("s"), msgs, Params{Window: 8000, Force: true})
	if err != nil || !res.Summarized {
		t.Fatalf("expected the older exchange to be summarized, res=%+v err=%v", res, err)
	}
	if last := res.Messages[len(res.Messages)-1]; len(last.ToolResults) != 1 || last.ToolResults[0].Output != big {
		t.Fatal("the latest exchange must stay verbatim")
	}
	if err := ValidatePairing(res.Messages); err != nil {
		t.Fatal(err)
	}

	out, n := shrinkTail(msgs)
	if n != 1 || !strings.HasPrefix(out[2].ToolResults[0].Output, elidedPrefix) || out[4].ToolResults[0].Output != big {
		t.Fatalf("shrinkTail must stub all but the latest exchange (n=%d)", n)
	}
}

func TestValidatePairing(t *testing.T) {
	call := func(ids ...string) provider.Message {
		m := provider.Message{Role: provider.RoleAssistant}
		for _, id := range ids {
			m.ToolCalls = append(m.ToolCalls, provider.NativeTool{ID: id, Name: "ls"})
		}
		return m
	}
	results := func(ids ...string) provider.Message {
		m := provider.Message{Role: provider.RoleUser}
		for _, id := range ids {
			m.ToolResults = append(m.ToolResults, provider.ToolResult{ID: id, Name: "ls", Output: "x"})
		}
		return m
	}
	user := provider.Message{Role: provider.RoleUser, Content: "hi"}
	cases := []struct {
		name string
		msgs []provider.Message
		ok   bool
	}{
		{"valid", []provider.Message{user, call("a", "b"), results("b", "a")}, true},
		{"orphan results", []provider.Message{user, results("a")}, false},
		{"unanswered call at end", []provider.Message{user, call("a")}, false},
		{"missing one result", []provider.Message{user, call("a", "b"), results("a")}, false},
		{"wrong id", []provider.Message{user, call("a"), results("z")}, false},
		{"text between call and results", []provider.Message{user, call("a"), user, results("a")}, false},
		{"starts with assistant", []provider.Message{{Role: provider.RoleAssistant, Content: "x"}, user}, false},
	}
	for _, c := range cases {
		err := ValidatePairing(c.msgs)
		if (err == nil) != c.ok {
			t.Fatalf("%s: err = %v", c.name, err)
		}
		if c.ok {
			continue
		}
		if c.name == "starts with assistant" {
			continue // not a pairing problem RepairPairing can fix
		}
		fixed := RepairPairing(c.msgs)
		if err := ValidatePairing(fixed); err != nil {
			t.Fatalf("%s: repair left %v", c.name, err)
		}
	}
	valid := []provider.Message{user, call("a"), results("a")}
	if got := RepairPairing(valid); &got[0] != &valid[0] {
		t.Fatal("repairing a valid history must return it unchanged")
	}
}

func TestTailStartNeverOpensOnResults(t *testing.T) {
	msgs := session(2, 9)
	for keep := 0; keep <= 6; keep++ {
		for minTail := 1; minTail <= 6; minTail++ {
			s := tailStart(msgs, keep, minTail)
			if s < 1 || s > len(msgs) {
				t.Fatalf("keep=%d min=%d: start %d out of range", keep, minTail, s)
			}
			if s < len(msgs) && isToolResults(msgs[s]) {
				t.Fatalf("keep=%d min=%d: tail opens on a results turn", keep, minTail)
			}
		}
	}
}

func TestHeadTailIsUTF8Safe(t *testing.T) {
	s := strings.Repeat("é", 5000)
	got := headTail(s, 1001)
	if !strings.Contains(got, "chars omitted") {
		t.Fatal("not shortened")
	}
	for _, part := range strings.Split(got, "\n") {
		if strings.ContainsRune(part, '\uFFFD') {
			t.Fatal("cut inside a rune")
		}
	}
	if truncateStr(s, 7) == "" || strings.ContainsRune(truncateStr(s, 7), '\uFFFD') {
		t.Fatal("truncateStr cut inside a rune")
	}
}

// newestSummaryText returns the text of the newest compaction summary in
// msgs (without its header), or "" when msgs holds none.
func newestSummaryText(msgs []provider.Message) string {
	for _, m := range slices.Backward(msgs) {
		if isSummary(m) {
			return strings.TrimSpace(strings.TrimPrefix(m.Content, SummaryHeader))
		}
	}
	return ""
}
