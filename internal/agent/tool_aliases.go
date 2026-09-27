package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/skills"
)

// Retired tool names. Several built-in tools did the same job under
// different names (shell-exec and bash, multi-edit and file-edit, the task-*
// family and todo-write, ...), which cost prompt tokens on every request and
// left the model choosing between equivalents; the read-only language-server
// tools (diagnostics, references, hover, lsp-restart) were four schemas for
// what is one tool with an operation. Each group now has one
// canonical tool; the old names stay callable as hidden aliases so a model
// trained on them, a carried conversation, or a hand-written hook keeps
// working, but only the canonical tool is advertised.
//
// The table is built in rather than read from the manifest so it also covers
// runs with a hard-coded tool list and no manifest policies (LLMCoder, the
// planner, the explorer).
type legacyTool struct {
	canonical string
	// args converts the retired tool's arguments to the canonical tool's;
	// nil when the canonical tool accepts them unchanged.
	args func(json.RawMessage) (json.RawMessage, error)
	// sameTool marks a retired name that was the very same tool as its
	// canonical one, not a narrower operation now folded into it: hooks
	// written for it keep firing on every call of the canonical tool (see
	// toolRuntime.toolHookRules).
	sameTool bool
}

var legacyTools = map[string]legacyTool{
	"shell-exec":     {canonical: "bash", sameTool: true},
	"bash-output":    {canonical: "bash", sameTool: true},
	"multi-edit":     {canonical: "file-edit"},
	"repo-search":    {canonical: "grep", args: repoSearchArgs},
	"ls":             {canonical: "glob"},
	"task-create":    {canonical: "todo-write", args: taskUpsertArgs(false)},
	"task-update":    {canonical: "todo-write", args: taskUpsertArgs(true)},
	"task-get":       {canonical: "todo-write", args: taskReadArgs},
	"task-list":      {canonical: "todo-write", args: taskReadArgs},
	"task-delete":    {canonical: "todo-write", args: taskDeleteArgs},
	"skill-read":     {canonical: "skill", sameTool: true},
	"activate-skill": {canonical: "skill", sameTool: true},
	"skill-activate": {canonical: "skill", sameTool: true},
	"skill-list":     {canonical: "skill"},
	"diagnostics":    {canonical: "lsp", args: lspOpArgs("diagnostics")},
	"references":     {canonical: "lsp", args: lspReferencesArgs},
	"hover":          {canonical: "lsp", args: lspOpArgs("hover")},
	"lsp-restart":    {canonical: "lsp", args: lspOpArgs("restart")},
}

