package tui

// The ask-user question modal: state, key handling and the queue of forms
// waiting their turn. Rendering lives in dialog_question_view.go.
//
// The agent asks a *form* — up to four questions put to the user together —
// so the modal owns the screen while one is open rather than squeezing into
// the input box. Every question keeps its own cursor, selection and typed
// text, so moving between tabs never costs the user an answer.

import (
	"fmt"
	"os"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/agent"
)

// questionCustomRow is the client-supplied free-text entry appended to a
// question's options. It is not one of the agent's answers, but it is still an
// answer to the question, so it is the last numbered *option* — above the rule.
const questionCustomRow = "Type something."

// questionChatRow is the escape hatch below the rule: not an answer at all, but
// a way out of the form and back into the conversation.
const questionChatRow = "Chat about this"

// questionSubmitRow commits a multi-select question. Every other row on such a
// page is a toggle, so the act of finishing needs a row of its own: without it a
// user who meant to tick three boxes would answer with the first one.
const questionSubmitRow = "Submit"

// questionForm is the live state of one ask-user form. The per-question slices
// are parallel to form.Questions: switching tabs changes an index and nothing
// else, which is what makes a revisited question come back exactly as it was
// left.
type questionForm struct {
	form agent.AskUserForm
	// tab indexes form.Questions; len(form.Questions) is the Submit tab.
	tab      int
	cursor   []int          // per-question row cursor
	selected []map[int]bool // per-question chosen option indices
	custom   []string       // per-question free text
	notes    []string       // per-question annotation
	// committed marks a multi-select question the user finished through its
	// Submit row. It is what separates "I chose none of these" from "I never
	// looked at this question": both hold an empty selection.
	committed []bool
	// editing is set while the active question's free-text entry has the
	// keyboard; the textarea holds the text until it is committed.
	editing bool
	// review is the cursor on the Submit tab's review page: 0 sends the form,
	// 1 goes back. originTab is the question tab the user left to get there, so
	// backing out returns them where they were rather than to the first
	// question.
	review    int
	originTab int
	// notesEditing is set while the note field has it instead. The two are
	// separate because a note is not an answer: committing one must not answer
	// the question, and a form that submits on its only answer must not submit
	// on a note.
	notesEditing bool
	// previewKey/previewLines memoise the focused option's sanitised preview.
	// The pane is re-rendered on every keystroke, and the sanitising pass is
	// O(whole preview) rather than O(what fits) — an option carrying a long
	// sketch would otherwise be paid for on each cursor move.
	previewKey   string
	previewLines []string
	response     chan askUserResponse
}

// newQuestionForm arms the per-question state and focuses each question's
// recommended option, so the answer the agent would pick is one keypress away.
func newQuestionForm(msg askUserRequestMsg) *questionForm {
	q := &questionForm{
		form:      msg.form,
		cursor:    make([]int, len(msg.form.Questions)),
		selected:  make([]map[int]bool, len(msg.form.Questions)),
		custom:    make([]string, len(msg.form.Questions)),
		notes:     make([]string, len(msg.form.Questions)),
		committed: make([]bool, len(msg.form.Questions)),
		response:  msg.response,
	}
	for i, question := range msg.form.Questions {
		q.selected[i] = map[int]bool{}
		for j, opt := range question.Options {
			if opt.IsRecommended {
				q.cursor[i] = j
				break
			}
		}
	}
	return q
}

// Row kinds outside the agent's options. All are negative so `option` stays
// the index into AskUserQuestion.Options for everything the agent supplied.
const (
	questionRowOptionCustom = -1
	questionRowOptionChat   = -2
	questionRowOptionSubmit = -3
	questionRowOptionSend   = -4
	questionRowOptionCancel = -5
)

// The review page's two actions. Sending is the first row and the default
// cursor: the page is reached by walking to the end of the form, so the thing
// the user came for is the thing under the cursor when they arrive.
const (
	questionSendRow   = "Submit answers"
	questionCancelRow = "Cancel"
)

// questionReviewRows is the review page's answer list. It is built as ordinary
// rows so the page draws with the same cursor, numbering and hotkeys as every
// question — the last page of the form must not have keys of its own.
func questionReviewRows() []questionRow {
	return []questionRow{
		{option: questionRowOptionSend, label: questionSendRow, number: 1},
		{option: questionRowOptionCancel, label: questionCancelRow, number: 2},
	}
}

