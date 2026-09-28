package agent

import (
	"slices"
	"testing"

	compactpkg "spettro/internal/compact"
	"spettro/internal/config"
)

// The retired-name to canonical-name mapping is written out in more than
// one package: legacyTools here (what a call is routed to), the manifest
// migrations' fold tables (which become the default manifest's aliases),
// and compact's shell-tool check. They must agree, or a call under a
// retired name is routed one way and summarized, advertised or migrated
// another.
func TestRetiredNameTablesAgree(t *testing.T) {
	m := config.DefaultAgentManifest()
	for canonical := range canonicalNames() {
		tool, ok := m.ToolByID(canonical)
		if !ok {
			t.Errorf("canonical tool %s is missing from the default manifest", canonical)
			continue
		}
		for _, retired := range LegacyToolNames(canonical) {
			if !slices.Contains(tool.Aliases, retired) {
				t.Errorf("%s routes to %s, but the default manifest's %s does not alias it (aliases %v)", retired, canonical, canonical, tool.Aliases)
			}
		}
	}
	for _, tool := range m.Tools {
		for _, alias := range tool.Aliases {
			if _, retired := legacyTools[alias]; retired && CanonicalToolName(alias) != tool.ID {
				t.Errorf("the manifest aliases %s to %s, the runtime routes it to %s", alias, tool.ID, CanonicalToolName(alias))
			}
		}
	}
	for _, name := range append([]string{"bash"}, LegacyToolNames("bash")...) {
		if !compactpkg.IsShellTool(name) {
			t.Errorf("compact does not count %s as the shell tool", name)
		}
	}
}

// canonicalNames is the set of tools some retired name routes to.
func canonicalNames() map[string]bool {
	out := map[string]bool{}
	for _, lt := range legacyTools {
		out[lt.canonical] = true
	}
	return out
}
