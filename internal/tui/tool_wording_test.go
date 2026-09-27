package tui

import (
	"strings"
	"testing"

	"spettro/internal/agent"
	"spettro/internal/config"
)

// builtinToolNames is every tool name the TUI may have to label: the
// built-ins of the default manifest, the fan-out tools the runtime adds on
// its own, and every retired name a resumed session may still contain.
func builtinToolNames() []string {
	names := []string{"ultra", "workflow", "approval"}
	for _, t := range config.DefaultAgentManifest().Tools {
		if t.IsBuiltin() {
			names = append(names, t.ID)
			names = append(names, agent.LegacyToolNames(t.ID)...)
		}
	}
	return names
}

// Every built-in has counted wording, so no group of its calls reads
// "Used 2 calls" or "Using Todo Write 2 time(s)…", and the running and
// finished forms of a group come from the same entry.
func TestEveryBuiltinHasGroupWording(t *testing.T) {
	for _, name := range builtinToolNames() {
		if _, ok := wordingFor(name); !ok {
			t.Errorf("%s has no wording", name)
			continue
		}
		running := formatToolGroupLabel(name, []ToolItem{{Name: name, Status: "running"}, {Name: name, Status: "running"}})
		done := formatToolGroupLabel(name, []ToolItem{{Name: name, Status: "success"}, {Name: name, Status: "success"}})
		if strings.HasPrefix(running, "Using ") || strings.HasPrefix(done, "Used ") {
			t.Errorf("%s: generic group label: running %q, done %q", name, running, done)
		}
		if !strings.HasSuffix(running, "…") || strings.HasSuffix(done, "…") {
			t.Errorf("%s: running %q should end with …, done %q should not", name, running, done)
		}
		if single := formatRunningLabel(name, `{}`); strings.HasPrefix(single, "Using ") {
			t.Errorf("%s: generic running label %q", name, single)
		}
	}
}

// The wording reads naturally: no noun repeating its verb.
func TestGroupWordingExamples(t *testing.T) {
	two := func(name, status string) string {
		return formatToolGroupLabel(name, []ToolItem{{Name: name, Status: status}, {Name: name, Status: status}})
	}
	for _, c := range []struct{ name, status, want string }{
		{"todo-write", "running", "Updating the todo list 2 times…"},
		{"todo-write", "success", "Updated the todo list 2 times"},
		{"pty-start", "running", "Starting 2 terminal sessions…"},
		{"download", "success", "Downloaded 2 files"},
		{"agent", "success", "Delegated 2 tasks"},
		{"enter-plan-mode", "success", "Entered plan mode 2 times"},
		{"mcp-auth", "success", "Updated MCP auth 2 times"},
		{"view-image", "success", "Viewed 2 images"},
		{"lsp", "running", "Querying the language server 2 times…"},
		{"grep", "running", "Grepping 2 patterns…"},
		{"grep", "success", "Grepped 2 patterns"},
	} {
		if got := two(c.name, c.status); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.name, c.status, got, c.want)
		}
	}
}

// file_path is the spelling Claude-trained models send; the label and the
// diff must find the file either way.
func TestFileToolsReadFilePathAlias(t *testing.T) {
	args := `{"file_path":"a/b.go","old_string":"x","new_string":"y"}`
	if got := formatToolLabel("file-edit", args); got != "Edited a/b.go" {
		t.Errorf("label = %q", got)
	}
	if got := formatRunningLabel("file-edit", args); got != "Editing a/b.go…" {
		t.Errorf("running label = %q", got)
	}
	if got := toolDescriptor("file-write", `{"file_path":"c.go"}`); got != "c.go" {
		t.Errorf("descriptor = %q", got)
	}
	if got := filePathArg(`{"path":"p.go","file_path":"q.go"}`); got != "p.go" {
		t.Errorf("path wins over file_path: %q", got)
	}
}
