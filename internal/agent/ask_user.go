package agent

// Agent-authored questions.
//
// The model asks through the ask-user tool, which decodes a *form*: an ordered
// list of questions, each with a short header (the tab label a UI renders), the
// full question line, option objects carrying label / description / preview /
// recommended, a multi-select flag and an optional custom-answer entry. Every
// consumer renders from this model.
//
// Consumers that can only put one question at a time to the user — today the
// TUI picker, the ACP transports, remote/Telegram — do not each reimplement the
// walk: QuestionByQuestion adapts their single-question callback into the form
// callback, flattening one question into an AskUserRequest and mapping the
// answer back. AskSingleQuestion is the inverse, for code inside this package
// that has one flat question to ask through a form callback.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Form caps. Over the cap is a model-correctable tool error, never silent
// truncation. The option cap counts agent-supplied options only: the client-side
// "type something" / "chat about this" rows sit outside it.
const (
	MaxAskUserQuestions = 4
	MaxAskUserOptions   = 8
)

// AskUserForm is one interaction: every question in it is put to the user
// together, and every answer comes back together.
type AskUserForm struct {
	Questions []AskUserQuestion
	// Context is one line of background that applies to the whole form.
	Context string
}

// AskUserQuestion is a single question in a form.
type AskUserQuestion struct {
	// Header is the short tab label, e.g. "Focus area". Derived from the
	// question text when the model does not supply one.
	Header string
	// Question is the full question line.
	Question string
	Options  []AskUserOption
	// MultiSelect allows more than one option to be chosen.
	MultiSelect bool
	// AllowCustom renders the client's free-text entry alongside the options.
	AllowCustom bool
}

// AskUserOption is one selectable answer.
type AskUserOption struct {
	Label string
	// Description is the muted line under the label; optional.
	Description string
	// Preview is preformatted content a UI can show beside the option list;
	// optional.
	Preview string
	// IsRecommended marks the answer the agent would pick. More than one is
	// only meaningful on a multi-select question.
	IsRecommended bool
}

// AskUserAnswer is what the user said about one question. Skipped is the
// difference between "no answer" and a chosen default: the model must never be
// told a human decided something they never saw.
type AskUserAnswer struct {
	Header string
	// Selected holds option labels; at most one unless the question was
	// multi-select.
	Selected []string
	// Custom is the user's own text, returned to the model verbatim.
	Custom string
	// Notes is an optional annotation the user attached to their answer.
	Notes string
	// Skipped is set when the form was submitted without answering this
	// question.
	Skipped bool
}

// AskUserCallback presents a whole form and returns one answer per question.
// Answers may come back in any order (they are matched by header) and may be
// shorter than the form: the missing ones count as skipped.
type AskUserCallback func(context.Context, AskUserForm) ([]AskUserAnswer, error)

// AskUserRequest is the single-question view of a form question, handed to
// consumers that can only ask one thing at a time. Options are flattened to
// display strings; QuestionByQuestion maps the chosen string back to the
// option it came from.
type AskUserRequest struct {
	Question          string
	Options           []string
	Context           string
	DefaultOption     string
	AllowFreeResponse bool
}

// AskUserQuestionCallback is the one-question-at-a-time consumer shape.
type AskUserQuestionCallback func(context.Context, AskUserRequest) (string, error)

// errAskUserNoAnswer is what the model sees when the interaction produced
// nothing usable. It never sees a substituted default.
var errAskUserNoAnswer = errors.New("user did not answer")

// askUserMultiSelectHint tells the user that a single-choice picker is standing
// in for a multi-select question, and how to answer anyway.
const askUserMultiSelectHint = "(more than one answer is allowed — pick one, or type several separated by commas)"

// askUserSkippedMarker distinguishes an unanswered question from an answered
// one in the tool result, so the model does not read silence as a default.
const askUserSkippedMarker = "(not answered)"

