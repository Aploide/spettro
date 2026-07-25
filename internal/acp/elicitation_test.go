package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/agent"
)

// Regression: the request went out without the session scope the spec flattens
// into it, so the client matched it against neither elicitation mode and
// rejected it as invalid params — the form never reached the user.
func TestElicitationRequest_CarriesTheSessionScope(t *testing.T) {
	tr := &fakeQuestionTransport{elicitationResp: acpsdk.UnstableCreateElicitationResponse{
		Accept: &acpsdk.UnstableCreateElicitationAccept{
			Content: map[string]any{"q-0": "SQLite"},
		},
	}}
	_, turn := questionFixture(t, tr, false, true)

	if _, err := turn.askForm(context.Background(), twoQuestionForm()); err != nil {
		t.Fatalf("askForm: %v", err)
	}

	raw, err := json.Marshal(tr.elicitationCalls[0])
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if sent["mode"] != elicitationModeForm {
		t.Fatalf("mode = %v, want the form discriminator", sent["mode"])
	}
	if sent["sessionId"] != "sess-q" {
		t.Fatalf("sessionId = %v, want the session the form belongs to", sent["sessionId"])
	}
	if _, ok := sent["requestedSchema"]; !ok {
		t.Fatalf("request lost its schema: %v", sent)
	}
	if _, ok := sent["message"]; !ok {
		t.Fatalf("request lost its message: %v", sent)
	}
}

// The single-question free-text path builds the same body, and it is scoped
// too: a client rejects an unscoped one whatever it was asked for.
func TestElicitationRequest_FreeTextIsScopedToo(t *testing.T) {
	tr := &fakeQuestionTransport{elicitationResp: acpsdk.UnstableCreateElicitationResponse{
		Accept: &acpsdk.UnstableCreateElicitationAccept{
			Content: map[string]any{elicitationAnswerField: "spettro-core"},
		},
	}}
	_, turn := questionFixture(t, tr, false, true)

	if _, err := turn.askUser(context.Background(), agent.AskUserRequest{
		Question:          "What should the package be called?",
		AllowFreeResponse: true,
	}); err != nil {
		t.Fatalf("askUser: %v", err)
	}
	if got := tr.elicitationCalls[0].SessionId; got != "sess-q" {
		t.Fatalf("sessionId = %q, want the session the question belongs to", got)
	}
}

// A client that cannot read the request showed the user nothing, so the form is
// still owed them: the walk asks it question by question instead of failing the
// turn with a protocol error.
func TestAskForm_RejectedElicitationFallsBackToTheWalk(t *testing.T) {
	tr := &fakeQuestionTransport{
		elicitationErr: &acpsdk.RequestError{Code: codeInvalidParams, Message: "Invalid params"},
		permissionResp: selectedPermission("opt-0"),
	}
	_, turn := questionFixture(t, tr, false, true)

	answers, err := turn.askForm(context.Background(), twoQuestionForm())
	if err != nil {
		t.Fatalf("askForm: %v", err)
	}
	if len(tr.permissionCalls) != 2 {
		t.Fatalf("expected the form to be walked, got %d permission requests", len(tr.permissionCalls))
	}
	if len(answers) != 2 || answers[0].Selected[0] != "Postgres" {
		t.Fatalf("the walk must still assemble every answer: %+v", answers)
	}
}

// A free-text question the client cannot be asked is guidance the model can
// act on, not a JSON-RPC error code.
func TestAskUser_RejectedElicitationIsGuidance(t *testing.T) {
	tr := &fakeQuestionTransport{
		elicitationErr: &acpsdk.RequestError{Code: codeInvalidParams, Message: "Invalid params"},
	}
	_, turn := questionFixture(t, tr, false, true)

	_, err := turn.askUser(context.Background(), agent.AskUserRequest{Question: "Name it?"})
	if !errors.Is(err, errQuestionUnreachable) {
		t.Fatalf("error = %v, want the guidance error", err)
	}
}

// Elicitation rides on a connection the SDK keeps to itself. If a dependency
// bump moves it, this fails here rather than silently costing every client the
// form transport.
func TestSDKConnectionIsReachable(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() {
		pw.Close()
		pr.Close()
	})
	conn := acpsdk.NewAgentSideConnection(newBridge(Options{}), io.Discard, pr)
	if sdkConnection(conn) == nil {
		t.Fatal("the SDK's JSON-RPC connection is no longer reachable; elicitation cannot be sent")
	}
	if sdkConnection(nil) != nil {
		t.Fatal("a nil connection must stay nil")
	}
}
