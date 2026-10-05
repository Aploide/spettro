package config

import "testing"

// TestNormalizeWorkflowSize: an unknown size tier is cleared rather than
// reaching the agent as a guideline nobody defined, and the empty value reads
// as medium everywhere it is consumed.
func TestNormalizeWorkflowSize(t *testing.T) {
	cases := []struct {
		in, want, tier string
		changed        bool
	}{
		{"", "", "medium", false},
		{"small", "small", "small", false},
		{"medium", "medium", "medium", false},
		{"large", "large", "large", false},
		{"unbounded", "unbounded", "unbounded", false},
		{"huge", "", "medium", true},
		{"Large", "", "medium", true},
	}
	for _, tc := range cases {
		cfg := Default()
		cfg.WorkflowSize = tc.in
		got, changed := normalize(cfg)
		if got.WorkflowSize != tc.want {
			t.Errorf("normalize(workflow_size=%q) = %q, want %q", tc.in, got.WorkflowSize, tc.want)
		}
		if got.WorkflowSizeTier() != tc.tier {
			t.Errorf("WorkflowSizeTier(%q) = %q, want %q", tc.in, got.WorkflowSizeTier(), tc.tier)
		}
		if tc.changed && !changed {
			t.Errorf("normalize(workflow_size=%q) did not report the rewrite", tc.in)
		}
	}
}
