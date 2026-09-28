package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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

// Found in review: Enter on a suggestion completes the input to the command
// name plus a space ("/clear "), and the menu is filtered again from that
// text. The trailing space made the typed name match no command name, so a
// command whose description contains the word ("/memory": "show/edit/clear
// ...") took the highlight and the second Enter opened its sub-menu instead
// of running the command. Typing a prefix, then Enter, Enter must run the
// completed command.
func TestSlashMenuEnterEnterRunsTheCompletedCommand(t *testing.T) {
	cases := []struct{ typed, want string }{
		{"/cle", "/clear"},
		{"/task", "/tasks"},
		{"/approv", "/approve"},
		{"/hel", "/help"},
	}
	for _, tc := range cases {
		m := footerModel(80, 24)
		m.ta.SetValue(tc.typed)
		m.syncInputSuggestions()
		nm, _ := m.updateMain(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = nm.(Model)
		if got := m.ta.Value(); got != tc.want+" " {
			t.Fatalf("%s + Enter: input is %q, want %q", tc.typed, got, tc.want+" ")
		}
		if len(m.cmdItems) == 0 || m.cmdItems[m.cmdCursor].name != tc.want {
			t.Fatalf("%s + Enter: highlighted %v, want %s", tc.typed, m.cmdItems, tc.want)
		}
		nm, _ = m.updateMain(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = nm.(Model)
		// Running a command empties the input; completing another one
		// would leave its name there.
		if got := m.ta.Value(); got != "" {
			t.Fatalf("%s + Enter + Enter: input is %q, want the command run", tc.typed, got)
		}
	}
}
