package acp

import (
	"context"
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/config"
	"spettro/internal/provider"
)

// /ultracode is a per-session toggle: no argument flips it, on/off set it,
// status reports it, and nothing is written to the shared config.
func TestUltracodeCommand(t *testing.T) {
	s := testSession(t)
	other := testSession(t)
	cfg := config.UserConfig{}
	pm := provider.NewManager()

	steps := []struct {
		input string
		want  bool
		reply string
	}{
		{"/ultracode", true, "ultracode on for this session"},
		{"/ultracode", false, "ultracode off"},
		{"/ultracode on", true, "ultracode on"},
		{"/ultracode status", true, "ultracode: on"},
		{"/ultracode bogus", true, "usage: /ultracode [on|off]"},
		{"/ultracode OFF", false, "ultracode off"},
		{"/ultracode status", false, "ultracode: off"},
	}
	for _, st := range steps {
		reply, _, handled := handleExtendedSlashCommand(nil, s, &cfg, pm, st.input)
		if !handled {
			t.Fatalf("%q not handled", st.input)
		}
		if !strings.Contains(reply, st.reply) {
			t.Fatalf("%q reply = %q, want it to contain %q", st.input, reply, st.reply)
		}
		if s.ultracode != st.want {
			t.Fatalf("after %q ultracode = %v, want %v", st.input, s.ultracode, st.want)
		}
	}
	if other.ultracode {
		t.Fatal("ultracode leaked into another session")
	}
	if sharedSettings(&cfg) != sharedSettings(&config.UserConfig{}) {
		t.Fatal("/ultracode must not change the settings shared with other sessions")
	}
}

func TestWorkflowSizeCommand(t *testing.T) {
	s := testSession(t)
	cfg := config.UserConfig{}
	pm := provider.NewManager()

	reply, _, handled := handleExtendedSlashCommand(nil, s, &cfg, pm, "/workflow-size")
	if !handled {
		t.Fatal("/workflow-size not handled")
	}
	for _, want := range append([]string{"workflow size: medium", "* medium", "~10 agents", "no agent guideline"}, config.WorkflowSizes...) {
		if !strings.Contains(reply, want) {
			t.Fatalf("show reply missing %q:\n%s", want, reply)
		}
	}

	cases := []struct {
		input, want, reply string
	}{
		{"/workflow-size large", "large", "workflow size set to large"},
		{"/workflow-size SMALL", "small", "workflow size set to small"},
		{"/workflows size unbounded", "unbounded", "workflow size set to unbounded"},
		{"/workflow size medium", "medium", "workflow size set to medium"},
		{"/workflow-size huge", "medium", "usage: /workflow-size"},
		{"/workflow-size large extra", "medium", "usage: /workflow-size"},
	}
	for _, tc := range cases {
		reply, _, handled := handleExtendedSlashCommand(nil, s, &cfg, pm, tc.input)
		if !handled || !strings.Contains(reply, tc.reply) {
			t.Fatalf("%q: handled=%v reply=%q", tc.input, handled, reply)
		}
		if cfg.WorkflowSizeTier() != tc.want {
			t.Fatalf("%q: tier = %q, want %q", tc.input, cfg.WorkflowSizeTier(), tc.want)
		}
		saved, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		if saved.WorkflowSizeTier() != tc.want {
			t.Fatalf("%q: persisted tier = %q, want %q", tc.input, saved.WorkflowSizeTier(), tc.want)
		}
	}
}

