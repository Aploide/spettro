package tui

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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