// questionRow is one row of the answer list: an agent option, the client's
// free-text entry, the multi-select Submit action, or the chat escape hatch.
type questionRow struct {
	// option indexes the question's Options, or is one of the questionRowOption*
	// constants for the client's own rows.
	option      int
	label       string
	description string
	// number is the digit hotkey the row answers to, or 0 for a row the list
	// leaves unnumbered.
	number      int
	recommended bool
	custom      bool
	chat        bool
	submit      bool
}

// questionRows is the answer list for one question: the agent's options in
// order, the free-text entry when the question allows one, the Submit action
// when the question is multi-select, and last — below the rule the renderer
// draws — the chat escape hatch, which is always offered.
func questionRows(q agent.AskUserQuestion) []questionRow {
	rows := make([]questionRow, 0, len(q.Options)+3)
	number := 0
	next := func() int { number++; return number }
	for i, opt := range q.Options {
		rows = append(rows, questionRow{
			option:      i,
			label:       opt.Label,
			description: opt.Description,
			number:      next(),
			recommended: opt.IsRecommended,
		})
	}
	if q.AllowCustom || len(q.Options) == 0 {
		rows = append(rows, questionRow{option: questionRowOptionCustom, label: questionCustomRow, number: next(), custom: true})
	}
	// Submit carries no number: the mockup indents it under the last option and
	// runs the numbering on to the chat exit below it. It is an action on the
	// answers, not one of them, so it is not something a digit picks either.
	if q.MultiSelect {
		rows = append(rows, questionRow{option: questionRowOptionSubmit, label: questionSubmitRow, submit: true})
	}
	return append(rows, questionRow{option: questionRowOptionChat, label: questionChatRow, number: next(), chat: true})
}

// questionRowByNumber resolves a digit hotkey to the row that shows it.
func questionRowByNumber(rows []questionRow, number int) int {
	for i, row := range rows {
		if row.number == number {
			return i
		}
	}
	return -1
}

// question returns the question the active tab points at, or false on the
// Submit tab.
func (q *questionForm) question() (agent.AskUserQuestion, bool) {
	if q.tab < 0 || q.tab >= len(q.form.Questions) {
		return agent.AskUserQuestion{}, false
	}
	return q.form.Questions[q.tab], true
}

// textEntry reports whether the keyboard belongs to a text field — the
// free-text answer or a note. Pasted text is forwarded only then; anywhere else
// on the form it would land in a list of options.
func (q *questionForm) textEntry() bool { return q.editing || q.notesEditing }

// onSubmitTab reports whether the trailing ✔ Submit chip is active.
func (q *questionForm) onSubmitTab() bool { return q.tab >= len(q.form.Questions) }

// tabCount is the number of chips in the strip: one per question plus Submit.
func (q *questionForm) tabCount() int { return len(q.form.Questions) + 1 }

// answered reports whether question i has something to send back. An untouched
// question is not "answered by default": the model is told it was skipped. A
// multi-select question the user submitted empty *is* answered — they said none
// of the options apply.
func (q *questionForm) answered(i int) bool {
	if i < 0 || i >= len(q.form.Questions) {
		return false
	}
	return q.committed[i] || len(q.selected[i]) > 0 || strings.TrimSpace(q.custom[i]) != ""
}

// complete reports whether every question has an answer, which is what the
// Submit tab warns about when it does not.
func (q *questionForm) complete() bool {
	for i := range q.form.Questions {
		if !q.answered(i) {
			return false
		}
	}
	return true
}

// firstUnanswered returns the index of the first question still waiting for an
// answer, or -1 when the form is complete.
func (q *questionForm) firstUnanswered() int {
	for i := range q.form.Questions {
		if !q.answered(i) {
			return i
		}
	}
	return -1
}

// choose records an answer: one option replaces the previous choice, but a
// multi-select question toggles the box instead, accumulating as many as the
// user ticks.
func (q *questionForm) choose(question, option int) {
	if q.form.Questions[question].MultiSelect {
		if q.selected[question][option] {
			delete(q.selected[question], option)
			return
		}
		q.selected[question][option] = true
		return
	}
	q.selected[question] = map[int]bool{option: true}
	q.custom[question] = ""
}

