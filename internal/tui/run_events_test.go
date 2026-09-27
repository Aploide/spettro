package tui

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"spettro/internal/agent"
)

// Events come out in the order they went in, across both kinds, and every
// event pushed before close is delivered before the reader ends.
func TestRunEventQueueKeepsOrderAcrossKinds(t *testing.T) {
	q := newRunEventQueue()
	for i := 0; i < 1000; i++ {
		if i%3 == 0 {
			tr := agent.ToolTrace{Name: fmt.Sprint(i)}
			q.push(runEvent{trace: &tr})
		} else {
			c := agent.StreamChunk{Delta: fmt.Sprint(i)}
			q.push(runEvent{chunk: &c})
		}
	}
	q.close()
	next := 0
	wait := waitForRunEvents(q)
	for {
		msg := wait()
		if msg == nil {
			break
		}
		for _, ev := range msg.(runEventsMsg).events {
			got := ""
			if ev.trace != nil {
				got = ev.trace.Name
			} else {
				got = ev.chunk.Delta
			}
			if got != fmt.Sprint(next) {
				t.Fatalf("event %d delivered as %q", next, got)
			}
			next++
		}
	}
	if next != 1000 {
		t.Fatalf("delivered %d of 1000 events", next)
	}
}

// The run's done message waits until the UI has applied every event: the
// queue only counts as drained when the reader, re-armed after the last
// batch was applied, finds it closed and empty.
func TestRunDoneWaitsForTheLastBatch(t *testing.T) {
	q := newRunEventQueue()
	c := agent.StreamChunk{Delta: "last words"}
	q.push(runEvent{chunk: &c})
	q.close()
	drained := make(chan struct{})
	go func() { q.waitDrained(5 * time.Second); close(drained) }()
	wait := waitForRunEvents(q)
	if msg := wait(); msg == nil {
		t.Fatal("the last batch was not delivered")
	}
	select {
	case <-drained:
		t.Fatal("drained before the last batch was applied")
	case <-time.After(50 * time.Millisecond):
	}
	if msg := wait(); msg != nil { // the handler re-arms after applying
		t.Fatalf("a closed, empty queue delivered %v", msg)
	}
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("the queue never reported drained")
	}
}

// push never blocks, even when nothing reads: the agent never waits on the
// UI.
func TestRunEventQueuePushNeverBlocks(t *testing.T) {
	q := newRunEventQueue()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100000; i++ {
			c := agent.StreamChunk{Delta: "x"}
			q.push(runEvent{chunk: &c})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("push blocked with no reader")
	}
}

// Concurrent producers and a reader: nothing is lost or duplicated (run
// with -race).
func TestRunEventQueueConcurrent(t *testing.T) {
	q := newRunEventQueue()
	var wg sync.WaitGroup
	for p := 0; p < 4; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				c := agent.StreamChunk{Delta: "x"}
				q.push(runEvent{chunk: &c})
			}
		}()
	}
	go func() { wg.Wait(); q.close() }()
	total := 0
	wait := waitForRunEvents(q)
	for msg := wait(); msg != nil; msg = wait() {
		total += len(msg.(runEventsMsg).events)
	}
	if total != 2000 {
		t.Fatalf("delivered %d of 2000 events", total)
	}
}

// A batch is applied with one refresh and re-arms the reader; the streamed
// text and the tool row both land on screen.
func TestRunEventsBatchIsApplied(t *testing.T) {
	m := footerModel(120, 40)
	m.thinking = true
	q := newRunEventQueue()
	m.runEvents = q
	c1 := agent.StreamChunk{Kind: agent.StreamKindAnswer, Delta: "streamed words"}
	tr := agent.ToolTrace{Name: "grep", Status: "running", Args: `{"pattern":"needle"}`}
	nm, cmd := m.Update(runEventsMsg{events: []runEvent{{chunk: &c1}, {trace: &tr}}})
	m = nm.(Model)
	if cmd == nil {
		t.Fatal("the reader was not re-armed")
	}
	view := ansi.Strip(m.vp.View())
	if !strings.Contains(view, "streamed words") {
		t.Fatalf("the streamed text is not on screen:\n%s", view)
	}
	if len(m.messages) != 2 || m.messages[1].Kind != "tool-stream" {
		t.Fatalf("the tool row did not follow the text: %+v", m.messages)
	}
}

// Stopping a run (Esc, a denied approval, a remote interrupt) abandons its
// queue: a batch the reader had already returned is dropped by Update, and
// the stopped run's command is not held back waiting for a drain that no
// reader will ever finish.
func TestStoppedRunReleasesItsDoneMessage(t *testing.T) {
	m := footerModel(120, 40)
	m.thinking = true
	q := newRunEventQueue()
	m.runEvents = q
	tr := agent.ToolTrace{Name: "bash", Status: "error"}
	q.push(runEvent{trace: &tr})
	batch := waitForRunEvents(q)()
	m.stopAgent()
	nm, _ := m.Update(batch)
	m = nm.(Model)
	for _, msg := range m.messages {
		if msg.Kind == "tool-stream" {
			t.Fatalf("a batch of the stopped run was applied: %+v", msg)
		}
	}
	q.close() // what the stopped run's command does when a.Run returns
	select {
	case <-q.drained:
	default:
		t.Fatal("waitDrained still blocks after the run was stopped")
	}
	if msg := waitForRunEvents(q)(); msg != nil {
		t.Fatalf("the reader of an abandoned queue returned %T, want nil", msg)
	}
}

// A reader blocked waiting for events returns as soon as the run is stopped.
func TestAbandonWakesABlockedReader(t *testing.T) {
	q := newRunEventQueue()
	done := make(chan tea.Msg, 1)
	go func() { done <- waitForRunEvents(q)() }()
	q.abandon()
	select {
	case msg := <-done:
		if msg != nil {
			t.Fatalf("reader returned %T, want nil", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reader did not return after abandon")
	}
}

// The done message of a stopped run must not end a run started after it:
// it names its run, and only the active run's messages are applied.
func TestStaleDoneMessageLeavesTheNewRunAlone(t *testing.T) {
	stopped := newRunEventQueue()
	for _, msg := range []tea.Msg{
		agentDoneMsg{run: stopped, err: fmt.Errorf("context canceled")},
		planDoneMsg{run: stopped, plan: "stale plan"},
		runEventsMsg{queue: stopped, events: []runEvent{{chunk: &agent.StreamChunk{Kind: agent.StreamKindAnswer, Delta: "stale"}}}},
	} {
		m := footerModel(120, 40)
		m.thinking = true
		cancelled := false
		m.cancelAgent = func() { cancelled = true }
		current := newRunEventQueue()
		m.runEvents = current
		nm, _ := m.Update(msg)
		m = nm.(Model)
		if !m.thinking || m.runEvents != current || m.cancelAgent == nil || cancelled {
			t.Fatalf("%T of a stopped run ended the new run: thinking=%v runEvents=%v", msg, m.thinking, m.runEvents == current)
		}
		if strings.Contains(ansi.Strip(m.View().Content), "context canceled") {
			t.Fatalf("%T of a stopped run showed its error on the new run", msg)
		}
	}
	// The active run's own done message still ends it.
	m := footerModel(120, 40)
	m.thinking = true
	current := newRunEventQueue()
	m.runEvents = current
	nm, _ := m.Update(agentDoneMsg{run: current, content: "finished"})
	if nm.(Model).thinking {
		t.Fatal("the active run's done message did not end it")
	}
}
