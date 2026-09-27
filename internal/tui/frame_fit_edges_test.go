package tui

// Frame-fit edge cases: states the render_fit_test sweep did not reach, each
// of which once drew a frame taller or wider than the terminal, or let
// untrusted text reach it raw. They use the helpers of render_fit_test.go
// (assertFrameFits, footerModel, hugeTranscriptModel, approvalModel).

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// runningFooterModel is a model in the middle of a run with a long todo list
// and two delegations: the working indicator and the todo/agent footer are
// both on screen.
func runningFooterModel(width, height int) Model {
	m := footerModel(width, height)
	m.messages = append(m.messages, ChatMessage{Role: RoleUser, Content: "refactor the parser"})
	footerTodos(&m, 10)
	footerWorkers(&m, 2)
	m.thinking = true
	m.agentStartAt = time.Now()
	return m
}

// assertStatusBarOnScreen fails when the frame's last row is not the status
// bar, which is what goes missing first when the frame is too tall.
func assertStatusBarOnScreen(t *testing.T, name, frame string) {
	t.Helper()
	rows := strings.Split(ansi.Strip(frame), "\n")
	if last := rows[len(rows)-1]; !strings.Contains(last, "ctx") {
		t.Fatalf("%s: the last row is not the status bar: %q\n%s", name, last, strings.Join(rows, "\n"))
	}
}

// During a run the todo/agent footer shares a short terminal with the
// working indicator and the input box. It gives up rows whenever the
// transcript would otherwise get none, not only while a dialog is open.
func TestRunFooterYieldsOnShortTerminals(t *testing.T) {
	for _, size := range [][2]int{{40, 15}, {60, 15}, {80, 15}, {60, 12}, {80, 24}} {
		m := runningFooterModel(size[0], size[1])
		m = m.recalcLayout()
		m.refreshViewport()
		frame := m.View().Content
		name := fmt.Sprintf("running footer at %dx%d", size[0], size[1])
		assertFrameFits(t, name, frame, size[0], size[1])
		assertStatusBarOnScreen(t, name, frame)
	}
	// On a normal terminal the footer keeps its full allowance.
	m := runningFooterModel(80, 24)
	if got, want := m.parallelFooterBudget(), footerBudget(24); got != want {
		t.Fatalf("80x24: footer budget %d, want the full %d", got, want)
	}
}

// The steer picker (Enter pressed mid-run) and the plan approval picker are
// drawn in the input box like the approval dialog, and the footer yields to
// them the same way.
func TestSteerAndPlanPickersFitAlongsideTheRunFooter(t *testing.T) {
	for _, size := range [][2]int{{40, 15}, {50, 18}, {80, 24}} {
		m := runningFooterModel(size[0], size[1])
		m.showSteerChoice = true
		m.steerPending = strings.Repeat("please also update the documentation and the tests ", 6)
		m = m.recalcLayout()
		frame := m.View().Content
		name := fmt.Sprintf("steer picker at %dx%d", size[0], size[1])
		assertFrameFits(t, name, frame, size[0], size[1])
		assertStatusBarOnScreen(t, name, frame)
		if !strings.Contains(ansi.Strip(frame), "Discard") {
			t.Fatalf("%s: the picker lost an option:\n%s", name, ansi.Strip(frame))
		}

		m = footerModel(size[0], size[1])
		m.messages = append(m.messages, ChatMessage{Role: RoleUser, Content: "plan it"})
		footerTodos(&m, 10)
		m.showPlanApproval = true
		m.pendingPlan = "1. do the thing\n2. do the other thing"
		m = m.recalcLayout()
		frame = m.View().Content
		name = fmt.Sprintf("plan approval at %dx%d", size[0], size[1])
		assertFrameFits(t, name, frame, size[0], size[1])
		assertStatusBarOnScreen(t, name, frame)
		if !strings.Contains(ansi.Strip(frame), "Edit") {
			t.Fatalf("%s: the picker lost an option:\n%s", name, ansi.Strip(frame))
		}
	}
}

// The slash-command overlay leaves room for the input box as drawn, an
// attachment chip included.
func TestCommandOverlayFitsWithAnAttachment(t *testing.T) {
	for _, size := range fitSizes {
		m := footerModel(size[0], size[1])
		m.attachments = []attachmentItem{{Kind: "file", Path: "/tmp/notes.md", RelPath: "notes.md"}}
		for i := range 30 {
			m.cmdItems = append(m.cmdItems, commandDef{name: fmt.Sprintf("/cmd-%d", i), desc: "a command"})
		}
		m = m.recalcLayout()
		assertFrameFits(t, "command overlay with an attachment", m.View().Content, size[0], size[1])
	}
}

// The status bar is one row even when its right-hand cluster alone is wider
// than the terminal: the context label is kept and what does not fit is cut.
func TestStatusBarRightClusterFits(t *testing.T) {
	for _, width := range []int{24, 30, 34, 40} {
		m := footerModel(width, 15)
		m.cfg.AutoCompactEnabled = false
		m.contextTokens = 123456
		bar := m.viewStatusBar(m.paneWidth())
		if h := strings.Count(bar, "\n") + 1; h != 1 {
			t.Fatalf("width %d: status bar is %d rows: %q", width, h, ansi.Strip(bar))
		}
		if w := ansi.StringWidth(bar); w > width {
			t.Fatalf("width %d: status bar is %d cells", width, w)
		}
		assertFrameFits(t, "status bar", m.View().Content, width, 15)
	}
}
