package tui

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"spettro/internal/agent"
)

func randomBlock(r *rand.Rand) string {
	rows := make([]string, 1+r.Intn(5))
	for i := range rows {
		switch r.Intn(5) {
		case 0:
			rows[i] = ""
		case 1:
			rows[i] = "\x1b[1;31m" + strings.Repeat("x", r.Intn(30)) + "\x1b[m"
		case 2:
			rows[i] = "漢字" + strings.Repeat("字", r.Intn(10)) + "\tt"
		default:
			rows[i] = fmt.Sprintf("row %d %s", i, strings.Repeat("a", r.Intn(40)))
		}
	}
	return strings.Join(rows, "\n")
}

// composeFrame is the old lipgloss joins, byte for byte, with and without a
// side panel, for random parts of every shape.
func TestComposeFrameMatchesLipgloss(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	for trial := 0; trial < 500; trial++ {
		header := randomBlock(r)
		mainTexts := make([]string, 1+r.Intn(5))
		parts := make([]framePart, len(mainTexts))
		for i := range mainTexts {
			mainTexts[i] = randomBlock(r)
			parts[i] = newFramePart(mainTexts[i])
		}
		mainPane := lipgloss.JoinVertical(lipgloss.Left, mainTexts...)
		want := lipgloss.JoinVertical(lipgloss.Left, header, mainPane)
		if got := composeFrame(newFramePart(header), parts, nil); got != want {
			t.Fatalf("trial %d without side panel:\n got %q\nwant %q", trial, got, want)
		}
		sideText := randomBlock(r)
		if r.Intn(2) == 0 {
			sideText += "\n" + randomBlock(r) + "\n" + randomBlock(r)
		}
		side := newFramePart(sideText)
		body := lipgloss.JoinHorizontal(lipgloss.Top, mainPane, " ", sideText)
		want = lipgloss.JoinVertical(lipgloss.Left, header, body)
		if got := composeFrame(newFramePart(header), parts, &side); got != want {
			t.Fatalf("trial %d with side panel:\n got %q\nwant %q", trial, got, want)
		}
	}
}

// The chrome is rendered once while a reply streams: a streamed token
// reuses the header, input box, status bar and side panel of the frame
// before, and any other message renders them again.
func TestStreamedTokensReuseTheChrome(t *testing.T) {
	m := cacheModel(t, 50)
	m.showSidePanel = true
	m.thinking = true
	m.agentStartAt = time.Now()
	m.SetStreamChForTesting()
	nm, _ := m.Update(streamChunkMsg{chunk: agent.StreamChunk{Kind: agent.StreamKindAnswer, Delta: "a"}})
	m = nm.(Model)
	m.View()
	side := m.frameMemo.side.text
	seq := m.chromeSeq
	nm, _ = m.Update(streamChunkMsg{chunk: agent.StreamChunk{Kind: agent.StreamKindAnswer, Delta: " token"}})
	m = nm.(Model)
	frame := m.View().Content
	if m.chromeSeq != seq || m.frameMemo.side.text != side {
		t.Fatal("a streamed token re-rendered the chrome")
	}
	if !strings.Contains(frame, "token") {
		t.Fatal("the streamed token is not in the frame")
	}
	nm, _ = m.Update(tickMsg(time.Now()))
	m = nm.(Model)
	if m.chromeSeq == seq {
		t.Fatal("a tick did not invalidate the chrome")
	}
	tr := agent.ToolTrace{Name: "grep", Status: "running", Args: `{"pattern":"x"}`}
	seq = m.chromeSeq
	nm, _ = m.Update(runEventsMsg{events: []runEvent{{trace: &tr}}})
	m = nm.(Model)
	if m.chromeSeq == seq {
		t.Fatal("a tool trace did not invalidate the chrome (the side panel lists it)")
	}
}

// A banner or typed input set outside Update still shows: the memo key
// covers the fields callers change directly.
func TestChromeMemoSeesDirectChanges(t *testing.T) {
	m := cacheModel(t, 10)
	nm, _ := m.Update(tickMsg(time.Now()))
	m = nm.(Model)
	m.View()
	m.showBanner("a banner set directly", "info")
	m.ta.SetValue("typed directly")
	frame := m.View().Content
	if !strings.Contains(frame, "a banner set directly") || !strings.Contains(frame, "typed directly") {
		t.Fatal("the memoized chrome hid a direct change")
	}
}
