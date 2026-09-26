package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// decodeV12 migrates a manifest and fails the test unless the result
// validates: TUI and headless runs ignore a manifest load error and fall back
// to an empty manifest, so a migration that breaks Validate fails silently.
func decodeV12(t *testing.T, src string) AgentManifest {
	t.Helper()
	m, _, _, err := DecodeAgentManifestWithMigrationInfo(strings.NewReader(src))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.Version != 12 {
		t.Fatalf("version = %d, want 12", m.Version)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("migrated manifest must validate: %v", err)
	}
	return m
}

func toolByID(t *testing.T, m AgentManifest, id string) ToolSpec {
	t.Helper()
	for _, tool := range m.Tools {
		if tool.ID == id {
			return tool
		}
	}
	t.Fatalf("tool %q missing", id)
	return ToolSpec{}
}

func agentTools(t *testing.T, m AgentManifest, id string) []string {
	t.Helper()
	a, ok := m.AgentByID(id)
	if !ok {
		t.Fatalf("agent %q missing", id)
	}
	return a.AllowedTools
}

func hasTool(m AgentManifest, id string) bool {
	return slices.ContainsFunc(m.Tools, func(t ToolSpec) bool { return t.ID == id })
}

// The comment above DefaultAgentManifest promises that fresh defaults and
// migrated manifests are identical. testdata/default_manifest_v11.toml is the
// default manifest exactly as the last v11 build wrote it (grok tools and all
// the duplicates included); migrating it must give today's default.
func TestV12MigrationOfV11DefaultMatchesDefault(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "default_manifest_v11.toml"))
	if err != nil {
		t.Fatal(err)
	}
	migrated := decodeV12(t, string(raw))
	got, err := toml.Marshal(migrated)
	if err != nil {
		t.Fatal(err)
	}
	want, err := toml.Marshal(DefaultAgentManifest())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		gl, wl := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
		for i := range min(len(gl), len(wl)) {
			if gl[i] != wl[i] {
				t.Fatalf("migrated v11 default differs from DefaultAgentManifest at line %d:\n got %s\nwant %s", i+1, gl[i], wl[i])
			}
		}
		t.Fatalf("migrated v11 default has %d lines, DefaultAgentManifest %d", len(gl), len(wl))
	}
}

const v11MergeManifest = `
version = 11
default_agent = "coder"

[runtime]
default_permission = "ask-first"
default_timeout_sec = 60

[[runtime.permission_rules]]
permission = "tool"
pattern = "shell-exec"
action = "deny"

[[runtime.permission_rules]]
permission = "execute"
pattern = "shell-exec"
action = "ask"

[[tools]]
id = "bash"
name = "My Bash"
description = "Execute a bash command and return output."
kind = "builtin"
enabled = false
timeout_sec = 60
requires_approval = false
permitted_actions = ["execute"]
aliases = ["bash-output"]
risk_level = "low"

[[tools]]
id = "shell-exec"
name = "Shell"
kind = "builtin"
enabled = true
timeout_sec = 300
requires_approval = true
permitted_actions = ["execute", "git"]
risk_level = "high"

[[tools.permission_rules]]
permission = "execute"
pattern = "rm *"
action = "deny"

[[tools]]
id = "file-read"
name = "File Reader"
kind = "builtin"
enabled = true
timeout_sec = 5
permitted_actions = ["read"]

[[tools]]
id = "task-create"
name = "Task Create"
kind = "builtin"
enabled = true
timeout_sec = 10
permitted_actions = ["write"]

[[tools]]
id = "task-list"
name = "Task List"
kind = "builtin"
enabled = true
timeout_sec = 10
permitted_actions = ["read"]

[[tools]]
id = "grok-image"
name = "Grok Image"
kind = "builtin"
enabled = true
timeout_sec = 120
permitted_actions = ["write", "network"]

[[tools]]
id = "ls"
name = "My ls"
kind = "script"
entry_point = "./scripts/ls.sh"
enabled = true
timeout_sec = 10
permitted_actions = ["read"]

[[agents]]
id = "coder"
name = "Coder"
mode = "orchestrator"
allowed_tools = ["file-read", "shell-exec", "bash", "task-create", "task-list", "grok-image", "ls"]
permitted_actions = ["read", "write", "execute"]
permission = "ask-first"
enabled = true

[[agents.permission_rules]]
permission = "*"
pattern = "shell-exec"
action = "ask"

[[agents]]
id = "reader"
name = "Reader"
mode = "worker"
allowed_tools = ["file-read", "task-list"]
permitted_actions = ["read"]
permission = "ask-first"
enabled = true

[[agents]]
id = "artist"
name = "Artist"
mode = "worker"
allowed_tools = ["grok-image"]
permission = "ask-first"
enabled = true
`

