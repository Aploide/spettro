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

// twoQuestionForm is the shape task 02 gave the tool: a single-select question
// with descriptions and a recommended answer, and a multi-select one.
func twoQuestionForm() agent.AskUserForm {
	return agent.AskUserForm{
		Context: "both are already provisioned",
		Questions: []agent.AskUserQuestion{
			{
				Header:   "Database",
				Question: "Which database?",
				Options: []agent.AskUserOption{
					{Label: "Postgres", Description: "already provisioned"},
					{Label: "SQLite", IsRecommended: true, Preview: "file: ./spettro.db"},
				},
				AllowCustom: true,
			},
			{
				Header:      "Checks",
				Question:    "Which checks run before commits?",
				Options:     []agent.AskUserOption{{Label: "go vet"}, {Label: "gofmt"}},
				MultiSelect: true,
			},
		},
	}
}

func sentFormPayload(t *testing.T, raw json.RawMessage) formPayload {
	t.Helper()
	var payload formPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode sent payload: %v", err)
	}
	return payload
}

// A client that mirrored the extension back gets the whole form in one call and
// answers every question at once.
func TestAskForm_ExtensionCarriesTheWholeForm(t *testing.T) {
	tr := &fakeQuestionTransport{extensionResp: map[string]any{
		"answers": []map[string]any{
			{"questionId": "q-0", "kind": answerKindOption, "optionId": "opt-1"},
			{"questionId": "q-1", "kind": answerKindOption, "optionIds": []string{"opt-0", "opt-1"}, "notes": "vet is the slow one"},
		},
	}}
	_, turn := questionFixture(t, tr, true, false)
	form := twoQuestionForm()

	answers, err := turn.askForm(context.Background(), form)
	if err != nil {
		t.Fatalf("askForm: %v", err)
	}
	if len(tr.extensionCalls) != 1 || len(tr.permissionCalls) != 0 {
		t.Fatalf("the whole form must go in one call: %d extension, %d permission",
			len(tr.extensionCalls), len(tr.permissionCalls))
	}

	payload := sentFormPayload(t, tr.extensionCalls[0])
	if payload.Version != formPayloadVersion {
		t.Fatalf("payload version = %d, want %d", payload.Version, formPayloadVersion)
	}
	if len(payload.Questions) != 2 {
		t.Fatalf("expected both questions on the wire, got %d", len(payload.Questions))
	}
	// Version 1's fields describe the first question, so a client that never
	// learned about forms still renders something answerable.
	if payload.Question != "Which database?" || len(payload.Options) != 2 {
		t.Fatalf("the v1 fields must describe the first question: %+v", payload.questionPayload)
	}
	first := payload.Questions[0]
	if first.Header != "Database" || !first.AllowCustomInput || first.MultiSelect {
		t.Fatalf("first question lost its shape: %+v", first)
	}
	if first.Options[0].Description != "already provisioned" || first.Options[1].Preview == "" {
		t.Fatalf("descriptions and previews must survive the extension transport: %+v", first.Options)
	}
	if !first.Options[1].IsRecommended {
		t.Fatalf("the recommended marker is missing: %+v", first.Options)
	}
	if !payload.Questions[1].MultiSelect {
		t.Fatal("multi_select must survive the extension transport")
	}

	if len(answers) != 2 {
		t.Fatalf("expected one answer per question, got %d", len(answers))
	}
	if len(answers[0].Selected) != 1 || answers[0].Selected[0] != "SQLite" {
		t.Fatalf("first answer = %+v", answers[0])
	}
	if strings.Join(answers[1].Selected, ",") != "go vet,gofmt" {
		t.Fatalf("a multi-select answer must come back whole: %+v", answers[1])
	}
	if answers[1].Notes != "vet is the slow one" {
		t.Fatalf("the note must reach the model: %+v", answers[1])
	}
}

