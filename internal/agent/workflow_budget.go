package agent

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// budgetDirectiveRe matches a token-budget directive written into a message:
// "+500k", "+1.5m", "+2M". It has to stand on its own — preceded by the start
// of the message or whitespace and ended by a word boundary — so "a+500k
// salary" or "C++" never reads as one, and the k/m suffix is mandatory so a
// bare "+5" in arithmetic stays arithmetic.
var budgetDirectiveRe = regexp.MustCompile(`(?:^|\s)(\+(\d+(?:\.\d+)?)\s?([kKmM]))\b`)

// maxBudgetDirective caps a directive at a billion tokens: anything larger is
// a typo, and an overflowing int would turn the ceiling into no ceiling.
const maxBudgetDirective = 1_000_000_000

// ParseBudgetDirective returns the token budget a "+500k"-style directive in
// task asks for. When several appear, the last one wins: it is the user's
// most recent word on the matter. ok is false when there is none.
func ParseBudgetDirective(task string) (tokens int, ok bool) {
	matches := budgetDirectiveRe.FindAllStringSubmatch(task, -1)
	if len(matches) == 0 {
		return 0, false
	}
	m := matches[len(matches)-1]
	n, err := strconv.ParseFloat(m[2], 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	switch strings.ToLower(m[3]) {
	case "k":
		n *= 1_000
	case "m":
		n *= 1_000_000
	}
	if n < 1 {
		return 0, false
	}
	if n > maxBudgetDirective {
		n = maxBudgetDirective
	}
	return int(n), true
}

// BudgetDirectiveSpans returns the [start, end) rune ranges of every budget
// directive in task, so the TUI can light them up with the same match that
// decides the budget — the same contract WorkflowActivationSpans keeps for
// the keyword.
func BudgetDirectiveSpans(task string) [][2]int {
	idx := budgetDirectiveRe.FindAllStringSubmatchIndex(task, -1)
	if len(idx) == 0 {
		return nil
	}
	spans := make([][2]int, 0, len(idx))
	for _, m := range idx {
		start, end := m[2], m[3] // the directive itself, without the leading space
		spans = append(spans, [2]int{
			utf8.RuneCountInString(task[:start]),
			utf8.RuneCountInString(task[:end]),
		})
	}
	return spans
}

// workflowPool is a turn's "+500k"-style budget directive: one pool of tokens
// shared by the workflow runs the turn starts, not a fresh grant per run.
//
// It is enforced where tokens are spent, not only where a run starts. A run's
// engine budget is a fixed figure taken when it starts, and a pooled run can
// keep spending long after that: it may pause at a checkpoint while the model
// starts another run, and then be continued — so a run that started with the
// whole pool, spent a fifth of it and paused, could otherwise spend the rest a
// second time after a later run used it. Every pooled run's workflowRunner
// therefore asks the pool before each agent it starts, and the pool counts
// what all its runs have spent so far, however many times each was continued.
//
// A run continued in a later turn stays bound to the pool of the turn that
// started it: its budget was granted against that directive, and a later
// directive (or none) is about that turn's own new runs.
type workflowPool struct {
	limit int

	mu   sync.Mutex
	runs []*liveWorkflow
}

func newWorkflowPool(limit int) *workflowPool {
	if limit <= 0 {
		return nil
	}
	return &workflowPool{limit: limit}
}

// add counts run's spend against the pool. Every run the turn starts is
// added, a run with an explicit budget_tokens too: that run is not limited by
// the pool, but what it spends is still spent from the turn's budget.
func (p *workflowPool) add(run *liveWorkflow) {
	if p == nil || run == nil {
		return
	}
	p.mu.Lock()
	p.runs = append(p.runs, run)
	p.mu.Unlock()
}

// spent is what the pool's runs have spent so far.
func (p *workflowPool) spent() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	runs := append([]*liveWorkflow(nil), p.runs...)
	p.mu.Unlock()
	total := 0
	for _, run := range runs {
		total += run.handle.Snapshot().Tokens
	}
	return total
}

// left is what remains of the pool; zero or less once it is spent.
func (p *workflowPool) left() int {
	if p == nil {
		return 0
	}
	return p.limit - p.spent()
}

// errSpent is what an agent a pooled run tries to start gets once the pool
// is spent. The engine resolves the agent() to null and counts it failed, so
// the script carries on with what it has rather than dying mid-wave.
func (p *workflowPool) errSpent() error {
	return fmt.Errorf("workflow: the turn's token budget of %d is spent (%d used); no further agent starts in this run", p.limit, p.spent())
}
