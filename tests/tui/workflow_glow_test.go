package tui_test

import (
	"regexp"
	"strings"
	"testing"

	"spettro/internal/agent"
	"spettro/internal/tui"
)

var sgr = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// The lit keyword is a promise about what the next turn can do, so it has to
// be driven by the same match the runtime uses. These render the real input
// box rather than the helper, because that is where a regression would land.
func TestInputBoxLightsTheWorkflowKeyword(t *testing.T) {
	lit := []string{
		"ultracode",
		"ultracode: audit the parser",
		"please use a workflow for this",
		"fan this out across sub-agents",
	}
	for _, text := range lit {
		m := tui.NewModelForTesting()
		m.SetTextareaValueForTesting(text)
		out := m.ViewInputForTesting(100)
		if !agent.WorkflowRequested(text) {
			t.Fatalf("%q no longer activates workflows; the test case is stale", text)
		}
		if !strings.Contains(sgr.ReplaceAllString(out, ""), text) {
			t.Fatalf("%q: the input box does not show the text at all", text)
		}
		if !strings.Contains(out, "\x1b[") {
			t.Fatalf("%q: no styling in the input box", text)
		}
		// A lit phrase is styled per cell, so it adds many more SGR runs than
		// the same amount of ordinary text does.
		bare := tui.NewModelForTesting()
		bare.SetTextareaValueForTesting(strings.Repeat("x", len(text)))
		gotRuns := len(sgr.FindAllString(out, -1))
		bareRuns := len(sgr.FindAllString(bare.ViewInputForTesting(100), -1))
		if gotRuns <= bareRuns {
			t.Fatalf("%q is not styled: %d SGR runs, same as plain text (%d)", text, gotRuns, bareRuns)
		}
	}
}

func TestInputBoxLeavesOrdinaryTextAlone(t *testing.T) {
	// A regression here would light up half the conversations in a repo that
	// has a .github/workflows directory.
	for _, text := range []string{
		"our deploy workflow is broken",
		"fix .github/workflows/ci.yml",
		"rename ultracoded to something else",
	} {
		if agent.WorkflowRequested(text) {
			t.Fatalf("%q activates workflows; the test case is stale", text)
		}
		m := tui.NewModelForTesting()
		m.SetTextareaValueForTesting(text)
		lit := m.ViewInputForTesting(100)

		bare := tui.NewModelForTesting()
		bare.SetTextareaValueForTesting(strings.Repeat("x", len(text)))
		// Same length, no keyword: the styling must be structurally identical,
		// i.e. the same sequence of SGR codes.
		if got, want := sgr.FindAllString(lit, -1), sgr.FindAllString(bare.ViewInputForTesting(100), -1); len(got) != len(want) {
			t.Fatalf("%q picked up extra styling: %d SGR runs vs %d for plain text", text, len(got), len(want))
		}
	}
}

// A "+500k" token budget lights up in the input box only when the message
// also turns workflows on — the only time the run honours it.
func TestInputBoxLightsBudgetDirectivesWithWorkflows(t *testing.T) {
	runs := func(text string) int {
		m := tui.NewModelForTesting()
		m.SetTextareaValueForTesting(text)
		return len(sgr.FindAllString(m.ViewInputForTesting(100), -1))
	}
	if _, ok := agent.ParseBudgetDirective("ultracode audit +500k"); !ok {
		t.Fatal("+500k is no longer a budget directive; the test case is stale")
	}
	// Same length either way, so any extra runs come from the directive.
	if lit, plain := runs("ultracode audit +500k"), runs("ultracode audit xxxxx"); lit <= plain {
		t.Fatalf("the directive does not glow next to the keyword: %d SGR runs vs %d", lit, plain)
	}
	if lit, plain := runs("plain audit +500k"), runs("plain audit xxxxx"); lit != plain {
		t.Fatalf("without workflows the directive must stay plain: %d SGR runs vs %d", lit, plain)
	}
}
