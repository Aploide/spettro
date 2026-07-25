package agent_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"spettro/internal/agent"
	"spettro/internal/config"
)

// The ask-user tool must be reachable from the agents a human actually talks
// to. Reachability needs the allow-list grant AND the "ask" action family:
// resolveToolPolicies drops an allow-listed tool whose actions the agent does
// not hold, which is how the tool stayed unreachable in every mode.
func TestAskUser_ReachableFromHumanFacingAgents(t *testing.T) {
	for _, agentID := range []string{"plan", "coding", "ask"} {
		t.Run(agentID, func(t *testing.T) {
			pm, providerName, modelName := scriptedManager(t, []string{
				`TOOL_CALL {"name":"ask-user","arguments":{"question":"Which database?","options":["Postgres","SQLite"],"default_option":"SQLite"}}`,
				"FINAL\nusing SQLite",
			})
			manifest := config.DefaultAgentManifest()
			spec, ok := manifest.AgentByID(agentID)
			if !ok {
				t.Fatalf("agent %q missing from the default manifest", agentID)
			}
			spec.Permission = config.PermissionYOLO

			var asked agent.AskUserRequest
			ag := agent.LLMAgent{
				Spec:            spec,
				ProviderManager: pm,
				ProviderName:    func() string { return providerName },
				ModelName:       func() string { return modelName },
				CWD:             t.TempDir(),
				Manifest:        &manifest,
				AskUser: func(_ context.Context, req agent.AskUserRequest) (string, error) {
					asked = req
					return "SQLite", nil
				},
			}

			result, err := ag.Run(context.Background(), "pick a database")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(result.Tools) == 0 {
				t.Fatal("expected an ask-user tool trace")
			}
			trace := result.Tools[0]
			if trace.Status == "error" {
				t.Fatalf("ask-user was not reachable: %s", trace.Output)
			}
			if asked.Question != "Which database?" {
				t.Fatalf("callback saw %q", asked.Question)
			}
			if asked.DefaultOption != "SQLite" {
				t.Fatalf("recommended option lost on the way to the UI: %+v", asked)
			}
			// The answer must come back to the model, not just to the UI.
			if !strings.Contains(trace.Output, "SQLite") {
				t.Fatalf("answer missing from the tool result: %q", trace.Output)
			}
		})
	}
}

// A question waits for the person, however long they take. The tool's
// manifest timeout_sec (10s for ask-user) bounds tool execution, and applying
// it here cancelled the question out from under the user and let the model
// carry on as if nobody was there.
func TestAskUser_WaitIsNotBoundedByTheToolTimeout(t *testing.T) {
	pm, providerName, modelName := scriptedManager(t, []string{
		`TOOL_CALL {"name":"ask-user","arguments":{"question":"Which database?","options":["Postgres","SQLite"]}}`,
		"FINAL\ndone",
	})
	manifest := config.DefaultAgentManifest()
	spec, _ := manifest.AgentByID("coding")
	spec.Permission = config.PermissionYOLO

	var deadline time.Time
	var hasDeadline bool
	ag := agent.LLMAgent{
		Spec:            spec,
		ProviderManager: pm,
		ProviderName:    func() string { return providerName },
		ModelName:       func() string { return modelName },
		CWD:             t.TempDir(),
		Manifest:        &manifest,
		AskUser: func(ctx context.Context, _ agent.AskUserRequest) (string, error) {
			deadline, hasDeadline = ctx.Deadline()
			return "Postgres", nil
		},
	}

	if _, err := ag.Run(context.Background(), "pick a database"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if hasDeadline {
		t.Fatalf("the question ran under a %s deadline; it must wait for the user indefinitely",
			time.Until(deadline).Round(time.Second))
	}
}

// Workers stay without the tool: their runs inherit the parent's callback, so
// a question from a nested worker would interrupt the user mid-orchestration
// with no context about who is asking.
func TestAskUser_WorkersCannotAsk(t *testing.T) {
	pm, providerName, modelName := scriptedManager(t, []string{
		`TOOL_CALL {"name":"ask-user","arguments":{"question":"Which database?"}}`,
		"FINAL\ndone",
	})
	manifest := config.DefaultAgentManifest()
	spec, _ := manifest.AgentByID("code")
	spec.Permission = config.PermissionYOLO

	called := false
	ag := agent.LLMAgent{
		Spec:            spec,
		ProviderManager: pm,
		ProviderName:    func() string { return providerName },
		ModelName:       func() string { return modelName },
		CWD:             t.TempDir(),
		Manifest:        &manifest,
		AskUser: func(context.Context, agent.AskUserRequest) (string, error) {
			called = true
			return "yes", nil
		},
	}

	if _, err := ag.Run(context.Background(), "do the thing"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if called {
		t.Fatal("a worker agent must not be able to interrupt the user with a question")
	}
}

// Without a callback the tool reports the gap instead of inventing an answer
// (headless mode relies on this, see docs/goal.md).
func TestAskUser_NoCallbackIsAnError(t *testing.T) {
	pm, providerName, modelName := scriptedManager(t, []string{
		`TOOL_CALL {"name":"ask-user","arguments":{"question":"Ship it?","default_option":"yes"}}`,
		"FINAL\ndone",
	})
	manifest := config.DefaultAgentManifest()
	spec, _ := manifest.AgentByID("coding")
	spec.Permission = config.PermissionYOLO

	ag := agent.LLMAgent{
		Spec:            spec,
		ProviderManager: pm,
		ProviderName:    func() string { return providerName },
		ModelName:       func() string { return modelName },
		CWD:             t.TempDir(),
		Manifest:        &manifest,
	}

	result, err := ag.Run(context.Background(), "ask something")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Tools) == 0 {
		t.Fatal("expected an ask-user tool trace")
	}
	if result.Tools[0].Status != "error" {
		t.Fatalf("expected an error trace, got %+v", result.Tools[0])
	}
	if strings.Contains(result.Tools[0].Output, "yes") {
		t.Fatalf("the default option must never stand in for an answer: %q", result.Tools[0].Output)
	}
}
