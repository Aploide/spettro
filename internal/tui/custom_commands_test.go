package tui

import (
	"strings"
	"testing"

	"spettro/internal/commands"
	"spettro/internal/config"
)

func newCustomCmdModel() Model {
	m := NewModelForTesting()
	m.customCommands = []commands.Command{
		{Name: "review", Description: "quick review", Prompt: "Please review {{args}} carefully.", Scope: "project"},
		{Name: "git:pr", Prompt: "Open a PR.", Scope: "user"},
		{Name: "st", Prompt: "status: !`echo hi`", Scope: "project"},
	}
	return m
}

// Custom commands must show up in the slash-command autocomplete catalog.
func TestFilterCommandsIncludesCustom(t *testing.T) {
	m := newCustomCmdModel()
	found := map[string]string{}
	for _, c := range m.filterCommands("") {
		found[c.name] = c.desc
	}
	if found["/review"] != "quick review" {
		t.Fatalf("missing /review with description, got %q", found["/review"])
	}
	if desc, ok := found["/git:pr"]; !ok || !strings.Contains(desc, "custom command") {
		t.Fatalf("missing /git:pr fallback description, got %q", desc)
	}
	if items := m.filterCommands("git:pr"); len(items) != 1 || items[0].name != "/git:pr" {
		t.Fatalf("query should match custom command, got %+v", items)
	}
}

// Running a custom command expands {{args}} and sends the result as a prompt
// (visible as the user message of the started run).
func TestHandleCommandDispatchesCustom(t *testing.T) {
	m := newCustomCmdModel()
	nm, _ := m.handleCommand("/review main.go and utils.go")
	got := nm.(Model)
	msgs := got.MessagesForTesting()
	if len(msgs) == 0 {
		t.Fatal("expected the expanded prompt to be sent")
	}
	last := msgs[len(msgs)-1]
	if last.Role != RoleUser || last.Content != "Please review main.go and utils.go carefully." {
		t.Fatalf("unexpected prompt message: %+v", last)
	}
}

// Namespaced names (subdirectory files) resolve case-insensitively.
func TestHandleCommandNamespacedCustom(t *testing.T) {
	m := newCustomCmdModel()
	nm, _ := m.handleCommand("/Git:PR")
	got := nm.(Model)
	msgs := got.MessagesForTesting()
	if len(msgs) == 0 || msgs[len(msgs)-1].Content != "Open a PR." {
		t.Fatalf("namespaced custom command not dispatched: %+v", msgs)
	}
}

// Shell interpolation is refused unless permission is yolo.
func TestCustomCommandShellGatedByPermission(t *testing.T) {
	m := newCustomCmdModel()
	m.cfg.Permission = config.PermissionAskFirst
	nm, _ := m.handleCommand("/st")
	got := nm.(Model)
	if len(got.MessagesForTesting()) != 0 {
		t.Fatal("gated command must not send a prompt")
	}
	if !strings.Contains(got.BannerForTesting(), "yolo") {
		t.Fatalf("expected permission error banner, got %q", got.BannerForTesting())
	}

	m.cfg.Permission = config.PermissionYOLO
	nm, _ = m.handleCommand("/st")
	got = nm.(Model)
	msgs := got.MessagesForTesting()
	if len(msgs) == 0 || msgs[len(msgs)-1].Content != "status: hi" {
		t.Fatalf("yolo should run interpolation: %+v", msgs)
	}
}

// A slash command that matches nothing still reports unknown.
func TestUnknownCommandStillErrors(t *testing.T) {
	m := newCustomCmdModel()
	nm, _ := m.handleCommand("/nosuchthing")
	got := nm.(Model)
	if !strings.Contains(got.BannerForTesting(), "unknown command") {
		t.Fatalf("expected unknown command banner, got %q", got.BannerForTesting())
	}
}

// /help lists custom commands.
func TestHelpListsCustomCommands(t *testing.T) {
	m := newCustomCmdModel()
	help := m.customCommandsHelp()
	for _, want := range []string{"/review", "quick review", "/git:pr", "{{args}}"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
		}
	}
	var empty Model
	if empty.customCommandsHelp() != "" {
		t.Fatal("no custom commands should render no help section")
	}
}
