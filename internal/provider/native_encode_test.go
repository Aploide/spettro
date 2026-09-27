package provider

// Guards for the native encoder's cache: a step costs a bounded number
// of allocations, and concurrent conversations never see each other's
// encodings.

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// A step that adds two messages to a 500-message history re-encodes only
// those two: a bounded number of allocations, independent of the history.
func TestNativeEncodeStepAllocs(t *testing.T) {
	req := guardRequest(500)
	enc := &chatEncoder{}
	enc.encode("p", "m", req)
	short := req
	short.Messages = req.Messages[:len(req.Messages)-2]
	// Alternate between the two lengths so every run re-encodes the two
	// newest messages.
	i := 0
	n := testing.AllocsPerRun(50, func() {
		r := req
		if i%2 == 1 {
			r = short
		}
		i++
		enc.encode("p", "m", r)
	})
	// Head, body chunk list, the two messages and their snapshots (14 at
	// the time of writing).
	const ceiling = 20
	if n > ceiling {
		t.Fatalf("encode step allocated %v times, want <= %d", n, ceiling)
	}
}

// Concurrent conversations (the main agent and its sub-agents) share one
// encoder; each must still get exactly its own body. Run with -race.
func TestNativeEncoderConcurrentConversations(t *testing.T) {
	shared := &chatEncoder{}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Pairs of goroutines share a first message, so they also
			// contend for one lane.
			msgs := []Message{{Role: RoleUser, Content: fmt.Sprint("task ", g/2)}}
			for step := range 30 {
				msgs = append(msgs,
					Message{Role: RoleAssistant, Content: fmt.Sprint("g", g, " step ", step)},
					Message{Role: RoleUser, Content: strings.Repeat("x", step)})
				req := Request{System: fmt.Sprint("sys ", g), Messages: msgs}
				got := shared.encode("p", "m", req).Bytes()
				want := (&chatEncoder{}).encode("p", "m", req).Bytes()
				if string(got) != string(want) {
					errs <- fmt.Errorf("goroutine %d step %d: body differs", g, step)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
