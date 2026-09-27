package agent

import (
	"fmt"
	"slices"
	"strings"
)

// Near-miss tool names.
//
// Models trained on other harnesses call tools by names that differ from
// Spettro's only in spelling: web_fetch, file_read, todo_write, Bash. Such a
// call used to fail as "not allowed", and the model often retried the same
// name. A call under a name Spettro does not know at all (not a built-in, a
// retired name, a manifest tool or one of its aliases) is now routed once,
// in parallelExec, before anything else looks at it (routeNearMissCall):
//
//  1. The name is folded (foldToolName: lower case, '_' and spaces read as
//     '-'). When exactly one tool the agent may call folds to the same key,
//     the call goes to that tool, with CalledAs recording the name the model
//     used so hooks written for that spelling still fire. A retired name
//     that folds to the key stands for the tool it is an alias of
//     (task_create is a todo-write call), as long as that tool is allowed.
//  2. Otherwise the call is left as it is and fails in execute as not
//     allowed; for an unknown name the error also names up to three allowed
//     tools closest to it ("did you mean ...").
//
// Routing never widens what the agent can do: only names in the agent's
// allow-list are candidates, and every check after routing (permission
// rules, hooks, approvals) runs exactly as for a call under the exact name.
// A name Spettro knows is never routed, even when the agent does not hold
// it, so an explorer calling file-write still gets "not allowed", and a
// retired name keeps the routing tool_names.go gives it.

// maxToolSuggestions caps the names a "did you mean" hint lists.
const maxToolSuggestions = 3

// foldToolName is the spelling-insensitive key of a tool name: trimmed,
// lower case, with '_' and spaces read as '-'.
func foldToolName(name string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '_', ' ':
			return '-'
		}
		return r
	}, strings.ToLower(strings.TrimSpace(name)))
}

// knownToolName reports whether Spettro knows name as a tool: a built-in, a
// retired built-in name, or a tool (or alias) of the manifest. Only unknown
// names are routed or get suggestions.
func (r *toolRuntime) knownToolName(name string) bool {
	if _, ok := builtinNativeToolDescs[name]; ok {
		return true
	}
	if _, ok := legacyTools[name]; ok {
		return true
	}
	if _, ok := r.toolPolicies[name]; ok {
		return true
	}
	return r.userToolNamed(name)
}

// routeNearMissCall returns the call a near-miss name stands for (see the
// comment at the top of this file) and true, or the call unchanged and false
// when the name is allowed, known, or does not fold to exactly one allowed
// tool. The routed call is named as the matching allow-list entry (or
// retired name) and still goes through canonicalCall; CalledAs holds the
// name the model used.
func (r *toolRuntime) routeNearMissCall(call toolCall, allowed map[string]struct{}) (toolCall, bool) {
	if _, ok := allowed[call.Tool]; ok || r.knownToolName(call.Tool) {
		return call, false
	}
	key := foldToolName(call.Tool)
	if key == "" {
		return call, false
	}
	// Candidates are keyed by the tool they reach, so an allow-list entry
	// and a retired alias of the same tool are one match, not two.
	targets := map[string]string{}
	consider := func(name string) {
		if foldToolName(name) != key {
			return
		}
		identity := r.canonicalName(name)
		if _, ok := allowed[identity]; ok {
			if prev, seen := targets[identity]; !seen || name < prev {
				targets[identity] = name
			}
		}
	}
	for name := range allowed {
		consider(name)
	}
	for name := range legacyTools {
		consider(name)
	}
	if len(targets) != 1 {
		return call, false
	}
	for _, name := range targets {
		return toolCall{Tool: name, Args: call.Args, CalledAs: call.Tool}, true
	}
	return call, false
}

// notAllowedError is execute's error for a call whose identity is not in the
// allow-list. For a name Spettro does not know it adds the closest allowed
// tool names, so a misspelt call is corrected on the next step instead of
// repeated.
func (r *toolRuntime) notAllowedError(name string, allowed map[string]struct{}) error {
	if r.knownToolName(name) {
		return fmt.Errorf("tool %q not allowed", name)
	}
	suggestions := closestToolNames(name, allowed, maxToolSuggestions)
	if len(suggestions) == 0 {
		return fmt.Errorf("tool %q not allowed: no tool you can call has that name", name)
	}
	return fmt.Errorf("tool %q not allowed: no tool you can call has that name; did you mean %s?", name, joinQuotedOr(suggestions))
}

// closestToolNames returns up to limit names from allowed that are close to
// name, closest first (ties by name). Names are compared folded; the same
// words in another order ("read_file" for file-read) count as closest, and
// otherwise the edit distance must be at most a third of the name's length
// (at least 2), so an unrelated name gets no suggestion at all.
func closestToolNames(name string, allowed map[string]struct{}, limit int) []string {
	key := foldToolName(name)
	if key == "" {
		return nil
	}
	maxDist := max(2, len(key)/3)
	type scored struct {
		name string
		dist int
	}
	var found []scored
	for candidate := range allowed {
		folded := foldToolName(candidate)
		dist := editDistance(key, folded)
		if sameWords(key, folded) {
			dist = 0
		}
		if dist <= maxDist {
			found = append(found, scored{candidate, dist})
		}
	}
	slices.SortFunc(found, func(a, b scored) int {
		if a.dist != b.dist {
			return a.dist - b.dist
		}
		return strings.Compare(a.name, b.name)
	})
	out := make([]string, 0, min(limit, len(found)))
	for _, s := range found[:min(limit, len(found))] {
		out = append(out, s.name)
	}
	return out
}

// sameWords reports whether two folded names are the same '-'-separated
// words in another order ("read-file" and "file-read").
func sameWords(a, b string) bool {
	wa, wb := strings.Split(a, "-"), strings.Split(b, "-")
	if len(wa) != len(wb) || len(wa) < 2 {
		return false
	}
	slices.Sort(wa)
	slices.Sort(wb)
	return slices.Equal(wa, wb)
}

// editDistance is the Levenshtein distance between a and b, counted in
// bytes (tool names are ASCII), with one row of state.
func editDistance(a, b string) int {
	row := make([]int, len(b)+1)
	for j := range row {
		row[j] = j
	}
	for i := 1; i <= len(a); i++ {
		diag := row[0]
		row[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			next := min(row[j]+1, row[j-1]+1, diag+cost)
			diag, row[j] = row[j], next
		}
	}
	return row[len(b)]
}

// joinQuotedOr renders names as `"a"`, `"a" or "b"`, `"a", "b" or "c"`.
func joinQuotedOr(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	if len(quoted) == 1 {
		return quoted[0]
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + " or " + quoted[len(quoted)-1]
}