// A retired tool's settings merge into the canonical tool toward what the
// operator already chose: approval if either asked for it, the longer
// timeout, the higher risk, enabled if either was, both rule sets. The
// canonical tool's own permitted_actions stay as they were.
func TestV12MergesRetiredToolIntoCanonical(t *testing.T) {
	m := decodeV12(t, v11MergeManifest)
	if hasTool(m, "shell-exec") {
		t.Fatal("shell-exec definition should be folded into bash")
	}
	bash := toolByID(t, m, "bash")
	if !bash.RequiresApproval || bash.TimeoutSec != 300 || bash.RiskLevel != "high" || !bash.Enabled {
		t.Fatalf("merged bash = approval %v, timeout %d, risk %s, enabled %v", bash.RequiresApproval, bash.TimeoutSec, bash.RiskLevel, bash.Enabled)
	}
	if !slices.Equal(bash.PermittedActions, []string{"execute"}) {
		t.Fatalf("permitted_actions must not be merged, got %v", bash.PermittedActions)
	}
	if len(bash.PermissionRules) != 1 || bash.PermissionRules[0].Pattern != "rm *" {
		t.Fatalf("shell-exec's rules should carry over, got %v", bash.PermissionRules)
	}
	if !slices.Equal(bash.Aliases, []string{"bash-output", "shell-exec"}) {
		t.Fatalf("aliases = %v", bash.Aliases)
	}
	if bash.Name != "My Bash" {
		t.Fatalf("operator's name replaced: %q", bash.Name)
	}
	if bash.Description == "Execute a bash command and return output." {
		t.Fatal("the stock v11 description should be refreshed")
	}
}

// With no canonical definition, the retired tool becomes it in place: new
// ID, name and description, the operator's settings kept.
func TestV12RenamesRetiredToolInPlace(t *testing.T) {
	m := decodeV12(t, v11MergeManifest)
	todo := toolByID(t, m, "todo-write")
	if todo.Name != "Todo Write" || todo.TimeoutSec != 10 {
		t.Fatalf("renamed todo-write = %+v", todo)
	}
	if !slices.Equal(todo.PermittedActions, []string{"write"}) {
		t.Fatalf("todo-write must keep task-create's write action only, got %v", todo.PermittedActions)
	}
	for _, id := range []string{"task-create", "task-list"} {
		if hasTool(m, id) {
			t.Fatalf("%s should be folded", id)
		}
		if !slices.Contains(todo.Aliases, id) {
			t.Fatalf("todo-write aliases %v lack %s", todo.Aliases, id)
		}
	}
}

// Allow-lists name the canonical tools, in order and without duplicates,
// and nobody gains access: an agent that could only read tasks loses the
// grant instead of gaining todo-write, and an agent left with nothing is
// disabled rather than invalid.
func TestV12RewritesAllowListsWithoutWideningAccess(t *testing.T) {
	m := decodeV12(t, v11MergeManifest)
	if got := agentTools(t, m, "coder"); !slices.Equal(got, []string{"file-read", "bash", "todo-write", "ls"}) {
		t.Fatalf("coder tools = %v", got)
	}
	if got := agentTools(t, m, "reader"); !slices.Equal(got, []string{"file-read"}) {
		t.Fatalf("read-only task access must not become todo-write: %v", got)
	}
	artist, _ := m.AgentByID("artist")
	if !slices.Equal(artist.AllowedTools, []string{"comment"}) || artist.Enabled {
		t.Fatalf("agent left without tools = %v enabled=%v, want [comment] disabled", artist.AllowedTools, artist.Enabled)
	}
	if hasTool(m, "grok-image") {
		t.Fatal("grok-image should be removed")
	}
}

