package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Found in a VHS run: "/skills" + Enter cleared the input and showed
// nothing, because its handler (like /jobs, /tasks, /hooks, /memory)
// returned without refreshing the viewport. handleCommand now refreshes
// whenever a command added to the transcript.
func TestCommandOutputIsOnScreenAtOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct{ command, want string }{
		{"/jobs", "no background jobs"},
		{"/skills", "skills"},
	} {
		m := footerModel(100, 30)
		m.refreshViewport()
		next, _ := m.handleCommand(tc.command)
		m = next.(Model)
		if !strings.Contains(ansi.Strip(m.vp.View()), tc.want) {
			t.Fatalf("%s: its output is not in the transcript view:\n%s", tc.command, ansi.Strip(m.vp.View()))
		}
	}
}