// QuestionByQuestion adapts a single-question consumer into a form callback by
// walking the form in order. An error from any question fails the whole form:
// declining a question declines the interaction, which is what every existing
// consumer already means by it.
func QuestionByQuestion(ask AskUserQuestionCallback) AskUserCallback {
	if ask == nil {
		return nil
	}
	return func(ctx context.Context, form AskUserForm) ([]AskUserAnswer, error) {
		answers := make([]AskUserAnswer, 0, len(form.Questions))
		for _, q := range form.Questions {
			req, labels := flattenAskUserQuestion(q, form.Context)
			raw, err := ask(ctx, req)
			if err != nil {
				return nil, err
			}
			answers = append(answers, q.answerFromText(raw, req.Options, labels))
		}
		return answers, nil
	}
}

// AskSingleQuestion puts one flat question to a form callback and resolves the
// answer back to a string.
func AskSingleQuestion(ctx context.Context, ask AskUserCallback, req AskUserRequest) (string, error) {
	if ask == nil {
		return "", fmt.Errorf("no interactive callback configured")
	}
	form := askUserFormFromRequest(req)
	answers, err := ask(ctx, form)
	if err != nil {
		return "", err
	}
	aligned := alignAskUserAnswers(form, answers)
	answer := aligned[0]
	switch {
	case answer.Custom != "":
		return answer.Custom, nil
	case len(answer.Selected) > 0:
		return strings.Join(answer.Selected, ", "), nil
	default:
		return "", errAskUserNoAnswer
	}
}

// flattenAskUserQuestion renders one question as a single-question request. The
// second return value is the canonical option label behind each display string,
// parallel to req.Options: descriptions are folded into the display string so a
// user choosing from a bare list still sees what separates the options.
func flattenAskUserQuestion(q AskUserQuestion, formContext string) (AskUserRequest, []string) {
	req := AskUserRequest{
		Question:          strings.TrimSpace(q.Question),
		Context:           strings.TrimSpace(formContext),
		AllowFreeResponse: q.AllowCustom || len(q.Options) == 0,
	}
	if q.MultiSelect {
		req.Question = strings.TrimSpace(req.Question + " " + askUserMultiSelectHint)
		// A single-choice picker cannot express a multi-select answer; free
		// text is the only way the user can give one.
		req.AllowFreeResponse = true
	}
	labels := make([]string, 0, len(q.Options))
	for _, o := range q.Options {
		display := o.Label
		if desc := strings.TrimSpace(o.Description); desc != "" {
			display = o.Label + " — " + desc
		}
		req.Options = append(req.Options, display)
		labels = append(labels, o.Label)
		if o.IsRecommended && req.DefaultOption == "" {
			req.DefaultOption = display
		}
	}
	return req, labels
}

// answerFromText maps a single-question consumer's reply back onto the
// question. display/labels are the parallel slices flattenAskUserQuestion
// returned, so a chosen display string resolves to the option's own label and
// anything else is kept verbatim as the user's own words.
func (q AskUserQuestion) answerFromText(text string, display, labels []string) AskUserAnswer {
	answer := AskUserAnswer{Header: q.Header}
	text = strings.TrimSpace(text)
	if text == "" {
		answer.Skipped = true
		return answer
	}
	if label, ok := matchAskUserOption(text, display, labels); ok {
		answer.Selected = []string{label}
		return answer
	}
	if q.MultiSelect {
		// The user typed several answers because the picker could only take
		// one; treat it as a selection when every part names an option.
		parts := strings.Split(text, ",")
		matched := make([]string, 0, len(parts))
		for _, part := range parts {
			label, ok := matchAskUserOption(part, display, labels)
			if !ok {
				matched = nil
				break
			}
			matched = append(matched, label)
		}
		if len(matched) > 0 {
			answer.Selected = matched
			return answer
		}
	}
	answer.Custom = text
	return answer
}

// matchAskUserOption resolves a reply to an option label, accepting either the
// display string the consumer showed or the label itself.
func matchAskUserOption(text string, display, labels []string) (string, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", false
	}
	for i, d := range display {
		if strings.EqualFold(text, strings.TrimSpace(d)) {
			return labels[i], true
		}
	}
	for _, label := range labels {
		if strings.EqualFold(text, strings.TrimSpace(label)) {
			return label, true
		}
	}
	return "", false
}

