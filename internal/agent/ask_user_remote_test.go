package agent

import (
	"strings"
	"testing"
)

func remoteForm() AskUserForm {
	return AskUserForm{
		Context: "both are already provisioned",
		Questions: []AskUserQuestion{
			{
				Header:   "Database",
				Question: "Which database?",
				Options: []AskUserOption{
					{Label: "Postgres", Description: "already provisioned"},
					{Label: "SQLite", IsRecommended: true},
				},
				AllowCustom: true,
			},
			{
				Header:      "Checks",
				Question:    "Which checks run before commits?",
				Options:     []AskUserOption{{Label: "go vet"}, {Label: "gofmt"}},
				MultiSelect: true,
			},
		},
	}
}

// The event carries the whole form and keeps every v1 field alongside it, so a
// client written before forms existed still has something to render.
func TestRemoteAskUserPayload_CarriesTheFormAndTheFlatShape(t *testing.T) {
	payload := RemoteAskUserPayload(remoteForm(), 1)

	if payload["version"] != RemoteAskUserVersion {
		t.Fatalf("version = %v, want %d", payload["version"], RemoteAskUserVersion)
	}
	if payload["count"] != 2 || payload["active"] != 1 {
		t.Fatalf("count/active = %v/%v", payload["count"], payload["active"])
	}
	questions, _ := payload["questions"].([]map[string]any)
	if len(questions) != 2 {
		t.Fatalf("expected both questions in the event, got %d", len(questions))
	}
	if questions[0]["header"] != "Database" || questions[1]["multi_select"] != true {
		t.Fatalf("questions lost their shape: %+v", questions)
	}
	options, _ := questions[0]["options"].([]map[string]any)
	if len(options) != 2 || options[0]["description"] != "already provisioned" || options[1]["is_recommended"] != true {
		t.Fatalf("option detail lost: %+v", options)
	}

	// The flat fields describe the *active* question — the one this client is
	// being asked to answer — and options stay plain strings there, which is
	// what a v1 client parses.
	if payload["question"] == "" || !strings.Contains(payload["question"].(string), "Which checks") {
		t.Fatalf("the flat question must be the active one: %v", payload["question"])
	}
	if _, ok := payload["options"].([]string); !ok {
		t.Fatalf("the flat options must stay []string, got %T", payload["options"])
	}
	if payload["allow_free_response"] != true {
		t.Fatal("a multi-select question needs free text: a flat picker cannot express several answers")
	}
}

// An out-of-range active index falls back to the first question rather than
// publishing an event with no flat shape at all.
func TestRemoteAskUserPayload_ClampsTheActiveIndex(t *testing.T) {
	payload := RemoteAskUserPayload(remoteForm(), 9)
	if payload["active"] != 0 {
		t.Fatalf("active = %v, want the first question", payload["active"])
	}
}

// A form-aware client answers every question at once, keyed by header.
func TestAnswersFromRemote_PerQuestionAnswers(t *testing.T) {
	form := remoteForm()
	answers := AnswersFromRemote(form, "", map[string]string{
		"Database": "Postgres",
		"checks":   "go vet, gofmt", // case is not the client's problem
	})

	if len(answers) != 2 {
		t.Fatalf("expected one answer per question, got %d", len(answers))
	}
	if len(answers[0].Selected) != 1 || answers[0].Selected[0] != "Postgres" {
		t.Fatalf("first answer = %+v", answers[0])
	}
	if strings.Join(answers[1].Selected, ",") != "go vet,gofmt" {
		t.Fatalf("a multi-select answer must resolve every part: %+v", answers[1])
	}
}

// Settled 2026-07-25: a client that only understands the flat shape answers the
// first question, and the rest come back skipped — deterministically, and never
// as if the user had agreed to the model's recommendation.
func TestAnswersFromRemote_FlatClientAnswersTheFirstQuestion(t *testing.T) {
	form := remoteForm()
	answers := AnswersFromRemote(form, "SQLite", nil)

	if len(answers[0].Selected) != 1 || answers[0].Selected[0] != "SQLite" {
		t.Fatalf("first answer = %+v", answers[0])
	}
	if !answers[1].Skipped {
		t.Fatalf("the question a flat client never saw must be skipped: %+v", answers[1])
	}

	out, err := formatAskUserAnswers(form, answers)
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	if !strings.Contains(out, askUserSkippedMarker) {
		t.Fatalf("the model has to be told which question went unanswered: %q", out)
	}
}

// Anything that is not an option is the user's own words, kept verbatim — and
// the display string a flat client showed resolves back to the option's label.
func TestResolveAskUserText_OptionsAndOwnWords(t *testing.T) {
	q := remoteForm().Questions[0]

	if got := ResolveAskUserText(q, "Postgres — already provisioned"); len(got.Selected) != 1 || got.Selected[0] != "Postgres" {
		t.Fatalf("the displayed string must resolve to the label: %+v", got)
	}
	got := ResolveAskUserText(q, "neither, use the MySQL box")
	if got.Custom != "neither, use the MySQL box" || len(got.Selected) != 0 {
		t.Fatalf("free text must stay verbatim: %+v", got)
	}
}

// Values come back in option order however the client sent them, and a value
// that is not an option is kept as the user's own words rather than dropped.
func TestResolveAskUserValues_OrderAndLeftovers(t *testing.T) {
	q := remoteForm().Questions[1]

	got := ResolveAskUserValues(q, []string{"gofmt", "staticcheck", "go vet"})
	if strings.Join(got.Selected, ",") != "go vet,gofmt" {
		t.Fatalf("selected = %+v, want option order", got.Selected)
	}
	if got.Custom != "staticcheck" {
		t.Fatalf("an unrecognised value must survive as the user's words: %+v", got)
	}
	if got.Skipped {
		t.Fatal("an answer with content is not a skip")
	}

	if empty := ResolveAskUserValues(q, []string{"  "}); !empty.Skipped {
		t.Fatalf("nothing at all is a skip: %+v", empty)
	}
}