// A user's own tool that happens to carry a retired name is theirs: not
// folded, not aliased.
func TestV12LeavesCustomToolsAlone(t *testing.T) {
	m := decodeV12(t, v11MergeManifest)
	ls := toolByID(t, m, "ls")
	if ls.Kind != "script" || ls.Name != "My ls" {
		t.Fatalf("custom ls changed: %+v", ls)
	}
	if hasTool(m, "glob") {
		t.Fatal("no glob existed, and a custom ls must not become one")
	}

	// With a built-in glob present, glob must not claim the custom tool's
	// name as an alias either: Validate would reject the clash and the
	// manifest would stop loading.
	src := v11MergeManifest + `
[[tools]]
id = "glob"
name = "Glob"
kind = "builtin"
enabled = true
timeout_sec = 30
permitted_actions = ["read", "search"]
`
	m = decodeV12(t, src)
	if glob := toolByID(t, m, "glob"); slices.Contains(glob.Aliases, "ls") {
		t.Fatalf("glob claimed the custom tool's name: %v", glob.Aliases)
	}
}

// Tool-level rules follow the rename, so a deny on shell-exec keeps denying
// the tool; command rules (execute) are patterns over commands and stay.
func TestV12RewritesToolRules(t *testing.T) {
	m := decodeV12(t, v11MergeManifest)
	rt := m.Runtime.PermissionRules
	if rt[0].Pattern != "bash" || rt[0].Action != RuleDeny {
		t.Fatalf("runtime tool rule = %+v, want deny on bash", rt[0])
	}
	if rt[1].Pattern != "shell-exec" {
		t.Fatalf("execute rule rewritten: %+v", rt[1])
	}
	coder, _ := m.AgentByID("coder")
	if coder.PermissionRules[0].Pattern != "bash" {
		t.Fatalf("agent * rule = %+v, want pattern bash", coder.PermissionRules[0])
	}
}

func TestV12MigrationIsIdempotent(t *testing.T) {
	m := decodeV12(t, v11MergeManifest)
	raw, err := toml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	again, _, changed, err := DecodeAgentManifestWithMigrationInfo(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("a v12 manifest must load unchanged")
	}
	raw2, _ := toml.Marshal(again)
	if string(raw) != string(raw2) {
		t.Fatal("reloading a migrated manifest changed it")
	}
}

// Loading a v11 project manifest rewrites it at v12 and keeps the original
// as a .bak.
func TestV12MigrationWritesBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, AgentManifestFilename)
	if err := os.WriteFile(path, []byte(v11MergeManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAgentManifestForProject(dir); err != nil {
		t.Fatal(err)
	}
	backups, _ := filepath.Glob(path + ".migrated-*.bak")
	if len(backups) != 1 {
		t.Fatalf("expected one backup, got %v", backups)
	}
	if old, _ := os.ReadFile(backups[0]); string(old) != v11MergeManifest {
		t.Fatal("backup must hold the original manifest")
	}
	rewritten, _ := os.ReadFile(path)
	if !strings.Contains(string(rewritten), "version = 12") || strings.Contains(string(rewritten), "id = 'shell-exec'") {
		t.Fatalf("manifest not rewritten at v12:\n%s", rewritten)
	}
}

// A manifest older than v11 still gets a working general-purpose agent: the
// v11 step grants the retired names the manifest holds and v12 folds them.
func TestV12GeneralPurposeFromPreV11Manifest(t *testing.T) {
	src := strings.Replace(v11MergeManifest, "version = 11", "version = 10", 1)
	m := decodeV12(t, src)
	gp, ok := m.AgentByID("general-purpose")
	if !ok {
		t.Fatal("general-purpose agent not added")
	}
	if !slices.Contains(gp.AllowedTools, "bash") || !slices.Contains(gp.AllowedTools, "todo-write") {
		t.Fatalf("general-purpose tools = %v, want bash and todo-write", gp.AllowedTools)
	}
	if slices.Contains(gp.AllowedTools, "ls") {
		t.Fatalf("general-purpose must not pick up the custom ls: %v", gp.AllowedTools)
	}
}

func TestValidateRejectsAliasClashes(t *testing.T) {
	base := DefaultAgentManifest()

	m := base
	m.Tools = slices.Clone(base.Tools)
	i := slices.IndexFunc(m.Tools, func(t ToolSpec) bool { return t.ID == "glob" })
	m.Tools[i].Aliases = []string{"grep"}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "another tool's id") {
		t.Fatalf("alias equal to a tool id: %v", err)
	}

	m.Tools = slices.Clone(base.Tools)
	m.Tools[i].Aliases = []string{"shell-exec"}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "claimed by both") {
		t.Fatalf("alias claimed twice: %v", err)
	}
}