// alignAskUserAnswers returns exactly one answer per question, in form order.
// Answers are matched by header, then positionally for a consumer that did not
// echo one; anything unmatched counts as skipped.
func alignAskUserAnswers(form AskUserForm, answers []AskUserAnswer) []AskUserAnswer {
	out := make([]AskUserAnswer, len(form.Questions))
	filled := make([]bool, len(form.Questions))
	used := make([]bool, len(answers))
	for i, q := range form.Questions {
		out[i] = AskUserAnswer{Header: q.Header, Skipped: true}
		for j, a := range answers {
			if used[j] || !strings.EqualFold(strings.TrimSpace(a.Header), strings.TrimSpace(q.Header)) {
				continue
			}
			used[j] = true
			filled[i] = true
			out[i] = a
			out[i].Header = q.Header
			break
		}
	}
	// Positional fallback for headerless answers, in the order they arrived.
	for i := range out {
		if filled[i] {
			continue
		}
		for j, a := range answers {
			if used[j] || strings.TrimSpace(a.Header) != "" {
				continue
			}
			used[j] = true
			out[i] = a
			out[i].Header = form.Questions[i].Header
			break
		}
	}
	// Skipped is derived, not trusted: "answered" is exactly "said something",
	// so a consumer cannot report a selection and a skip at the same time.
	for i := range out {
		selected := make([]string, 0, len(out[i].Selected))
		for _, label := range out[i].Selected {
			if label = strings.TrimSpace(label); label != "" {
				selected = append(selected, label)
			}
		}
		out[i].Selected = selected
		out[i].Custom = strings.TrimSpace(out[i].Custom)
		out[i].Notes = strings.TrimSpace(out[i].Notes)
		out[i].Skipped = len(selected) == 0 && out[i].Custom == "" && out[i].Notes == ""
	}
	return out
}

// formatAskUserAnswers renders the answers into the tool result the model
// reads: one `<header>: <answer>` line per question, multi-select comma-joined,
// the user's own words quoted verbatim, unanswered questions marked so the
// model cannot mistake them for a default.
func formatAskUserAnswers(form AskUserForm, answers []AskUserAnswer) (string, error) {
	aligned := alignAskUserAnswers(form, answers)
	lines := make([]string, 0, len(aligned))
	answered := false
	for i, a := range aligned {
		parts := append([]string(nil), a.Selected...)
		if a.Custom != "" {
			parts = append(parts, strconv.Quote(a.Custom))
		}
		text := askUserSkippedMarker
		if len(parts) > 0 {
			text = strings.Join(parts, ", ")
			answered = true
		}
		if note := strings.TrimSpace(a.Notes); note != "" {
			text += " — note: " + strconv.Quote(note)
			answered = true
		}
		lines = append(lines, form.Questions[i].Header+": "+text)
	}
	if !answered {
		return "", errAskUserNoAnswer
	}
	return strings.Join(lines, "\n"), nil
}

// --- tool schema decoding ---

// askUserOptionArgs accepts an option as an object or as a bare string, so the
// legacy `options: ["a","b"]` shape and a model that mixes the two both decode.
type askUserOptionArgs struct {
	Label         string `json:"label"`
	Description   string `json:"description"`
	Preview       string `json:"preview"`
	IsRecommended bool   `json:"is_recommended"`
}

func (o *askUserOptionArgs) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, `"`) {
		var label string
		if err := json.Unmarshal(data, &label); err != nil {
			return err
		}
		*o = askUserOptionArgs{Label: label}
		return nil
	}
	type plain askUserOptionArgs
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*o = askUserOptionArgs(decoded)
	return nil
}

func (o askUserOptionArgs) option() AskUserOption {
	return AskUserOption{
		Label:         strings.TrimSpace(o.Label),
		Description:   strings.TrimSpace(o.Description),
		Preview:       o.Preview,
		IsRecommended: o.IsRecommended,
	}
}

type askUserQuestionArgs struct {
	Header      string              `json:"header"`
	Question    string              `json:"question"`
	Options     []askUserOptionArgs `json:"options"`
	MultiSelect bool                `json:"multi_select"`
	AllowCustom bool                `json:"allow_custom"`
}

