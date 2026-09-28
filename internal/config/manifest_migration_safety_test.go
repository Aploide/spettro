package config

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

// A retired built-in renamed into its missing canonical tool must not keep
// the canonical name among its aliases: Validate rejects a tool whose alias
// is a tool's ID, so the migrated manifest would stop spettro from starting.
// Scenario A: skill-read taught the name "skill" (as Claude Code and Codex
// models call it).
func TestRenameInPlaceDropsTheCanonicalAlias(t *testing.T) {
	src := strings.Replace(v13SkillManifest, `aliases = ["activate-skill", "skill-activate"]`, `aliases = ["activate-skill", "skill-activate", "skill"]`, 1)
	m := decodeV12(t, src)
	skill := toolByID(t, m, "skill")
	if slices.Contains(skill.Aliases, "skill") {
		t.Fatalf("skill aliases itself: %v", skill.Aliases)
	}
}

// Scenario B: an old manifest with shell-exec and no bash, where the
// operator gave shell-exec the alias "bash".
func TestRenameInPlaceDropsTheCanonicalAliasOfShell(t *testing.T) {
	src := `
version = 11
default_agent = "coder"

[runtime]
default_permission = "ask-first"
default_timeout_sec = 60

[[tools]]
id = "file-read"
name = "File Reader"
kind = "builtin"
enabled = true
timeout_sec = 30
permitted_actions = ["read"]

[[tools]]
id = "shell-exec"
name = "Shell"
kind = "builtin"
enabled = true
timeout_sec = 30
permitted_actions = ["execute"]
aliases = ["bash"]

[[agents]]
id = "coder"
name = "Coder"
mode = "orchestrator"
allowed_tools = ["file-read", "shell-exec"]
permitted_actions = ["read", "execute"]
permission = "ask-first"
enabled = true
`
	m := decodeV12(t, src)
	bash := toolByID(t, m, "bash")
	if slices.Contains(bash.Aliases, "bash") {
		t.Fatalf("bash aliases itself: %v", bash.Aliases)
	}
	if got := agentTools(t, m, "coder"); !slices.Contains(got, "bash") {
		t.Fatalf("coder lost its shell: %v", got)
	}
}

// oldDefaultManifest writes the v11 default manifest into a fresh project
// directory and returns the directory.
func oldDefaultManifest(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "default_manifest_v11.toml"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, AgentManifestFilename), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Persisting a migration is a convenience: when the manifest's directory is
// read-only (a read-only checkout, mount or container, or a directory the
// process sandbox keeps it from writing), the migrated manifest is still
// returned instead of an error or an empty manifest.
func TestLoadForProjectSurvivesAReadOnlyDirectory(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not stop writes here")
	}
	dir := oldDefaultManifest(t)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	m, err := LoadAgentManifestForProject(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.Version != latestManifestVersion || len(m.Agents) == 0 {
		t.Fatalf("got version %d with %d agents, want the migrated manifest", m.Version, len(m.Agents))
	}
}

// Concurrent first loads of an old manifest (several ACP sessions opened at
// once, or a TUI and an editor started together after an upgrade) must all
// succeed: the rewrite may not share one temp file between them.
func TestLoadForProjectConcurrentMigrations(t *testing.T) {
	for round := 0; round < 10; round++ {
		dir := oldDefaultManifest(t)
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := LoadAgentManifestForProject(dir); err != nil {
					errs <- err
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("round %d: %v", round, err)
		}
	}
}

// The rewritten manifest and its backup keep the original's permissions
// (manifests can carry tokens in http/mcp entry points), and a manifest that
// is a symlink stays one: the target is migrated in place.
func TestMigrationKeepsModeAndSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks and modes differ on Windows")
	}
	shared := oldDefaultManifest(t)
	target := filepath.Join(shared, AgentManifestFilename)
	project := t.TempDir()
	link := filepath.Join(project, AgentManifestFilename)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAgentManifestForProject(project); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the manifest symlink was replaced by a file (%v)", err)
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("migrated manifest mode = %v, want 0600", fi.Mode().Perm())
	}
	data, err := os.ReadFile(target)
	if err != nil || !strings.Contains(string(data), "version = 14") {
		t.Fatalf("the symlink target was not migrated: %v", err)
	}
	baks, _ := filepath.Glob(target + ".migrated-*.bak")
	if len(baks) != 1 {
		t.Fatalf("backups next to the target = %v", baks)
	}
	if bi, err := os.Stat(baks[0]); err != nil || bi.Mode().Perm() != 0o600 {
		t.Errorf("backup mode = %v (%v), want 0600", bi.Mode().Perm(), err)
	}
}

// v7 manifest with the shell switched off: listing it showed no trust, so
// the v8 retrofit must not hand the agent the pty tools (which run
// arbitrary commands too).
func TestPTYRetrofitNeedsAUsableShell(t *testing.T) {
	for name, edit := range map[string]string{
		"disabled": `enabled = false`,
		"denied":   "enabled = true\n\n[[runtime.permission_rules]]\npermission = \"tool\"\npattern = \"shell-exec\"\naction = \"deny\"",
	} {
		t.Run(name, func(t *testing.T) {
			src := `
version = 7
default_agent = "coder"

[runtime]
default_permission = "ask-first"
default_timeout_sec = 60

[[tools]]
id = "file-read"
name = "File Reader"
kind = "builtin"
enabled = true
timeout_sec = 30
permitted_actions = ["read"]

[[agents]]
id = "coder"
name = "Coder"
mode = "orchestrator"
allowed_tools = ["file-read", "shell-exec"]
permitted_actions = ["read", "execute"]
permission = "ask-first"
enabled = true

[[tools]]
id = "shell-exec"
name = "Shell"
kind = "builtin"
timeout_sec = 30
permitted_actions = ["execute"]
` + edit + "\n"
			m := decodeV12(t, src)
			for _, id := range []string{"pty-start", "pty-write", "pty-kill"} {
				if slices.Contains(usableTools(m, "coder"), id) {
					t.Fatalf("coder gained %s; usable now: %v", id, usableTools(m, "coder"))
				}
			}
		})
	}
}