// answers renders the collected state into the shape the agent tool reads.
// Selections come out in option order, never in the order they were ticked, so
// the same set of boxes always produces the same answer.
func (q *questionForm) answers() []agent.AskUserAnswer {
	out := make([]agent.AskUserAnswer, 0, len(q.form.Questions))
	for i, question := range q.form.Questions {
		answer := agent.AskUserAnswer{
			Header: question.Header,
			Custom: strings.TrimSpace(q.custom[i]),
			Notes:  strings.TrimSpace(q.notes[i]),
		}
		for j, opt := range question.Options {
			if q.selected[i][j] {
				answer.Selected = append(answer.Selected, opt.Label)
			}
		}
		answer.Skipped = !q.committed[i] && len(answer.Selected) == 0 && answer.Custom == ""
		out = append(out, answer)
	}
	return out
}

// reply unblocks the tool call this form belongs to. Every exit from the modal
// goes through it, so a form can never be dismissed while its run waits.
func (q *questionForm) reply(resp askUserResponse) {
	answerAskUser(askUserRequestMsg{form: q.form, response: q.response}, resp)
}

// singlePage reports a form that is one question with one answer. It renders
// without the tab strip and answering it submits: there is no second question
// to move to, so making the user visit a Submit tab would be chrome for its
// own sake. A multi-select question is not a single page even when it is
// alone — picking an option cannot mean "and I am done".
func (q *questionForm) singlePage() bool {
	return len(q.form.Questions) == 1 && !q.form.Questions[0].MultiSelect
}

// --- key handling ---

func (m Model) updateQuestion(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	q := m.pendingQuestion
	if q == nil {
		return m, nil
	}
	if q.editing {
		return m.updateQuestionCustom(msg)
	}
	if q.notesEditing {
		return m.updateQuestionNote(msg)
	}
	switch msg.String() {
	case "left", "shift+tab":
		q.rememberQuestionTab()
		if q.tab > 0 {
			q.tab--
		}
		return m.focusQuestionTab(), nil
	case "right", "tab":
		q.rememberQuestionTab()
		if q.tab < q.tabCount()-1 {
			q.tab++
		}
		return m.focusQuestionTab(), nil
	case "ctrl+d":
		// Settled 2026-07-25: submit from anywhere on the form, under the same
		// rules the review page's Submit row applies — unanswered questions go
		// as skipped. It never fires from a text field: those are handled above,
		// where bubbles' textarea keeps ctrl+d as delete-forward.
		return m.submitQuestionForm(), nil
	case "esc":
		if q.onSubmitTab() {
			// The review page is a page of the form, not a way out of it.
			// Declining is esc from a question, where the user can see what they
			// would be refusing.
			return m.leaveQuestionReview(), nil
		}
		// Declining is all-or-nothing: a partial answer is a Submit-tab
		// action, never a side effect of backing out.
		return m.rejectAskUser("question declined"), nil
	}
	if q.onSubmitTab() {
		return m.updateQuestionReview(msg), nil
	}
	question, _ := q.question()
	rows := questionRows(question)
	if len(rows) == 0 {
		return m, nil
	}
	if q.cursor[q.tab] >= len(rows) {
		q.cursor[q.tab] = len(rows) - 1
	}
	switch key := msg.String(); key {
	case "up", "ctrl+p":
		if q.cursor[q.tab] > 0 {
			q.cursor[q.tab]--
		}
	case "down", "ctrl+n":
		if q.cursor[q.tab] < len(rows)-1 {
			q.cursor[q.tab]++
		}
	case "enter":
		return m.activateQuestionRow(rows, q.cursor[q.tab]), nil
	case "space":
		// Checkbox rows toggle with space, as they do everywhere else in the
		// TUI. Only on a multi-select page: on a single-select one the same
		// keypress would answer the question outright, and a stray space is not
		// a decision.
		if question.MultiSelect {
			return m.toggleQuestionRow(rows, q.cursor[q.tab]), nil
		}
	case "n":
		// A note annotates the *question*, not the row the cursor happens to be
		// on, so it is reachable from anywhere on the page. It is not a digit
		// hotkey and not a selection: opening the field records nothing.
		return m.openQuestionNote(), nil
	default:
		// The mockups number every row, so the digits have to work: 1-9 jumps
		// to that row and picks it in one keypress. A tenth row (eight options,
		// the free-text entry and the chat exit) is reachable by cursor only —
		// "0" for a tenth item reads as a typo, not a shortcut.
		if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
			if idx := questionRowByNumber(rows, int(key[0]-'0')); idx >= 0 {
				q.cursor[q.tab] = idx
				return m.activateQuestionRow(rows, idx), nil
			}
		}
	}
	return m, nil
}

