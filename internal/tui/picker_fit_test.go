package tui

// Picker-fit tests: found by looking at VHS screenshots at 40x15, where the
// model selector, the resume list and the onboarding picker came out taller
// than the terminal (a fixed "height-12, at least 4" list, a key hint that
// wrapped, model ids that wrapped), so the frame was cropped and the keys
// were gone. Each must now fit whole at every size: both borders, the
// selected row and every key hint on screen.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"spettro/internal/provider"
	"spettro/internal/session"
)

// pickerModels is a local endpoint serving n models, every fifth with an id
// far wider than any dialog.
func pickerModels(n int) []provider.Model {
	var out []provider.Model
	for i := range n {
		name := fmt.Sprintf("model-%02d", i)
		if i%5 == 0 {
			name += "-" + strings.Repeat("very-long-name-", 8)
		}
		out = append(out, provider.Model{Provider: "http://127.0.0.1:9", ProviderName: "Local endpoint",
			Name: name, Context: 128000, Local: true})
	}
	return out
}

// assertWholeDialog fails unless the frame fits and shows the dialog's
// borders and every one of want.
func assertWholeDialog(t *testing.T, name, frame string, width, height int, want ...string) {
	t.Helper()
	assertFrameFits(t, name, frame, width, height)
	plain := ansi.Strip(frame)
	for _, w := range want {
		if !strings.Contains(plain, w) {
			t.Fatalf("%s at %dx%d: %q is not on screen:\n%s", name, width, height, w, plain)
		}
	}
}

func TestModelSelectorFitsWhole(t *testing.T) {
	for _, size := range fitSizes {
		m := footerModel(size[0], size[1])
		m.providers.AddLocalModels(pickerModels(30))
		m = m.openSelector("")
		m.selCursor = 20
		frame := m.View().Content
		assertWholeDialog(t, "model selector", frame, size[0], size[1],
			"╭", "╰", "› model-20", "enter select", "esc close", "↑")
	}
}

func TestResumeDialogFitsWhole(t *testing.T) {
	var sessions []session.Summary
	for i := range 30 {
		sessions = append(sessions, session.Summary{ID: fmt.Sprint(i), StartedAt: time.Now(), UpdatedAt: time.Now(),
			Preview: fmt.Sprintf("prompt %02d %s", i, strings.Repeat("宽 text ", 20))})
	}
	for _, size := range fitSizes {
		m := footerModel(size[0], size[1])
		m.SetResumeItemsForTesting(sessions)
		m.showResume = true
		m.resumeCursor = 25
		m.ensureResumeWindow()
		frame := m.View().Content
		assertWholeDialog(t, "resume", frame, size[0], size[1], "╭", "╰", "› ", "prompt 25", "esc close")
		// Paging moves by exactly the rows on screen.
		shown := strings.Count(ansi.Strip(frame), " prompt ")
		if got := m.resumeMaxRows(); got != shown {
			t.Fatalf("resume at %v: page size %d, but %d sessions are on screen", size, got, shown)
		}
	}
}

// The connect and theme dialogs cut their key hint to one row, so at 40x15
// it ended "esc…" / "esc ca…"; the keys now wrap onto a second row.
func TestConnectAndThemeDialogsShowEveryKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, size := range fitSizes {
		for command, keys := range map[string][]string{
			"/connect": {"enter connect", "esc close"},
			"/theme":   {"enter apply", "esc cancel"},
		} {
			m := footerModel(size[0], size[1])
			next, _ := m.handleCommand(command)
			m = next.(Model).recalcLayout()
			assertWholeDialog(t, command, m.View().Content, size[0], size[1], append([]string{"╭", "╰"}, keys...)...)
		}
	}
}

func TestOnboardingPickerFitsWhole(t *testing.T) {
	for _, size := range fitSizes {
		m := footerModel(size[0], size[1])
		m.showOnboarding = true
		m.onboarding = onboardingState{items: pickerModels(40), cursor: 22}
		frame := m.View().Content
		assertWholeDialog(t, "onboarding", frame, size[0], size[1], "› model-22", "enter confirm", "esc quit", "more")
	}
}