// askUserArgs is both shapes at once: the form (`questions`) and the legacy
// flat single question kept working for older prompts and custom agent files.
type askUserArgs struct {
	Questions []askUserQuestionArgs `json:"questions"`
	Context   string                `json:"context"`

	Question          string              `json:"question"`
	Options           []askUserOptionArgs `json:"options"`
	DefaultOption     string              `json:"default_option"`
	AllowFreeResponse bool                `json:"allow_free_response"`
}

// decodeAskUserForm parses the tool arguments into a validated form. Every
// error it returns is phrased for the model to correct and retry.
func decodeAskUserForm(rawArgs []byte) (AskUserForm, error) {
	var args askUserArgs
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return AskUserForm{}, fmt.Errorf("args: %w", err)
	}
	form := AskUserForm{Context: strings.TrimSpace(args.Context)}
	switch {
	case len(args.Questions) > 0:
		// Silently ignoring the flat fields would drop an option list the model
		// meant the user to see.
		if strings.TrimSpace(args.Question) != "" || len(args.Options) > 0 ||
			strings.TrimSpace(args.DefaultOption) != "" || args.AllowFreeResponse {
			return AskUserForm{}, fmt.Errorf("pass either questions[] or the flat question/options fields, not both")
		}
		for _, qa := range args.Questions {
			q := AskUserQuestion{
				Header:      strings.TrimSpace(qa.Header),
				Question:    strings.TrimSpace(qa.Question),
				MultiSelect: qa.MultiSelect,
				AllowCustom: qa.AllowCustom,
			}
			for _, oa := range qa.Options {
				q.Options = append(q.Options, oa.option())
			}
			form.Questions = append(form.Questions, q)
		}
	case strings.TrimSpace(args.Question) == "":
		return AskUserForm{}, fmt.Errorf("at least one question is required")
	default:
		// Legacy flat payload: one question, options as plain strings, the
		// recommended option named by default_option. Blank options are
		// dropped and a question without options becomes free-text, exactly as
		// this shape has always behaved.
		legacy := AskUserRequest{
			Question:          strings.TrimSpace(args.Question),
			Context:           form.Context,
			DefaultOption:     strings.TrimSpace(args.DefaultOption),
			AllowFreeResponse: args.AllowFreeResponse,
		}
		for _, oa := range args.Options {
			if label := strings.TrimSpace(oa.Label); label != "" {
				legacy.Options = append(legacy.Options, label)
			}
		}
		form = askUserFormFromRequest(legacy)
	}
	fillAskUserHeaders(&form)
	if err := validateAskUserForm(form); err != nil {
		return AskUserForm{}, err
	}
	return form, nil
}

// askUserFormFromRequest normalises a flat single question into a one-question
// form: the shape the legacy tool payload and every internal caller speak.
func askUserFormFromRequest(req AskUserRequest) AskUserForm {
	def := strings.TrimSpace(req.DefaultOption)
	q := AskUserQuestion{
		Header:      strings.TrimSpace(req.Question),
		Question:    strings.TrimSpace(req.Question),
		AllowCustom: req.AllowFreeResponse || len(req.Options) == 0,
	}
	for _, label := range req.Options {
		q.Options = append(q.Options, AskUserOption{
			Label:         label,
			IsRecommended: def != "" && strings.EqualFold(label, def),
		})
	}
	form := AskUserForm{Questions: []AskUserQuestion{q}, Context: strings.TrimSpace(req.Context)}
	fillAskUserHeaders(&form)
	return form
}