// activateQuestionRow is what enter and the digit hotkeys both do to the row
// under them: open the free-text entry, leave for the chat, or record the
// answer.
func (m Model) activateQuestionRow(rows []questionRow, idx int) Model {
	q := m.pendingQuestion
	if idx < 0 || idx >= len(rows) {
		return m
	}
	row := rows[idx]
	switch {
	case row.chat:
		return m.chatAboutQuestion()
	case row.submit:
		return m.commitQuestion()
	case row.custom:
		q.editing = true
		m.ta.Reset()
		m.ta.SetValue(q.custom[q.tab])
		m.showBanner("type your answer and press enter", "info")
		return m
	}
	q.choose(q.tab, row.option)
	if q.singlePage() {
		return m.submitQuestionForm()
	}
	if question, ok := q.question(); ok && question.MultiSelect {
		// A ticked box is not an answer yet, so say what finishes the question:
		// otherwise the Submit row under the list reads as decoration.
		m.showBanner("tick as many as apply, then "+questionSubmitRow, "info")
		return m
	}
	// Settled: no auto-advance. The selection is recorded, the tab's glyph
	// flips, and the cursor stays put so revising an answer never means
	// arrowing back to it.
	m.showBanner(questionRecordedBanner(), "info")
	return m
}

// toggleQuestionRow is what `space` does on a multi-select page: the same thing
// enter does, except on the two rows where "toggle" would mean something else.
// The chat exit is a way out of the form rather than a checkbox, and must not be
// taken by a stray space; a free-text row that already holds an answer unticks
// it instead of reopening the field, which is the only way to take that answer
// back out of the selection set.
func (m Model) toggleQuestionRow(rows []questionRow, idx int) Model {
	q := m.pendingQuestion
	if idx < 0 || idx >= len(rows) {
		return m
	}
	switch row := rows[idx]; {
	case row.chat:
		return m
	case row.custom && strings.TrimSpace(q.custom[q.tab]) != "":
		q.custom[q.tab] = ""
		m.showBanner("your own answer was removed from the selection", "info")
		return m
	}
	return m.activateQuestionRow(rows, idx)
}

// commitQuestion records a multi-select question's selection set as its answer.
// Committing an empty set is allowed and is *not* a skip: "none of these" is a
// decision, and the model is told which of the two it was given. It never moves
// the active tab — the form is still sent from the Submit chip in the strip.
func (m Model) commitQuestion() Model {
	q := m.pendingQuestion
	q.committed[q.tab] = true
	if len(q.selected[q.tab]) == 0 && strings.TrimSpace(q.custom[q.tab]) == "" {
		m.showBanner("recorded: none of these — "+glyphs().submit+" Submit to send", "info")
		return m
	}
	m.showBanner(questionRecordedBanner(), "info")
	return m
}

// --- the review page ---

// rememberQuestionTab records the question the user is leaving, so the review
// page knows where "back" is. Cancelling to the first question would lose the
// place of anyone reviewing a four-question form from its end.
func (q *questionForm) rememberQuestionTab() {
	if !q.onSubmitTab() {
		q.originTab = q.tab
	}
}

// updateQuestionReview is the review page's key handling: the same cursor,
// enter and digit hotkeys the question pages use, over two rows.
func (m Model) updateQuestionReview(msg tea.KeyPressMsg) Model {
	q := m.pendingQuestion
	rows := questionReviewRows()
	switch key := msg.String(); key {
	case "up", "ctrl+p":
		q.review = max(q.review-1, 0)
	case "down", "ctrl+n":
		q.review = min(q.review+1, len(rows)-1)
	case "enter":
		return m.activateQuestionReviewRow(q.review)
	default:
		if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
			if idx := questionRowByNumber(rows, int(key[0]-'0')); idx >= 0 {
				q.review = idx
				return m.activateQuestionReviewRow(idx)
			}
		}
	}
	return m
}

