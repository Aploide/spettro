package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"spettro/internal/config"
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
// A retired name is an alias only while its canonical tool is the built-in.
// When a tool of the operator's own holds the canonical name (or the retired
// name itself), tool_names.go decides what a call under the name reaches.
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
	"activate-skill": {canonical: "skill-read", sameTool: true},
	"skill-activate": {canonical: "skill-read", sameTool: true},
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

// hookAlias is the retired name the hooks of this (canonical) call also
// match (see toolRuntime.toolHookRules): the name the model called the tool
// by, or, for a call of the built-in lsp made under its own name, the retired
// tool its op replaced. Each op is exactly one of the old tools, so a hook
// written for "diagnostics" keeps firing on lsp {op: "diagnostics"} and on
// nothing else. A retired name the operator's own tool now answers to (a
// "hover" script) is that tool's, so its hooks do not fire on the op; and a
// call of the operator's own lsp has no ops at all.
func (r *toolRuntime) hookAlias(call toolCall) string {
	if call.CalledAs != "" || call.Tool != "lsp" {
		return call.CalledAs
	}
	if r.builtinFor("lsp") != "lsp" {
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
	if call.Tool != "lsp" || !spec.IsBuiltin() {
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
