package tui

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"spettro/internal/jobs"
	"spettro/internal/session"
	"spettro/internal/spettro"
	"spettro/internal/theme"
)

// An idle TUI arms no animation tick: after the tick in flight lands,
// nothing re-arms it.
func TestIdleDoesNotTick(t *testing.T) {
	m := footerModel(120, 40)
	m.tickArmed = true
	nm, cmd := m.Update(tickMsg(time.Now()))
	m = nm.(Model)
	if m.tickArmed || cmd != nil {
		t.Fatalf("an idle model re-armed the animation tick (armed=%v cmd=%v)", m.tickArmed, cmd != nil)
	}
}

// Every animated element keeps the tick running while it is on screen, and
// only one tick chain runs at a time.
func TestAnimatedElementsKeepTicking(t *testing.T) {
	cases := []struct {
		name string
		set  func(m *Model)
	}{
		{"working indicator", func(m *Model) { m.thinking = true; m.agentStartAt = time.Now() }},
		{"onboarding", func(m *Model) { m.showOnboarding = true }},
		{"sign-in", func(m *Model) { m.showLogin = true }},
		{"MAX plan label", func(m *Model) {
			m.cfg.APIKeys = map[string]string{spettro.ProviderID: "ep_x"}
			m.cfg.SpettroPlan = "max"
		}},
		{"goal elapsed time", func(m *Model) { m.activeGoal = &goalState{Objective: "x", StartedAt: time.Now()} }},
		{"running delegation", func(m *Model) {
			m.parallelAgents = []parallelAgentEntry{{ID: "a", Status: "running"}}
		}},
		{"running workflow", func(m *Model) { m.workflow = &workflowRun{Status: "running"} }},
		{"in-progress task", func(m *Model) { m.todos = []session.Todo{{Content: "x", Status: "in_progress"}} }},
		{"glowing input keyword", func(m *Model) { m.ta.SetValue("run a workflow") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := footerModel(120, 40)
			tc.set(&m)
			if !m.needsAnimation() {
				t.Fatal("needsAnimation is false while the element is on screen")
			}
			// A tick lands: the next one must be armed.
			m.tickArmed = true
			frame := m.eyeFrame
			nm, cmd := m.Update(tickMsg(time.Now()))
			m = nm.(Model)
			if m.eyeFrame != frame+1 {
				t.Fatalf("the tick did not advance the frame")
			}
			if !m.tickArmed || cmd == nil {
				t.Fatal("the animation stopped: no tick re-armed")
			}
			// While that tick is in flight nothing arms a second one.
			if cmd := m.armTimers(); cmd != nil {
				t.Fatal("a second tick chain was started")
			}
		})
	}
}

// A banner clears itself with a one-shot timer at its expiry, and a timer
// left over from an older banner clears nothing.
func TestBannerClearsOnItsOwnTimer(t *testing.T) {
	m := footerModel(120, 40)
	m.tickArmed = true // not idle-ticking: the banner timer must do the work
	m.showBanner("first", "info")
	nm, cmd := m.Update(tea.FocusMsg{})
	m = nm.(Model)
	if cmd == nil || !m.bannerTimerAt.Equal(m.bannerClearAt) {
		t.Fatal("no timer was armed for the banner")
	}
	first := m.bannerClearAt
	m.showBanner("second", "info")
	m.bannerClearAt = first.Add(time.Second)
	nm, _ = m.Update(bannerExpiredMsg{at: first})
	m = nm.(Model)
	if m.banner != "second" {
		t.Fatalf("an older banner's timer cleared the newer banner (banner=%q)", m.banner)
	}
	nm, _ = m.Update(bannerExpiredMsg{at: m.bannerClearAt})
	m = nm.(Model)
	if m.banner != "" {
		t.Fatalf("the banner did not clear at its expiry (banner=%q)", m.banner)
	}
}

// The cursor is steady unless cursor_blink is set: a blinking cursor wakes
// the TUI twice a second.
func TestCursorBlinkSetting(t *testing.T) {
	if s := textareaStyles(theme.Dark(), false); s.Cursor.Blink {
		t.Fatal("the default cursor blinks")
	}
	if s := textareaStyles(theme.Dark(), true); !s.Cursor.Blink {
		t.Fatal("cursor_blink=true does not blink")
	}
}

// Status bar values that change with no message reaching the TUI (a /loop
// counting down, a background job that ends) are redrawn by the 1 s clock
// tick, which runs only while one of them is on screen.
func TestClockElementsKeepTicking(t *testing.T) {
	t.Run("loop countdown", func(t *testing.T) {
		m := footerModel(120, 40)
		m.activeLoop = &loopState{ID: 1, Interval: 5 * time.Minute, Prompt: "x", StartedAt: time.Now(), NextAt: time.Now().Add(5 * time.Minute)}
		nm, _ := m.Update(tea.FocusMsg{})
		m = nm.(Model)
		if !m.clockArmed {
			t.Fatal("an idle /loop armed no clock tick")
		}
		if view := ansi.Strip(m.View().Content); !strings.Contains(view, "next in 5m0s") {
			t.Fatalf("the countdown is not on screen:\n%s", view)
		}
		// Ten seconds pass; the clock tick redraws the memoized status bar.
		m.activeLoop.NextAt = m.activeLoop.NextAt.Add(-10 * time.Second)
		nm, _ = m.Update(clockTickMsg(time.Now()))
		m = nm.(Model)
		if view := ansi.Strip(m.View().Content); !strings.Contains(view, "next in 4m50s") {
			t.Fatalf("the countdown did not move:\n%s", view)
		}
		if !m.clockArmed {
			t.Fatal("the clock stopped while the loop is active")
		}
		m.activeLoop = nil
		nm, _ = m.Update(clockTickMsg(time.Now()))
		if nm.(Model).clockArmed {
			t.Fatal("the clock kept ticking after the loop ended")
		}
	})
	t.Run("background job counter", func(t *testing.T) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestClockHelperProcess$")
		cmd.Env = append(os.Environ(), "SPETTRO_TUI_CLOCK_HELPER=1")
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		job, err := jobs.Default().Start(cmd, "helper")
		if err != nil {
			t.Fatal(err)
		}
		m := footerModel(120, 40)
		nm, _ := m.Update(tea.FocusMsg{})
		m = nm.(Model)
		if !m.clockArmed {
			stdin.Close()
			t.Fatal("a running background job armed no clock tick")
		}
		if view := ansi.Strip(m.View().Content); !strings.Contains(view, "1 bg job") {
			stdin.Close()
			t.Fatalf("the job counter is not on screen:\n%s", view)
		}
		stdin.Close() // the helper exits
		deadline := time.Now().Add(10 * time.Second)
		for job.Running() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if job.Running() {
			t.Fatal("the helper process did not exit")
		}
		nm, _ = m.Update(clockTickMsg(time.Now()))
		m = nm.(Model)
		if view := ansi.Strip(m.View().Content); strings.Contains(view, "bg job") {
			t.Fatalf("the ended job is still counted:\n%s", view)
		}
		if m.clockArmed {
			t.Fatal("the clock kept ticking with no job running")
		}
	})
}

// TestClockHelperProcess is the background job of TestClockElementsKeepTicking:
// it runs until its stdin is closed.
func TestClockHelperProcess(t *testing.T) {
	if os.Getenv("SPETTRO_TUI_CLOCK_HELPER") != "1" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}