// activateQuestionReviewRow sends the form or goes back to it. Nothing else can
// happen from this page: it is the end of the form, not another question.
func (m Model) activateQuestionReviewRow(idx int) Model {
	if idx == 0 {
		return m.submitQuestionForm()
	}
	return m.leaveQuestionReview()
}

// leaveQuestionReview returns to the question the user came from with every
// answer intact. Cancel is "not yet", not "never" — declining the form is esc
// from a question page, where what is being refused is on screen.
func (m Model) leaveQuestionReview() Model {
	q := m.pendingQuestion
	q.tab = min(max(q.originTab, 0), max(len(q.form.Questions)-1, 0))
	q.review = 0
	m.showBanner("back to your answers", "info")
	return m.focusQuestionTab()
}

// chatAboutQuestion takes the escape hatch below the rule. Settled 2026-07-25:
// this ends the run's turn rather than steering it — the tool reports that the
// user wants to talk, the agent finishes on that, and whatever the user types
// next is an ordinary new turn. Distinct from a decline, which tells the model
// the user refused the question.
func (m Model) chatAboutQuestion() Model {
	q := m.pendingQuestion
	if q == nil {
		return m
	}
	q.reply(askUserResponse{err: agent.ErrAskUserReplyInChat})
	m.showBanner("say what you think below — the agent is waiting for your message", "info")
	m = m.advanceQuestionQueue()
	m.refreshViewport()
	return m
}

// focusQuestionTab opens the free-text entry when the tab the user just landed
// on has nothing to pick from: with no option list to arrow through, the
// keyboard belongs in the textarea rather than behind one more enter. A tab
// whose answer was already typed shows it instead, so navigating past a
// finished question does not reopen it.
func (m Model) focusQuestionTab() Model {
	q := m.pendingQuestion
	question, ok := q.question()
	if !ok || len(question.Options) > 0 || strings.TrimSpace(q.custom[q.tab]) != "" {
		return m
	}
	q.editing = true
	m.ta.Reset()
	return m
}

// updateQuestionCustom handles the free-text entry: the textarea holds the
// draft until enter commits it to the active question.
func (m Model) updateQuestionCustom(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	q := m.pendingQuestion
	switch msg.String() {
	case "enter":
		text := strings.TrimSpace(m.ta.Value())
		if text == "" {
			m.showBanner("type your answer, then press enter", "warn")
			return m, nil
		}
		q.custom[q.tab] = text
		if question, ok := q.question(); ok && !question.MultiSelect {
			q.selected[q.tab] = map[int]bool{}
		}
		q.editing = false
		m.ta.Reset()
		if q.singlePage() {
			return m.submitQuestionForm(), nil
		}
		m.showBanner(questionRecordedBanner(), "info")
		return m, nil
	case "esc":
		// Back to the list rather than straight out. Even a question with no
		// options has rows worth reaching — the chat escape hatch is one of
		// them — and a second esc still declines from there.
		q.editing = false
		m.ta.Reset()
		m.showBanner("choose a row or press esc again to decline", "info")
		return m, nil
	default:
		var taCmd tea.Cmd
		m.ta, taCmd = m.ta.Update(msg)
		return m, taCmd
	}
}

// openQuestionNote hands the keyboard to the note field of the active
// question, seeded with whatever note it already carries so `n` edits rather
// than restarts.
func (m Model) openQuestionNote() Model {
	q := m.pendingQuestion
	if _, ok := q.question(); !ok {
		return m
	}
	q.notesEditing = true
	m.ta.Reset()
	m.ta.SetValue(q.notes[q.tab])
	m.showBanner("type your note", "info")
	return m
}

