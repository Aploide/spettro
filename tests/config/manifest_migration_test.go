package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/config"
)

func TestLoadAgentManifestForProject_AutoMigratesV1AndCreatesBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.AgentManifestFilename)
	raw := `
version = 1
default_agent = "plan"

[runtime]
default_permission = "ask-first"
default_timeout_sec = 60
allow_network_tools = false
log_tool_calls = true

[[tools]]
id = "repo-search"
name = "Repository Search"
kind = "builtin"
enabled = true
timeout_sec = 30
requires_approval = false
permitted_actions = ["read", "search"]

[[agents]]
id = "plan"
name = "Planning"
mode = "orchestrator"
allowed_tools = ["repo-search"]
permitted_actions = ["read", "search", "plan"]
permission = "ask-first"
max_steps = 10
handoffs = []
enabled = true
`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write v1 manifest: %v", err)
	}

	m, err := config.LoadAgentManifestForProject(dir)
	if err != nil {
		t.Fatalf("LoadAgentManifestForProject: %v", err)
	}
	if m.Version != 7 {
		t.Fatalf("expected migrated version 7, got %d", m.Version)
	}
	rewritten, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rewritten manifest: %v", err)
	}
	if !strings.Contains(string(rewritten), "version = 7") {
		t.Fatalf("expected rewritten manifest version=7, got:\n%s", string(rewritten))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	foundBackup := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), config.AgentManifestFilename+".migrated-") && strings.HasSuffix(e.Name(), ".bak") {
			foundBackup = true
			break
		}
	}
	if !foundBackup {
		t.Fatal("expected migration backup file")
	}
}
