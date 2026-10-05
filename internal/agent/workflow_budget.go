package agent

import (
	"regexp"
	"strconv"
	"strings"
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