// /clear starts the conversation over; a run paused at a checkpoint waits for
// the old one, so it is stopped, and its card is closed on the wire — as a
// deliberate stop, not a failure — and forgotten. Merely forgetting the card
// left the editor showing it "waiting for orchestrator" for good.
func TestClearStopsSessionWorkflows(t *testing.T) {
	s := testSession(t)
	rec := newWireRecorder(t)
	s.id = "sess-wf"
	s.notify = rec.b.sessionNotifier(s.id)
	runs := s.liveWorkflowRunsLocked()
	cards := s.workflowCardsLocked()
	turn := rec.turn(cards)
	turn.onWorkflowTool(wfTrace("workflow", "running", `{"run_id":"wf_1","workflow":"audit"}`, ""))
	turn.onWorkflowTool(wfTrace("workflow", "paused",
		`{"run_id":"wf_1","workflow":"audit","checkpoint_id":"cp-1","message":"fix which?"}`, ""))
	callID := cards.runs["wf_1"].callID
	cfg := config.UserConfig{}

	if _, _, handled := handleSlashCommand(s, &cfg, provider.NewManager(), "/clear"); !handled {
		t.Fatal("/clear not handled")
	}
	if s.workflowRuns != nil {
		t.Fatal("/clear must drop the session's workflow runs")
	}
	if len(cards.runs) != 0 {
		t.Fatalf("/clear must forget the workflow cards: %v", cards.runs)
	}
	if next := s.liveWorkflowRunsLocked(); next == nil || next == runs {
		t.Fatal("the next turn must get a fresh registry")
	}
	// Two updates from the turn (open, pause), then the close.
	ups := rec.updates(t, 3)
	last := ups[len(ups)-1]
	if last.ToolCallID != string(callID) || last.Status != "completed" {
		t.Fatalf("/clear must close the paused card as completed, last update = %+v", last)
	}
	if !strings.Contains(last.text(), "■ stopped: the conversation was cleared") ||
		strings.Contains(last.text(), "waiting for orchestrator") {
		t.Fatalf("the closed card must say why it stopped:\n%s", last.text())
	}
}

func TestCloseSessionStopsWorkflows(t *testing.T) {
	b := newBridge(Options{})
	s := &acpSession{id: "sess-close"}
	s.liveWorkflowRunsLocked()
	b.sessions[s.id] = s
	if _, err := b.CloseSession(context.Background(), acpsdk.CloseSessionRequest{SessionId: "sess-close"}); err != nil {
		t.Fatal(err)
	}
	if s.workflowRuns != nil {
		t.Fatal("closing a session must stop its workflow runs")
	}
}

func TestStopAllWorkflowsOnDisconnect(t *testing.T) {
	b := newBridge(Options{})
	for _, id := range []string{"a", "b"} {
		s := &acpSession{id: id}
		s.liveWorkflowRunsLocked()
		b.sessions[id] = s
	}
	b.stopAllWorkflows()
	for id, s := range b.sessions {
		if s.workflowRuns != nil {
			t.Fatalf("session %s still holds its runs", id)
		}
	}
}

// Every turn of a session — prompt, /loop iteration, /goal iteration — must
// share one registry and one set of cards, or a run paused in one turn could
// not be continued in the next.
func TestSessionWorkflowStateIsShared(t *testing.T) {
	s := &acpSession{}
	if a, b := s.liveWorkflowRunsLocked(), s.liveWorkflowRunsLocked(); a == nil || a != b {
		t.Fatal("liveWorkflowRunsLocked must return one registry per session")
	}
	if a, b := s.workflowCardsLocked(), s.workflowCardsLocked(); a == nil || a != b {
		t.Fatal("workflowCardsLocked must return one card set per session")
	}
}

func TestWorkflowCommandsAdvertised(t *testing.T) {
	advertised := map[string]acpsdk.AvailableCommand{}
	for _, c := range acpAvailableCommands {
		advertised[c.Name] = c
	}
	for _, name := range []string{"ultracode", "workflow-size", "workflows"} {
		if _, ok := advertised[name]; !ok {
			t.Errorf("/%s is not advertised", name)
		}
		if !acpReservedCommandNames()[name] {
			t.Errorf("/%s is not reserved against skills", name)
		}
	}
	if hint := advertised["workflows"].Input.Unstructured.Hint; !strings.Contains(hint, "size") || !strings.Contains(hint, "task") {
		t.Errorf("/workflows hint = %q", hint)
	}
	for _, want := range []string{"/ultracode", "/workflow-size", "/workflows run <name> [json | task]"} {
		if !strings.Contains(acpHelpText, want) {
			t.Errorf("help text missing %q", want)
		}
	}
}
