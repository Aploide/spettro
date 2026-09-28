package tui

import (
	"strings"
	"testing"
	"time"

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

// Found in a VHS screenshot at 40x15: the ask-user key legend was one row
// cut to "↑↓ or 1-9 pick  enter records  n …", so "esc declines" (the way
// out) and "tab/←→ switch" were never on screen. The legend now packs into
// as many rows as it needs, every key whole, and the frame still fits.
func TestAskUserLegendShowsEveryKeyWhenNarrow(t *testing.T) {
	form := agent.AskUserForm{Questions: []agent.AskUserQuestion{
		{Header: "Architecture", Question: "Which architecture should we use for the new storage layer? " + strings.Repeat("Consider latency and cost. ", 6),
			Options: []agent.AskUserOption{{Label: "Keep SQLite", Description: "WAL mode"}, {Label: "Postgres"}, {Label: "Append-only log"}}},
		{Header: "Scope", Question: "How far?", Options: []agent.AskUserOption{{Label: "Minimal"}, {Label: "Full"}}},
	}}
	for _, size := range [][2]int{{40, 15}, {50, 16}, {80, 24}} {
		for _, running := range []bool{false, true} {
			m := footerModel(size[0], size[1])
			m.SetPendingAskUserFormForTesting(form)
			if running {
				m.thinking = true
				m.agentStartAt = time.Now()
			}
			m = m.recalcLayout()
			frame := m.View().Content
			assertFrameFits(t, "ask-user legend", frame, size[0], size[1])
			plain := ansi.Strip(frame)
			for _, key := range []string{"enter records", "n notes", "tab/←→ switch", "esc declines"} {
				if !strings.Contains(plain, key) {
					t.Errorf("%v running=%v: %q is not on screen:\n%s", size, running, key, plain)
				}
			}
			if !strings.Contains(plain, "Keep SQLite") {
				t.Errorf("%v running=%v: no option left on screen:\n%s", size, running, plain)
			}
		}
	}
}
