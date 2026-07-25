package acp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/agent"
)

// fakeQuestionTransport records what the question flow sent and replays a
// scripted client response, so each transport can be driven without a live
// connection.
type fakeQuestionTransport struct {
	extensionCalls   []json.RawMessage
	permissionCalls  []acpsdk.RequestPermissionRequest
	elicitationCalls []acpsdk.UnstableCreateElicitationRequest

	extensionResp    any
	extensionErr     error
	permissionResp   acpsdk.RequestPermissionResponse
	permissionErr    error
	elicitationResp  acpsdk.UnstableCreateElicitationResponse
	elicitationErr   error
	elicitationCount int
}

func (f *fakeQuestionTransport) CallExtension(_ context.Context, method string, params any) (json.RawMessage, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	f.extensionCalls = append(f.extensionCalls, raw)
	if f.extensionErr != nil {
		return nil, f.extensionErr
	}
	if f.extensionResp == nil {
		return nil, nil
	}
	return json.Marshal(f.extensionResp)
}

func (f *fakeQuestionTransport) RequestPermission(_ context.Context, params acpsdk.RequestPermissionRequest) (acpsdk.RequestPermissionResponse, error) {
	f.permissionCalls = append(f.permissionCalls, params)
	return f.permissionResp, f.permissionErr
}

func (f *fakeQuestionTransport) UnstableCreateElicitation(_ context.Context, params acpsdk.UnstableCreateElicitationRequest) (acpsdk.UnstableCreateElicitationResponse, error) {
	f.elicitationCalls = append(f.elicitationCalls, params)
	f.elicitationCount++
	return f.elicitationResp, f.elicitationErr
}

// questionFixture wires a bridge to a fake transport with the given client
// capabilities and returns the turn that asks the question.
func questionFixture(t *testing.T, tr questionTransport, withExtension, withElicitation bool) (*bridge, *turnState) {
	t.Helper()
	b := newBridge(Options{})
	b.questionTransport = tr
	if withExtension {
		b.clientExtensions = map[string]bool{extQuestionAsk: true}
	}
	if withElicitation {
		b.clientCaps = acpsdk.ClientCapabilities{
			Elicitation: &acpsdk.ElicitationCapabilities{Form: &acpsdk.ElicitationFormCapabilities{}},
		}
	}
	return b, &turnState{
		bridge:    b,
		ctx:       context.Background(),
		sessionID: "sess-q",
		open:      map[string][]acpsdk.ToolCallId{},
	}
}

func selectedPermission(optionID string) acpsdk.RequestPermissionResponse {
	return acpsdk.RequestPermissionResponse{
		Outcome: acpsdk.RequestPermissionOutcome{
			Selected: &acpsdk.RequestPermissionOutcomeSelected{
				Outcome:  "selected",
				OptionId: acpsdk.PermissionOptionId(optionID),
			},
		},
	}
}

var twoOptionQuestion = agent.AskUserRequest{
	Question:      "Which database?",
	Context:       "both are already provisioned",
	Options:       []string{"Postgres", "SQLite"},
	DefaultOption: "SQLite",
}

// The extension transport carries the whole payload, including the recommended
// flag, and resolves a tagged option answer to that option's label.
func TestAskUser_ExtensionTransport(t *testing.T) {
	tr := &fakeQuestionTransport{extensionResp: questionAnswer{Kind: answerKindOption, OptionID: "opt-0"}}
	_, turn := questionFixture(t, tr, true, false)

	answer, err := turn.askUser(context.Background(), twoOptionQuestion)
	if err != nil {
		t.Fatalf("askUser: %v", err)
	}
	if answer != "Postgres" {
		t.Fatalf("answer = %q, want the selected option's label", answer)
	}
	if len(tr.extensionCalls) != 1 {
		t.Fatalf("expected 1 extension call, got %d", len(tr.extensionCalls))
	}
	if len(tr.permissionCalls) != 0 {
		t.Fatal("extension transport must not also send a permission request")
	}

	var sent questionPayload
	if err := json.Unmarshal(tr.extensionCalls[0], &sent); err != nil {
		t.Fatalf("decode sent payload: %v", err)
	}
	if sent.Version != questionPayloadVersion {
		t.Fatalf("payload version = %d, want %d", sent.Version, questionPayloadVersion)
	}
	if sent.Question != "Which database?" || sent.Context != "both are already provisioned" {
		t.Fatalf("payload lost question/context: %+v", sent)
	}
	if len(sent.Options) != 2 {
		t.Fatalf("expected 2 options, got %d", len(sent.Options))
	}
	if sent.Options[0].IsRecommended || !sent.Options[1].IsRecommended {
		t.Fatalf("recommended flag not on the default option: %+v", sent.Options)
	}
	if sent.AllowCustomInput {
		t.Fatal("allowCustomInput must stay false when free response was not requested")
	}
}