// updateQuestionNote handles the note field. Both keys that close it keep the
// text — a note is a scratch annotation, and there is nothing to confirm — so
// enter and esc differ only in what they say afterwards. Neither answers the
// question: a form the user only annotated is still unanswered, and a
// single-page form does not submit on it.
func (m Model) updateQuestionNote(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	q := m.pendingQuestion
	switch msg.String() {
	case "enter", "esc":
		note := strings.TrimSpace(m.ta.Value())
		q.notes[q.tab] = note
		q.notesEditing = false
		m.ta.Reset()
		if note == "" {
			m.showBanner("note cleared", "info")
			return m, nil
		}
		m.showBanner("note attached", "info")
		return m, nil
	default:
		var taCmd tea.Cmd
		m.ta, taCmd = m.ta.Update(msg)
		return m, taCmd
	}
}

// questionRecordedBanner says what a recorded answer does next, in the glyph
// set the terminal can actually draw.
func questionRecordedBanner() string {
	return "answer recorded — tab to the next question, or " + glyphs().submit + " Submit"
}

// --- delivery ---

// answerAskUser delivers a reply to the tool call blocked on this form.
// The response channel is buffered and read exactly once, so a non-blocking
// send is enough — and a second send (a race between, say, a Telegram answer
// and a keypress) is dropped rather than deadlocking the UI.
func answerAskUser(msg askUserRequestMsg, resp askUserResponse) {
	select {
	case msg.response <- resp:
	default:
	}
}

// presentQuestion makes a form the active one. Everything that targets "the
// form on screen" — the modal, the desktop notification, the remote/Telegram
// answer expectation — is armed here, so a queued form stays invisible to
// those surfaces until its turn.
func (m Model) presentQuestion(msg askUserRequestMsg) Model {
	m.pendingQuestion = newQuestionForm(msg)
	m.ta.Reset()
	m = m.focusQuestionTab()
	banner := "agent is waiting for your answer"
	if n := len(m.questionQueue); n > 0 {
		banner = fmt.Sprintf("%s (%d more after this)", banner, n)
	}
	m.showBanner(banner, "info")
	m.notifyIfUnfocused("Agent is waiting for your answer")
	m.publishQuestionRemote()
	return m
}

// publishQuestionRemote pushes the question the user is being asked to the
// remote/Telegram surfaces, which can only carry one at a time. Task 08 gives
// them the whole form; until then they see the active question, and answering
// it moves them to the next one that still needs an answer.
func (m Model) publishQuestionRemote() {
	q := m.pendingQuestion
	if q == nil {
		return
	}
	question, ok := q.question()
	if !ok {
		return
	}
	options := make([]string, 0, len(question.Options))
	for _, opt := range question.Options {
		options = append(options, opt.Label)
	}
	def := ""
	for _, opt := range question.Options {
		if opt.IsRecommended {
			def = opt.Label
			break
		}
	}
	m.publishRemote("ask_user", map[string]any{
		"question":            question.Question,
		"options":             options,
		"context":             q.form.Context,
		"default":             def,
		"allow_free_response": question.AllowCustom || len(question.Options) == 0,
	})
}

// advanceQuestionQueue clears the form just answered and promotes the next one
// the agent asked while the user was busy.
func (m Model) advanceQuestionQueue() Model {
	m.pendingQuestion = nil
	m.ta.Reset()
	m.telegramClearAnswerExpectations()
	if len(m.questionQueue) == 0 {
		return m
	}
	next := m.questionQueue[0]
	m.questionQueue = m.questionQueue[1:]
	return m.presentQuestion(next)
}

// discardQuestionQueue answers every waiting form with err, so no tool call is
// left blocked when the run they belong to goes away.
func (m *Model) discardQuestionQueue(err error) {
	for _, queued := range m.questionQueue {
		answerAskUser(queued, askUserResponse{err: err})
	}
	m.questionQueue = nil
}

// submitQuestionForm sends the answers collected so far. Questions left
// untouched come back marked skipped rather than defaulted (task 07 adds the
// review page's warning before this can happen by accident).
func (m Model) submitQuestionForm() Model {
	q := m.pendingQuestion
	if q == nil {
		return m
	}
	q.reply(askUserResponse{answers: q.answers()})
	banner := "answer sent"
	if !q.complete() {
		banner = "answers sent — unanswered questions were marked skipped"
	}
	m.showBanner(banner, "info")
	m = m.advanceQuestionQueue()
	m.refreshViewport()
	return m
}

