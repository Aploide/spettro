package config

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// sharedBuiltinNames are the built-in names a tool of the operator's own may
// collide with: every canonical tool and retired name the v12 and v13
// migrations fold, plus the skill-read aliases, which no migration folds but
// the agent runtime still routes.
func sharedBuiltinNames() []string {
	names := []string{"skill-read", "activate-skill", "skill-activate"}
	for _, g := range toolFolds() {
		names = append(names, g.canonical)
		names = append(names, g.retired...)
	}
	slices.Sort(names)
	return names
}

// userToolManifest is a v10 manifest (so the v11 general-purpose retrofit and
// the v12 and v13 folds all run) that defines, as built-ins, every shared
// name but taken, plus file-read and comment, and a script tool of the
// operator's own called taken. Agent "coder" holds every tool, the
// operator's included; agent "other" holds every built-in but not the
// operator's tool.
func userToolManifest(taken string) string {
	var b strings.Builder
	b.WriteString("version = 10\ndefault_agent = \"coder\"\n\n[runtime]\ndefault_permission = \"ask-first\"\ndefault_timeout_sec = 60\n")
	builtins := []string{"file-read", "comment"}
	for _, name := range sharedBuiltinNames() {
		if name != taken && !slices.Contains(builtins, name) {
			builtins = append(builtins, name)
		}
	}
	for _, id := range builtins {
		fmt.Fprintf(&b, "\n[[tools]]\nid = %q\nname = %q\nkind = \"builtin\"\nenabled = true\ntimeout_sec = 30\npermitted_actions = [\"read\", \"write\", \"execute\"]\n", id, id)
	}
	fmt.Fprintf(&b, "\n[[tools]]\nid = %q\nname = \"Mine\"\nkind = \"script\"\nentry_point = \"./mine.sh\"\nenabled = true\ntimeout_sec = 30\npermitted_actions = [\"read\"]\n", taken)
	list := func(ids []string) string {
		quoted := make([]string, len(ids))
		for i, id := range ids {
			quoted[i] = fmt.Sprintf("%q", id)
		}
		return "[" + strings.Join(quoted, ", ") + "]"
	}
	agent := func(id string, tools []string) {
		fmt.Fprintf(&b, "\n[[agents]]\nid = %q\nname = %q\nmode = \"coding\"\nrole = \"primary\"\nallowed_tools = %s\npermitted_actions = [\"read\", \"write\", \"execute\"]\npermission = \"ask-first\"\nenabled = true\n", id, id, list(tools))
	}
	agent("coder", append(slices.Clone(builtins), taken))
	agent("other", builtins)
	return b.String()
}

// A tool of the operator's own always keeps the name it shares with a
// built-in, whichever canonical or retired name it is, and the migrations
// never hand it out in a built-in's place:
//
//   - the operator's tool is not folded, renamed, merged or given aliases,
//     and no built-in takes its name as an alias;
//   - an allow-list that held it still holds it under that name, and no
//     allow-list gains it (the v11 general-purpose agent included);
//   - with a canonical name taken, the built-ins that tool would absorb are
//     not folded: they keep their own definitions, and the allow-lists keep
//     their names;
//   - with a retired name taken, the rest of its group folds as usual.
func TestMigrationsLeaveUserToolsWithBuiltinNamesAlone(t *testing.T) {
	for _, taken := range sharedBuiltinNames() {
		t.Run(taken, func(t *testing.T) {
			src := userToolManifest(taken)
			before := decodeManifestAt(t, src)
			m := decodeV12(t, src)

			user := toolByID(t, m, taken)
			if user.Kind != "script" || user.Name != "Mine" || len(user.Aliases) != 0 {
				t.Fatalf("the user's %s was changed: %+v", taken, user)
			}
			for _, tool := range m.Tools {
				if slices.Contains(tool.Aliases, taken) {
					t.Errorf("built-in %s took the user's name %s as an alias", tool.ID, taken)
				}
			}
			for _, a := range m.Agents {
				held := 0
				for _, id := range a.AllowedTools {
					if id == taken {
						held++
					}
				}
				want := 0
				if a.ID == "coder" {
					want = 1
				}
				if held != want {
					t.Errorf("agent %s lists the user's %s %d times, want %d: %v", a.ID, taken, held, want, a.AllowedTools)
				}
			}

			for _, g := range toolFolds() {
				switch {
				case g.canonical == taken:
					for _, old := range g.retired {
						if tool := toolByID(t, m, old); !tool.IsBuiltin() {
							t.Errorf("%s is no longer the built-in", old)
						}
						for _, id := range []string{"coder", "other"} {
							if !slices.Contains(agentTools(t, m, id), old) {
								t.Errorf("agent %s lost %s, which cannot fold into the user's %s", id, old, taken)
							}
						}
					}
				case slices.Contains(g.retired, taken):
					canonical := toolByID(t, m, g.canonical)
					if !canonical.IsBuiltin() {
						t.Errorf("%s is not the built-in", g.canonical)
					}
					for _, old := range g.retired {
						if old != taken && hasTool(m, old) {
							t.Errorf("%s was not folded into %s", old, g.canonical)
						}
					}
				}
			}
			if len(m.Agents) != len(before.Agents)+1 {
				t.Fatalf("want the general-purpose agent added, got agents %d (before %d)", len(m.Agents), len(before.Agents))
			}
		})
	}
}

// The older retrofits read an agent's tools as trust in a new one; a tool of
// the operator's own that shares the name of the built-in they look for
// shows no such trust, and one that shares the new tool's name is never
// granted in its place.
func TestRetrofitsIgnoreUserToolsWithBuiltinNames(t *testing.T) {
	src := `
version = 6
default_agent = "coder"

[runtime]
default_permission = "ask-first"
default_timeout_sec = 60

[[tools]]
id = "file-read"
name = "File Read"
kind = "builtin"
enabled = true
timeout_sec = 30
permitted_actions = ["read"]

[[tools]]
id = "grep"
name = "My grep"
kind = "script"
entry_point = "./grep.sh"
enabled = true
timeout_sec = 30
permitted_actions = ["read"]

[[tools]]
id = "shell-exec"
name = "My shell"
kind = "script"
entry_point = "./shell.sh"
enabled = true
timeout_sec = 30
permitted_actions = ["execute"]

[[agents]]
id = "coder"
name = "Coder"
mode = "coding"
role = "primary"
allowed_tools = ["file-read", "grep", "shell-exec"]
permitted_actions = ["read", "execute"]
permission = "ask-first"
enabled = true
`
	m := decodeV12(t, src)
	tools := agentTools(t, m, "coder")
	for _, id := range []string{"repo-search", "pty-start", "pty-write", "pty-kill"} {
		if slices.Contains(tools, id) {
			t.Errorf("coder was granted %s on the strength of a user's tool: %v", id, tools)
		}
	}
	for _, id := range []string{"grep", "shell-exec"} {
		if tool := toolByID(t, m, id); tool.Kind != "script" || !slices.Contains(tools, id) {
			t.Errorf("the user's %s was changed or dropped: %+v, coder %v", id, tool, tools)
		}
	}
}
