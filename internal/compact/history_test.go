package compact

import (
	"context"
	"strings"
	"testing"

	"spettro/internal/provider"
)

func fakeSend(summary string) SendFunc {
	return func(_ context.Context, _ provider.Request) (provider.Response, error) {
		return provider.Response{Content: summary}, nil
	}
}

func msgsOfLen(n int) []provider.Message {
	msgs := make([]provider.Message, 0, n)
	for i := range n {
		role := provider.RoleUser
		if i%2 == 1 {
			role = provider.RoleAssistant
		}
		msgs = append(msgs, provider.Message{Role: role, Content: strings.Repeat("x", 400)})
	}
	return msgs
}

func TestCompactHistoryNoOpUnderThreshold(t *testing.T) {
	msgs := msgsOfLen(10)
	out, did, err := CompactHistory(context.Background(), fakeSend("summary"), "", msgs, 1_000_000, false)
	if err != nil || did {
		t.Fatalf("expected no-op under threshold, did=%v err=%v", did, err)
	}
	if len(out) != len(msgs) {
		t.Fatalf("messages changed on no-op: %d != %d", len(out), len(msgs))
	}
}

func TestCompactHistoryForceCompacts(t *testing.T) {
	msgs := msgsOfLen(10)
	out, did, err := CompactHistory(context.Background(), fakeSend("the summary"), "", msgs, 1_000_000, true)
	if err != nil || !did {
		t.Fatalf("expected forced compaction, did=%v err=%v", did, err)
	}
	// first turn + summary + the 3 latest user turns of the summarized span
	// (msgs 2, 4, 6) + the 2-message tail
	if len(out) != 7 {
		t.Fatalf("unexpected compacted length: %d", len(out))
	}
	for i, want := range []int{2, 4, 6} {
		if out[2+i].Content != msgs[want].Content || out[2+i].Role != provider.RoleUser {
			t.Fatalf("user turn %d not carried over verbatim", want)
		}
	}
	if out[0].Content != msgs[0].Content {
		t.Fatal("first user turn not preserved")
	}
	if !strings.Contains(out[1].Content, "the summary") {
		t.Fatalf("summary turn missing: %q", out[1].Content)
	}
	if out[len(out)-1].Content != msgs[len(msgs)-1].Content {
		t.Fatal("tail not preserved verbatim")
	}
}

func TestCompactHistoryAutoFiresUnderPressure(t *testing.T) {
	msgs := msgsOfLen(20) // ~2000 estimated tokens against a tiny window
	out, did, err := CompactHistory(context.Background(), fakeSend("s"), "", msgs, 1000, false)
	if err != nil || !did {
		t.Fatalf("expected auto compaction under pressure, did=%v err=%v", did, err)
	}
	if len(out) >= len(msgs) {
		t.Fatalf("history did not shrink: %d >= %d", len(out), len(msgs))
	}
}

// The trigger follows the caller's measure (provider-reported usage, tool
// schemas) rather than the bare chars/4 history estimate, in both
// directions.
func TestCompactHistoryMeasuredUsesCallerMeasure(t *testing.T) {
	msgs := msgsOfLen(10) // ~1000 estimated tokens: far below a 200k window
	// ~19k tokens a message: calibrated real sizes far above chars/4.
	huge := func(_ string, ms []provider.Message) int { return 19_000 * len(ms) }
	if _, did, err := CompactHistoryMeasured(context.Background(), fakeSend("s"), "", msgs, 200_000, false, Config{}, 0, huge); err != nil || !did {
		t.Fatalf("a measured prompt near the window must compact, did=%v err=%v", did, err)
	}
	tiny := func(string, []provider.Message) int { return 10 }
	// ~10k estimated tokens against a 40k window would trip the trigger.
	if _, did, _ := CompactHistoryMeasured(context.Background(), fakeSend("s"), "", msgsOfLen(100), 40_000, false, Config{}, 0, tiny); did {
		t.Fatal("a small measured prompt must not compact even when chars/4 says otherwise")
	}
}

func TestCompactHistoryNeverSplitsToolCallFromResult(t *testing.T) {
	msgs := msgsOfLen(20)
	// Place an assistant tool-call right at the default cut boundary
	// (len-keepLast-1) so the boundary must move forward past its results.
	msgs[15] = provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.NativeTool{{Name: "shell"}}}
	msgs[16] = provider.Message{Role: provider.RoleUser, ToolResults: []provider.ToolResult{{Name: "shell", Output: "ok"}}}
	out, did, err := CompactHistory(context.Background(), fakeSend("s"), "", msgs, 1000, false)
	if err != nil || !did {
		t.Fatalf("expected compaction, did=%v err=%v", did, err)
	}
	for i, m := range out {
		if len(m.ToolCalls) > 0 {
			if i+1 >= len(out) || len(out[i+1].ToolResults) == 0 {
				t.Fatal("assistant tool-call kept without its tool results")
			}
		}
	}
}

