package agent

import (
	"strings"
	"testing"

	"spettro/internal/config"
)

func TestResolveFanOutTarget_DefaultsToCodeWorker(t *testing.T) {
	manifest := config.DefaultAgentManifest()
	spec, err := resolveFanOutTarget(&manifest, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec.ID != "code" {
		t.Fatalf("want default target \"code\", got %q", spec.ID)
	}
}

// A fan-out member that could itself fan out would let one call nest a
// fan-out inside another, so orchestrators and primaries are refused.
func TestResolveFanOutTarget_RejectsOrchestratorAndUnknown(t *testing.T) {
	manifest := config.DefaultAgentManifest()
	if _, err := resolveFanOutTarget(&manifest, "coding"); err == nil || !strings.Contains(err.Error(), "must be a worker/subagent") {
		t.Fatalf("want error for orchestrator target, got %v", err)
	}
	if _, err := resolveFanOutTarget(&manifest, "nope"); err == nil || !strings.Contains(err.Error(), "unknown agent type") {
		t.Fatalf("want error for unknown target, got %v", err)
	}
}

// The workflow wrapper names itself once; the shared resolver carries no
// prefix of its own for it to strip.
func TestResolveWorkflowTarget_PrefixesErrors(t *testing.T) {
	manifest := config.DefaultAgentManifest()
	_, err := resolveWorkflowTarget(&manifest, "nope")
	if err == nil || err.Error() != `workflow: unknown agent type "nope"` {
		t.Fatalf("got %v", err)
	}
}
