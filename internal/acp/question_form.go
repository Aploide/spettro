package acp

// Whole-form agent questions over ACP.
//
// question.go carries one question at a time, which is all core ACP can express.
// The model asks a *form* of up to four related questions, so this file adds the
// two transports that can take one whole:
//
//  1. `_spettro/question/ask` with `questions[]` — a client that mirrored the
//     extension back at handshake renders its own wizard and answers everything
//     in one response;
//  2. `elicitation/create` in form mode — one JSON-Schema property per question,
//     which is the spec's own way of asking for several values at once.
//
// Below them the form is walked question by question through the ladder in
// question.go. A walk is what a client that can only render a permission prompt
// gets, and any refusal along it declines the whole form: half a form delivered
// as if the user had skipped the rest would be a lie about what they said.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/agent"
)

// formQuestionIDPrefix names a question on the wire and, in the elicitation
// path, keys its schema property. Headers are the model's words — they can
// carry spaces, punctuation and non-ASCII — so they are carried as data rather
// than used as identifiers.
const formQuestionIDPrefix = "q-"

// formOption is one selectable answer, with the fields task 02 gave options.
type formOption struct {
	ID            string `json:"id"`
	Label         string `json:"label"`
	Description   string `json:"description,omitempty"`
	Preview       string `json:"preview,omitempty"`
	IsRecommended bool   `json:"isRecommended,omitempty"`
}

// formQuestion is one question of the form.
type formQuestion struct {
	ID       string `json:"id"`
	Header   string `json:"header"`
	Question string `json:"question"`
	// Options is always present (possibly empty) so a client can branch on
	// length without a nil check.
	Options          []formOption `json:"options"`
	MultiSelect      bool         `json:"multiSelect"`
	AllowCustomInput bool         `json:"allowCustomInput"`
}

// formPayload is the v2 question payload: the whole form in `questions`, and
// the v1 flat fields describing its first question so a client written against
// version 1 still renders something a user can answer.
type formPayload struct {
	questionPayload
	Questions []formQuestion `json:"questions"`
}

// formAnswer is one question's answer on the wire. Both `optionIds` and the v1
// `optionId` are read, so a client that answers a single-select question the
// way version 1 spelled it is not wrong.
type formAnswer struct {
	QuestionID string   `json:"questionId,omitempty"`
	Header     string   `json:"header,omitempty"`
	Kind       string   `json:"kind"`
	OptionIDs  []string `json:"optionIds,omitempty"`
	OptionID   string   `json:"optionId,omitempty"`
	Text       string   `json:"text,omitempty"`
	Notes      string   `json:"notes,omitempty"`
}

// formAnswerEnvelope is what the extension call returns. A v2 client fills
// `answers`; a v1 client answers the flat question with the bare tagged shape,
// which is read as the answer to the first question.
type formAnswerEnvelope struct {
	Answers []formAnswer `json:"answers"`
	questionAnswer
}

// buildFormPayload converts a form into the wire shape.
func buildFormPayload(sessionID acpsdk.SessionId, form agent.AskUserForm) formPayload {
	questions := make([]formQuestion, 0, len(form.Questions))
	for i, q := range form.Questions {
		options := make([]formOption, 0, len(q.Options))
		for j, opt := range q.Options {
			options = append(options, formOption{
				ID:            fmt.Sprintf("opt-%d", j),
				Label:         opt.Label,
				Description:   strings.TrimSpace(opt.Description),
				Preview:       opt.Preview,
				IsRecommended: opt.IsRecommended,
			})
		}
		questions = append(questions, formQuestion{
			ID:               formQuestionID(i),
			Header:           q.Header,
			Question:         strings.TrimSpace(q.Question),
			Options:          options,
			MultiSelect:      q.MultiSelect,
			AllowCustomInput: q.AllowCustom || len(q.Options) == 0,
		})
	}

	// The flat fields are version 1's, describing the first question: a client
	// that never learned about forms answers that one, and the rest come back
	// unanswered rather than silently defaulted.
	flat, _ := agent.FlatAskUserRequest(form, 0)
	payload := buildQuestionPayload(sessionID, flat)
	payload.Version = formPayloadVersion
	return formPayload{questionPayload: payload, Questions: questions}
}