func TestCompactHistoryWithPolicyDisabledIsNoOp(t *testing.T) {
	msgs := msgsOfLen(20) // well over pressure for a 1000-token window
	cfg := Config{AutoEnabled: false, AutoThresholdPct: 85, MaxFailures: 3}
	out, did, err := CompactHistoryWithPolicy(context.Background(), fakeSend("s"), "", msgs, 1000, false, cfg, 0)
	if err != nil || did {
		t.Fatalf("expected no-op with auto compaction disabled, did=%v err=%v", did, err)
	}
	if len(out) != len(msgs) {
		t.Fatalf("messages changed while disabled: %d != %d", len(out), len(msgs))
	}
}

func TestCompactHistoryWithPolicyDisabledStillForces(t *testing.T) {
	msgs := msgsOfLen(10)
	cfg := Config{AutoEnabled: false, AutoThresholdPct: 85, MaxFailures: 3}
	_, did, err := CompactHistoryWithPolicy(context.Background(), fakeSend("s"), "", msgs, 1_000_000, true, cfg, 0)
	if err != nil || !did {
		t.Fatalf("force must bypass the off switch, did=%v err=%v", did, err)
	}
}

func TestCompactHistoryWithPolicyPausesAfterFailures(t *testing.T) {
	msgs := msgsOfLen(20)
	cfg := Config{AutoEnabled: true, AutoThresholdPct: 85, MaxFailures: 3}
	if _, did, err := CompactHistoryWithPolicy(context.Background(), fakeSend("s"), "", msgs, 1000, false, cfg, 3); err != nil || did {
		t.Fatalf("expected pause at MaxFailures, did=%v err=%v", did, err)
	}
	if _, did, err := CompactHistoryWithPolicy(context.Background(), fakeSend("s"), "", msgs, 1000, false, cfg, 2); err != nil || !did {
		t.Fatalf("expected compaction below MaxFailures, did=%v err=%v", did, err)
	}
}

func TestCompactHistoryWithPolicyZeroConfigDefaultsOn(t *testing.T) {
	msgs := msgsOfLen(20)
	_, did, err := CompactHistoryWithPolicy(context.Background(), fakeSend("s"), "", msgs, 1000, false, Config{}, 0)
	if err != nil || !did {
		t.Fatalf("zero-value config must keep auto compaction on, did=%v err=%v", did, err)
	}
}

func TestCompactHistoryForceTooShortIsNoOp(t *testing.T) {
	msgs := msgsOfLen(3)
	_, did, err := CompactHistory(context.Background(), fakeSend("s"), "", msgs, 1000, true)
	if err != nil || did {
		t.Fatalf("expected no-op on tiny history, did=%v err=%v", did, err)
	}
}

func trMsgs(bigOutput string) []provider.Message {
	// task, assistant tool-call turn, tool-result turn with a spooled big
	// output, then filler so the middle is compactable.
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "the task"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.NativeTool{{ID: "c1", Name: "shell-exec", Args: []byte(`{"command":"go test ./..."}`)}}},
		{Role: provider.RoleUser, ToolResults: []provider.ToolResult{{ID: "c1", Name: "shell-exec", Output: bigOutput, IsErr: true, SpoolID: "spool:7"}}},
	}
	msgs = append(msgs, msgsOfLen(8)...)
	return msgs
}

func bigOut() string {
	return "FIRST LINE of output\n" + strings.Repeat("noise line\n", 65000) + "FAIL spettro/internal/agent\n"
}

func TestCompactStage1OffloadSkipsSummarizer(t *testing.T) {
	msgs := trMsgs(bigOut())
	send := func(_ context.Context, _ provider.Request) (provider.Response, error) {
		t.Fatal("summarizer must not be called when stage 1 suffices")
		return provider.Response{}, nil
	}
	// Window sized so the estimate exceeds the auto threshold only because of
	// the ~715KB tool result; once offloaded, the rest is far below it.
	out, did, err := CompactHistoryWithPolicy(context.Background(), send, "", msgs, 200000, false, Config{AutoEnabled: true}, 0)
	if err != nil || !did {
		t.Fatalf("expected stage-1 compaction, did=%v err=%v", did, err)
	}
	if len(out) != len(msgs) {
		t.Fatalf("stage 1 must not drop turns: %d != %d", len(out), len(msgs))
	}
	stub := out[2].ToolResults[0].Output
	if !strings.HasPrefix(stub, "[output elided: 715049 chars, spool:7") || !strings.Contains(stub, `tool-output {"id":"spool:7"}`) {
		t.Fatalf("missing offload stub: %q", stub)
	}
	if !strings.Contains(stub, "shell-exec") || !strings.Contains(stub, "status error") ||
		!strings.Contains(stub, "FIRST LINE") || !strings.Contains(stub, "FAIL spettro/internal/agent") {
		t.Fatalf("stub lacks tool/status/head/tail: %q", stub)
	}
	if !strings.Contains(stub, `go test`) {
		t.Fatalf("stub lacks args digest: %q", stub)
	}
	// Small results and the caller's slice stay untouched.
	if msgs[2].ToolResults[0].Output != bigOut() {
		t.Fatal("input slice mutated")
	}
	if est := EstimateHistoryTokens("", out); est >= EstimateHistoryTokens("", msgs) {
		t.Fatalf("token estimate did not drop: %d", est)
	}
}

