package provider

// Guards for the native encoder's cache: a step costs a bounded number
// of allocations, and concurrent conversations never see each other's
// encodings.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
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

// Sub-agent fan-out: the main agent and ultra's 32 sub-agents take turns on
// the Manager's one encoder, and each conversation keeps its lane, so an
// unchanged history encodes nothing again.
func TestNativeEncoderKeepsALanePerFanOutConversation(t *testing.T) {
	const conversations = 33
	enc := &chatEncoder{}
	reqs := make([]Request, conversations)
	for i := range reqs {
		reqs[i] = guardRequest(100)
		reqs[i].Messages = slices.Clone(reqs[i].Messages)
		reqs[i].Messages[0] = Message{Role: RoleUser, Content: fmt.Sprint("sub-agent task ", i)}
		enc.encode("p", "m", reqs[i])
	}
	n := testing.AllocsPerRun(10, func() {
		for _, r := range reqs {
			enc.encode("p", "m", r)
		}
	})
	// A hit costs the body, its chunk list and the fixed pieces (about 6
	// allocations); a recycled lane costs over 200 at 100 messages.
	const ceiling = 10
	if per := n / conversations; per > ceiling {
		t.Fatalf("%v allocations per conversation and round, want <= %d: lanes were recycled", per, ceiling)
	}
}

// The lanes' cached encodings stay within encoderCacheLimit together.
func TestNativeEncoderStaysWithinItsLimit(t *testing.T) {
	enc := &chatEncoder{}
	big := strings.Repeat("x", 2<<20)
	for i := range 20 {
		enc.encode("p", "m", Request{Messages: []Message{
			{Role: RoleUser, Content: fmt.Sprint("task ", i)},
			{Role: RoleAssistant, Content: big},
		}})
	}
	enc.mu.Lock()
	defer enc.mu.Unlock()
	sum := 0
	for _, l := range enc.lanes {
		sum += l.size
	}
	if enc.total > encoderCacheLimit || sum != enc.total || len(enc.lanes) == 0 {
		t.Fatalf("%d lanes charged %d bytes (sum %d), limit %d", len(enc.lanes), enc.total, sum, encoderCacheLimit)
	}
}

// A conversation unused for encoderLaneIdle (ended, cleared, or continued
// in a new lane after compaction) releases its lane.
func TestNativeEncoderDropsIdleLanes(t *testing.T) {
	enc := &chatEncoder{}
	req := guardRequest(10)
	enc.encode("p", "m", req)
	enc.mu.Lock()
	armed := enc.idle != nil
	enc.dropIdleLocked(time.Now())
	kept := len(enc.lanes)
	enc.dropIdleLocked(time.Now().Add(2 * encoderLaneIdle))
	left, total := len(enc.lanes), enc.total
	enc.mu.Unlock()
	if !armed || kept != 1 || left != 0 || total != 0 {
		t.Fatalf("armed %v, kept %d, left %d lanes holding %d bytes", armed, kept, left, total)
	}
	got := enc.encode("p", "m", req).Bytes()
	if want := (&chatEncoder{}).encode("p", "m", req).Bytes(); string(got) != string(want) {
		t.Fatal("body differs after the lane was dropped")
	}
}

// Images go into the body as the media cache's own bytes: a request with a
// 400 KB screenshot allocates far less than its 533 KB data URL.
func TestNativeEncodeDoesNotCopyImages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(path, make([]byte, 400_000), 0o644); err != nil {
		t.Fatal(err)
	}
	req := guardRequest(10)
	req.Messages = slices.Clone(req.Messages)
	req.Messages[2].ToolResults = []ToolResult{{ID: req.Messages[2].ToolResults[0].ID, Output: "ok", Images: []string{path}}}
	enc := &chatEncoder{}
	enc.encode("p", "m", req) // fills the encoder and media caches
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	body := enc.encode("p", "m", req)
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 256<<10 {
		t.Fatalf("encoding allocated %d bytes: the image was copied", allocated)
	}
	if body.Len() < 533_000 {
		t.Fatalf("body is %d bytes: the image is missing", body.Len())
	}
}

// A request of a single message (a compaction summary carries the whole
// transcript in one) takes no lane: it neither holds its transcript nor
// pushes out a conversation's lane.
func TestNativeEncoderKeepsNoLaneForOneShotRequests(t *testing.T) {
	enc := &chatEncoder{}
	main := guardRequest(10)
	enc.encode("p", "m", main)
	summary := Request{System: "summarize", Messages: []Message{{Role: RoleUser, Content: strings.Repeat("transcript ", 10_000)}}}
	got := enc.encode("p", "m", summary).Bytes()
	enc.mu.Lock()
	lanes, total := len(enc.lanes), enc.total
	enc.mu.Unlock()
	if lanes != 1 || total > 64<<10 {
		t.Fatalf("%d lanes holding %d bytes after a one-shot request, want the main conversation's lane only", lanes, total)
	}
	if want := (&chatEncoder{}).encode("p", "m", summary).Bytes(); string(got) != string(want) {
		t.Fatal("one-shot body differs from a fresh encoder's")
	}
}

// A fan-out whose histories together pass encoderCacheLimit keeps the
// lanes that fit, and those keep hitting, instead of every conversation
// missing in turn as with plain least-recently-used eviction.
func TestNativeEncoderKeepsWhatFitsWhenAFanOutDoesNot(t *testing.T) {
	const conversations = 12
	enc := &chatEncoder{}
	chunk := strings.Repeat("y", encoderCacheLimit/8)
	reqs := make([]Request, conversations)
	for i := range reqs {
		reqs[i] = Request{Messages: []Message{
			{Role: RoleUser, Content: fmt.Sprint("sub-agent task ", i)},
			{Role: RoleAssistant, Content: chunk},
		}}
	}
	kept := func() map[*encoderLane]bool {
		enc.mu.Lock()
		defer enc.mu.Unlock()
		if enc.total > encoderCacheLimit {
			t.Fatalf("lanes hold %d bytes, limit %d", enc.total, encoderCacheLimit)
		}
		set := map[*encoderLane]bool{}
		for _, l := range enc.lanes {
			set[l] = true
		}
		return set
	}
	for _, r := range reqs {
		enc.encode("p", "m", r)
	}
	first := kept()
	for _, r := range reqs {
		enc.encode("p", "m", r)
	}
	second := kept()
	if len(first) < conversations/2 {
		t.Fatalf("only %d of %d lanes kept", len(first), conversations)
	}
	for l := range first {
		if !second[l] {
			t.Fatal("a lane that fitted was dropped by the next round: the fan-out thrashes")
		}
	}
}