// A question the client left out of its answers is unanswered, not defaulted:
// the model must never read silence as agreement with its recommendation.
func TestAskForm_ExtensionMissingAnswerIsSkipped(t *testing.T) {
	tr := &fakeQuestionTransport{extensionResp: map[string]any{
		"answers": []map[string]any{{"header": "Checks", "kind": answerKindCustom, "text": "  none of them  "}},
	}}
	_, turn := questionFixture(t, tr, true, false)

	answers, err := turn.askForm(context.Background(), twoQuestionForm())
	if err != nil {
		t.Fatalf("askForm: %v", err)
	}
	if !answers[0].Skipped {
		t.Fatalf("the unanswered question must come back skipped: %+v", answers[0])
	}
	if answers[1].Custom != "none of them" || answers[1].Skipped {
		t.Fatalf("the answered one must not: %+v", answers[1])
	}
}

// A client answering the flat v1 question is answering the form's first one.
func TestAskForm_ExtensionV1AnswerFillsTheFirstQuestion(t *testing.T) {
	tr := &fakeQuestionTransport{extensionResp: questionAnswer{Kind: answerKindOption, OptionID: "opt-0"}}
	_, turn := questionFixture(t, tr, true, false)

	answers, err := turn.askForm(context.Background(), twoQuestionForm())
	if err != nil {
		t.Fatalf("askForm: %v", err)
	}
	if len(answers[0].Selected) != 1 || answers[0].Selected[0] != "Postgres" {
		t.Fatalf("the v1 answer must land on the first question: %+v", answers[0])
	}
	if !answers[1].Skipped {
		t.Fatalf("the questions a v1 client never saw must be skipped: %+v", answers[1])
	}
}

// Declining at the top level declines the interaction — the model is told
// nobody answered rather than being handed a form of skips.
func TestAskForm_ExtensionDeclineIsNotAFormOfSkips(t *testing.T) {
	tr := &fakeQuestionTransport{extensionResp: questionAnswer{Kind: answerKindDeclined}}
	_, turn := questionFixture(t, tr, true, false)

	if _, err := turn.askForm(context.Background(), twoQuestionForm()); !errors.Is(err, errQuestionUnanswered) {
		t.Fatalf("err = %v, want the unanswered error", err)
	}
}

// An elicitation-capable client gets the whole form as a JSON Schema: enum for
// a single-select question, array-of-enum for a multi-select one.
func TestAskForm_ElicitationSchemaCoversBothQuestionShapes(t *testing.T) {
	tr := &fakeQuestionTransport{elicitationResp: acpsdk.UnstableCreateElicitationResponse{
		Accept: &acpsdk.UnstableCreateElicitationAccept{
			Content: map[string]any{
				"q-0": "SQLite",
				"q-1": []any{"gofmt", "go vet"},
			},
		},
	}}
	_, turn := questionFixture(t, tr, false, true)
	form := twoQuestionForm()

	answers, err := turn.askForm(context.Background(), form)
	if err != nil {
		t.Fatalf("askForm: %v", err)
	}
	if len(tr.elicitationCalls) != 1 || len(tr.permissionCalls) != 0 {
		t.Fatalf("elicitation must be preferred over walking the form: %d elicitation, %d permission",
			len(tr.elicitationCalls), len(tr.permissionCalls))
	}

	schema := tr.elicitationCalls[0].RequestedSchema
	// One property per question, and the free-text field the first question's
	// `allow_custom` earns beside its picker.
	if len(schema.Properties) != 3 {
		t.Fatalf("expected a property per question plus the free-text field, got %d", len(schema.Properties))
	}
	single, _ := schema.Properties["q-0"].(map[string]any)
	if single["type"] != "string" {
		t.Fatalf("single-select property = %+v", single)
	}
	labels, _ := single["enum"].([]string)
	if len(labels) != 2 || labels[0] != "Postgres" {
		t.Fatalf("a question that allows free text keeps its options: %+v", single)
	}
	if desc, _ := single["description"].(string); !strings.Contains(desc, "Postgres: already provisioned") {
		t.Fatalf("option descriptions must survive into the schema: %q", desc)
	}
	multi, _ := schema.Properties["q-1"].(map[string]any)
	if multi["type"] != "array" {
		t.Fatalf("multi-select property = %+v", multi)
	}
	items, _ := multi["items"].(map[string]any)
	itemLabels, _ := items["enum"].([]string)
	if len(itemLabels) != 2 || itemLabels[0] != "go vet" {
		t.Fatalf("multi-select items = %+v", items)
	}
	if len(schema.Required) != 0 {
		t.Fatalf("nothing is required: a partly answered form is still an answer, got %v", schema.Required)
	}

	if len(answers[0].Selected) != 1 || answers[0].Selected[0] != "SQLite" {
		t.Fatalf("first answer = %+v", answers[0])
	}
	// Option order, not the order the client happened to send.
	if strings.Join(answers[1].Selected, ",") != "go vet,gofmt" {
		t.Fatalf("multi-select answer = %+v", answers[1])
	}
}

