package agent

import (
	"path/filepath"
	"slices"
	"testing"

	"spettro/internal/config"
)

// codingAgentAdvertisedTools is the tool list the coding agent holds, in
// manifest order: the core tools its requests advertise up front plus the
// deferred ones tool-search loads (see TestCodingAgentAdvertisesCoreToolsOnly).
// Pinned so a new duplicate, or a retired name creeping back into a
// manifest, shows up as a diff here.
var codingAgentAdvertisedTools = []string{
	"agent", "glob", "grep", "file-read", "file-write", "file-edit",
	"lsp", "bash", "job-output", "job-kill",
	"tool-search", "todo-write", "task-stop", "config", "send-message", "comment",
	"skill-read", "skill-list", "save-memory", "web-fetch", "download", "view-image",
	"rename-symbol", "pty-start", "pty-write", "pty-kill", "tool-output",
	"ask-user",
}

// advertisedTools returns the names of the tool schemas an agent's requests
// carry: its allow-list after policy resolution, as buildToolSpecs renders
// it.
func advertisedTools(t *testing.T, m config.AgentManifest, agentID string) []string {
	t.Helper()
	spec, ok := m.AgentByID(agentID)
	if !ok {
		t.Fatalf("agent %q missing", agentID)
	}
	allowed, _ := resolveToolPolicies(spec, &m)
	var names []string
	for _, ts := range buildToolSpecs(allowed) {
		names = append(names, ts.Name)
	}
	return names
}

func TestCodingAgentAdvertisedTools(t *testing.T) {
	project, err := config.LoadAgentManifest(filepath.Join("..", "..", config.AgentManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]config.AgentManifest{
		"default manifest": config.DefaultAgentManifest(),
		"project manifest": project,
	} {
		got := advertisedTools(t, m, "coding")
		if len(got) != len(codingAgentAdvertisedTools) {
			t.Errorf("%s: coding agent advertises %d tools, want %d: %v", name, len(got), len(codingAgentAdvertisedTools), got)
		}
		// Order follows each manifest's allow-list; the set is what matters.
		want := slices.Sorted(slices.Values(codingAgentAdvertisedTools))
		if sorted := slices.Sorted(slices.Values(got)); !slices.Equal(sorted, want) {
			t.Errorf("%s: coding agent advertises %v, want %v", name, sorted, want)
		}
	}
}

// No agent is offered the same tool twice: no name appears twice, no retired
// name is advertised next to (or instead of) its canonical tool, and no two
// tools share a description.
func TestNoAgentAdvertisesDuplicateTools(t *testing.T) {
	project, err := config.LoadAgentManifest(filepath.Join("..", "..", config.AgentManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]config.AgentManifest{
		"default manifest": config.DefaultAgentManifest(),
		"project manifest": project,
	} {
		for _, a := range m.Agents {
			canonical := map[string]string{}
			descs := map[string]string{}
			for _, tool := range advertisedTools(t, m, a.ID) {
				if _, retired := legacyTools[tool]; retired {
					t.Errorf("%s: agent %s advertises retired tool %s", name, a.ID, tool)
				}
				c := CanonicalToolName(tool)
				if prev, dup := canonical[c]; dup {
					t.Errorf("%s: agent %s advertises %s and %s, the same tool", name, a.ID, prev, tool)
				}
				canonical[c] = tool
				desc, _ := toolDescription(tool)
				if prev, dup := descs[desc]; dup {
					t.Errorf("%s: agent %s advertises %s and %s with the same description", name, a.ID, prev, tool)
				}
				descs[desc] = tool
			}
		}
	}
}
