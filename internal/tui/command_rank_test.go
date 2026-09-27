package tui

import (
	"strings"
	"testing"
)

// Found in a VHS run: typing "/skills" and pressing Enter opened the "/skill"
// sub-menu, because "/skill" (whose description mentions "Agent Skills")
// came first in the menu and Enter runs the highlighted entry. The exact name
// now comes first, then name prefixes, then other name matches, then
// commands matched only by their description.
func TestSlashMenuRanksTheExactNameFirst(t *testing.T) {
	m := footerModel(80, 24)
	items := m.filterCommands("skills")
	if len(items) == 0 || items[0].name != "/skills" {
		t.Fatalf("/skills is not the first match: %v", items)
	}
	// The same bug made "/clear" + Enter run the highlighted "/memory
	// clear" (a sub-command runs at once) instead of clearing the chat.
	items = m.filterCommands("clear")
	if len(items) == 0 || items[0].name != "/clear" {
		t.Fatalf("/clear is not the first match: %v", items)
	}
	items = m.filterCommands("skill")
	if len(items) == 0 || items[0].name != "/skill" {
		t.Fatalf("/skill is not the first match for its own name: %v", items)
	}
	// Name matches outrank description matches.
	seenDescOnly := false
	for _, c := range m.filterCommands("mode") {
		nameMatch := strings.Contains(strings.ToLower(c.name), "mode")
		if !nameMatch {
			seenDescOnly = true
		} else if seenDescOnly {
			t.Fatalf("name match %s listed after a description-only match", c.name)
		}
	}

	// Enter on the typed exact name runs it instead of completing another,
	// even when the cursor was moved down the longer list before.
	m.ta.SetValue("/s")
	m.syncInputSuggestions()
	m.cmdCursor = 2
	m.ta.SetValue("/skills")
	m.syncInputSuggestions()
	if m.cmdItems[m.cmdCursor].name != "/skills" {
		t.Fatalf("highlighted %s after typing /skills", m.cmdItems[m.cmdCursor].name)
	}
}