// rejectAskUser declines the whole form: nothing is partially delivered.
func (m Model) rejectAskUser(banner string) Model {
	if q := m.pendingQuestion; q != nil {
		q.reply(askUserResponse{err: fmt.Errorf("user declined to answer")})
	}
	m.showBanner(banner, "warn")
	m = m.advanceQuestionQueue()
	m.refreshViewport()
	return m
}

// answerQuestionRemotely records free text sent from a surface that cannot
// navigate the form (Telegram today) against the question it was shown. Unlike
// the TUI it *does* advance: the remote user has no other way to reach the
// next question, and stopping on an answered one would strand the form.
func (m Model) answerQuestionRemotely(text, banner string) Model {
	q := m.pendingQuestion
	if q == nil {
		return m
	}
	if q.onSubmitTab() {
		q.tab = max(len(q.form.Questions)-1, 0)
	}
	question, ok := q.question()
	if !ok {
		return m
	}
	labels := make([]string, 0, len(question.Options))
	for _, opt := range question.Options {
		labels = append(labels, opt.Label)
	}
	if idx := indexOfLabel(labels, text); idx >= 0 {
		q.choose(q.tab, idx)
	} else {
		q.custom[q.tab] = strings.TrimSpace(text)
		q.selected[q.tab] = map[int]bool{}
	}
	if next := q.firstUnanswered(); next >= 0 {
		q.tab = next
		m.showBanner(banner+" — next question sent", "info")
		m.publishQuestionRemote()
		return m
	}
	m.showBanner(banner, "info")
	return m.submitQuestionForm()
}

// indexOfLabel resolves a reply to one of the agent's options, so a remote user
// naming an option answers with it instead of as free text.
func indexOfLabel(labels []string, text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return -1
	}
	for i, label := range labels {
		if strings.EqualFold(text, strings.TrimSpace(label)) {
			return i
		}
	}
	return -1
}

// --- glyphs ---

// questionGlyphSet is the symbol vocabulary of the question modal, picked once
// so the tab strip, the option rows, the preview pane and the review page can
// never disagree about what a checkbox looks like.
type questionGlyphSet struct {
	unchecked   string
	checked     string
	submit      string
	cursor      string
	recommended string
	warn        string
	// rule is one cell of the horizontal separator above the chat escape hatch.
	rule string
	// bullet leads a question on the review page: it names the question rather
	// than offering it, so it is not a checkbox and not a cursor.
	bullet string
	// clip marks a preview line cut off at the pane's right edge. Preview text
	// is preformatted, so it is clipped rather than wrapped and the cut has to
	// be visible — otherwise a truncated sketch reads as the whole one.
	clip string
}

var (
	questionGlyphsUnicode = questionGlyphSet{
		// ○/● rather than ☐/☒: the boxed glyphs draw as hairlines in most
		// terminal fonts, and the filled circle is what the rest of the TUI
		// already uses for a checked row (see the storage dialog).
		unchecked: "○", checked: "●", submit: "✓", cursor: "❯", recommended: "●", warn: "⚠", rule: "─", clip: "›", bullet: "●",
	}
	questionGlyphsASCII = questionGlyphSet{
		unchecked: "[ ]", checked: "[x]", submit: ">", cursor: ">", recommended: "*", warn: "!", rule: "-", clip: ">", bullet: "*",
	}
)

// questionGlyphOverride forces a set in tests; nil means detect.
var questionGlyphOverride *questionGlyphSet

// glyphs returns the symbol set this terminal can render. The codebase has no
// capability probe, and the terminfo databases do not describe glyph coverage,
// so the locale is the signal available: a non-UTF-8 locale cannot encode the
// box-drawing symbols at all.
var glyphs = func() func() questionGlyphSet {
	detect := sync.OnceValue(func() questionGlyphSet {
		if localeIsUTF8() {
			return questionGlyphsUnicode
		}
		return questionGlyphsASCII
	})
	return func() questionGlyphSet {
		if questionGlyphOverride != nil {
			return *questionGlyphOverride
		}
		return detect()
	}
}()

func localeIsUTF8() bool {
	for _, key := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		v := strings.ToLower(os.Getenv(key))
		if v == "" {
			continue
		}
		return strings.Contains(v, "utf-8") || strings.Contains(v, "utf8")
	}
	// No locale set at all: assume the modern default rather than degrading
	// every terminal that simply does not export one.
	return true
}
