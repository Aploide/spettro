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
	// first turn + summary + keepLast(2) tail
	if len(out) != 4 {
		t.Fatalf("unexpected compacted length: %d", len(out))
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