func TestCompactStage2CarriesStubsIntoPrompt(t *testing.T) {
	msgs := trMsgs(bigOut())
	var prompt string
	send := func(_ context.Context, req provider.Request) (provider.Response, error) {
		prompt = promptOf(req)
		return provider.Response{Content: "summary keeping spool:7"}, nil
	}
	// Tiny window: pruning alone cannot get under threshold, so the
	// summarizer runs; it sees a bounded excerpt (head and the tail, where
	// the failure is) plus the spool ID, never the whole output.
	out, did, err := CompactHistoryWithPolicy(context.Background(), send, "", msgs, 500, false, Config{AutoEnabled: true}, 0)
	if err != nil || !did {
		t.Fatalf("expected two-stage compaction, did=%v err=%v", did, err)
	}
	if prompt == "" || !strings.Contains(prompt, "full output stored as spool:7") {
		t.Fatalf("summarizer prompt lacks the spool ID")
	}
	if !strings.Contains(prompt, "FIRST LINE") || !strings.Contains(prompt, "FAIL spettro/internal/agent") {
		t.Fatal("summarizer prompt lost the head or tail of the failing output")
	}
	if len(prompt) > transcriptBudget(500) {
		t.Fatalf("summarizer prompt is %d chars, over its %d budget", len(prompt), transcriptBudget(500))
	}
	if len(out) >= len(msgs) {
		t.Fatal("history did not shrink after stage 2")
	}
}

func TestCompactStage1PreservesSmallAndUnspooledResults(t *testing.T) {
	small := provider.ToolResult{ID: "c2", Name: "ls", Output: "a.txt\nb.txt", SpoolID: "spool:9"}
	unspooled := provider.ToolResult{ID: "c3", Name: "grep", Output: strings.Repeat("y", 3000)}
	msgs := trMsgs(bigOut())
	msgs[2].ToolResults = append(msgs[2].ToolResults, small, unspooled)
	out, _ := pruneToolOutputs(msgs, len(msgs)-4, pruneSpooled, 0)
	trs := out[2].ToolResults
	if trs[1].Output != small.Output {
		t.Fatal("small result was pruned")
	}
	if trs[2].Output != unspooled.Output {
		t.Fatal("the spooled tier pruned a result without spool backing")
	}
	if !strings.HasPrefix(trs[0].Output, "[output elided:") {
		t.Fatal("oversized spooled result not pruned")
	}
	// The aggressive tier also prunes the unspooled one, keeping an excerpt.
	out, _ = pruneToolOutputs(msgs, len(msgs)-4, pruneAll, 0)
	if got := out[2].ToolResults[2].Output; !strings.HasPrefix(got, "[output elided: 3000 chars, no stored copy] grep") || !strings.Contains(got, "yyy") {
		t.Fatalf("unspooled result not pruned with an excerpt: %q", got)
	}
}

// A stamp record with Seen is its path's whole state, so its Shell mark,
// false included, replaces an earlier one when records are merged.
func TestMergeStampsShellMarkFollowsSeen(t *testing.T) {
	seen := strings.Repeat("a", 64)
	marked := []provider.Message{
		{FileStamps: []provider.FileStamp{{Path: "/r/f.go", Seen: seen, Read: seen}}},
		{FileStamps: []provider.FileStamp{{Path: "/r/f.go", Seen: strings.Repeat("b", 64), Shell: true}}},
	}
	if got := mergeStamps(marked); len(got) != 1 || !got[0].Shell || got[0].Read != seen {
		t.Fatalf("shell re-stamp lost in merge: %+v", got)
	}
	cleared := append(marked, provider.Message{FileStamps: []provider.FileStamp{{Path: "/r/f.go", Seen: seen, Read: seen}}})
	if got := mergeStamps(cleared); len(got) != 1 || got[0].Shell {
		t.Fatalf("a later re-read did not clear the mark: %+v", got)
	}
}
