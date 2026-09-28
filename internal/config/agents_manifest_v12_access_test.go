package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// loadV11Default returns the v11 default manifest as written, unmigrated, for
// a test to edit before migrating it.
func loadV11Default(t *testing.T) AgentManifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "default_manifest_v11.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var m AgentManifest
	if err := toml.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Version != 11 {
		t.Fatalf("fixture version = %d", m.Version)
	}
	return m
}

// migrateEdited migrates an edited v11 manifest.
func migrateEdited(t *testing.T, m AgentManifest) AgentManifest {
	t.Helper()
	raw, err := toml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return decodeV12(t, string(raw))
}

// usableTools is what an agent can actually call: its allow-list entries the
// manifest resolves (by ID or alias) to a tool it may use.
func usableTools(m AgentManifest, agentID string) []string {
	a, ok := m.AgentByID(agentID)
	if !ok {
		return nil
	}
	var out []string
	for _, id := range a.AllowedTools {
		if t, ok := m.toolNamed(id); ok && m.ToolUsableBy(a, t) {
			out = append(out, id)
		}
	}
	return out
}

// assertNoAddedAccess checks, for every agent, that what it can call after
// the migration is covered by what it could call before, retired names read
// as their canonical tools.
func assertNoAddedAccess(t *testing.T, before, after AgentManifest) {
	t.Helper()
	for _, a := range before.Agents {
		had := map[string]bool{}
		for _, id := range usableTools(before, a.ID) {
			had[id] = true
			if canon, ok := canonicalOf(id); ok {
				had[canon] = true
			}
		}
		for _, id := range usableTools(after, a.ID) {
			if !had[id] {
				t.Errorf("agent %s gained %s (before: %v, after: %v)", a.ID, id, usableTools(before, a.ID), usableTools(after, a.ID))
			}
		}
	}
}

func setEnabled(t *testing.T, m *AgentManifest, id string, on bool) {
	t.Helper()
	i := m.toolIndex(id)
	if i < 0 {
		t.Fatalf("tool %q missing", id)
	}
	m.Tools[i].Enabled = on
}

func assertUsable(t *testing.T, m AgentManifest, agentID string, want, notWant []string) {
	t.Helper()
	got := usableTools(m, agentID)
	for _, id := range want {
		if !slices.Contains(got, id) {
			t.Errorf("agent %s cannot call %s; can call %v", agentID, id, got)
		}
	}
	for _, id := range notWant {
		if slices.Contains(got, id) {
			t.Errorf("agent %s can call %s; can call %v", agentID, id, got)
		}
	}
}

// With the stock default nothing is denied or disabled, so every agent keeps
// exactly what it could call, under canonical names, less the grok tools.
func TestV12DefaultKeepsEveryAgentsAccess(t *testing.T) {
	before := loadV11Default(t)
	after := migrateEdited(t, before)
	assertNoAddedAccess(t, before, after)
	for _, a := range before.Agents {
		var want []string
		for _, id := range usableTools(before, a.ID) {
			if canon, ok := canonicalOf(id); ok {
				id = canon
			}
			if !slices.Contains(want, id) && !slices.Contains(removedTools, id) {
				want = append(want, id)
			}
		}
		if got := usableTools(after, a.ID); !slices.Equal(got, want) {
			t.Errorf("agent %s: after %v, want %v", a.ID, got, want)
		}
	}
}

// A rule that denied only a retired tool in v11 left its canonical tool
// alone; after v12 it still does.
func TestV12DenyOnRetiredToolKeepsCanonical(t *testing.T) {
	before := loadV11Default(t)
	before.Runtime.PermissionRules = append(before.Runtime.PermissionRules,
		PermissionRule{Permission: "tool", Pattern: "repo-search", Action: RuleDeny},
		PermissionRule{Permission: "tool", Pattern: "ls", Action: RuleDeny},
		PermissionRule{Permission: "tool", Pattern: "task-get", Action: RuleDeny},
	)
	// A tool-scoped rule that switched multi-edit off.
	i := before.toolIndex("multi-edit")
	before.Tools[i].PermissionRules = append(before.Tools[i].PermissionRules,
		PermissionRule{Permission: "tool", Pattern: "*", Action: RuleDeny},
		PermissionRule{Permission: "edit", Pattern: "secrets/*", Action: RuleDeny},
		PermissionRule{Permission: "edit", Pattern: "docs/*", Action: RuleAllow},
	)
	after := migrateEdited(t, before)
	assertNoAddedAccess(t, before, after)
	assertUsable(t, after, "coding", []string{"grep", "glob", "file-edit", "todo-write"}, nil)

	fe := toolByID(t, after, "file-edit")
	if !slices.Contains(fe.PermissionRules, PermissionRule{Permission: "edit", Pattern: "secrets/*", Action: RuleDeny}) {
		t.Errorf("multi-edit's path deny should carry over: %v", fe.PermissionRules)
	}
	for _, r := range fe.PermissionRules {
		if r.Action == RuleAllow || r.Pattern == "*" {
			t.Errorf("rule %+v would loosen or switch off file-edit", r)
		}
	}
}