// A question left blank in the elicitation response is unanswered.
func TestAskForm_ElicitationPartialAnswerSkipsTheRest(t *testing.T) {
	tr := &fakeQuestionTransport{elicitationResp: acpsdk.UnstableCreateElicitationResponse{
		Accept: &acpsdk.UnstableCreateElicitationAccept{
			Content: map[string]any{"q-0": "use DuckDB instead"},
		},
	}}
	_, turn := questionFixture(t, tr, false, true)

	answers, err := turn.askForm(context.Background(), twoQuestionForm())
	if err != nil {
		t.Fatalf("askForm: %v", err)
	}
	if answers[0].Custom != "use DuckDB instead" {
		t.Fatalf("free text must reach the model verbatim: %+v", answers[0])
	}
	if !answers[1].Skipped {
		t.Fatalf("the blank question must come back skipped: %+v", answers[1])
	}
}

// A rejected elicitation is nobody answering, not everybody skipping.
func TestAskForm_ElicitationRejectIsNotAnAnswer(t *testing.T) {
	tr := &fakeQuestionTransport{}
	_, turn := questionFixture(t, tr, false, true)

	if _, err := turn.askForm(context.Background(), twoQuestionForm()); !errors.Is(err, errQuestionUnanswered) {
		t.Fatalf("err = %v, want the unanswered error", err)
	}
}

// With neither transport the form is walked: one permission request per
// question, assembled into one answer set.
func TestAskForm_PermissionWalkAssemblesOneAnswerSet(t *testing.T) {
	tr := &fakeQuestionTransport{permissionResp: selectedPermission("opt-0")}
	_, turn := questionFixture(t, tr, false, false)

	answers, err := turn.askForm(context.Background(), twoQuestionForm())
	if err != nil {
		t.Fatalf("askForm: %v", err)
	}
	if len(tr.permissionCalls) != 2 {
		t.Fatalf("expected one permission request per question, got %d", len(tr.permissionCalls))
	}
	// Descriptions have nowhere to go on a permission option, so they are folded
	// into the name — a bare picker still shows what separates the choices.
	first := tr.permissionCalls[0].Options[0]
	if first.Name != "Postgres — already provisioned" {
		t.Fatalf("option name = %q, want the description folded in", first.Name)
	}
	if len(answers) != 2 || answers[0].Selected[0] != "Postgres" || answers[1].Selected[0] != "go vet" {
		t.Fatalf("the walk must assemble every answer: %+v", answers)
	}
}

// A cancel part-way through the walk declines the whole form. Delivering the
// answers collected so far would report the questions the user never reached as
// ones they chose to skip.
func TestAskForm_WalkCancelDeclinesTheWholeForm(t *testing.T) {
	tr := &fakeQuestionTransport{permissionResp: acpsdk.RequestPermissionResponse{
		Outcome: acpsdk.RequestPermissionOutcome{Cancelled: &acpsdk.RequestPermissionOutcomeCancelled{Outcome: "cancelled"}},
	}}
	_, turn := questionFixture(t, tr, false, false)

	answers, err := turn.askForm(context.Background(), twoQuestionForm())
	if err == nil {
		t.Fatalf("a cancelled question must fail the form, got %+v", answers)
	}
	if answers != nil {
		t.Fatalf("nothing may be delivered from a declined form: %+v", answers)
	}
	if len(tr.permissionCalls) != 1 {
		t.Fatalf("the walk must stop at the cancel, got %d requests", len(tr.permissionCalls))
	}
}

