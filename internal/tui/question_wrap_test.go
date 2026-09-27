package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"spettro/internal/agent"
)

// Found in a VHS screenshot: a long ask-user question wrapped with its first
// row indented two cells and every further row back at column 0 of the box.
// Every row of the question now keeps the indent.
func TestAskUserQuestionWrapsWithItsIndent(t *testing.T) {
	const marker = "QSTART"
	question := marker + " " + strings.Repeat("which storage layer should we use for this project ", 6) + "QEND"
	for _, size := range [][2]int{{80, 40}, {120, 40}, {200, 50}} {
		m := footerModel(size[0], size[1])
		m.SetPendingAskUserFormForTesting(agent.AskUserForm{Questions: []agent.AskUserQuestion{{
			Header:   "Storage",
			Question: question,
			Options:  []agent.AskUserOption{{Label: "SQLite"}, {Label: "Postgres"}},
		}}})
		m = m.recalcLayout()
		frame := m.View().Content
		assertFrameFits(t, "ask-user", frame, size[0], size[1])
		rows := strings.Split(ansi.Strip(frame), "\n")
		first, last := -1, -1
		for i, row := range rows {
			if strings.Contains(row, marker) {
				first = i
			}
			if strings.Contains(row, "QEND") {
				last = i
			}
		}
		if first < 0 || last <= first {
			t.Fatalf("at %v the question is not on screen as several rows:\n%s", size, strings.Join(rows, "\n"))
		}
		col := strings.Index(rows[first], marker)
		for _, row := range rows[first+1 : last+1] {
			if lead := len(row) - len(strings.TrimLeft(row, "│ ")); lead != col {
				t.Fatalf("at %v a wrapped question row starts at column %d, the first at %d:\n%s",
					size, lead, col, strings.Join(rows[first:last+1], "\n"))
			}
		}
	}
}