// Custom text from the extension transport reaches the model verbatim, not as
// a synthetic option label.
func TestAskUser_ExtensionCustomTextIsVerbatim(t *testing.T) {
	tr := &fakeQuestionTransport{extensionResp: questionAnswer{Kind: answerKindCustom, Text: "  use DuckDB instead  "}}
	_, turn := questionFixture(t, tr, true, false)

	answer, err := turn.askUser(context.Background(), twoOptionQuestion)
	if err != nil {
		t.Fatalf("askUser: %v", err)
	}
	if answer != "use DuckDB instead" {
		t.Fatalf("answer = %q, want the custom text verbatim", answer)
	}
}

// A client that advertised the extension but does not serve it must not lose
// the question: the flow falls back to the permission transport.
func TestAskUser_ExtensionMethodNotFoundFallsBack(t *testing.T) {
	tr := &fakeQuestionTransport{
		extensionErr:   acpsdk.NewMethodNotFound(extQuestionAsk),
		permissionResp: selectedPermission("opt-1"),
	}
	_, turn := questionFixture(t, tr, true, false)

	answer, err := turn.askUser(context.Background(), twoOptionQuestion)
	if err != nil {
		t.Fatalf("askUser: %v", err)
	}
	if answer != "SQLite" {
		t.Fatalf("answer = %q, want the option selected over the fallback transport", answer)
	}
	if len(tr.permissionCalls) != 1 {
		t.Fatalf("expected the permission fallback, got %d calls", len(tr.permissionCalls))
	}
}

// The permission transport mirrors the payload into _meta, flags the
// recommended option, and appends the synthetic free-text option.
func TestAskUser_PermissionTransportPayload(t *testing.T) {
	tr := &fakeQuestionTransport{permissionResp: selectedPermission("opt-0")}
	_, turn := questionFixture(t, tr, false, false)

	req := twoOptionQuestion
	req.AllowFreeResponse = true
	answer, err := turn.askUser(context.Background(), req)
	if err != nil {
		t.Fatalf("askUser: %v", err)
	}
	if answer != "Postgres" {
		t.Fatalf("answer = %q, want the selected option's label", answer)
	}
	if len(tr.permissionCalls) != 1 {
		t.Fatalf("expected 1 permission call, got %d", len(tr.permissionCalls))
	}
	sent := tr.permissionCalls[0]

	payload, ok := sent.Meta[questionMetaKey].(questionPayload)
	if !ok {
		t.Fatalf("permission request _meta missing %q: %+v", questionMetaKey, sent.Meta)
	}
	if !payload.AllowCustomInput {
		t.Fatal("allowCustomInput must be set when free response is allowed")
	}
	if len(sent.Options) != 3 {
		t.Fatalf("expected 2 options + the custom entry, got %d", len(sent.Options))
	}
	if sent.Options[1].Meta[optionRecommendedMetaKey] != true {
		t.Fatalf("recommended marker missing on the default option: %+v", sent.Options[1])
	}
	if sent.Options[0].Meta[optionRecommendedMetaKey] == true {
		t.Fatal("non-default option must not be marked recommended")
	}
	custom := sent.Options[2]
	if string(custom.OptionId) != questionCustomOptionID || custom.Meta[optionCustomInputMetaKey] != true {
		t.Fatalf("free-text entry must be last and flagged: %+v", custom)
	}
	if title := sent.ToolCall.Title; title == nil || !strings.Contains(*title, "Which database?") {
		t.Fatalf("permission request lost the question title: %+v", sent.ToolCall)
	}
}

// A client that understands the payload answers in _meta; custom text there
// round-trips verbatim even though core ACP cannot express it.
func TestAskUser_PermissionMetaCustomAnswer(t *testing.T) {
	tr := &fakeQuestionTransport{permissionResp: acpsdk.RequestPermissionResponse{
		Meta: map[string]any{questionAnswerMetaKey: map[string]any{
			"kind": answerKindCustom,
			"text": "neither — use the existing MySQL box",
		}},
		Outcome: acpsdk.RequestPermissionOutcome{
			Selected: &acpsdk.RequestPermissionOutcomeSelected{Outcome: "selected", OptionId: questionCustomOptionID},
		},
	}}
	_, turn := questionFixture(t, tr, false, false)

	req := twoOptionQuestion
	req.AllowFreeResponse = true
	answer, err := turn.askUser(context.Background(), req)
	if err != nil {
		t.Fatalf("askUser: %v", err)
	}
	if answer != "neither — use the existing MySQL box" {
		t.Fatalf("answer = %q, want the custom text verbatim", answer)
	}
}

