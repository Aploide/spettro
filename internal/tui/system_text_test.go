package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Found in a VHS screenshot of /help at 80x24: a listing row too long for
// the pane wrapped to column 0 ("clipboard" under the key column), where it
// read as a new entry. Wrapped rows now hang under the description column.
func TestSystemListingWrapsUnderItsSecondColumn(t *testing.T) {
	const width = 60
	row := "  drag (mouse)   select text on screen; release copies it to the clipboard and more"
	got := wrapSystemText(row, width)
	if len(got) < 2 {
		t.Fatalf("expected the row to wrap at %d cells, got %q", width, got)
	}
	col := strings.Index(row, "select")
	for i, line := range got {
		if w := ansi.StringWidth(line); w > width {
			t.Fatalf("row %d is %d cells wide: %q", i, w, line)
		}
		if i > 0 && (len(line)-len(strings.TrimLeft(line, " ")) != col) {
			t.Fatalf("wrapped row %d does not start under the description (column %d): %q", i, col, line)
		}
	}

	// A row with no second column hangs at its own indent; short rows and
	// styled rows are left as they are.
	prose := "   " + strings.Repeat("word ", 30)
	for i, line := range wrapSystemText(prose, width)[1:] {
		if !strings.HasPrefix(line, "   ") || strings.HasPrefix(line, "    ") {
			t.Fatalf("prose row %d lost its indent: %q", i+1, line)
		}
	}
	if got := wrapSystemText("short\n  a  b", width); strings.Join(got, "\n") != "short\n  a  b" {
		t.Fatalf("short rows changed: %q", got)
	}
}

// The /help screen fits the transcript at every width and says what the
// transcript keys do (it once described ctrl+o as a side panel toggle).
func TestHelpScreenFitsAndNamesTranscriptKeys(t *testing.T) {
	for _, size := range fitSizes {
		m := footerModel(size[0], size[1])
		next, _ := m.handleCommand("/help")
		m = next.(Model)
		m.refreshViewport()
		for i, line := range strings.Split(m.renderMessages(), "\n") {
			if w := ansi.StringWidth(line); w > m.transcriptWidth() {
				t.Fatalf("at %v help row %d is %d cells wide: %q", size, i, w, ansi.Strip(line))
			}
		}
	}
	for _, want := range []string{"ctrl+g", "toggle tool details in the transcript", "pgup pgdn"} {
		if !strings.Contains(helpText, want) {
			t.Fatalf("/help does not mention %q", want)
		}
	}
}
