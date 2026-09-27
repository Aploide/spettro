package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"spettro/internal/config"
	"spettro/internal/provider"
)

// Which tool a call under a given name reaches.
//
// Three kinds of tool share one namespace:
//
//   - built-ins, carried out by the dispatch in toolRuntime.execute;
//   - retired built-in names (legacyTools in tool_aliases.go: shell-exec,
//     multi-edit, repo-search, ls, task-*, activate-skill, diagnostics, ...),
//     each an alias of one canonical built-in (bash, file-edit, grep, glob,
//     todo-write, skill-read, lsp);
//   - tools of the operator's own: manifest tools of kind mcp, script or
//     http, which may take any name, a built-in's or a retired one's
//     included.
//
// One rule decides which a call reaches, and the functions below are the only
// place it is applied:
//
//  1. A tool of the operator's own always wins for calls by its ID or one of
//     its aliases. The call is never rewritten into a built-in, and no
//     built-in's code carries it out. Spettro has no runner for those kinds,
//     so the call fails without doing anything (userToolError), after the
//     allow-list, permission rules and hooks have seen it under its own name
//     like any other call.
//  2. A retired name whose canonical name is not taken by a tool of the
//     operator's own is an alias: the call is rewritten into its canonical
//     tool (canonicalToolCall), with CalledAs recording the name the model
//     used.
//  3. A retired name whose canonical name a tool of the operator's own holds
//     stands "unfolded": the built-in that shipped under the retired name
//     stays reachable under that name, and never becomes a call of the
//     operator's tool. The call keeps its name, so the allow-list, permission
//     rules, approvals, hooks and traces see the retired name, while the
//     canonical built-in's code does the work on the converted arguments
//     (builtinCall). The v12/v13 manifest migrations do not fold such a
//     built-in (config.AgentManifest.consolidateBuiltinTools), so an agent
//     that held it still lists it under its own name, and it is advertised
//     under that name (unfoldedToolSpecs).
//  4. Any other name is the built-in of that name.
//
// A call therefore has two sides. Its identity, call.Tool once canonicalCall
// has run, decides whether it may run at all and which permission rules,
// approvals and hooks apply. The built-in that carries it out
// (builtinCall) decides how it is batched, timed, checkpointed and
// dispatched. The two differ only for an unfolded retired name, and for a
// tool of the operator's own, which no built-in carries out.

// userTool returns the tool of the operator's own that answers to name (as
// its ID or one of its aliases). It looks at this agent's tools and then at
// every tool in the manifest: a name another agent's tool holds is the
// operator's too, not a built-in's.
func (r *toolRuntime) userTool(name string) (config.ToolSpec, bool) {
	if spec, ok := r.toolPolicies[name]; ok && !spec.IsBuiltin() {
		return spec, true
	}
	if r.manifest != nil {
		for _, t := range r.manifest.Tools {
			if !t.IsBuiltin() && (t.ID == name || slices.Contains(t.Aliases, name)) {
				return t, true
			}
		}
	}
	return config.ToolSpec{}, false
}

// userToolNamed reports whether a tool of the operator's own answers to name
// (rule 1).
func (r *toolRuntime) userToolNamed(name string) bool {
	_, ok := r.userTool(name)
	return ok
}

// unfoldedTool reports whether name is a retired built-in name standing
// unfolded (rule 3): no tool of the operator's own answers to it, but one
// holds the name of its canonical tool.
func (r *toolRuntime) unfoldedTool(name string) bool {
	lt, ok := legacyTools[name]
	return ok && !r.userToolNamed(name) && r.userToolNamed(lt.canonical)
}

// canonicalCall gives a call as the model made it its identity: a retired
// name that is an alias (rule 2) is rewritten into its canonical tool, and
// every other call (a tool of the operator's own, an unfolded retired name,
// any other built-in) is returned unchanged.
func (r *toolRuntime) canonicalCall(call toolCall) (toolCall, error) {
	if r.userToolNamed(call.Tool) || r.unfoldedTool(call.Tool) {
		return call, nil
	}
	return canonicalToolCall(call)
}

// canonicalName is canonicalCall for a bare name: the identity a call under
// name gets.
func (r *toolRuntime) canonicalName(name string) string {
	if r.userToolNamed(name) || r.unfoldedTool(name) {
		return name
	}
	return CanonicalToolName(name)
}

// builtinFor returns the built-in whose code carries out a call under name:
// name itself for a built-in, the canonical tool for a retired name (alias
// or unfolded), and "" for a tool of the operator's own, which no built-in
// carries out. Behaviour keyed on a tool's name (the goal-mode shell timeout,
// the long timeout of ultra and workflow) goes by this, not by the identity.
func (r *toolRuntime) builtinFor(name string) string {
	if r.userToolNamed(name) {
		return ""
	}
	return CanonicalToolName(name)
}

