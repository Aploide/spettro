package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestDecodeAskUserForm_NewSchema(t *testing.T) {
	form, err := decodeAskUserForm([]byte(`{
		"context":"the parser rewrite",
		"questions":[
			{"header":"Focus area","question":"What should I start with?","options":[
				{"label":"Lexer","description":"token boundaries first","is_recommended":true},
				{"label":"Parser","preview":"expr := term (('+'|'-') term)*"}
			]},
			{"header":"Extras","question":"Anything else to cover?","multi_select":true,"allow_custom":true,"options":[
				{"label":"Tests"},{"label":"Docs"}
			]}
		]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if form.Context != "the parser rewrite" {
		t.Fatalf("form context lost: %q", form.Context)
	}
	if len(form.Questions) != 2 {
		t.Fatalf("got %d questions, want 2", len(form.Questions))
	}
	first := form.Questions[0]
	if first.Header != "Focus area" || first.Question != "What should I start with?" {
		t.Fatalf("first question decoded as %+v", first)
	}
	if !first.Options[0].IsRecommended || first.Options[0].Description != "token boundaries first" {
		t.Fatalf("option flags lost: %+v", first.Options[0])
	}
	if first.Options[1].Preview == "" {
		t.Fatalf("preview lost: %+v", first.Options[1])
	}
	if first.MultiSelect || first.AllowCustom {
		t.Fatalf("flags invented on the first question: %+v", first)
	}
	second := form.Questions[1]
	if !second.MultiSelect || !second.AllowCustom {
		t.Fatalf("multi_select/allow_custom lost: %+v", second)
	}
}

// The flat payload older prompts and custom agent files send must keep working,
// normalised into a one-question form.
func TestDecodeAskUserForm_LegacyFlatPayload(t *testing.T) {
	form, err := decodeAskUserForm([]byte(`{"question":"Which database?","options":["Postgres","SQLite",""],"context":"local dev only","default_option":"SQLite","allow_free_response":true}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(form.Questions) != 1 {
		t.Fatalf("got %d questions, want 1", len(form.Questions))
	}
	q := form.Questions[0]
	if q.Question != "Which database?" || form.Context != "local dev only" {
		t.Fatalf("question/context lost: %+v %q", q, form.Context)
	}
	// Blank options were always dropped on this path rather than rejected.
	if len(q.Options) != 2 {
		t.Fatalf("got options %+v, want Postgres and SQLite", q.Options)
	}
	if q.Options[0].IsRecommended || !q.Options[1].IsRecommended {
		t.Fatalf("default_option did not become the recommended flag: %+v", q.Options)
	}
	if !q.AllowCustom {
		t.Fatal("allow_free_response did not become AllowCustom")
	}
	if q.Header == "" {
		t.Fatal("a headerless question must get a derived tab label")
	}
}

// A question with no options at all is free-text on the legacy path, exactly as
// it has always been (the ACP transports rely on it).
func TestDecodeAskUserForm_LegacyOptionlessIsFreeText(t *testing.T) {
	form, err := decodeAskUserForm([]byte(`{"question":"What should I name it?"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !form.Questions[0].AllowCustom {
		t.Fatal("an option-less legacy question must allow custom text")
	}
}

// Options may arrive as bare strings inside questions[] too; a model mixing the
// shapes should not get a decode error it cannot act on.
func TestDecodeAskUserForm_StringOptionsInsideQuestions(t *testing.T) {
	form, err := decodeAskUserForm([]byte(`{"questions":[{"question":"Ship it?","options":["yes",{"label":"no"}]}]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := form.Questions[0].Options; len(got) != 2 || got[0].Label != "yes" || got[1].Label != "no" {
		t.Fatalf("options decoded as %+v", got)
	}
}

func TestDecodeAskUserForm_DerivedHeadersAreDistinct(t *testing.T) {
	form, err := decodeAskUserForm([]byte(`{"questions":[
		{"question":"Which database should I use for this?","allow_custom":true},
		{"question":"Which database should I use for that?","allow_custom":true}]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if a, b := form.Questions[0].Header, form.Questions[1].Header; a == b {
		t.Fatalf("derived headers collided (%q): they key the answers", a)
	}
}

func TestDecodeAskUserForm_Rejections(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"no questions", `{"questions":[]}`, "at least one question"},
		{"no question text", `{"questions":[{"options":[{"label":"a"}]}]}`, "question text is required"},
		{"too many questions", `{"questions":[
			{"question":"1?","allow_custom":true},{"question":"2?","allow_custom":true},
			{"question":"3?","allow_custom":true},{"question":"4?","allow_custom":true},
			{"question":"5?","allow_custom":true}]}`, "at most 4 questions"},
		{"too many options", `{"questions":[{"question":"pick","options":[
			{"label":"1"},{"label":"2"},{"label":"3"},{"label":"4"},{"label":"5"},
			{"label":"6"},{"label":"7"},{"label":"8"},{"label":"9"}]}]}`, "at most 8 options"},
		{"duplicate headers", `{"questions":[
			{"header":"Scope","question":"1?","allow_custom":true},
			{"header":"scope","question":"2?","allow_custom":true}]}`, "duplicate header"},
		{"empty option label", `{"questions":[{"question":"pick","options":[{"label":"a"},{"label":" "}]}]}`, "label is required"},
		{"duplicate option labels", `{"questions":[{"question":"pick","options":[{"label":"a"},{"label":"A"}]}]}`, "duplicate option label"},
		{"nothing to answer", `{"questions":[{"question":"pick"}]}`, "nothing for the user to answer"},
		{"two recommendations", `{"questions":[{"question":"pick","options":[
			{"label":"a","is_recommended":true},{"label":"b","is_recommended":true}]}]}`, "recommends 2 options"},
		{"both shapes", `{"question":"a?","questions":[{"question":"b?","allow_custom":true}]}`, "not both"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeAskUserForm([]byte(tc.args))
			if err == nil {
				t.Fatal("expected a model-correctable error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// Recommending several options is meaningful when the question takes several
// answers.
func TestDecodeAskUserForm_MultiSelectMayRecommendSeveral(t *testing.T) {
	if _, err := decodeAskUserForm([]byte(`{"questions":[{"question":"pick","multi_select":true,"options":[
		{"label":"a","is_recommended":true},{"label":"b","is_recommended":true}]}]}`)); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func TestFormatAskUserAnswers(t *testing.T) {
	form := AskUserForm{Questions: []AskUserQuestion{
		{Header: "Focus area", Question: "Where first?"},
		{Header: "Extras", Question: "Anything else?", MultiSelect: true},
		{Header: "Deadline", Question: "By when?", AllowCustom: true},
		{Header: "Rollout", Question: "How to ship?"},
	}}
	out, err := formatAskUserAnswers(form, []AskUserAnswer{
		{Header: "Focus area", Selected: []string{"Lexer"}},
		{Header: "Extras", Selected: []string{"Tests", "Docs"}},
		{Header: "Deadline", Custom: "before the demo on friday", Notes: "hard deadline"},
		// Rollout intentionally unanswered.
	})
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 4 {
		t.Fatalf("want one line per question, got %d:\n%s", len(lines), out)
	}
	if lines[0] != "Focus area: Lexer" {
		t.Fatalf("single answer rendered as %q", lines[0])
	}
	if lines[1] != "Extras: Tests, Docs" {
		t.Fatalf("multi-select answer rendered as %q", lines[1])
	}
	if !strings.Contains(lines[2], `"before the demo on friday"`) {
		t.Fatalf("custom text must come back quoted and verbatim: %q", lines[2])
	}
	if !strings.Contains(lines[2], "hard deadline") {
		t.Fatalf("note lost: %q", lines[2])
	}
	if !strings.Contains(lines[3], askUserSkippedMarker) {
		t.Fatalf("an unanswered question must be marked, not silently omitted: %q", lines[3])
	}
}

// "None of these" is an answer to a multi-select question. It carries no
// content, so the consumer says so by clearing Skipped, and the model has to
// read it as a decision rather than as the silence of a question never opened.
func TestFormatAskUserAnswers_ExplicitEmptyAnswerIsNotASkip(t *testing.T) {
	form := AskUserForm{Questions: []AskUserQuestion{
		{Header: "Checks", Question: "Which checks?", MultiSelect: true},
		{Header: "Rollout", Question: "How to ship?"},
	}}
	out, err := formatAskUserAnswers(form, []AskUserAnswer{
		{Header: "Checks", Skipped: false},
		{Header: "Rollout", Skipped: true},
	})
	if err != nil {
		t.Fatalf("an explicitly empty answer is an answer, so the form is answered: %v", err)
	}
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[0], askUserNoneMarker) {
		t.Fatalf("want the none-of-these marker, got %q", lines[0])
	}
	if !strings.Contains(lines[1], askUserSkippedMarker) {
		t.Fatalf("the untouched question must still read as skipped: %q", lines[1])
	}
}

// A form nobody answered is an error, not a result the model can mistake for
// agreement with its recommendation.
func TestFormatAskUserAnswers_NothingAnsweredIsAnError(t *testing.T) {
	form := AskUserForm{Questions: []AskUserQuestion{{Header: "Scope", Question: "Which?"}}}
	if _, err := formatAskUserAnswers(form, []AskUserAnswer{{Header: "Scope", Skipped: true}}); err == nil {
		t.Fatal("expected an error when no question was answered")
	}
	if _, err := formatAskUserAnswers(form, nil); err == nil {
		t.Fatal("expected an error when the consumer returned no answers at all")
	}
}

// Answers are keyed by header, so a consumer that returns them out of order (or
// skips one) still lands on the right questions.
func TestAlignAskUserAnswers_MatchesByHeaderThenPosition(t *testing.T) {
	form := AskUserForm{Questions: []AskUserQuestion{
		{Header: "One", Question: "1?"},
		{Header: "Two", Question: "2?"},
	}}
	aligned := alignAskUserAnswers(form, []AskUserAnswer{
		{Header: "Two", Selected: []string{"b"}},
		{Selected: []string{"a"}},
	})
	if got := aligned[0].Selected; len(got) != 1 || got[0] != "a" {
		t.Fatalf("headerless answer did not fall back to position: %+v", aligned[0])
	}
	if got := aligned[1].Selected; len(got) != 1 || got[0] != "b" {
		t.Fatalf("header match lost: %+v", aligned[1])
	}
}

// The adapter is what every single-question consumer uses; a form must survive
// the round trip through it.
func TestQuestionByQuestion_RoundTrip(t *testing.T) {
	form := AskUserForm{
		Context: "the parser rewrite",
		Questions: []AskUserQuestion{
			{Header: "Focus area", Question: "Where first?", Options: []AskUserOption{
				{Label: "Lexer", Description: "token boundaries first", IsRecommended: true},
				{Label: "Parser"},
			}},
			{Header: "Extras", Question: "Anything else?", MultiSelect: true, Options: []AskUserOption{
				{Label: "Tests"}, {Label: "Docs"},
			}},
			{Header: "Deadline", Question: "By when?", AllowCustom: true},
		},
	}

	var seen []AskUserRequest
	ask := QuestionByQuestion(func(_ context.Context, req AskUserRequest) (string, error) {
		seen = append(seen, req)
		switch len(seen) {
		case 1:
			// Answer with the display string the picker actually showed.
			return req.Options[0], nil
		case 2:
			return "Tests, Docs", nil
		default:
			return "before the demo", nil
		}
	})

	answers, err := ask(context.Background(), form)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if len(seen) != 3 {
		t.Fatalf("the adapter asked %d questions, want 3", len(seen))
	}
	if seen[0].Context != "the parser rewrite" {
		t.Fatalf("form context did not reach the consumer: %q", seen[0].Context)
	}
	if !strings.Contains(seen[0].Options[0], "token boundaries first") {
		t.Fatalf("the description must be visible to a consumer that shows labels only: %q", seen[0].Options[0])
	}
	if seen[0].DefaultOption != seen[0].Options[0] {
		t.Fatalf("recommended option lost: %+v", seen[0])
	}
	if !seen[1].AllowFreeResponse || !strings.Contains(seen[1].Question, "more than one") {
		t.Fatalf("a multi-select question must tell a single-choice picker how to answer: %+v", seen[1])
	}
	if !seen[2].AllowFreeResponse || len(seen[2].Options) != 0 {
		t.Fatalf("free-text question flattened as %+v", seen[2])
	}

	if got := answers[0].Selected; len(got) != 1 || got[0] != "Lexer" {
		t.Fatalf("the display string must resolve back to the option label: %+v", answers[0])
	}
	if answers[0].Custom != "" {
		t.Fatalf("a chosen option must not come back as custom text: %+v", answers[0])
	}
	if got := answers[1].Selected; len(got) != 2 || got[0] != "Tests" || got[1] != "Docs" {
		t.Fatalf("typed multi-select answer not resolved: %+v", answers[1])
	}
	if answers[2].Custom != "before the demo" {
		t.Fatalf("custom text must round-trip verbatim: %+v", answers[2])
	}
}

// Declining one question declines the interaction: the tool call fails rather
// than reporting a partly invented form.
func TestQuestionByQuestion_ErrorFailsTheForm(t *testing.T) {
	form := AskUserForm{Questions: []AskUserQuestion{
		{Header: "One", Question: "1?", AllowCustom: true},
		{Header: "Two", Question: "2?", AllowCustom: true},
	}}
	calls := 0
	ask := QuestionByQuestion(func(context.Context, AskUserRequest) (string, error) {
		calls++
		return "", context.Canceled
	})
	if _, err := ask(context.Background(), form); err == nil {
		t.Fatal("expected the consumer's error to fail the form")
	}
	if calls != 1 {
		t.Fatalf("the adapter kept asking after an error (%d calls)", calls)
	}
}

// An empty reply is a skip, not an answer.
func TestQuestionByQuestion_EmptyReplyIsSkipped(t *testing.T) {
	form := AskUserForm{Questions: []AskUserQuestion{{Header: "One", Question: "1?", AllowCustom: true}}}
	answers, err := QuestionByQuestion(func(context.Context, AskUserRequest) (string, error) {
		return "  ", nil
	})(context.Background(), form)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if !answers[0].Skipped {
		t.Fatalf("blank reply recorded as an answer: %+v", answers[0])
	}
}

// AskSingleQuestion is the inverse adapter, used by callers inside this package
// that have one flat question to put to a form callback.
func TestAskSingleQuestion(t *testing.T) {
	var got AskUserForm
	answer, err := AskSingleQuestion(context.Background(), func(_ context.Context, form AskUserForm) ([]AskUserAnswer, error) {
		got = form
		return []AskUserAnswer{{Header: form.Questions[0].Header, Selected: []string{"Switch"}}}, nil
	}, AskUserRequest{Question: "Switch model?", Options: []string{"Switch", "Abort"}, DefaultOption: "Switch"})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if answer != "Switch" {
		t.Fatalf("answer resolved to %q", answer)
	}
	if len(got.Questions) != 1 || !got.Questions[0].Options[0].IsRecommended {
		t.Fatalf("flat request normalised as %+v", got)
	}

	if _, err := AskSingleQuestion(context.Background(), func(context.Context, AskUserForm) ([]AskUserAnswer, error) {
		return []AskUserAnswer{{Skipped: true}}, nil
	}, AskUserRequest{Question: "Switch model?", Options: []string{"Switch", "Abort"}, DefaultOption: "Switch"}); err == nil {
		t.Fatal("a skipped question must not resolve to the recommended option")
	}
}

// The chat escape hatch is neither an answer nor a decline: the tool succeeds
// with a result that says the user wants to talk, and the run stops on it so
// their next message arrives as an ordinary new turn (settled 2026-07-25 — no
// steering, no keeping the run alive while they compose).
func TestRunAskUser_ChatExitEndsTheTurn(t *testing.T) {
	rt := &toolRuntime{askUser: func(context.Context, AskUserForm) ([]AskUserAnswer, error) {
		return nil, ErrAskUserReplyInChat
	}}
	out, err := rt.runAskUser(context.Background(), []byte(`{"question":"Which database?","options":["Postgres","SQLite"]}`))
	if err != nil {
		t.Fatalf("the chat exit must not fail the tool call: %v", err)
	}
	if !strings.Contains(out, "reply in chat") {
		t.Fatalf("the model must be told why there is no answer, got %q", out)
	}
	if !rt.shouldStop() {
		t.Fatal("the chat exit must end the turn")
	}
	if got := rt.stopMessage(); got != askUserChatStopReason {
		t.Fatalf("unexpected closing line: %q", got)
	}
}

// A decline is still a failure: the model must not read "the user declined" as
// an invitation to keep going without an answer.
func TestRunAskUser_DeclineStillFailsTheCall(t *testing.T) {
	rt := &toolRuntime{askUser: func(context.Context, AskUserForm) ([]AskUserAnswer, error) {
		return nil, errors.New("user declined to answer")
	}}
	if _, err := rt.runAskUser(context.Background(), []byte(`{"question":"Which database?","options":["Postgres"]}`)); err == nil {
		t.Fatal("a decline must fail the tool call")
	}
	if rt.shouldStop() {
		t.Fatal("a decline must not end the turn — only the chat exit does")
	}
}