func formQuestionID(i int) string { return fmt.Sprintf("%s%d", formQuestionIDPrefix, i) }

// resolve maps the client's answers back onto the form. Answers are matched by
// question id, then by header; anything unmatched is dropped rather than
// guessed at, and a question with no answer comes back skipped.
func (p formPayload) resolve(form agent.AskUserForm, env formAnswerEnvelope) ([]agent.AskUserAnswer, error) {
	// A top-level decline is a decline of the interaction, not of one question.
	switch env.Kind {
	case answerKindDeclined, answerKindCancelled:
		return nil, errQuestionUnanswered
	}
	if len(env.Answers) == 0 {
		if env.questionAnswer.Kind == "" {
			return nil, errQuestionUnanswered
		}
		// Version 1's answer to the flat question.
		text, err := p.questionPayload.resolve(env.questionAnswer)
		if err != nil {
			return nil, err
		}
		return agent.AnswersFromRemote(form, text, nil), nil
	}

	out := make([]agent.AskUserAnswer, len(form.Questions))
	for i, q := range form.Questions {
		out[i] = agent.AskUserAnswer{Header: q.Header, Skipped: true}
	}
	for _, answer := range env.Answers {
		idx := p.questionIndex(answer)
		if idx < 0 || idx >= len(form.Questions) {
			continue
		}
		resolved, ok := p.resolveOne(form.Questions[idx], p.Questions[idx], answer)
		if !ok {
			continue
		}
		resolved.Notes = strings.TrimSpace(answer.Notes)
		out[idx] = resolved
	}
	return out, nil
}

// questionIndex resolves which question an answer belongs to.
func (p formPayload) questionIndex(answer formAnswer) int {
	for i, q := range p.Questions {
		if answer.QuestionID != "" && answer.QuestionID == q.ID {
			return i
		}
	}
	for i, q := range p.Questions {
		if answer.Header != "" && strings.EqualFold(strings.TrimSpace(answer.Header), strings.TrimSpace(q.Header)) {
			return i
		}
	}
	return -1
}

// resolveOne turns one wire answer into the agent's shape. A question the
// client declined stays skipped: within a form that is a per-question "no
// answer", which the model already knows how to read.
func (p formPayload) resolveOne(question agent.AskUserQuestion, wire formQuestion, answer formAnswer) (agent.AskUserAnswer, bool) {
	out := agent.AskUserAnswer{Header: question.Header}
	switch answer.Kind {
	case answerKindDeclined, answerKindCancelled:
		return out, false
	case answerKindCustom:
		text := strings.TrimSpace(answer.Text)
		if text == "" {
			return out, false
		}
		out.Custom = text
		return out, true
	}

	ids := answer.OptionIDs
	if len(ids) == 0 && answer.OptionID != "" {
		ids = []string{answer.OptionID}
	}
	for _, id := range ids {
		for _, opt := range wire.Options {
			if opt.ID == id {
				out.Selected = append(out.Selected, opt.Label)
				break
			}
		}
	}
	// Text alongside a selection is the user's own words on a question that
	// allowed both; text alone on an option question is still an answer.
	out.Custom = strings.TrimSpace(answer.Text)
	if len(out.Selected) == 0 && out.Custom == "" {
		return out, false
	}
	return out, true
}

