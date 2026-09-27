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
	// The count survives the narrow panel of a 120-column terminal.
	m.width = 120
	m = m.recalcLayout()
	if view := ansi.Strip(m.viewSidePanel(m.sidePanelWidth())); !strings.Contains(view, "500 earlier dropped") {
		t.Fatalf("a 120-column terminal cuts the dropped count:\n%s", view)
	}
}

// A window that ends on an agent's header row still names the agent, whose
// first item is just below the window.
func TestSidePanelHeaderAtTheWindowEdgeNamesItsAgent(t *testing.T) {
	m := footerModel(160, 40)
	var items []sidePanelItem
	for i := 0; i < 6; i++ {
		items = append(items, sidePanelItem{Kind: "tool", Title: "a", Agent: "coding"})
	}
	items = append(items, sidePanelItem{Kind: "tool", Title: "b", Agent: "explorer"})
	// Rows: header, six coding items, header, the explorer item. A window
	// of eight rows around the first item ends on the second header.
	visible, rowToItem := m.sidePanelList(items, 40, 8)
	last := len(visible) - 1
	if rowToItem[last] != -1 {
		t.Fatalf("the window does not end on a header: %v", rowToItem)
	}
	want := activityAgentLabel("explorer")
	if got := strings.TrimSpace(ansi.Strip(visible[last])); got != want {
		t.Fatalf("header at the window's edge reads %q, want %q", got, want)
	}
}
