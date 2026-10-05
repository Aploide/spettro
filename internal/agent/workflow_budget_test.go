package agent

import (
	"strings"
	"testing"
)

func TestParseBudgetDirective(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"ultracode +500k audit the parser", 500_000, true},
		{"+1.5m ultracode", 1_500_000, true},
		{"ultracode, budget +2M", 2_000_000, true},
		{"+750K then +250k", 250_000, true},
		{"ultracode +500 k", 500_000, true},
		{"C++ is fine", 0, false},
		{"a+500k salary", 0, false},
		{"+5 apples", 0, false},
		{"+500kb of logs", 0, false},
		{"+0k", 0, false},
		{"+99999999m", maxBudgetDirective, true},
	}
	for _, tc := range cases {
		got, ok := ParseBudgetDirective(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseBudgetDirective(%q) = %d, %v; want %d, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestBudgetDirectiveSpans(t *testing.T) {
	spans := BudgetDirectiveSpans("ultracode +500k éé +1m")
	want := [][2]int{{10, 15}, {19, 22}}
	if len(spans) != len(want) {
		t.Fatalf("spans = %v, want %v", spans, want)
	}
	for i := range want {
		if spans[i] != want[i] {
			t.Fatalf("spans = %v, want %v", spans, want)
		}
	}
	if BudgetDirectiveSpans("no directive here") != nil {
		t.Fatal("expected no spans")
	}
}

func TestWorkflowSizeNote(t *testing.T) {
	if got := workflowSizeNote("small", 5, 5); got != "" {
		t.Errorf("at the guideline: %q, want no note", got)
	}
	if got := workflowSizeNote("unbounded", 0, 400); got != "" {
		t.Errorf("unbounded tier: %q, want no note", got)
	}
	got := workflowSizeNote("small", 5, 15)
	for _, want := range []string{"started 15 agents", "small guideline of ~5", "plan("} {
		if !strings.Contains(got, want) {
			t.Errorf("note %q missing %q", got, want)
		}
	}
}