// The floor: a client that ignores _meta entirely still answers by picking one
// of the plain permission options.
func TestAskUser_PlainPermissionFloor(t *testing.T) {
	tr := &fakeQuestionTransport{permissionResp: selectedPermission("opt-1")}
	_, turn := questionFixture(t, tr, false, false)

	answer, err := turn.askUser(context.Background(), twoOptionQuestion)
	if err != nil {
		t.Fatalf("askUser: %v", err)
	}
	if answer != "SQLite" {
		t.Fatalf("answer = %q, want the plain option label", answer)
	}
}

// Selecting the synthetic free-text option is a request to collect text, not
// an answer: with elicitation available the text is collected there.
func TestAskUser_CustomOptionEscalatesToElicitation(t *testing.T) {
	tr := &fakeQuestionTransport{
		permissionResp: selectedPermission(questionCustomOptionID),
		elicitationResp: acpsdk.UnstableCreateElicitationResponse{
			Accept: &acpsdk.UnstableCreateElicitationAccept{
				Action:  "accept",
				Content: map[string]any{elicitationAnswerField: "ClickHouse"},
			},
		},
	}
	_, turn := questionFixture(t, tr, false, true)

	req := twoOptionQuestion
	req.AllowFreeResponse = true
	answer, err := turn.askUser(context.Background(), req)
	if err != nil {
		t.Fatalf("askUser: %v", err)
	}
	if answer != "ClickHouse" {
		t.Fatalf("answer = %q, want the elicited text", answer)
	}
	if tr.elicitationCount != 1 {
		t.Fatalf("expected 1 elicitation, got %d", tr.elicitationCount)
	}
}

// The synthetic option's own label must never be reported as the answer when
// no text can be collected.
func TestAskUser_CustomOptionWithoutElicitationFails(t *testing.T) {
	tr := &fakeQuestionTransport{permissionResp: selectedPermission(questionCustomOptionID)}
	_, turn := questionFixture(t, tr, false, false)

	req := twoOptionQuestion
	req.AllowFreeResponse = true
	answer, err := turn.askUser(context.Background(), req)
	if err == nil {
		t.Fatalf("expected an error, got answer %q", answer)
	}
	if !errors.Is(err, errQuestionUnreachable) {
		t.Fatalf("error = %v, want the guidance error", err)
	}
}

// An option-less question is free text: with the elicitation capability it is
// asked as a one-field form.
func TestAskUser_OptionlessUsesElicitation(t *testing.T) {
	tr := &fakeQuestionTransport{elicitationResp: acpsdk.UnstableCreateElicitationResponse{
		Accept: &acpsdk.UnstableCreateElicitationAccept{
			Action:  "accept",
			Content: map[string]any{elicitationAnswerField: "call it spettro-core"},
		},
	}}
	_, turn := questionFixture(t, tr, false, true)

	answer, err := turn.askUser(context.Background(), agent.AskUserRequest{
		Question:          "What should the package be called?",
		AllowFreeResponse: true,
	})
	if err != nil {
		t.Fatalf("askUser: %v", err)
	}
	if answer != "call it spettro-core" {
		t.Fatalf("answer = %q, want the elicited text", answer)
	}
	if len(tr.permissionCalls) != 0 {
		t.Fatal("an option-less question must not become a permission request")
	}
	form := tr.elicitationCalls[0].Form
	if form == nil || form.Mode != "form" {
		t.Fatalf("expected a form elicitation, got %+v", tr.elicitationCalls[0])
	}
	if _, ok := form.RequestedSchema.Properties[elicitationAnswerField]; !ok {
		t.Fatalf("form schema missing the answer field: %+v", form.RequestedSchema)
	}
}

