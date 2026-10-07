package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// inputBoxRows returns the input box's rows of a rendered frame: from its top
// border to its bottom border.
func inputBoxRows(t *testing.T, frame string) []string {
	t.Helper()
	lines := strings.Split(ansi.Strip(frame), "\n")
	top, bottom := -1, -1
	for i, l := range lines {
		s := strings.TrimSpace(l)
		if strings.HasPrefix(s, "┌") {
			top = i
		}
		if strings.HasPrefix(s, "└") && top >= 0 {
			bottom = i
		}
	}
	if top < 0 || bottom < top {
		t.Fatalf("no square input box in the frame:\n%s", ansi.Strip(frame))
	}
	return lines[top : bottom+1]
}

// TestInputBoxStartsOneLineWithoutAgentLabel: the empty box is one text row
// in a square border, with no agent label inside it — the header already
// names the agent.
func TestInputBoxStartsOneLineWithoutAgentLabel(t *testing.T) {
	m := footerModel(100, 30)
	m.ta.Placeholder = "enter message…" // as New sets it
	m = m.recalcLayout()
	box := inputBoxRows(t, m.View().Content)
	if len(box) != 3 {
		t.Fatalf("empty input box is %d rows, want 3 (border, one line, border):\n%s", len(box), strings.Join(box, "\n"))
	}
	if strings.Contains(box[1], "coding") || strings.Contains(box[1], "◆") || strings.Contains(box[1], "┃") {
		t.Fatalf("the box still carries the agent label or gutter: %q", box[1])
	}
	if !strings.Contains(box[1], "enter message") {
		t.Fatalf("the placeholder should be on the box's only line: %q", box[1])
	}
}

// TestInputBoxGrowsWithTheDraft: the box grows a row per line of draft,
// soft wraps included, and the transcript gives up exactly those rows.
func TestInputBoxGrowsWithTheDraft(t *testing.T) {
	m := footerModel(60, 40).recalcLayout()
	emptyVP := m.vp.Height()

	m.ta.SetValue("first line\nsecond line")
	m = m.recalcLayout()
	if got := len(inputBoxRows(t, m.View().Content)); got != 4 {
		t.Fatalf("two-line draft: box is %d rows, want 4", got)
	}
	if m.vp.Height() != emptyVP-1 {
		t.Fatalf("transcript = %d rows, want %d: the box's growth must come out of it", m.vp.Height(), emptyVP-1)
	}

	// One logical line long enough to wrap three times on a 60-wide pane.
	m.ta.SetValue(strings.Repeat("word ", 30))
	m = m.recalcLayout()
	if got := len(inputBoxRows(t, m.View().Content)); got < 5 {
		t.Fatalf("a wrapped line must grow the box too: %d rows", got)
	}

	m.ta.SetValue("")
	m = m.recalcLayout()
	if got := len(inputBoxRows(t, m.View().Content)); got != 3 || m.vp.Height() != emptyVP {
		t.Fatalf("cleared draft: box %d rows, transcript %d; want 3 and %d", got, m.vp.Height(), emptyVP)
	}
}

// TestInputBoxNeverOverflows: a draft far taller than the terminal stops
// growing the box at half the terminal (less on a short one), scrolls inside
// it, and the whole frame still fits — down to tiny terminals.
func TestInputBoxNeverOverflows(t *testing.T) {
	draft := strings.Repeat("a line of a very long pasted draft\n", 200)
	for _, size := range [][2]int{{40, 10}, {40, 15}, {80, 24}, {120, 40}, {200, 60}} {
		m := footerModel(size[0], size[1])
		m.ta.SetValue(draft)
		m = m.recalcLayout()
		frame := m.View().Content
		assertFrameFits(t, "long draft", frame, size[0], size[1])
		box := inputBoxRows(t, frame)
		if text := len(box) - 2; text > size[1]/2 {
			t.Fatalf("%v: box has %d text rows, more than half the terminal", size, text)
		}
		if m.vp.Height() < 1 {
			t.Fatalf("%v: the transcript lost its last row", size)
		}
	}
}

func TestInputMaxRows(t *testing.T) {
	cases := []struct{ height, chrome, want int }{
		{40, 4, 20}, // half the terminal
		{10, 4, 3},  // a short terminal: what is left after chrome, border, one transcript row
		{6, 4, 1},   // never below one row
		{100, 4, 50},
	}
	for _, c := range cases {
		if got := inputMaxRows(c.height, c.chrome); got != c.want {
			t.Errorf("inputMaxRows(%d, %d) = %d, want %d", c.height, c.chrome, got, c.want)
		}
	}
}

// TestHeaderAlwaysNamesTheAgent: with the label gone from the input box, the
// header is the only place the active agent is named, so a narrow terminal
// drops the right-hand tags before it drops the agent.
func TestHeaderAlwaysNamesTheAgent(t *testing.T) {
	for _, w := range []int{40, 50, 60, 80, 120} {
		m := footerModel(w, 24)
		m.mode = "coding"
		m.cfg.ThinkingLevel = "high"
		header := ansi.Strip(m.viewHeader())
		if !strings.Contains(header, "coding") {
			t.Errorf("width %d: header lost the agent name: %q", w, header)
		}
		if got := ansi.StringWidth(header); got > w {
			t.Errorf("width %d: header is %d cells wide", w, got)
		}
	}
}