// A single-question form keeps question.go's ladder: a native picker beats a
// text field, so elicitation does not take over a question the permission
// transport renders better.
func TestAskForm_SingleQuestionKeepsThePermissionLadder(t *testing.T) {
	tr := &fakeQuestionTransport{permissionResp: selectedPermission("opt-1")}
	_, turn := questionFixture(t, tr, false, true)

	form := agent.AskUserForm{Questions: twoQuestionForm().Questions[:1]}
	answers, err := turn.askForm(context.Background(), form)
	if err != nil {
		t.Fatalf("askForm: %v", err)
	}
	if len(tr.permissionCalls) != 1 || len(tr.elicitationCalls) != 0 {
		t.Fatalf("one question must go through the permission ladder: %d permission, %d elicitation",
			len(tr.permissionCalls), len(tr.elicitationCalls))
	}
	if answers[0].Selected[0] != "SQLite" {
		t.Fatalf("answer = %+v", answers[0])
	}
}

// A cancelled context resolves the call once, with an error — never a hang and
// never a fabricated answer.
func TestAskForm_CancelledContextFailsOnce(t *testing.T) {
	tr := &fakeQuestionTransport{permissionErr: context.Canceled}
	_, turn := questionFixture(t, tr, false, false)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := turn.askForm(ctx, twoQuestionForm()); err == nil {
		t.Fatal("a cancelled form must fail")
	}
}

// With no connection at all the model is told the question could not be put to
// anyone, rather than being handed the agent's own recommendation.
func TestAskForm_NoTransportFails(t *testing.T) {
	_, turn := questionFixture(t, nil, false, false)
	turn.bridge.questionTransport = nil

	if _, err := turn.askForm(context.Background(), twoQuestionForm()); !errors.Is(err, errQuestionUnreachable) {
		t.Fatalf("err = %v, want the unreachable error", err)
	}
}

// The schema one question turns into, per shape. Regression: a question with
// options *and* `allow_custom` used to be sent as a bare string — dropping the
// enum was the only way to leave free text unconstrained, and it cost the user
// the option list, the descriptions and the recommended marker on exactly the
// questions that offered the most.
func TestElicitationSchema_PerQuestionShape(t *testing.T) {
	options := []agent.AskUserOption{
		{Label: "Always ask", Description: "before anything destructive", IsRecommended: true},
		{Label: "Never ask"},
	}
	cases := []struct {
		name       string
		question   agent.AskUserQuestion
		wantType   string
		wantEnum   []string
		wantCustom bool
	}{
		{
			name:     "options only",
			question: agent.AskUserQuestion{Header: "Confirm", Question: "How?", Options: options},
			wantType: "string",
			wantEnum: []string{"Always ask", "Never ask"},
		},
		{
			name:     "custom only",
			question: agent.AskUserQuestion{Header: "Name", Question: "Call it what?", AllowCustom: true},
			wantType: "string",
		},
		{
			name:       "options and custom",
			question:   agent.AskUserQuestion{Header: "Confirm", Question: "How?", Options: options, AllowCustom: true},
			wantType:   "string",
			wantEnum:   []string{"Always ask", "Never ask"},
			wantCustom: true,
		},
		{
			name:     "multi-select",
			question: agent.AskUserQuestion{Header: "Checks", Question: "Which?", Options: options, MultiSelect: true},
			wantType: "array",
			wantEnum: []string{"Always ask", "Never ask"},
		},
		{
			name:       "multi-select and custom",
			question:   agent.AskUserQuestion{Header: "Checks", Question: "Which?", Options: options, MultiSelect: true, AllowCustom: true},
			wantType:   "array",
			wantEnum:   []string{"Always ask", "Never ask"},
			wantCustom: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			form := agent.AskUserForm{Questions: []agent.AskUserQuestion{tc.question}}
			props := elicitationProperties(form, buildFormPayload("sess", form))

			prop, _ := props["q-0"].(map[string]any)
			if prop["type"] != tc.wantType {
				t.Fatalf("type = %v, want %q (%+v)", prop["type"], tc.wantType, prop)
			}
			enum, _ := prop["enum"].([]string)
			if tc.wantType == "array" {
				items, _ := prop["items"].(map[string]any)
				enum, _ = items["enum"].([]string)
			}
			if strings.Join(enum, ",") != strings.Join(tc.wantEnum, ",") {
				t.Fatalf("options = %v, want %v", enum, tc.wantEnum)
			}
			// The question itself, and a line for every option the picker alone
			// does not describe — including which one the agent recommends.
			if desc, _ := prop["description"].(string); len(tc.wantEnum) > 0 &&
				!strings.Contains(desc, "Always ask: before anything destructive (recommended)") {
				t.Fatalf("description lost the option detail: %q", desc)
			}

			custom, hasCustom := props["q-0-custom"].(map[string]any)
			if hasCustom != tc.wantCustom {
				t.Fatalf("free-text field present = %v, want %v (%v)", hasCustom, tc.wantCustom, props)
			}
			if tc.wantCustom && custom["type"] != "string" {
				t.Fatalf("the free-text field must take text: %+v", custom)
			}
			if want := len(props); (tc.wantCustom && want != 2) || (!tc.wantCustom && want != 1) {
				t.Fatalf("property count = %d for %+v", want, props)
			}
		})
	}
}

