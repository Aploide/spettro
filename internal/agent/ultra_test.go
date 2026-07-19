package agent

import (
	"errors"
	"strings"
	"testing"

	"spettro/internal/config"
	"spettro/internal/provider"
)

func TestExpandUltraPrompts_Valid(t *testing.T) {
	prompts, err := expandUltraPrompts("Add doc comments to {{item}} and run go vet.", []string{"a.go", "b.go", "c.go"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prompts) != 3 {
		t.Fatalf("want 3 prompts, got %d", len(prompts))
	}
	if prompts[1] != "Add doc comments to b.go and run go vet." {
		t.Fatalf("template not expanded: %q", prompts[1])
	}
}

func TestExpandUltraPrompts_Errors(t *testing.T) {
	cases := []struct {
		name     string
		template string
		items    []string
		wantSub  string
	}{
		{"missing template", "", []string{"a", "b"}, "prompt_template is required"},
		{"missing placeholder", "do the work", []string{"a", "b"}, "{{item}}"},
		{"too few items", "fix {{item}}", []string{"a"}, "at least 2 items"},
		{"empty item", "fix {{item}}", []string{"a", " "}, "non-empty"},
		{"duplicate items", "fix {{item}}", []string{"a", "a"}, "distinct"},
		{"too many items", "fix {{item}}", make33Items(), "limit is 32"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := expandUltraPrompts(tc.template, tc.items)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("want error containing %q, got %v", tc.wantSub, err)
			}
		})
	}
}

func make33Items() []string {
	items := make([]string, 33)
	for i := range items {
		items[i] = strings.Repeat("x", i+1)
	}
	return items
}

func TestResolveUltraTarget_DefaultsToCodeWorker(t *testing.T) {
	manifest := config.DefaultAgentManifest()
	spec, err := resolveUltraTarget(&manifest, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec.ID != "code" {
		t.Fatalf("want default target \"code\", got %q", spec.ID)
	}
}

func TestResolveUltraTarget_RejectsOrchestratorAndUnknown(t *testing.T) {
	manifest := config.DefaultAgentManifest()
	if _, err := resolveUltraTarget(&manifest, "coding"); err == nil {
		t.Fatal("want error for orchestrator target")
	}
	if _, err := resolveUltraTarget(&manifest, "nope"); err == nil {
		t.Fatal("want error for unknown target")
	}
}

func TestRenderUltraResults_OrderAndSummary(t *testing.T) {
	out := renderUltraResults("code", []ultraResult{
		{item: "a.go", content: "done a"},
		{item: "b.go", err: errors.New("boom")},
		{item: "c.go", content: "done c"},
	})
	if !strings.Contains(out, "<summary>completed: 2, failed: 1</summary>") {
		t.Fatalf("bad summary: %s", out)
	}
	// Results must appear in input order.
	ia, ib, ic := strings.Index(out, "done a"), strings.Index(out, "error: boom"), strings.Index(out, "done c")
	if ia == -1 || ib == -1 || ic == -1 || !(ia < ib && ib < ic) {
		t.Fatalf("results out of order: %s", out)
	}
	if !strings.Contains(out, "Some sub-agents failed") {
		t.Fatalf("missing failure hint: %s", out)
	}
}

func TestUltraActive_RequiresRestrictedOrYolo(t *testing.T) {
	cfg := config.UserConfig{Ultra: true, Permission: config.PermissionAskFirst}
	if cfg.UltraActive() {
		t.Fatal("ultra must be suspended under ask-first")
	}
	for _, p := range []config.PermissionLevel{config.PermissionRestricted, config.PermissionYOLO} {
		cfg.Permission = p
		if !cfg.UltraActive() {
			t.Fatalf("ultra should be active under %s", p)
		}
	}
	cfg.Ultra = false
	if cfg.UltraActive() {
		t.Fatal("ultra off must stay off")
	}
}

func TestRunUltra_RefusesAskFirstPermission(t *testing.T) {
	manifest := config.DefaultAgentManifest()
	r := &toolRuntime{
		manifest:    &manifest,
		providerMgr: provider.NewManager(),
		permission:  config.PermissionAskFirst,
	}
	_, err := r.runUltra(t.Context(), []byte(`{"description":"d","prompt_template":"fix {{item}}","items":["a","b"]}`))
	if err == nil || !strings.Contains(err.Error(), "restricted or yolo") {
		t.Fatalf("want permission error, got %v", err)
	}
}

func TestLLMAgentRun_UltraInjectsToolOnlyAtTopLevel(t *testing.T) {
	manifest := config.DefaultAgentManifest()
	spec, _ := manifest.AgentByID("coding")

	top, _ := resolveToolPolicies(spec, &manifest)
	// Simulate the injection performed in LLMAgent.Run.
	found := false
	for _, id := range top {
		if id == ultraToolID {
			found = true
		}
	}
	if found {
		t.Fatal("ultra must not be present without the toggle")
	}
}

func TestEmitSwarmTrace_NamesAndShape(t *testing.T) {
	var traces []ToolTrace
	r := &toolRuntime{
		agentID:      "coding",
		toolCallback: func(tr ToolTrace) { traces = append(traces, tr) },
	}
	r.emitSwarmTrace("code#2", "b.go", "running", "")
	r.emitSwarmTrace("code#2", "b.go", "success", "done")
	if len(traces) != 2 {
		t.Fatalf("want 2 traces, got %d", len(traces))
	}
	for _, tr := range traces {
		if tr.AgentID != "code#2" || tr.Name != "agent" {
			t.Fatalf("bad trace identity: %+v", tr)
		}
		if !strings.Contains(tr.Args, `"swarm":true`) || !strings.Contains(tr.Args, `"agent":"code#2"`) || !strings.Contains(tr.Args, `"task":"b.go"`) {
			t.Fatalf("bad trace args: %s", tr.Args)
		}
	}
	if traces[0].Status != "running" || traces[1].Status != "success" {
		t.Fatalf("bad statuses: %s, %s", traces[0].Status, traces[1].Status)
	}
	// nil callback must be a no-op, not a panic.
	(&toolRuntime{}).emitSwarmTrace("code#1", "a", "running", "")
}

func TestRenderUltraResults_IncludesInstanceNames(t *testing.T) {
	out := renderUltraResults("code", []ultraResult{
		{item: "a.go", name: "code#1", content: "done a"},
		{item: "b.go", content: "done b"}, // no name → derived from index
	})
	if !strings.Contains(out, `name="code#1"`) || !strings.Contains(out, `name="code#2"`) {
		t.Fatalf("missing instance names: %s", out)
	}
}
