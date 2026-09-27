package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The activity feed keeps the newest maxActivityItems and counts the rest.
func TestActivityFeedIsCapped(t *testing.T) {
	m := footerModel(140, 40)
	for i := 0; i < maxActivityItems+500; i++ {
		m.upsertActivity(activityItem{Key: fmt.Sprint("k", i), Kind: "tool", Title: "t"})
	}
	if len(m.activityFeed) != maxActivityItems || m.activityDropped != 500 {
		t.Fatalf("feed holds %d items, %d dropped", len(m.activityFeed), m.activityDropped)
	}
	if m.activityFeed[0].Key != "k500" {
		t.Fatalf("the oldest kept item is %q, want k500", m.activityFeed[0].Key)
	}
	// An update to a kept item replaces it in place.
	m.upsertActivity(activityItem{Key: "k600", Kind: "tool", Title: "updated"})
	if len(m.activityFeed) != maxActivityItems || m.activityFeed[100].Title != "updated" {
		t.Fatal("an update to a kept item did not replace it")
	}
	m.showSidePanel = true
	if parts := m.sidePanelHeaderParts(m.sidePanelWidth()); !strings.Contains(ansi.Strip(parts[1]), "500 earlier dropped") {
		t.Fatalf("the panel does not report the dropped items: %q", ansi.Strip(parts[1]))
	}
}
