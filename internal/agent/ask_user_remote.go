package agent

// Carrying a form over a flat wire.
//
// The TUI renders a whole AskUserForm; the remote event stream and the Telegram
// relay carry text. These adapters are what every such consumer shares: one
// versioned payload describing the form, and one way of reading a reply back
// onto it. Keeping them here rather than in each transport is what stops the
// two surfaces from disagreeing about what "the answer to question 2" means.

import "strings"

// RemoteAskUserVersion is the payload version of the `ask_user` remote event.
//
// v1 was one flat question per event. v2 adds `questions[]` describing the
// whole form, and keeps every v1 field alongside it — pointed at the question
// the user is being asked right now — so a client written against v1 keeps
// working without knowing forms exist.
const RemoteAskUserVersion = 2

// RemoteAskUserPayload renders a form as the `ask_user` event body. active is
// the question the flat fields describe: the TUI republishes as the user walks
// the form, and a client that can only answer one thing at a time is answering
// that one.
func RemoteAskUserPayload(form AskUserForm, active int) map[string]any {
	if active < 0 || active >= len(form.Questions) {
		active = 0
	}
	questions := make([]map[string]any, 0, len(form.Questions))
	for _, q := range form.Questions {
		options := make([]map[string]any, 0, len(q.Options))
		for _, opt := range q.Options {
			entry := map[string]any{"label": opt.Label}
			if desc := strings.TrimSpace(opt.Description); desc != "" {
				entry["description"] = desc
			}
			if opt.IsRecommended {
				entry["is_recommended"] = true
			}
			options = append(options, entry)
		}
		questions = append(questions, map[string]any{
			"header":              q.Header,
			"question":            strings.TrimSpace(q.Question),
			"options":             options,
			"multi_select":        q.MultiSelect,
			"allow_free_response": q.AllowCustom || len(q.Options) == 0,
		})
	}

	// The flat fields are the v1 shape, describing the active question. Options
	// stay a plain []string there: that is what a v1 client parses, and what the
	// Telegram renderer already reads.
	flat, _ := flattenAskUserQuestion(form.Questions[active], form.Context)
	return map[string]any{
		"version":             RemoteAskUserVersion,
		"count":               len(form.Questions),
		"active":              active,
		"questions":           questions,
		"question":            flat.Question,
		"options":             flat.Options,
		"context":             flat.Context,
		"default":             flat.DefaultOption,
		"allow_free_response": flat.AllowFreeResponse,
	}
}

// FlatAskUserRequest renders one question of a form as the single-question
// request a flat transport carries, with the option label behind each display
// string alongside it. It is how a consumer that can only ask one thing at a
// time gets the same wording as one that shows the whole form.
func FlatAskUserRequest(form AskUserForm, i int) (AskUserRequest, []string) {
	if i < 0 || i >= len(form.Questions) {
		return AskUserRequest{Context: strings.TrimSpace(form.Context)}, nil
	}
	return flattenAskUserQuestion(form.Questions[i], form.Context)
}

// AnswersFromRemote maps a remote reply onto the form. byHeader carries the
// per-question answers a v2 client sends; flat is the single answer a v1 client
// sends, which is read as the answer to the *first* question.
//
// That is the settled degradation for a client that does not understand forms:
// the questions it was never shown come back skipped, so the model is told
// nobody answered them rather than being handed silence it could read as
// agreement.
func AnswersFromRemote(form AskUserForm, flat string, byHeader map[string]string) []AskUserAnswer {
	answers := make([]AskUserAnswer, 0, len(form.Questions))
	for i, q := range form.Questions {
		text := lookupAskUserHeader(byHeader, q.Header)
		if text == "" && i == 0 {
			text = flat
		}
		if strings.TrimSpace(text) == "" {
			answers = append(answers, AskUserAnswer{Header: q.Header, Skipped: true})
			continue
		}
		answers = append(answers, ResolveAskUserText(q, text))
	}
	return answers
}

// lookupAskUserHeader finds a question's answer in a client-supplied map,
// tolerating the case having been changed in transit.
func lookupAskUserHeader(byHeader map[string]string, header string) string {
	if len(byHeader) == 0 || strings.TrimSpace(header) == "" {
		return ""
	}
	if text, ok := byHeader[header]; ok {
		return text
	}
	for key, text := range byHeader {
		if strings.EqualFold(strings.TrimSpace(key), strings.TrimSpace(header)) {
			return text
		}
	}
	return ""
}

// ResolveAskUserText maps one free-text reply onto one question: a reply naming
// an option — by its label, or by the "label — description" string a flat
// consumer displayed — selects it, and anything else is the user's own words,
// kept verbatim.
func ResolveAskUserText(q AskUserQuestion, text string) AskUserAnswer {
	display, labels := flattenAskUserQuestion(q, "")
	return q.answerFromText(text, display.Options, labels)
}

// ResolveAskUserValues maps a list of values onto one question, which is what a
// transport that can express a multi-select answer returns. Values naming an
// option are selected in option order; anything else is joined into the user's
// own words, so a client mixing the two loses neither.
func ResolveAskUserValues(q AskUserQuestion, values []string) AskUserAnswer {
	display, labels := flattenAskUserQuestion(q, "")
	answer := AskUserAnswer{Header: q.Header}
	var custom []string
	selected := map[string]bool{}
	for _, value := range values {
		if value = strings.TrimSpace(value); value == "" {
			continue
		}
		if label, ok := matchAskUserOption(value, display.Options, labels); ok {
			selected[label] = true
			continue
		}
		custom = append(custom, value)
	}
	// Option order, never the order the client happened to send them in: the
	// same set of choices has to produce the same answer every time.
	for _, opt := range q.Options {
		if selected[opt.Label] {
			answer.Selected = append(answer.Selected, opt.Label)
		}
	}
	answer.Custom = strings.Join(custom, ", ")
	answer.Skipped = len(answer.Selected) == 0 && answer.Custom == ""
	return answer
}
