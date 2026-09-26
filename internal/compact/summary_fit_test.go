package compact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"spettro/internal/provider"
)

// smallExchanges is a long run of small tool exchanges: the shape that made
// fitting the summarizer transcript quadratic.
func smallExchanges(n int) []provider.Message {
	msgs := make([]provider.Message, 0, 2*n)
	for i := range n {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs,
			provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.NativeTool{{ID: id, Name: "grep", Args: json.RawMessage(fmt.Sprintf(`{"pattern":"p%d"}`, i))}}},
			provider.Message{Role: provider.RoleUser, ToolResults: []provider.ToolResult{{ID: id, Name: "grep", Output: strings.Repeat("hit ", 10+i%40)}}},
		)
	}
	return msgs
}

// naiveDropOldest is the reference behavior: the fewest dropped turns whose
// rendering fits room.
func naiveDropOldest(turns []provider.Message, names map[string]string, caps renderCaps, room int) string {
	for skip := 1; skip < len(turns); skip++ {
		body := omittedNote(skip) + renderTurns(turns[skip:], names, caps)
		if len(body) <= room {
			return body
		}
	}
	if len(turns) == 0 {
		return ""
	}
	return omittedNote(len(turns)-1) + headTail(renderTurns(turns[len(turns)-1:], names, caps), max(room, 1000))
}

func TestDropOldestMatchesReference(t *testing.T) {
	turns := smallExchanges(40)
	names := callNames(turns)
	full := len(renderTurns(turns, names, minCaps))
	for _, room := range []int{0, 50, 500, full / 3, full / 2, full - 1, full + 100} {
		if got, want := dropOldest(turns, names, minCaps, room), naiveDropOldest(turns, names, minCaps, room); got != want {
			t.Fatalf("room %d: dropOldest differs from the reference\ngot  %.200q\nwant %.200q", room, got, want)
		}
	}
	if got := dropOldest(nil, names, minCaps, 100); got != "" {
		t.Fatalf("no turns rendered %q", got)
	}
}

// Fitting a very long transcript into the summarizer's budget renders each
// turn a bounded number of times: a 1M-window session of ~20k small
// exchanges compacts in well under a second of prompt building (the
// quadratic version took tens of seconds).
func TestSummaryPromptLongTranscriptIsLinear(t *testing.T) {
	middle := smallExchanges(20000)
	task := provider.Message{Role: provider.RoleUser, Content: "Task:\nfix it"}
	start := time.Now()
	prompt := summaryPrompt(task, middle, "", transcriptBudget(200000))
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("building the summarizer prompt took %v", el)
	}
	if len(prompt) > transcriptBudget(200000) {
		t.Fatalf("prompt of %d chars exceeds its budget %d", len(prompt), transcriptBudget(200000))
	}
	if !strings.Contains(prompt, "older turns omitted]") || !strings.Contains(prompt, `"pattern":"p19999"`) {
		t.Fatal("the transcript should drop the oldest turns and keep the newest")
	}
}

// A cancelled run is not a summarizer failure: even a forced pass with the
// extractive fallback reports the cancellation and leaves the history alone.
func TestCompactFallbackNotOnCancellation(t *testing.T) {
	msgs := session(2, 6)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	send := func(ctx context.Context, _ provider.Request) (provider.Response, error) {
		return provider.Response{}, ctx.Err()
	}
	res, err := Compact(ctx, send, msgs, Params{Window: 1_000_000, Force: true, ExtractiveFallback: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if res.Compacted() || res.Fallback || len(res.Messages) != len(msgs) {
		t.Fatalf("a cancelled pass replaced the history: %+v", res)
	}

	dctx, dcancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer dcancel()
	if _, err := Compact(dctx, send, msgs, Params{Window: 1_000_000, Force: true, ExtractiveFallback: true}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
}