// builtinCall returns a call, as canonicalCall left it, the way the built-in
// that carries it out sees it: unchanged for a built-in, and for an unfolded
// retired name the canonical tool's call with the arguments converted and
// CalledAs set to the retired name (the lsp tool words its errors after it).
// A call of a tool of the operator's own has no built-in to carry it out and
// gets userToolError; a retired call whose arguments do not convert gets the
// conversion error.
func (r *toolRuntime) builtinCall(call toolCall) (toolCall, error) {
	if spec, ok := r.userTool(call.Tool); ok {
		return toolCall{}, userToolError(call.Tool, spec)
	}
	return canonicalToolCall(call)
}

// userToolError is the result of a call of a tool of the operator's own.
// Spettro has no runner for tools of kind mcp, script or http, so nothing was
// run; the message says so plainly because the model reads it.
func userToolError(name string, spec config.ToolSpec) error {
	return fmt.Errorf("tool %q is a %s tool defined in the agent manifest, and spettro cannot run %s tools: nothing was run", name, spec.Kind, spec.Kind)
}

// builtinNames returns the names in tools that do not name a tool of the
// operator's own, in order. Only built-ins have a schema to advertise: a
// held tool of the operator's own that shares a built-in's name must not be
// advertised with the built-in's description and schema.
func (r *toolRuntime) builtinNames(tools []string) []string {
	return slices.DeleteFunc(slices.Clone(tools), func(name string) bool {
		return r.userToolNamed(strings.TrimSpace(name))
	})
}

// retiredToolSurface is the description and schema a retired built-in is
// advertised with while it stands unfolded.
type retiredToolSurface struct {
	desc   string
	schema json.RawMessage
}

// retiredToolSurfaces are the surfaces of the retired built-ins whose
// arguments differ from their canonical tool's. A retired name missing here
// (shell-exec, bash-output, multi-edit, ls, activate-skill, skill-activate)
// takes exactly its canonical tool's arguments, so it is advertised with the
// canonical tool's description and schema (see unfoldedSurface).
var retiredToolSurfaces = map[string]retiredToolSurface{
	"repo-search": {
		desc:   "Symbol search: the ranked definitions of an identifier, then its usages.",
		schema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"the identifier to look up"}},"required":["query"]}`),
	},
	"task-create": {
		desc:   "Add one task to the session task list (an existing id updates that task instead). dependencies lists task IDs that must be completed first. Returns the whole list.",
		schema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"content":{"type":"string"},"status":{"type":"string"},"owner":{"type":"string"},"source":{"type":"string"},"priority":{"type":"string"},"dependencies":{"type":"array","items":{"type":"string"}}},"required":["content"]}`),
	},
	"task-update": {
		desc:   "Update one existing task by id; an unknown id is an error, and an empty dependencies list keeps the stored ones. Returns the whole list.",
		schema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"content":{"type":"string"},"status":{"type":"string"},"owner":{"type":"string"},"source":{"type":"string"},"priority":{"type":"string"},"dependencies":{"type":"array","items":{"type":"string"}}},"required":["id"]}`),
	},
	"task-get": {
		desc:   "Return the whole session task list, in dependency order.",
		schema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}}}`),
	},
	"task-list": {
		desc:   "Return the whole session task list, in dependency order, with blocked_by and ready for each task.",
		schema: json.RawMessage(`{"type":"object","properties":{}}`),
	},
	"task-delete": {
		desc:   "Delete a task by id, or set clear_completed to prune completed and cancelled tasks. Returns the whole list.",
		schema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"clear_completed":{"type":"boolean"}}}`),
	},
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
}

// unfoldedSurface returns the description and schema a retired built-in is
// advertised with while it stands unfolded: its own when its arguments
// differ from its canonical tool's, else the canonical tool's.
func unfoldedSurface(name string) (string, json.RawMessage) {
	if s, ok := retiredToolSurfaces[name]; ok {
		return s.desc, s.schema
	}
	canonical := legacyTools[name].canonical
	desc, _ := toolDescription(canonical)
	return desc, builtinNativeToolSchemas[canonical]
}

// unfoldedToolSpecs returns the native tool specs of the unfolded retired
// built-ins this agent holds (rule 3), in allow-list order. A held name
// counts only when it is the built-in's own definition (not an alias of
// another tool).
func (r *toolRuntime) unfoldedToolSpecs(allowedTools []string) []provider.ToolSpec {
	var out []provider.ToolSpec
	for _, name := range allowedTools {
		name = strings.TrimSpace(name)
		spec, ok := r.toolPolicies[name]
		if !ok || spec.ID != name || !spec.IsBuiltin() || !r.unfoldedTool(name) {
			continue
		}
		if slices.ContainsFunc(out, func(t provider.ToolSpec) bool { return t.Name == name }) {
			continue
		}
		desc, schema := unfoldedSurface(name)
		out = append(out, provider.ToolSpec{Name: name, Description: desc, Schema: schema})
	}
	return out
}