// Regression: an option-less question with a default option used to be
// auto-answered with that default, telling the model a human decided
// something they never saw.
func TestAskUser_OptionlessNeverSubstitutesDefault(t *testing.T) {
	tr := &fakeQuestionTransport{}
	_, turn := questionFixture(t, tr, false, false)

	answer, err := turn.askUser(context.Background(), agent.AskUserRequest{
		Question:      "Ship it?",
		DefaultOption: "yes, ship it",
	})
	if err == nil {
		t.Fatalf("expected the guidance error, got answer %q", answer)
	}
	if !errors.Is(err, errQuestionUnreachable) {
		t.Fatalf("error = %v, want the guidance error", err)
	}
	if len(tr.permissionCalls)+len(tr.elicitationCalls)+len(tr.extensionCalls) != 0 {
		t.Fatal("no transport should have been attempted")
	}
}

func TestAskUser_CancelledOutcomeIsNotAnAnswer(t *testing.T) {
	tr := &fakeQuestionTransport{permissionResp: acpsdk.RequestPermissionResponse{
		Outcome: acpsdk.RequestPermissionOutcome{
			Cancelled: &acpsdk.RequestPermissionOutcomeCancelled{Outcome: "cancelled"},
		},
	}}
	_, turn := questionFixture(t, tr, false, false)

	if _, err := turn.askUser(context.Background(), twoOptionQuestion); !errors.Is(err, errQuestionUnanswered) {
		t.Fatalf("error = %v, want the unanswered error", err)
	}
}

// A declined elicitation is not an answer either.
func TestAskUser_DeclinedElicitationIsNotAnAnswer(t *testing.T) {
	tr := &fakeQuestionTransport{elicitationResp: acpsdk.UnstableCreateElicitationResponse{
		Decline: &acpsdk.UnstableCreateElicitationDecline{Action: "decline"},
	}}
	_, turn := questionFixture(t, tr, false, true)

	if _, err := turn.askUser(context.Background(), agent.AskUserRequest{Question: "Name it?"}); !errors.Is(err, errQuestionUnanswered) {
		t.Fatalf("error = %v, want the unanswered error", err)
	}
}

// Initialize must record what the client can do: the transports are gated on
// it, and before this the capabilities were dropped on the floor.
func TestInitialize_RecordsClientCapabilitiesAndExtensions(t *testing.T) {
	b := newBridge(Options{})

	resp, err := b.Initialize(context.Background(), acpsdk.InitializeRequest{
		ProtocolVersion: acpsdk.ProtocolVersionNumber,
		ClientCapabilities: acpsdk.ClientCapabilities{
			Elicitation: &acpsdk.ElicitationCapabilities{Form: &acpsdk.ElicitationFormCapabilities{}},
		},
		Meta: map[string]any{metaExtensionsKey: map[string]any{
			"version": float64(extensionsVersion),
			"methods": []any{extQuestionAsk},
		}},
	})
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if !b.clientSupportsElicitationForm() {
		t.Fatal("client elicitation capability was not recorded")
	}
	if !b.clientHasExtension(extQuestionAsk) {
		t.Fatal("client extension surface was not recorded")
	}

	ext, ok := resp.Meta[metaExtensionsKey].(map[string]any)
	if !ok {
		t.Fatalf("initialize response lost the extension advertisement: %+v", resp.Meta)
	}
	if ext["version"] != extensionsVersion {
		t.Fatalf("advertised version = %v, want %d", ext["version"], extensionsVersion)
	}
	clientMethods, _ := ext["clientMethods"].([]string)
	if len(clientMethods) == 0 || clientMethods[0] != extQuestionAsk {
		t.Fatalf("agent must advertise the client-served question method, got %v", ext["clientMethods"])
	}
}

// A client that says nothing about extensions gets no methods called on it.
func TestInitialize_NoExtensionAdvertisementIsSafe(t *testing.T) {
	b := newBridge(Options{})
	if _, err := b.Initialize(context.Background(), acpsdk.InitializeRequest{ProtocolVersion: acpsdk.ProtocolVersionNumber}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if b.clientHasExtension(extQuestionAsk) {
		t.Fatal("no extension may be assumed without an advertisement")
	}
	if b.clientSupportsElicitationForm() {
		t.Fatal("no elicitation may be assumed without the capability")
	}
}

// An unconnected bridge must fail the question instead of panicking on a nil
// connection.
func TestAskUser_NoTransportFails(t *testing.T) {
	b := newBridge(Options{})
	turn := &turnState{bridge: b, ctx: context.Background(), sessionID: "s", open: map[string][]acpsdk.ToolCallId{}}

	if _, err := turn.askUser(context.Background(), twoOptionQuestion); !errors.Is(err, errQuestionUnreachable) {
		t.Fatalf("error = %v, want the guidance error", err)
	}
}
