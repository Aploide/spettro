package agent

import (
	"strings"
	"testing"
)

// collect runs a sequence of text deltas through a streamDemux and returns the
// concatenated thinking and answer output, plus the number of answer resets.
func collect(t *testing.T, deltas []string) (thinking, answer string, resets int) {
	t.Helper()
	var tb, ab strings.Builder
	d := newStreamDemux(func(c StreamChunk) {
		switch c.Kind {
		case StreamKindThinking:
			tb.WriteString(c.Delta)
		case StreamKindAnswer:
			if c.Reset {
				resets++
				ab.Reset()
			}
			ab.WriteString(c.Delta)
		}
	})
	for _, delta := range deltas {
		d.text(delta)
	}
	d.flush()
	return tb.String(), ab.String(), resets
}

func TestStreamDemux_FinalPlainAnswer(t *testing.T) {
	_, answer, resets := collect(t, []string{"FINAL ", "Hello ", "world"})
	if answer != "Hello world" {
		t.Fatalf("answer = %q, want %q", answer, "Hello world")
	}
	if resets != 0 {
		t.Fatalf("resets = %d, want 0", resets)
	}
}

func TestStreamDemux_FinalColonPrefix(t *testing.T) {
	_, answer, _ := collect(t, []string{"FINAL: done"})
	if answer != "done" {
		t.Fatalf("answer = %q, want %q", answer, "done")
	}
}

func TestStreamDemux_ToolCallSuppressed(t *testing.T) {
	_, answer, resets := collect(t, []string{`TOOL_CALL {"tool":"glob"`, `,"args":{}}`})
	if answer != "" {
		t.Fatalf("answer = %q, want empty (tool steps are not shown)", answer)
	}
	if resets == 0 {
		t.Fatalf("expected a reset when a tool call is detected")
	}
}

func TestStreamDemux_ProseWithoutMarker(t *testing.T) {
	_, answer, _ := collect(t, []string{"Sure, here you go."})
	if answer != "Sure, here you go." {
		t.Fatalf("answer = %q, want %q", answer, "Sure, here you go.")
	}
}

func TestStreamDemux_ThinkTagsInline(t *testing.T) {
	thinking, answer, _ := collect(t, []string{"<think>reasoning here</think>FINAL the answer"})
	if thinking != "reasoning here" {
		t.Fatalf("thinking = %q, want %q", thinking, "reasoning here")
	}
	if answer != "the answer" {
		t.Fatalf("answer = %q, want %q", answer, "the answer")
	}
}

func TestStreamDemux_ThinkTagSplitAcrossDeltas(t *testing.T) {
	// The opening and closing tags are split across delta boundaries to exercise
	// the carry buffer.
	thinking, answer, _ := collect(t, []string{"<thi", "nk>deep ", "thoughts</thi", "nk>FINAL ok"})
	if thinking != "deep thoughts" {
		t.Fatalf("thinking = %q, want %q", thinking, "deep thoughts")
	}
	if answer != "ok" {
		t.Fatalf("answer = %q, want %q", answer, "ok")
	}
}

func TestStreamDemux_ReasoningDeltasAreThinking(t *testing.T) {
	var tb strings.Builder
	d := newStreamDemux(func(c StreamChunk) {
		if c.Kind == StreamKindThinking {
			tb.WriteString(c.Delta)
		}
	})
	d.reasoning("step 1 ")
	d.reasoning("step 2")
	d.flush()
	if got := tb.String(); got != "step 1 step 2" {
		t.Fatalf("thinking = %q, want %q", got, "step 1 step 2")
	}
}