// v5 manifest where file-edit is switched off: references plus a listed but
// unusable file-edit must not add rename-symbol, a tool that writes files.
func TestRenameSymbolRetrofitNeedsAUsableFileEdit(t *testing.T) {
	src := `
version = 5
default_agent = "coder"

[runtime]
default_permission = "ask-first"
default_timeout_sec = 60

[[tools]]
id = "file-read"
name = "File Reader"
kind = "builtin"
enabled = true
timeout_sec = 30
permitted_actions = ["read"]

[[tools]]
id = "references"
name = "References"
kind = "builtin"
enabled = true
timeout_sec = 30
permitted_actions = ["read", "search"]

[[tools]]
id = "file-edit"
name = "File Edit"
kind = "builtin"
enabled = false
timeout_sec = 30
permitted_actions = ["write"]

[[agents]]
id = "coder"
name = "Coder"
mode = "orchestrator"
allowed_tools = ["file-read", "references", "file-edit"]
permitted_actions = ["read", "search", "write"]
permission = "ask-first"
enabled = true
`
	m := decodeV12(t, src)
	if got := usableTools(m, "coder"); slices.Contains(got, "rename-symbol") {
		t.Fatalf("coder gained rename-symbol: %v", got)
	}
}

// An agent that reached bash only through shell-exec must not inherit the
// canonical bash tool's own allow rules: tool rules are evaluated last, so
// they would let every command run without approval.
func TestV12FoldDoesNotHandOutCanonicalAllowRules(t *testing.T) {
	before := loadV11Default(t)
	bi := before.toolIndex("bash")
	allowAll := PermissionRule{Permission: "execute", Pattern: "*", Action: RuleAllow}
	before.Tools[bi].PermissionRules = append(before.Tools[bi].PermissionRules, allowAll)
	before.Agents = append(before.Agents, AgentSpec{
		ID: "tester2", Name: "T2", Mode: "worker", Permission: "ask-first", Enabled: true,
		AllowedTools: []string{"file-read", "shell-exec"}, PermittedActions: []string{"read", "execute"},
	})
	sh := before.Tools[before.toolIndex("shell-exec")]
	a2, _ := before.AgentByID("tester2")
	if got := EvaluatePermissionRule("execute", "rm -rf build", before.Runtime.PermissionRules, a2.PermissionRules, sh.PermissionRules); got != RuleAsk {
		t.Fatalf("precondition: before the fold rm -rf evaluates to %s", got)
	}
	after := migrateEdited(t, before)
	bash := toolByID(t, after, "bash")
	decide := func(agentID string) RuleAction {
		a, _ := after.AgentByID(agentID)
		return EvaluatePermissionRule("execute", "rm -rf build", after.Runtime.PermissionRules, a.PermissionRules, bash.PermissionRules)
	}
	if got := decide("tester2"); got != RuleAsk {
		t.Errorf("tester2 (shell-exec only before): rm -rf evaluates to %s, want ask", got)
	}
	// An agent that held bash before keeps the operator's allow.
	if got := decide("test"); got != RuleAllow {
		t.Errorf("test (held bash before): rm -rf evaluates to %s, want allow", got)
	}
}

// The v11 retrofit gives the general-purpose handoff only to primaries that
// could already do what general-purpose can: a read-only agent must not be
// able to write files or run commands through a delegation.
func TestGeneralPurposeHandoffDoesNotWidenAReadOnlyPrimary(t *testing.T) {
	src := `
version = 10
default_agent = "ask"

[runtime]
default_permission = "ask-first"
default_timeout_sec = 60

[[tools]]
id = "agent"
name = "Agent"
kind = "builtin"
enabled = true
timeout_sec = 30
permitted_actions = ["read"]

[[tools]]
id = "file-read"
name = "File Reader"
kind = "builtin"
enabled = true
timeout_sec = 30
permitted_actions = ["read"]

[[tools]]
id = "file-write"
name = "File Writer"
kind = "builtin"
enabled = true
timeout_sec = 30
permitted_actions = ["write"]

[[tools]]
id = "bash"
name = "Bash"
kind = "builtin"
enabled = true
timeout_sec = 30
permitted_actions = ["execute"]

[[agents]]
id = "ask"
name = "Ask"
mode = "orchestrator"
role = "primary"
allowed_tools = ["agent", "file-read"]
permitted_actions = ["read", "search"]
permission = "ask-first"
handoffs = ["explore"]
enabled = true

[[agents]]
id = "coding"
name = "Coding"
mode = "orchestrator"
role = "primary"
allowed_tools = ["agent", "file-read", "file-write", "bash"]
permitted_actions = ["read", "search", "write", "execute"]
permission = "ask-first"
handoffs = ["explore"]
enabled = true

[[agents]]
id = "explore"
name = "Explore"
mode = "worker"
allowed_tools = ["file-read"]
permitted_actions = ["read"]
permission = "ask-first"
enabled = true
`
	m := decodeV12(t, src)
	ask, _ := m.AgentByID("ask")
	if slices.Contains(ask.Handoffs, "general-purpose") {
		t.Fatalf("read-only ask gained the general-purpose handoff: %v", ask.Handoffs)
	}
	coding, _ := m.AgentByID("coding")
	if !slices.Contains(coding.Handoffs, "general-purpose") {
		t.Fatalf("coding, which can do everything general-purpose can, did not gain it: %v", coding.Handoffs)
	}
}
