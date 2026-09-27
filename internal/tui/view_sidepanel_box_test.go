package tui

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"spettro/internal/termtext"
	"spettro/internal/theme"
)

// lipglossSidePanelBox is sidePanelBox as it was written with a lipgloss
// border style, kept as the oracle for the hand-built frame.
func lipglossSidePanelBox(body string, width, innerHeight int) string {
	contentW := sidePanelContentWidth(width)
	lines := strings.Split(clampLines(body, innerHeight), "\n")
	for i, line := range lines {
		lines[i] = termtext.Fit(strings.TrimRight(line, " "), contentW)
	}
	return lipgloss.NewStyle().
		Width(width).
		Height(innerHeight+2).
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(theme.Current().Border).
		Padding(0, 1).
		Render(strings.Join(lines, "\n"))
}

// The hand-built frame draws what the lipgloss border style drew, byte for
// byte, for short and long bodies, wide and styled lines, and every size.
func TestSidePanelBoxMatchesLipgloss(t *testing.T) {
	styled := lipgloss.NewStyle().Foreground(theme.Current().Error).Bold(true)
	bodies := []string{
		"",
		"Activity",
		"Activity\nOperational tool activity\n\n  coding\n    └ bash go test ./...",
		strings.Repeat("a very long line that cannot fit inside the panel at all ", 3),
		styled.Render("styled row") + "   \n" + styled.Render(strings.Repeat("wide ", 30)),
		"漢字テスト 全角文字は二列を占める 漢字テスト 全角文字は二列を占める",
		strings.Repeat("row\n", 60),
		"trailing spaces are not content      \n\n\nx",
	}
	for _, body := range bodies {
		for _, width := range []int{12, 24, 40, 57} {
			for _, inner := range []int{1, 2, 5, 20} {
				want := lipglossSidePanelBox(body, width, inner)
				got := sidePanelBox(body, width, inner)
				if got != want {
					t.Fatalf("width %d, inner %d, body %q:\ngot:\n%s\nwant:\n%s\ngot  %q\nwant %q",
						width, inner, body, got, want, got, want)
				}
			}
		}
	}
}

// The whole panel, joined by hand, still matches lipgloss's joins.
func TestSidePanelViewMatchesLipglossJoins(t *testing.T) {
	for _, n := range []int{0, 3, 40} {
		m := footerModel(140, 40)
		m.showSidePanel = true
		for i := 0; i < n; i++ {
			m.upsertActivity(activityItem{Key: fmt.Sprint(i), Kind: "tool", ID: "bash", AgentID: fmt.Sprint("agent", i/7),
				Title: "bash go test", Detail: "ok", Body: "line1\nline2", Status: "done"})
		}
		width := m.sidePanelWidth()
		got := m.viewSidePanel(width)
		rows := strings.Split(got, "\n")
		for i, r := range rows {
			if w := lipgloss.Width(r); w != width {
				t.Fatalf("%d items: row %d is %d cells wide, want %d: %q", n, i, w, width, r)
			}
		}
		if want := m.sidePanelInnerHeight() - sidePanelHintRows + 2 + sidePanelHintRows; len(rows) != want {
			t.Fatalf("%d items: panel is %d rows tall, want %d", n, len(rows), want)
		}
	}
}