// fillAskUserHeaders derives a tab label for every question the model left
// headerless. Headers key the tab strip and the answers, so a derived one that
// collides with another is disambiguated here rather than rejected — only
// duplicates the model itself supplied are its mistake to fix (see
// validateAskUserForm).
func fillAskUserHeaders(form *AskUserForm) {
	taken := map[string]struct{}{}
	for _, q := range form.Questions {
		if h := strings.TrimSpace(q.Header); h != "" {
			taken[strings.ToLower(h)] = struct{}{}
		}
	}
	for i := range form.Questions {
		q := &form.Questions[i]
		q.Header = strings.TrimSpace(q.Header)
		if q.Header != "" {
			continue
		}
		base := deriveAskUserHeader(q.Question)
		if base == "" {
			base = fmt.Sprintf("Question %d", i+1)
		}
		header := base
		for n := 2; ; n++ {
			if _, clash := taken[strings.ToLower(header)]; !clash {
				break
			}
			header = fmt.Sprintf("%s %d", base, n)
		}
		taken[strings.ToLower(header)] = struct{}{}
		q.Header = header
	}
}

// deriveAskUserHeader shortens a question into a tab label: the first few words,
// stripped of trailing punctuation.
func deriveAskUserHeader(question string) string {
	const maxWords = 4
	const maxRunes = 24
	words := strings.Fields(strings.TrimSpace(question))
	if len(words) > maxWords {
		words = words[:maxWords]
	}
	header := strings.TrimRight(strings.Join(words, " "), " ?!.,;:")
	if len([]rune(header)) > maxRunes {
		header = strings.TrimSpace(string([]rune(header)[:maxRunes]))
	}
	return header
}

// validateAskUserForm rejects a malformed form with a message the model can act
// on. Nothing is silently dropped or truncated.
func validateAskUserForm(form AskUserForm) error {
	if len(form.Questions) == 0 {
		return fmt.Errorf("at least one question is required")
	}
	if len(form.Questions) > MaxAskUserQuestions {
		return fmt.Errorf("a form takes at most %d questions, got %d; ask the rest in a follow-up call once these are answered",
			MaxAskUserQuestions, len(form.Questions))
	}
	headers := map[string]struct{}{}
	for i, q := range form.Questions {
		if strings.TrimSpace(q.Question) == "" {
			return fmt.Errorf("question %d: question text is required", i+1)
		}
		key := strings.ToLower(strings.TrimSpace(q.Header))
		if _, dup := headers[key]; dup {
			return fmt.Errorf("duplicate header %q: headers label the tabs and key the answers, so each must be distinct", q.Header)
		}
		headers[key] = struct{}{}
		if len(q.Options) > MaxAskUserOptions {
			return fmt.Errorf("question %q takes at most %d options, got %d", q.Header, MaxAskUserOptions, len(q.Options))
		}
		if len(q.Options) == 0 && !q.AllowCustom {
			return fmt.Errorf("question %q has no options and allow_custom is false: there is nothing for the user to answer", q.Header)
		}
		labels := map[string]struct{}{}
		recommended := 0
		for j, o := range q.Options {
			if strings.TrimSpace(o.Label) == "" {
				return fmt.Errorf("question %q option %d: label is required", q.Header, j+1)
			}
			lkey := strings.ToLower(strings.TrimSpace(o.Label))
			if _, dup := labels[lkey]; dup {
				return fmt.Errorf("question %q: duplicate option label %q; answers come back by label, so each must be distinct", q.Header, o.Label)
			}
			labels[lkey] = struct{}{}
			if o.IsRecommended {
				recommended++
			}
		}
		if recommended > 1 && !q.MultiSelect {
			return fmt.Errorf("question %q recommends %d options but takes a single answer; mark only the one you would pick", q.Header, recommended)
		}
	}
	return nil
}

// runAskUser is the tool entry point: decode the form, put it to the user, and
// render the answers back for the model.
func (r *toolRuntime) runAskUser(ctx context.Context, rawArgs []byte) (string, error) {
	form, err := decodeAskUserForm(rawArgs)
	if err != nil {
		return "", fmt.Errorf("ask-user: %w", err)
	}
	if r.askUser == nil {
		return "", fmt.Errorf("ask-user: interactive callback not configured")
	}
	answers, err := r.askUser(ctx, form)
	if err != nil {
		return "", fmt.Errorf("ask-user: %w", err)
	}
	out, err := formatAskUserAnswers(form, answers)
	if err != nil {
		return "", fmt.Errorf("ask-user: %w", err)
	}
	return out, nil
}
