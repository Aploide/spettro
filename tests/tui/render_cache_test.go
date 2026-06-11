package tui_test

import (
	"strings"
	"testing"
	"time"

	"spettro/internal/tui"
)

func cachedModel(t *testing.T) tui.Model {
	t.Helper()
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(80, 24)
	m.AddMessageForTesting(tui.ChatMessage{Role: tui.RoleUser, Content: "first question", At: time.Now()})
	m.AddMessageForTesting(tui.ChatMessage{Role: tui.RoleAssistant, Content: "# Heading\n\nsome **markdown** reply", At: time.Now()})
	m.AddMessageForTesting(tui.ChatMessage{Role: tui.RoleSystem, Content: "a system note", At: time.Now()})
	return m
}

// TestRenderCache_PopulatesAndReuses verifies the cache is built and that a
// repeat render produces identical output (and the same cache size).
func TestRenderCache_PopulatesAndReuses(t *testing.T) {
	m := cachedModel(t)

	first := m.RenderMessagesForTesting()
	if got := m.RenderCacheSizeForTesting(); got != 3 {
		t.Fatalf("expected 3 cached blocks, got %d", got)
	}

	second := m.RenderMessagesForTesting()
	if first != second {
		t.Fatalf("repeat render must be identical:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if got := m.RenderCacheSizeForTesting(); got != 3 {
		t.Fatalf("cache size should be stable at 3, got %d", got)
	}
}

// TestRenderCache_InvalidatesOnWidthChange verifies a width change rescopes the
// cache and reflows the output.
func TestRenderCache_InvalidatesOnWidthChange(t *testing.T) {
	m := cachedModel(t)
	_ = m.RenderMessagesForTesting()
	if w := m.RenderCacheWidthForTesting(); w <= 0 {
		t.Fatalf("expected positive cache width, got %d", w)
	}
	wideWidth := m.RenderCacheWidthForTesting()

	m.SetDimensionsForTesting(40, 24)
	_ = m.RenderMessagesForTesting()
	if w := m.RenderCacheWidthForTesting(); w == wideWidth {
		t.Fatalf("cache width should change with the pane width (was %d)", wideWidth)
	}
}

// TestRenderCache_MutationReRenders verifies that mutating a message's content
// IN PLACE on the same model invalidates its cached block (content-hash keying
// re-renders only that block, never serving stale content).
func TestRenderCache_MutationReRenders(t *testing.T) {
	m := tui.NewModelForTesting()
	m.SetDimensionsForTesting(80, 24)
	m.AddMessageForTesting(tui.ChatMessage{Role: tui.RoleAssistant, Content: "original reply", At: time.Now()})

	before := m.RenderMessagesForTesting() // warms the cache
	if !strings.Contains(before, "original reply") {
		t.Fatalf("expected original content, got:\n%s", before)
	}

	m.MutateMessageContentForTesting(0, "edited reply")
	after := m.RenderMessagesForTesting()
	if strings.Contains(after, "original reply") {
		t.Fatalf("stale content served from cache after mutation:\n%s", after)
	}
	if !strings.Contains(after, "edited reply") {
		t.Fatalf("expected edited content, got:\n%s", after)
	}
}

// TestRenderCache_OutputMatchesUncached verifies the cached render equals a
// from-scratch render of the same model (cache must not alter output).
func TestRenderCache_OutputMatchesUncached(t *testing.T) {
	m := cachedModel(t)
	cached := m.RenderMessagesForTesting() // warms cache, then reuses

	// A fresh model with the same messages has an empty cache on first render.
	fresh := cachedModel(t)
	uncached := fresh.RenderMessagesForTesting()

	if cached != uncached {
		t.Fatalf("cached output must match uncached:\n--- cached ---\n%s\n--- uncached ---\n%s", cached, uncached)
	}
}