// askForm is the ask-user entry point for ACP: the whole form if the client can
// take one, otherwise the question-by-question walk.
func (t *turnState) askForm(ctx context.Context, form agent.AskUserForm) ([]agent.AskUserAnswer, error) {
	if len(form.Questions) == 0 {
		return nil, errQuestionUnanswered
	}
	tr := t.bridge.transport()
	if tr == nil {
		return nil, errQuestionUnreachable
	}

	payload := buildFormPayload(t.sessionID, form)
	if t.bridge.clientHasExtension(extQuestionAsk) {
		env, err := askFormViaExtension(ctx, tr, payload)
		switch {
		case err == nil:
			return payload.resolve(form, env)
		case !isMethodNotFound(err):
			return nil, err
		}
		// Advertised but not implemented: fall through rather than failing a
		// form the client could still render some other way.
	}

	// Elicitation is preferred over walking the form: a JSON-Schema form asks
	// for every answer in one interaction, where the walk costs the user one
	// prompt per question. A single question keeps the question.go ladder,
	// where a native permission picker beats a text field.
	if len(form.Questions) > 1 && t.bridge.clientSupportsElicitationForm() {
		answers, err := askFormViaElicitation(ctx, tr, form, payload)
		if err == nil {
			return answers, nil
		}
		// A client that turned the request away — no such method, or a body it
		// could not read — showed the user nothing, so the form is still owed
		// them; anything else is an answer, or the lack of one, and stands.
		if !elicitationRejected(err) {
			return nil, err
		}
	}

	// The walk. QuestionByQuestion fails the whole form on the first refusal,
	// which is what a decline means here: the user is not answering this
	// interaction, and a partial delivery would misreport that as silence about
	// the questions they never saw.
	return agent.QuestionByQuestion(t.askUser)(ctx, form)
}

// askFormViaExtension is transport 1: the whole form out, every answer back.
func askFormViaExtension(ctx context.Context, tr questionTransport, payload formPayload) (formAnswerEnvelope, error) {
	raw, err := tr.CallExtension(ctx, extQuestionAsk, payload)
	if err != nil {
		return formAnswerEnvelope{}, err
	}
	var env formAnswerEnvelope
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &env); err != nil {
			return formAnswerEnvelope{}, fmt.Errorf("decode question answers: %w", err)
		}
	}
	return env, nil
}