// LegacyToolNames returns the retired names that now route to canonical,
// sorted; nil when it replaced none.
func LegacyToolNames(canonical string) []string {
	var out []string
	for name, lt := range legacyTools {
		if lt.canonical == canonical {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// CanonicalToolName returns the tool a retired name routes to, or name
// itself when it is not retired.
func CanonicalToolName(name string) string {
	if lt, ok := legacyTools[name]; ok {
		return lt.canonical
	}
	return name
}

// canonicalToolCall rewrites a call made under a retired name into the
// equivalent call of its canonical tool. Calls under any other name are
// returned unchanged.
func canonicalToolCall(call toolCall) (toolCall, error) {
	lt, ok := legacyTools[call.Tool]
	if !ok {
		return call, nil
	}
	out := toolCall{Tool: lt.canonical, Args: call.Args, CalledAs: call.Tool}
	if lt.args != nil {
		args, err := lt.args(call.Args)
		if err != nil {
			return call, fmt.Errorf("%s args: %w", call.Tool, err)
		}
		out.Args = args
	}
	return out, nil
}

// canonicalCall is canonicalToolCall for this run. A retired name that a
// tool of the operator's own answers to (a script or MCP tool that happens to
// be called "ls" or "hover") is that tool, not an alias. And when the
// operator's own tool holds a canonical name (lsp, skill), the migration
// left the built-ins it would have folded into it unfolded: their calls keep
// their own names (see unfoldedTool).
func (r *toolRuntime) canonicalCall(call toolCall) (toolCall, error) {
	if r.userToolNamed(call.Tool) || r.unfoldedTool(call.Tool) {
		return call, nil
	}
	return canonicalToolCall(call)
}

// userToolNamed reports whether name is the ID or an alias of a tool that is
// not a built-in: one of this agent's, or any in the manifest.
func (r *toolRuntime) userToolNamed(name string) bool {
	if spec, ok := r.toolPolicies[name]; ok && !isBuiltinTool(spec) {
		return true
	}
	if r.manifest != nil {
		for _, t := range r.manifest.Tools {
			if !isBuiltinTool(t) && (t.ID == name || slices.Contains(t.Aliases, name)) {
				return true
			}
		}
	}
	return false
}

func isBuiltinTool(t config.ToolSpec) bool {
	return t.Kind == "" || t.Kind == "builtin"
}

// unfoldableTools are the canonical tools whose manifest migration (v13 for
// lsp, v14 for skill) is skipped when the operator already has a tool of
// their own under the canonical name: folding a built-in into the
// operator's tool would change what that tool is (see
// config.consolidateBuiltinTools). The retired built-ins then stay in the
// manifest under their old names, and the runtime has to keep running and
// advertising them under those names (see unfoldedTool).
//
// The v12 folds (bash, file-edit, grep, glob, todo-write) are not listed:
// their retired names have no dispatch or schema of their own any more.
var unfoldableTools = map[string]bool{"lsp": true, skills.ToolName: true}

// unfoldedTool reports whether name is a retired built-in whose fold did not
// happen because the operator owns its canonical name (see
// unfoldableTools). Such a call keeps its name and runs the built-in the
// agent holds, rather than becoming a call of the operator's tool.
func (r *toolRuntime) unfoldedTool(name string) bool {
	lt, ok := legacyTools[name]
	return ok && unfoldableTools[lt.canonical] && r.userToolNamed(lt.canonical)
}

// unfoldedLSPTool reports whether name is one of the retired language-server
// built-ins standing unfolded (the operator has a tool of their own called
// lsp).
func (r *toolRuntime) unfoldedLSPTool(name string) bool {
	return r.unfoldedTool(name) && legacyTools[name].canonical == "lsp"
}

// unfoldedSkillTool reports whether name is one of the retired skill
// built-ins (skill-read, its old aliases, skill-list) standing unfolded (the
// operator has a tool of their own called skill).
func (r *toolRuntime) unfoldedSkillTool(name string) bool {
	return r.unfoldedTool(name) && legacyTools[name].canonical == skills.ToolName
}

// unfoldedToolAdvert is the description and schema a retired built-in is
// advertised with while it stands unfolded: the tool as it was advertised
// before its fold.
type unfoldedToolAdvert struct {
	desc   string
	schema json.RawMessage
}

// unfoldedToolAdverts holds the adverts of the retired built-ins that can
// stand unfolded. Only a manifest tool's own ID is advertised; the old
// aliases of skill-read (activate-skill, skill-activate) stay callable
// without being listed.
var unfoldedToolAdverts = map[string]unfoldedToolAdvert{
	"diagnostics": {
		desc:   "Return current language-server diagnostics for a file (or every file seen so far when path is omitted).",
		schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
	},
	"references": {
		desc:   "Language-server lookup: find references to a symbol, or its definition with kind=\"definition\". Position by symbol name or 1-based line/character.",
		schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"symbol":{"type":"string"},"kind":{"type":"string","enum":["references","definition"]},"line":{"type":"integer"},"character":{"type":"integer"}},"required":["path"]}`),
	},
	"hover": {
		desc:   "Language-server hover: type signature and documentation for a symbol. Position by symbol name or 1-based line/character.",
		schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"symbol":{"type":"string"},"line":{"type":"integer"},"character":{"type":"integer"}},"required":["path"]}`),
	},
	"lsp-restart": {
		desc:   "Restart a wedged language server (all servers when none named).",
		schema: json.RawMessage(`{"type":"object","properties":{"server":{"type":"string"}}}`),
	},
	"skill-read": {
		desc:   "Load an Agent Skill by name: returns its instructions and the directory holding its bundled files. args, when given, fill the skill's $ARGUMENTS placeholders.",
		schema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"the skill to load"},"args":{"type":"string","description":"optional arguments for the skill"}},"required":["name"]}`),
	},
	"skill-list": {
		desc:   "List the Agent Skills you may load (query filters by name or description).",
		schema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
	},
}

// unfoldedToolSpecs returns the native tool specs of the unfolded retired
// built-ins this agent holds, in allow-list order.
func (r *toolRuntime) unfoldedToolSpecs(allowedTools []string) []provider.ToolSpec {
	var out []provider.ToolSpec
	for _, name := range allowedTools {
		name = strings.TrimSpace(name)
		spec, ok := r.toolPolicies[name]
		if !ok || spec.ID != name || !isBuiltinTool(spec) || !r.unfoldedTool(name) {
			continue
		}
		advert, ok := unfoldedToolAdverts[name]
		if !ok || slices.ContainsFunc(out, func(t provider.ToolSpec) bool { return t.Name == name }) {
			continue
		}
		out = append(out, provider.ToolSpec{Name: name, Description: advert.desc, Schema: advert.schema})
	}
	return out
}

// hookAlias is the retired name the hooks of this (canonical) call also
// match (see toolRuntime.toolHookRules): the name the model called the tool
// by, or, for an lsp call made under its own name, the retired tool its op
// replaced. Each op is exactly one of the old tools, so a hook written for
// "diagnostics" keeps firing on lsp {op: "diagnostics"} and on nothing else.
// A retired name the operator's own tool now answers to (a "hover" script) is
// that tool's, so its hooks do not fire on the op.
func (r *toolRuntime) hookAlias(call toolCall) string {
	if call.CalledAs != "" || call.Tool != "lsp" {
		return call.CalledAs
	}
	if r.userToolNamed("lsp") {
		return ""
	}
	name := lspOpTools[lspCallOp(call.Args)]
	if name == "" || r.userToolNamed(name) {
		return ""
	}
	return name
}

// lspOpDenied applies, to a call of the built-in lsp tool, the lsp-op
// permission rules (config.LSPOpDenied): { permission = "lsp-op", pattern =
// "restart", action = "deny" } still lets the agent look symbols up but not
// restart a server. The v13 manifest migration writes such a rule for each
// op an agent could not call before (it held diagnostics but not
// lsp-restart, say), so folding the tools into one never hands an agent an
// operation it did not have. Rules for any other permission, "tool" and "*"
// included, only decide whether lsp can be called at all.
func (r *toolRuntime) lspOpDenied(call toolCall, spec config.ToolSpec) error {
	if call.Tool != "lsp" || !isBuiltinTool(spec) {
		return nil
	}
	op := lspCallOp(call.Args)
	if _, ok := lspOpTools[op]; !ok {
		return nil
	}
	if config.LSPOpDenied(op, r.runtimeRules, r.agentRules, spec.PermissionRules) {
		return fmt.Errorf("lsp op %q denied by policy (a %s rule denies it)", op, config.LSPOpPermission)
	}
	return nil
}

// lspOpTools maps each lsp op to the retired tool that did it.
var lspOpTools = map[string]string{
	"diagnostics": "diagnostics",
	"references":  "references",
	"definition":  "references",
	"hover":       "hover",
	"restart":     "lsp-restart",
}

// lspOpArgs maps a retired language-server tool onto the lsp op that does
// the same thing. The old tools' arguments are the op's arguments, and an
// "op" the old tool would have ignored does not change which op runs.
func lspOpArgs(op string) func(json.RawMessage) (json.RawMessage, error) {
	return func(raw json.RawMessage) (json.RawMessage, error) {
		var in map[string]json.RawMessage
		if err := decodeJSONStrict(raw, &in); err != nil {
			return nil, err
		}
		if in == nil {
			in = map[string]json.RawMessage{}
		}
		in["op"], _ = json.Marshal(op)
		return json.Marshal(in)
	}
}

// lspReferencesArgs maps references {kind} onto lsp: kind "definition" is
// op definition, anything else op references with the kind left in place,
// where the lsp tool checks it after the path and position and reports a bad
// one as the references tool did.
func lspReferencesArgs(raw json.RawMessage) (json.RawMessage, error) {
	var in map[string]json.RawMessage
	if err := decodeJSONStrict(raw, &in); err != nil {
		return nil, err
	}
	if in == nil {
		in = map[string]json.RawMessage{}
	}
	op := "references"
	var kind string
	if k, ok := in["kind"]; ok && json.Unmarshal(k, &kind) == nil && kind == "definition" {
		op = "definition"
		delete(in, "kind")
	}
	in["op"], _ = json.Marshal(op)
	return json.Marshal(in)
}

// repoSearchArgs maps repo-search {query} onto grep's symbol search, which
// runs the same ranked-definitions lookup.
func repoSearchArgs(raw json.RawMessage) (json.RawMessage, error) {
	var in struct {
		Query string `json:"query"`
	}
	if err := decodeJSONStrict(raw, &in); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"symbol": in.Query})
}

// taskUpsertArgs maps task-create / task-update (one task object) onto a
// merging todo-write of that task. task-update names an existing task, so it
// must carry an ID, an unknown ID stays an error instead of becoming a new
// task, and an empty dependencies list keeps the stored one, as task-update
// always treated it (todo-write's merge reads [] as "clear").
func taskUpsertArgs(update bool) func(json.RawMessage) (json.RawMessage, error) {
	return func(raw json.RawMessage) (json.RawMessage, error) {
		var item map[string]json.RawMessage
		if err := decodeJSONStrict(raw, &item); err != nil {
			return nil, err
		}
		if update {
			var id string
			if json.Unmarshal(item["id"], &id) != nil || strings.TrimSpace(id) == "" {
				return nil, fmt.Errorf("id is required")
			}
			if deps, ok := item["dependencies"]; ok {
				var list []string
				if json.Unmarshal(deps, &list) == nil && len(list) == 0 {
					delete(item, "dependencies")
				}
			}
			item["update_only"] = json.RawMessage("true")
		} else {
			delete(item, "update_only")
		}
		return json.Marshal(map[string]any{"merge": true, "todos": []any{item}})
	}
}

// taskReadArgs maps task-get / task-list onto a read-only todo-write call,
// which returns the whole list.
func taskReadArgs(raw json.RawMessage) (json.RawMessage, error) {
	var in map[string]json.RawMessage
	if err := decodeJSONStrict(raw, &in); err != nil {
		return nil, err
	}
	return json.RawMessage(`{}`), nil
}

// taskDeleteArgs maps task-delete {id | clear_completed} onto todo-write's
// delete / clear_completed.
func taskDeleteArgs(raw json.RawMessage) (json.RawMessage, error) {
	var in struct {
		ID             string   `json:"id"`
		ClearCompleted flexBool `json:"clear_completed"`
	}
	if err := decodeJSONStrict(raw, &in); err != nil {
		return nil, err
	}
	if in.ClearCompleted {
		return json.RawMessage(`{"clear_completed":true}`), nil
	}
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return nil, fmt.Errorf("id is required (or set clear_completed)")
	}
	return json.Marshal(map[string]any{"delete": []string{id}})
}