// A canonical tool the operator switched off stays off, even when its
// retired twin was on: the flag is per tool, so turning it back on would
// hand it to every agent that listed it.
func TestV12DisabledCanonicalStaysOff(t *testing.T) {
	before := loadV11Default(t)
	setEnabled(t, &before, "grep", false)
	setEnabled(t, &before, "glob", false)
	setEnabled(t, &before, "bash", false)
	setEnabled(t, &before, "todo-write", false)
	before.Agents = append(before.Agents, AgentSpec{
		ID: "b", Name: "B", Mode: "worker", Permission: "ask-first", Enabled: true,
		AllowedTools: []string{"file-read", "glob", "bash", "todo-write"},
	})
	after := migrateEdited(t, before)
	assertNoAddedAccess(t, before, after)
	for _, id := range []string{"grep", "glob", "bash", "todo-write"} {
		if toolByID(t, after, id).Enabled {
			t.Errorf("%s was switched back on", id)
		}
	}
	assertUsable(t, after, "b", []string{"file-read"}, []string{"glob", "bash", "todo-write"})
}

// A retired tool the agent could not call (denied by any rule, including an
// execute rule on its ID or a glob) grants nothing after the fold.
func TestV12DeniedRetiredToolGrantsNothing(t *testing.T) {
	before := loadV11Default(t)
	before.Runtime.PermissionRules = append(before.Runtime.PermissionRules,
		PermissionRule{Permission: "execute", Pattern: "shell-exec", Action: RuleDeny},
		PermissionRule{Permission: "tool", Pattern: "task-*", Action: RuleDeny},
	)
	setEnabled(t, &before, "multi-edit", false)
	before.Agents = append(before.Agents, AgentSpec{
		ID: "a", Name: "A", Mode: "worker", Permission: "ask-first", Enabled: true,
		AllowedTools: []string{"file-read", "shell-exec", "task-create", "task-list", "multi-edit"},
	})
	after := migrateEdited(t, before)
	assertNoAddedAccess(t, before, after)
	if got := agentTools(t, after, "a"); !slices.Equal(got, []string{"file-read"}) {
		t.Fatalf("agent a tools = %v, want [file-read]", got)
	}
}

// With no todo-write definition, the retired tool turned into it is an
// enabled one, so a disabled task-create does not switch off the task list
// that task-update gave agents.
func TestV12RenameInPlacePrefersEnabledTool(t *testing.T) {
	before := loadV11Default(t)
	before.Tools = slices.DeleteFunc(before.Tools, func(t ToolSpec) bool { return t.ID == "todo-write" })
	setEnabled(t, &before, "task-create", false)
	before.Agents = append(before.Agents,
		AgentSpec{ID: "updater", Name: "U", Mode: "worker", Permission: "ask-first", Enabled: true, AllowedTools: []string{"file-read", "task-update"}},
		AgentSpec{ID: "creator", Name: "C", Mode: "worker", Permission: "ask-first", Enabled: true, AllowedTools: []string{"file-read", "task-create"}},
	)
	after := migrateEdited(t, before)
	assertNoAddedAccess(t, before, after)
	if !toolByID(t, after, "todo-write").Enabled {
		t.Fatal("todo-write was built from the disabled task-create")
	}
	assertUsable(t, after, "updater", []string{"todo-write"}, nil)
	if got := agentTools(t, after, "creator"); !slices.Equal(got, []string{"file-read"}) {
		t.Fatalf("creator could not call task-create, so it gains nothing: %v", got)
	}
}
