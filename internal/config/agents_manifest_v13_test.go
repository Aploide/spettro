package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// loadV12Default returns the v12 default manifest as the last v12 build wrote
// it, unmigrated, for a test to edit before migrating it.
func loadV12Default(t *testing.T) AgentManifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "default_manifest_v12.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var m AgentManifest
	if err := toml.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Version != 12 {
		t.Fatalf("fixture version = %d", m.Version)
	}
	return m
}

// lspOpsUsable is the set of language-server operations, by the name of the
// tool that did each before v13, an agent can call: before the fold, the
// retired tools it can call; after it, those the lsp tool's rules leave it
// (the runtime applies a rule naming a retired tool to its op, see
// agent.lspOpDenied).
func lspOpsUsable(m AgentManifest, agentID string) []string {
	a, ok := m.AgentByID(agentID)
	if !ok {
		return nil
	}
	var out []string
	for _, id := range a.AllowedTools {
		t, ok := m.toolNamed(id)
		if !ok || !m.ToolUsableBy(a, t) {
			continue
		}
		if slices.Contains(lspConsolidatedTools[0].retired, id) && t.ID == id {
			out = append(out, id)
			continue
		}
		if t.ID != "lsp" || t.Kind != "builtin" {
			continue
		}
		for _, op := range lspConsolidatedTools[0].retired {
			asOp := t
			asOp.ID = op
			if m.ToolAllowedByRules(a, asOp) && !slices.Contains(out, op) {
				out = append(out, op)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// assertNoAddedOps checks, for every agent, that the language-server
// operations it can call after v13 are ones it could call before.
func assertNoAddedOps(t *testing.T, before, after AgentManifest) {
	t.Helper()
	for _, a := range before.Agents {
		had := lspOpsUsable(before, a.ID)
		for _, op := range lspOpsUsable(after, a.ID) {
			if !slices.Contains(had, op) {
				t.Errorf("agent %s gained the %s op (before: %v, after: %v)", a.ID, op, had, lspOpsUsable(after, a.ID))
			}
		}
	}
}

func assertSameManifest(t *testing.T, got, want AgentManifest) {
	t.Helper()
	g, err := toml.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	w, err := toml.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(g) == string(w) {
		return
	}
	gl, wl := strings.Split(string(g), "\n"), strings.Split(string(w), "\n")
	for i := range min(len(gl), len(wl)) {
		if gl[i] != wl[i] {
			t.Fatalf("manifests differ at line %d:\n got %s\nwant %s", i+1, gl[i], wl[i])
		}
	}
	t.Fatalf("manifests differ in length: %d lines, want %d", len(gl), len(wl))
}

// testdata/default_manifest_v12.toml is the default manifest exactly as the
// last v12 build wrote it; migrating it must give today's default.
func TestV13MigrationOfV12DefaultMatchesDefault(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "default_manifest_v12.toml"))
	if err != nil {
		t.Fatal(err)
	}
	assertSameManifest(t, decodeV12(t, string(raw)), DefaultAgentManifest())
}

// With the stock default every agent that held the language-server tools
// held all four, so each keeps exactly what it could call, lsp for the four,
// and needs no rule to hold it to that.
func TestV13DefaultKeepsEveryAgentsAccess(t *testing.T) {
	before := loadV12Default(t)
	after := migrateEdited(t, before)
	assertNoAddedAccess(t, before, after)
	assertNoAddedOps(t, before, after)
	for _, a := range before.Agents {
		var want []string
		for _, id := range usableTools(before, a.ID) {
			if canon, ok := canonicalOf(id); ok {
				id = canon
			}
			if !slices.Contains(want, id) {
				want = append(want, id)
			}
		}
		if got := usableTools(after, a.ID); !slices.Equal(got, want) {
			t.Errorf("agent %s: after %v, want %v", a.ID, got, want)
		}
		if got := lspOpsUsable(after, a.ID); !slices.Equal(got, lspOpsUsable(before, a.ID)) {
			t.Errorf("agent %s: ops after %v, before %v", a.ID, got, lspOpsUsable(before, a.ID))
		}
		b, _ := after.AgentByID(a.ID)
		if !slices.Equal(b.PermissionRules, a.PermissionRules) {
			t.Errorf("agent %s: rules changed to %v", a.ID, b.PermissionRules)
		}
	}
	for _, id := range []string{"diagnostics", "references", "hover", "lsp-restart"} {
		if hasTool(after, id) {
			t.Errorf("%s should be folded into lsp", id)
		}
	}
	if a, _ := after.AgentByID("coding"); !slices.Contains(a.AllowedTools, "lsp") || !slices.Contains(a.AllowedTools, "rename-symbol") {
		t.Errorf("coding tools = %v, want lsp and rename-symbol", a.AllowedTools)
	}
}

// An agent that held only some of the language-server tools gets lsp with a
// rule denying each op it did not have: lsp-restart alone does not become
// diagnostics, references and hover too.
func TestV13GrantsOnlyTheOpsAnAgentHad(t *testing.T) {
	before := loadV12Default(t)
	before.Agents = append(before.Agents,
		AgentSpec{ID: "restarter", Name: "R", Mode: "worker", Permission: "ask-first", Enabled: true, AllowedTools: []string{"file-read", "lsp-restart"}},
		AgentSpec{ID: "looker", Name: "L", Mode: "worker", Permission: "ask-first", Enabled: true, AllowedTools: []string{"file-read", "references", "hover"}},
		AgentSpec{ID: "reader", Name: "Rd", Mode: "worker", Permission: "ask-first", Enabled: true, AllowedTools: []string{"file-read"}},
	)
	after := migrateEdited(t, before)
	assertNoAddedAccess(t, before, after)
	assertNoAddedOps(t, before, after)

	if got := agentTools(t, after, "restarter"); !slices.Equal(got, []string{"file-read", "lsp"}) {
		t.Fatalf("restarter tools = %v", got)
	}
	if got := lspOpsUsable(after, "restarter"); !slices.Equal(got, []string{"lsp-restart"}) {
		t.Fatalf("restarter ops = %v, want only lsp-restart", got)
	}
	if got := lspOpsUsable(after, "looker"); !slices.Equal(got, []string{"hover", "references"}) {
		t.Fatalf("looker ops = %v, want hover and references", got)
	}
	if got := agentTools(t, after, "reader"); !slices.Equal(got, []string{"file-read"}) {
		t.Fatalf("an agent with no language-server tool must not gain lsp: %v", got)
	}
	looker, _ := after.AgentByID("looker")
	want := []PermissionRule{
		{Permission: "tool", Pattern: "diagnostics", Action: RuleDeny},
		{Permission: "tool", Pattern: "lsp-restart", Action: RuleDeny},
	}
	if !slices.Equal(looker.PermissionRules, want) {
		t.Fatalf("looker rules = %v, want %v", looker.PermissionRules, want)
	}
}

// A language-server tool the agent could not call (disabled, or denied by a
// rule) grants nothing, and its op stays out of reach of the agents that get
// lsp through the others.
func TestV13DeniedOrDisabledToolGrantsNothing(t *testing.T) {
	before := loadV12Default(t)
	setEnabled(t, &before, "diagnostics", false)
	before.Runtime.PermissionRules = append(before.Runtime.PermissionRules,
		PermissionRule{Permission: "tool", Pattern: "lsp-restart", Action: RuleDeny},
	)
	before.Agents = append(before.Agents,
		AgentSpec{ID: "diag", Name: "D", Mode: "worker", Permission: "ask-first", Enabled: true, AllowedTools: []string{"file-read", "diagnostics", "lsp-restart"}},
	)
	after := migrateEdited(t, before)
	assertNoAddedAccess(t, before, after)
	assertNoAddedOps(t, before, after)

	lsp := toolByID(t, after, "lsp")
	if !lsp.Enabled {
		t.Fatal("lsp should be made from an enabled tool, not the disabled diagnostics")
	}
	if got := agentTools(t, after, "diag"); !slices.Equal(got, []string{"file-read"}) {
		t.Fatalf("diag could call no language-server tool, so it gains nothing: %v", got)
	}
	if got := lspOpsUsable(after, "coding"); !slices.Equal(got, []string{"hover", "references"}) {
		t.Fatalf("coding ops = %v, want hover and references", got)
	}
	// The runtime rule is kept as written.
	if !slices.Contains(after.Runtime.PermissionRules, PermissionRule{Permission: "tool", Pattern: "lsp-restart", Action: RuleDeny}) {
		t.Fatalf("runtime rules rewritten: %v", after.Runtime.PermissionRules)
	}
}

const v12LSPManifest = `
version = 12
default_agent = "coder"

[runtime]
default_permission = "ask-first"
default_timeout_sec = 60

[[tools]]
id = "file-read"
name = "File Reader"
kind = "builtin"
enabled = true
timeout_sec = 5
permitted_actions = ["read"]

[[tools]]
id = "diagnostics"
name = "My Diagnostics"
description = "Fetch language-server diagnostics for a file or the workspace."
kind = "builtin"
enabled = true
timeout_sec = 20
permitted_actions = ["read", "search"]
risk_level = "low"

[[tools]]
id = "lsp-restart"
name = "LSP Restart"
kind = "builtin"
enabled = true
timeout_sec = 90
requires_approval = true
permitted_actions = ["read"]
risk_level = "medium"

[[tools.permission_rules]]
permission = "tool"
pattern = "*"
action = "ask"

[[tools]]
id = "hover"
name = "My hover"
kind = "script"
entry_point = "./scripts/hover.sh"
enabled = true
timeout_sec = 10
permitted_actions = ["read"]

[[agents]]
id = "coder"
name = "Coder"
mode = "orchestrator"
allowed_tools = ["file-read", "diagnostics", "lsp-restart", "hover"]
permitted_actions = ["read", "search"]
permission = "ask-first"
enabled = true
`

// With no lsp definition, diagnostics becomes it in place (the operator's
// settings kept, the name and description the tool's own) and the others
// merge into it toward the stricter side. A user's own tool that shares a
// retired name is theirs: not folded, not aliased, and not denied.
func TestV13FoldsIntoLSPAndLeavesCustomToolsAlone(t *testing.T) {
	m := decodeV12(t, v12LSPManifest)
	lsp := toolByID(t, m, "lsp")
	if lsp.Name != "LSP" || lsp.Description == "Fetch language-server diagnostics for a file or the workspace." {
		t.Fatalf("lsp name/description = %q / %q", lsp.Name, lsp.Description)
	}
	if lsp.TimeoutSec != 90 || !lsp.RequiresApproval || lsp.RiskLevel != "medium" {
		t.Fatalf("merged lsp = timeout %d, approval %v, risk %s", lsp.TimeoutSec, lsp.RequiresApproval, lsp.RiskLevel)
	}
	if !slices.Equal(lsp.PermittedActions, []string{"read", "search"}) {
		t.Fatalf("lsp permitted_actions = %v, want diagnostics' own", lsp.PermittedActions)
	}
	if len(lsp.PermissionRules) != 1 || lsp.PermissionRules[0].Action != RuleAsk {
		t.Fatalf("lsp-restart's ask rule should carry over: %v", lsp.PermissionRules)
	}
	if !slices.Equal(lsp.Aliases, []string{"diagnostics", "references", "lsp-restart"}) {
		t.Fatalf("lsp aliases = %v (hover belongs to the custom tool)", lsp.Aliases)
	}
	if h := toolByID(t, m, "hover"); h.Kind != "script" {
		t.Fatalf("custom hover changed: %+v", h)
	}
	if got := agentTools(t, m, "coder"); !slices.Equal(got, []string{"file-read", "lsp", "hover"}) {
		t.Fatalf("coder tools = %v", got)
	}
	coder, _ := m.AgentByID("coder")
	want := []PermissionRule{{Permission: "tool", Pattern: "references", Action: RuleDeny}}
	if !slices.Equal(coder.PermissionRules, want) {
		t.Fatalf("coder rules = %v, want only references denied (not the custom hover)", coder.PermissionRules)
	}
}

func TestV13MigrationIsIdempotent(t *testing.T) {
	m := decodeV12(t, v12LSPManifest)
	raw, err := toml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	again, _, changed, err := DecodeAgentManifestWithMigrationInfo(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("a v13 manifest must load unchanged")
	}
	raw2, _ := toml.Marshal(again)
	if string(raw) != string(raw2) {
		t.Fatal("reloading a migrated manifest changed it")
	}
}

// Loading a v12 project manifest rewrites it at v13 and keeps the original as
// a .bak.
func TestV13MigrationWritesBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, AgentManifestFilename)
	if err := os.WriteFile(path, []byte(v12LSPManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAgentManifestForProject(dir); err != nil {
		t.Fatal(err)
	}
	backups, _ := filepath.Glob(path + ".migrated-*.bak")
	if len(backups) != 1 {
		t.Fatalf("expected one backup, got %v", backups)
	}
	if old, _ := os.ReadFile(backups[0]); string(old) != v12LSPManifest {
		t.Fatal("backup must hold the original manifest")
	}
	rewritten, _ := os.ReadFile(path)
	if !strings.Contains(string(rewritten), "version = 13") || strings.Contains(string(rewritten), "id = 'diagnostics'") {
		t.Fatalf("manifest not rewritten at v13:\n%s", rewritten)
	}
}
