package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"spettro/internal/config"
	"spettro/internal/provider"
)

// /ultra is the ultracode toggle: hosts pass cfg.UltraActive() as
// LLMAgent.Ultracode. A toggled turn with no keyword in it must get exactly
// what the keyword gets — the standing-mode guidance, the workflow tool and
// runs that start without a consent prompt — and nothing of the swarm that
// /ultra used to switch on.
func TestUltraToggleRunsWorkflowsWithoutConfirmation(t *testing.T) {
	script := "export const meta = {name: 'noop', description: 'does nothing'}\nreturn {ok: true}"
	start, _ := json.Marshal(map[string]any{"script": script})
	turn := func(a LLMAgent, task string) (asked bool, reqs []map[string]any) {
		a.AskUser = func(context.Context, AskUserForm) ([]AskUserAnswer, error) {
			asked = true
			return []AskUserAnswer{{Selected: []string{"Run it"}}}, nil
		}
		runs := NewWorkflowRuns()
		t.Cleanup(runs.StopAll)
		a.WorkflowRuns = runs
		reqs = runAgentTurn(t, a, "coding", t.TempDir(), task,
			loopReply{toolName: workflowToolID, toolArgs: string(start)},
			loopReply{content: "done"})
		return asked, reqs
	}

	asked, reqs := turn(LLMAgent{Ultracode: true}, "audit the parser")
	system, tools := requestSystemAndTools(reqs[0])
	if !strings.Contains(system, "ULTRACODE is on") || !contains(tools, workflowToolID) {
		t.Fatalf("the toggle should inject the ultracode guidance and the workflow tool (tools %v)", tools)
	}
	if contains(tools, "ultra") || strings.Contains(system, "ULTRA MODE") {
		t.Fatalf("the old swarm must be gone: tools %v", tools)
	}
	if asked {
		t.Fatal("a run under the toggle is pre-approved and must not prompt")
	}
	if result, _ := json.Marshal(reqs[1]); !strings.Contains(string(result), "workflow_result") {
		t.Fatalf("the run should have started: %s", result)
	}

	// The contrast that shows the test can fail: a plain-English request
	// gets the tool but not the standing yes, so the same run is confirmed.
	asked, reqs = turn(LLMAgent{}, "use a workflow to audit the parser")
	if system, _ := requestSystemAndTools(reqs[0]); strings.Contains(system, "ULTRACODE is on") {
		t.Fatal("a plain request must get the judge-it guidance")
	}
	if !asked {
		t.Fatal("a run without the toggle or keyword must ask first")
	}
}

// The ultra tool no longer exists: no mode, role or depth grants it, and the
// tool list a run advertises never names it.
func TestUltraToolIsNeverGranted(t *testing.T) {
	manifest := config.DefaultAgentManifest()
	for _, spec := range manifest.Agents {
		allowed, _ := resolveToolPolicies(spec, &manifest)
		for _, g := range []workflowGuidance{{}, {Enabled: true}, {Enabled: true, Ultracode: true}} {
			for _, depth := range []int{0, 1} {
				if tools, _ := fanOutTools(allowed, g, depth); contains(tools, "ultra") {
					t.Fatalf("%s at depth %d was granted ultra: %v", spec.ID, depth, tools)
				}
			}
		}
	}
	if _, ok := builtinNativeToolDescs["ultra"]; ok {
		t.Fatal("ultra must not be described to the model")
	}
}

// Conversations from before the swarm was removed may carry past ultra
// calls. They are history, nothing more: the run must replay them without
// complaint, and a model that calls ultra again out of habit gets an error
// result it can recover from rather than a failed turn.
func TestPastUltraCallsInHistoryAreHarmless(t *testing.T) {
	args := json.RawMessage(`{"description":"d","prompt_template":"fix {{item}}","items":["a.go","b.go"]}`)
	history := []provider.Message{
		{Role: provider.RoleUser, Content: "fix both files"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.NativeTool{{ID: "old-1", Name: "ultra", Args: args}}},
		{Role: provider.RoleUser, ToolResults: []provider.ToolResult{{ID: "old-1", Name: "ultra", Output: "<ultra_result>\n<summary>completed: 2, failed: 0</summary>\n</ultra_result>"}}},
		{Role: provider.RoleAssistant, Content: "Both fixed."},
	}
	reqs := runAgentTurn(t, LLMAgent{Messages: history}, "coding", t.TempDir(), "now the third one",
		loopReply{toolName: "ultra", toolArgs: string(args)},
		loopReply{content: "done directly"})
	if len(reqs) != 2 {
		t.Fatalf("want the stray call answered and the turn finished, got %d requests", len(reqs))
	}
	first, _ := json.Marshal(reqs[0])
	if !strings.Contains(string(first), "old-1") {
		t.Fatalf("the past ultra call should replay as history: %s", first)
	}
	if _, tools := requestSystemAndTools(reqs[0]); contains(tools, "ultra") {
		t.Fatal("history must not bring the tool back")
	}
	second, _ := json.Marshal(reqs[1])
	if !strings.Contains(string(second), `tool \"ultra\" not allowed`) {
		t.Fatalf("a stray ultra call should come back as a not-allowed error: %s", second)
	}
}
