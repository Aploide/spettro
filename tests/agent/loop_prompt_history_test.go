package agent_test

import (
	"strings"
	"testing"

	"spettro/internal/agent"
)

// TestBuildLoopPrompt_IncludesHistoryWhenProvided verifies EFF-2: a non-empty
// History is rendered as a "Conversation so far" section placed before the
// current Task.
func TestBuildLoopPrompt_IncludesHistoryWhenProvided(t *testing.T) {
	history := "user: how do I build?\nassistant: run make build"
	prompt := agent.BuildLoopPromptForTesting("You are an assistant.", "now run the tests", history, "", 1)

	if !strings.Contains(prompt, "Conversation so far") {
		t.Fatalf("expected a conversation section, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "run make build") {
		t.Fatalf("expected history content in prompt, got:\n%s", prompt)
	}
	// History must appear BEFORE the current Task.
	convIdx := strings.Index(prompt, "Conversation so far")
	taskIdx := strings.Index(prompt, "Task:")
	if convIdx < 0 || taskIdx < 0 || convIdx >= taskIdx {
		t.Fatalf("conversation section must precede Task (conv=%d task=%d)", convIdx, taskIdx)
	}
}

// TestBuildLoopPrompt_OmitsHistoryOnFirstTurn verifies an empty History renders
// no conversation section, and is byte-for-byte identical to a prompt built
// with no history argument at all — the first-turn behavior must not change.
func TestBuildLoopPrompt_OmitsHistoryOnFirstTurn(t *testing.T) {
	prompt := agent.BuildLoopPromptForTesting("You are an assistant.", "do the thing", "", "", 1)
	if strings.Contains(prompt, "Conversation so far") {
		t.Fatalf("empty history must not render a conversation section, got:\n%s", prompt)
	}

	// Whitespace-only history is treated as empty.
	blank := agent.BuildLoopPromptForTesting("You are an assistant.", "do the thing", "   \n  ", "", 1)
	if blank != prompt {
		t.Fatalf("whitespace history must be identical to no history.\n--- empty ---\n%s\n--- blank ---\n%s", prompt, blank)
	}
}
