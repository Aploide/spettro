package agent_test

// Mid-run model steering (TODO 12): a message pushed onto the SteeringQueue
// while the run executes must be appended to the conversation as a user turn
// at the next step boundary — without restarting the run and without touching
// any earlier message (prompt-cache prefix stability).

import (
	"context"
	"strings"
	"sync"
	"testing"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/provider"
)

func TestSteeringQueue_PushDrain(t *testing.T) {
	q := agent.NewSteeringQueue()
	if q.Len() != 0 {
		t.Fatalf("new queue should be empty, got %d", q.Len())
	}
	q.Push("first")
	q.Push("  ") // blank: ignored
	q.Push("second")
	if q.Len() != 2 {
		t.Fatalf("expected 2 queued messages, got %d", q.Len())
	}
	got := q.Drain()
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("unexpected drain result: %v", got)
	}
	if q.Len() != 0 || len(q.Drain()) != 0 {
		t.Fatal("queue should be empty after drain")
	}

	// nil receiver must be safe (TUI calls before a run ever started).
	var nilQ *agent.SteeringQueue
	nilQ.Push("x")
	if nilQ.Len() != 0 || nilQ.Drain() != nil {
		t.Fatal("nil queue must be inert")
	}
}

func TestSteering_InjectedAtNextStepBoundary(t *testing.T) {
	dir := t.TempDir()

	cs := newCaptureServer(t, []string{
		`TOOL_CALL {"tool":"comment","args":{"message":"working on it"}}`,
		`TOOL_CALL {"tool":"comment","args":{"message":"adjusting course"}}`,
		"FINAL\nDone, steering applied.",
	})
	pm := provider.NewManager()
	pm.AddLocalModels([]provider.Model{{Provider: cs.srv.URL, Name: "fake-model", Local: true}})

	steering := agent.NewSteeringQueue()
	// Push the steering message from the first tool execution, mimicking the
	// user typing while step 1 runs: it must appear in request 2, not request 1.
	var once sync.Once
	a := agent.LLMAgent{
		Spec: config.AgentSpec{
			ID:           "test-runner",
			Mode:         "worker",
			AllowedTools: []string{"comment"},
			Permission:   config.PermissionYOLO,
			Enabled:      true,
		},
		ProviderManager: pm,
		ProviderName:    func() string { return cs.srv.URL },
		ModelName:       func() string { return "fake-model" },
		CWD:             dir,
		Steering:        steering,
		ToolCallback: func(tr agent.ToolTrace) {
			if tr.Name == "comment" && tr.Status == "running" {
				once.Do(func() { steering.Push("focus only on the README, skip everything else") })
			}
		},
	}

	result, err := a.Run(context.Background(), "Do a multi-step task.")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(result.Content, "Done, steering applied") {
		t.Fatalf("run should complete normally, got %q", result.Content)
	}

	if strings.Contains(cs.promptAt(0), "focus only on the README") {
		t.Error("steering must not appear in the request sent before it was pushed")
	}
	prompt2 := cs.promptAt(1)
	if !strings.Contains(prompt2, "focus only on the README") {
		t.Errorf("steering message missing from the next step's request:\n%s", prompt2)
	}
	if !strings.Contains(prompt2, "user steering") {
		t.Errorf("steering marker missing from the next step's request:\n%s", prompt2)
	}

	// The steering turn must be carried in the returned conversation (so the
	// next run's prefix includes it) as an appended user message.
	found := false
	for _, msg := range result.Messages {
		if msg.Role == provider.RoleUser && strings.Contains(msg.Content, "focus only on the README") {
			found = true
			break
		}
	}
	if !found {
		t.Error("steering message not present in RunResult.Messages")
	}
	if steering.Len() != 0 {
		t.Errorf("steering queue should be drained, %d left", steering.Len())
	}
}