// A user who picks an option *and* types their own words said both, and the
// model is told both: the picker and the text field are two fields of one
// question, and dropping either would misreport the form they filled in.
func TestAskForm_ElicitationOptionAndCustomTextBothCount(t *testing.T) {
	tr := &fakeQuestionTransport{elicitationResp: acpsdk.UnstableCreateElicitationResponse{
		Accept: &acpsdk.UnstableCreateElicitationAccept{
			Content: map[string]any{
				"q-0":        "Postgres",
				"q-0-custom": "  but only until the migration lands  ",
				"q-1":        []any{"gofmt"},
			},
		},
	}}
	_, turn := questionFixture(t, tr, false, true)

	answers, err := turn.askForm(context.Background(), twoQuestionForm())
	if err != nil {
		t.Fatalf("askForm: %v", err)
	}
	if len(answers[0].Selected) != 1 || answers[0].Selected[0] != "Postgres" {
		t.Fatalf("the picked option must survive the typed text: %+v", answers[0])
	}
	if answers[0].Custom != "but only until the migration lands" {
		t.Fatalf("the typed text must reach the model: %+v", answers[0])
	}
	if answers[0].Skipped {
		t.Fatalf("an answered question must not come back skipped: %+v", answers[0])
	}
}

// Typing an option's label into the free-text field is picking that option, not
// inventing an answer that happens to read like one.
func TestAskForm_ElicitationCustomTextNamingAnOptionPicksIt(t *testing.T) {
	tr := &fakeQuestionTransport{elicitationResp: acpsdk.UnstableCreateElicitationResponse{
		Accept: &acpsdk.UnstableCreateElicitationAccept{
			Content: map[string]any{"q-0-custom": "SQLite"},
		},
	}}
	_, turn := questionFixture(t, tr, false, true)

	answers, err := turn.askForm(context.Background(), twoQuestionForm())
	if err != nil {
		t.Fatalf("askForm: %v", err)
	}
	if len(answers[0].Selected) != 1 || answers[0].Selected[0] != "SQLite" || answers[0].Custom != "" {
		t.Fatalf("answer = %+v, want the named option chosen", answers[0])
	}
}

// A multi-select question that allows free text gets both too: the typed answer
// joins the ticked boxes rather than replacing them, which is what the TUI does
// with the same question.
func TestAskForm_ElicitationMultiSelectKeepsCustomTextBesideTheBoxes(t *testing.T) {
	form := twoQuestionForm()
	form.Questions[1].AllowCustom = true
	tr := &fakeQuestionTransport{elicitationResp: acpsdk.UnstableCreateElicitationResponse{
		Accept: &acpsdk.UnstableCreateElicitationAccept{
			Content: map[string]any{
				"q-1":        []any{"go vet", "gofmt"},
				"q-1-custom": "and staticcheck",
			},
		},
	}}
	_, turn := questionFixture(t, tr, false, true)

	answers, err := turn.askForm(context.Background(), form)
	if err != nil {
		t.Fatalf("askForm: %v", err)
	}
	if strings.Join(answers[1].Selected, ",") != "go vet,gofmt" {
		t.Fatalf("ticked boxes = %+v", answers[1])
	}
	if answers[1].Custom != "and staticcheck" {
		t.Fatalf("the typed answer must join the selection: %+v", answers[1])
	}
}