// askFormViaElicitation is transport 2: a schema property per question — `enum`
// for a single-select, `array` of `enum` for a multi-select, a plain string
// where the question has no options — plus a free-text field beside the picker
// on a question that also takes the user's own words.
//
// Nothing is marked required. A form the user answers in part is the same
// interaction the TUI allows, and the questions they left alone come back
// skipped; requiring them would turn "I don't know" into a dead end.
func askFormViaElicitation(ctx context.Context, tr questionTransport, form agent.AskUserForm, payload formPayload) ([]agent.AskUserAnswer, error) {
	title := "Answer"
	req := newElicitationForm(acpsdk.SessionId(payload.SessionId), elicitationFormMessage(form), acpsdk.UnstableElicitationSchema{
		Title:      &title,
		Type:       "object",
		Properties: elicitationProperties(form, payload),
	})
	if context := strings.TrimSpace(form.Context); context != "" {
		req.RequestedSchema.Description = &context
	}
	req.Meta = map[string]any{questionMetaKey: payload}

	resp, err := tr.CreateElicitation(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Accept == nil {
		return nil, errQuestionUnanswered
	}

	answers := make([]agent.AskUserAnswer, 0, len(form.Questions))
	answered := false
	for i, q := range form.Questions {
		wire := payload.Questions[i]
		// The picker and the free-text field are two fields of one question, so
		// they are read as one answer: what the user typed alongside a choice is
		// their own words on it, and resolving both together is what lets typed
		// text that names an option count as picking it.
		values := elicitationValues(resp.Accept.Content[wire.ID])
		values = append(values, elicitationValues(resp.Accept.Content[elicitationCustomID(wire.ID)])...)
		if len(values) == 0 {
			answers = append(answers, agent.AskUserAnswer{Header: q.Header, Skipped: true})
			continue
		}
		answered = true
		answers = append(answers, agent.ResolveAskUserValues(q, values))
	}
	if !answered {
		return nil, errQuestionUnanswered
	}
	return answers, nil
}

// elicitationCustomSuffix names the free-text field belonging to a question
// that also has options. Question ids are `q-<n>`, so the suffixed id can never
// collide with another question's.
const elicitationCustomSuffix = "-custom"

func elicitationCustomID(questionID string) string { return questionID + elicitationCustomSuffix }

// elicitationProperties is the whole form's schema: one property per question,
// and a second one for each question that offers options *and* accepts the
// user's own words.
//
// Nothing in the elicitation schema is both a picker and a text box, so such a
// question is asked as two fields rather than one. It used to be asked as a
// bare string — dropping the enum was the only way to leave the free text
// unconstrained — which cost the user the option list, the descriptions and the
// recommended marker on exactly the questions that offered the most.
func elicitationProperties(form agent.AskUserForm, payload formPayload) map[string]any {
	properties := make(map[string]any, len(payload.Questions))
	for i, wire := range payload.Questions {
		properties[wire.ID] = elicitationProperty(form.Questions[i], wire)
		if id, prop, ok := elicitationCustomProperty(wire); ok {
			properties[id] = prop
		}
	}
	return properties
}

// elicitationProperty is one question's picker. Option descriptions and the
// recommended marker have no place of their own in an enum, so they are folded
// into the property's description — a client rendering a dropdown still shows
// what separates the choices, and which one the agent would pick.
func elicitationProperty(q agent.AskUserQuestion, wire formQuestion) map[string]any {
	prop := map[string]any{"type": "string"}
	if wire.Header != "" {
		prop["title"] = wire.Header
	}
	prop["description"] = elicitationDescription(wire)

	labels := make([]string, 0, len(wire.Options))
	for _, opt := range wire.Options {
		labels = append(labels, opt.Label)
	}
	switch {
	case len(labels) == 0:
		// Free text: nothing to constrain it with, and no second field to add —
		// this property is the free-text answer.
	case q.MultiSelect:
		prop["type"] = "array"
		prop["items"] = map[string]any{"type": "string", "enum": labels}
	default:
		prop["enum"] = labels
	}
	return prop
}

// elicitationCustomProperty is the free-text field beside the picker, or false
// for a question that has no picker to sit beside. Blank is its resting state:
// a user who is happy with one of the options must not have to clear a field to
// say so.
func elicitationCustomProperty(wire formQuestion) (string, map[string]any, bool) {
	if !wire.AllowCustomInput || len(wire.Options) == 0 {
		return "", nil, false
	}
	title := "Your own answer"
	if wire.Header != "" {
		title = wire.Header + " — your own answer"
	}
	return elicitationCustomID(wire.ID), map[string]any{
		"type":        "string",
		"title":       title,
		"description": "Leave blank to answer with one of the options, or type your own answer here.",
	}, true
}

// elicitationDescription is the question, plus a line for every option the
// picker alone does not fully describe.
func elicitationDescription(wire formQuestion) string {
	lines := []string{wire.Question}
	for _, opt := range wire.Options {
		detail := opt.Description
		if opt.IsRecommended {
			detail = strings.TrimSpace(detail + " (recommended)")
		}
		if detail == "" {
			// The label is already in the enum; a line repeating it says nothing.
			continue
		}
		lines = append(lines, "- "+opt.Label+": "+detail)
	}
	return strings.Join(lines, "\n")
}

// elicitationFormMessage is the prompt above the whole form.
func elicitationFormMessage(form agent.AskUserForm) string {
	if len(form.Questions) == 1 {
		return strings.TrimSpace(form.Questions[0].Question)
	}
	return fmt.Sprintf("The agent has %d questions for you.", len(form.Questions))
}

// elicitationValues normalises one field of an elicitation response: clients
// send a string for a single answer and an array for a multi-select one.
func elicitationValues(raw any) []string {
	switch value := raw.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return nil
		}
		return []string{value}
	case []string:
		return value
	case []any:
		out := make([]string, 0, len(value))
		for _, entry := range value {
			if text, ok := entry.(string); ok && strings.TrimSpace(text) != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}
