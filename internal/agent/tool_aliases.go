package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Retired tool names. Several built-in tools did the same job under
// different names (shell-exec and bash, multi-edit and file-edit, the task-*
// family and todo-write, ...), which cost prompt tokens on every request and
// left the model choosing between equivalents. Each group now has one
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
}

var legacyTools = map[string]legacyTool{
	"shell-exec":     {canonical: "bash"},
	"bash-output":    {canonical: "bash"},
	"multi-edit":     {canonical: "file-edit"},
	"repo-search":    {canonical: "grep", args: repoSearchArgs},
	"ls":             {canonical: "glob"},
	"task-create":    {canonical: "todo-write", args: taskUpsertArgs(false)},
	"task-update":    {canonical: "todo-write", args: taskUpsertArgs(true)},
	"task-get":       {canonical: "todo-write", args: taskReadArgs},
	"task-list":      {canonical: "todo-write", args: taskReadArgs},
	"task-delete":    {canonical: "todo-write", args: taskDeleteArgs},
	"activate-skill": {canonical: "skill-read"},
	"skill-activate": {canonical: "skill-read"},
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
	out := toolCall{Tool: lt.canonical, Args: call.Args}
	if lt.args != nil {
		args, err := lt.args(call.Args)
		if err != nil {
			return call, fmt.Errorf("%s args: %w", call.Tool, err)
		}
		out.Args = args
	}
	return out, nil
}

// canonicalCall is canonicalToolCall for this run: a retired name the
// manifest defines as a tool of its own (a user's script or MCP tool that
// happens to be called "ls") is that tool, not an alias.
func (r *toolRuntime) canonicalCall(call toolCall) (toolCall, error) {
	if spec, ok := r.toolPolicies[call.Tool]; ok && spec.ID == call.Tool && spec.Kind != "" && spec.Kind != "builtin" {
		return call, nil
	}
	return canonicalToolCall(call)
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
// must carry an ID.
func taskUpsertArgs(needID bool) func(json.RawMessage) (json.RawMessage, error) {
	return func(raw json.RawMessage) (json.RawMessage, error) {
		var item map[string]json.RawMessage
		if err := decodeJSONStrict(raw, &item); err != nil {
			return nil, err
		}
		if needID {
			var id string
			if json.Unmarshal(item["id"], &id) != nil || strings.TrimSpace(id) == "" {
				return nil, fmt.Errorf("id is required")
			}
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
