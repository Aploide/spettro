package agent

import (
	"slices"
	"strings"
	"sync"

	"spettro/internal/lsp"
	"spettro/internal/provider"
)

// Deferred tools. Every request carries the schema of every advertised tool,
// and most agents hold far more tools than a turn uses: the coding agent
// holds about thirty and a typical run calls nine. So only a core set is
// advertised up front; an agent's other built-ins are deferred. A deferred
// tool stays on the allow-list and callable (a call made by name works, and
// advertises it from then on), and tool-search finds it: its result carries
// the matching tools' descriptions and schemas and advertises them from the
// next step on. Deferral never grants a tool: only tools the agent already
// holds are advertised, deferred or found.
//
// The advertised list changes only when a deferred tool is activated, so the
// prompt cache misses on that one step, not on every step.

// coreTools are the built-ins advertised from the first step. The rest of an
// agent's built-ins are deferred behind tool-search.
var coreTools = map[string]bool{
	"agent":      true,
	"glob":       true,
	"grep":       true,
	"file-read":  true,
	"file-write": true,
	"file-edit":  true,
	"bash":       true,
	"todo-write": true,
	"web-fetch":  true,
	"lsp":        true,
	// The retired language-server built-ins, when they stand unfolded (see
	// unfoldedLSPTool), are the lsp tool under its old names.
	"diagnostics":   true,
	"references":    true,
	"hover":         true,
	"lsp-restart":   true,
	"tool-output":   true,
	"job-output":    true,
	"job-kill":      true,
	"ask-user":      true,
	"tool-search":   true,
	"comment":       true,
	"goal-complete": true,
	// Plan mode and the fan-out tools come with prompt sections of their own
	// that tell the model to use them.
	"enter-plan-mode": true,
	"exit-plan-mode":  true,
	ultraToolID:       true,
	workflowToolID:    true,
	// The MCP resource tools are how an agent granted them reaches the
	// configured servers at all; mcp-auth is rare and stays deferred.
	"mcp-list-resources": true,
	"mcp-read-resource":  true,
}

// lspBuiltinTools are the built-ins that need a language server. With none
// configured or installed for the workspace they only ever fail, so they are
// neither advertised nor offered by tool-search.
var lspBuiltinTools = []string{"lsp", "rename-symbol", "diagnostics", "references", "hover", "lsp-restart"}

// lspAvailable reports whether a language server is configured or installed
// for the workspace. lsp.ForWorkspace caches its answer for the process, so
// the tool list a session advertises does not change under it; a server
// installed mid-session is picked up by the next one. Swappable in tests.
var lspAvailable = func(cwd string) bool { return lsp.ForWorkspace(cwd) != nil }

// toolSurface is the set of tool schemas a run advertises: the core tools,
// then each deferred tool activated so far, in activation order.
type toolSurface struct {
	mu       sync.Mutex
	core     []provider.ToolSpec
	deferred map[string]provider.ToolSpec
	// deferredOrder lists the deferred tools in allow-list order.
	deferredOrder []string
	// hidden are held tools left off the surface entirely (the language-server
	// tools without a server).
	hidden map[string]bool
	// dropped lists the held tools that hidden left off the surface.
	dropped []string
	active  []provider.ToolSpec
}

// newToolSurface splits specs into core and deferred tools. Deferral needs
// tool-search among specs, since it is the way to find a deferred tool;
// without it every tool is advertised. isCore reports the tools always
// advertised; hidden tools are dropped.
func newToolSurface(specs []provider.ToolSpec, isCore func(string) bool, hidden map[string]bool) *toolSurface {
	s := &toolSurface{deferred: map[string]provider.ToolSpec{}, hidden: hidden}
	canDefer := slices.ContainsFunc(specs, func(t provider.ToolSpec) bool { return t.Name == "tool-search" })
	for _, spec := range specs {
		switch {
		case hidden[spec.Name]:
			s.dropped = append(s.dropped, spec.Name)
		case !canDefer || isCore(spec.Name):
			s.core = append(s.core, spec)
		default:
			s.deferred[spec.Name] = spec
			s.deferredOrder = append(s.deferredOrder, spec.Name)
		}
	}
	return s
}

// specs returns the tool list for the next request.
func (s *toolSurface) specs() []provider.ToolSpec {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.active) == 0 {
		return s.core
	}
	out := make([]provider.ToolSpec, 0, len(s.core)+len(s.active))
	out = append(out, s.core...)
	return append(out, s.active...)
}

// deferredNames lists the deferred tools, activated ones included, in
// allow-list order. It does not change during a run, so the system prompt
// that names them stays stable.
func (s *toolSurface) deferredNames() []string {
	if s == nil {
		return nil
	}
	return s.deferredOrder
}

// isDeferred reports whether name is a deferred tool not yet activated.
func (s *toolSurface) isDeferred(name string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.deferred[name]
	return ok && !s.activeLocked(name)
}

// droppedNames lists the held tools left off the surface.
func (s *toolSurface) droppedNames() []string {
	if s == nil {
		return nil
	}
	return s.dropped
}

// isHidden reports whether name is a held tool left off the surface.
func (s *toolSurface) isHidden(name string) bool {
	return s != nil && s.hidden[name]
}

// deferredSpec returns a deferred tool's spec (activated or not).
func (s *toolSurface) deferredSpec(name string) (provider.ToolSpec, bool) {
	if s == nil {
		return provider.ToolSpec{}, false
	}
	spec, ok := s.deferred[name]
	return spec, ok
}

func (s *toolSurface) activeLocked(name string) bool {
	return slices.ContainsFunc(s.active, func(t provider.ToolSpec) bool { return t.Name == name })
}

// activate advertises the named deferred tools from the next request on and
// returns the ones that were not active yet. Names that are not deferred
// tools are ignored.
func (s *toolSurface) activate(names ...string) []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var added []string
	for _, name := range names {
		spec, ok := s.deferred[name]
		if !ok || s.activeLocked(name) {
			continue
		}
		s.active = append(s.active, spec)
		added = append(added, name)
	}
	return added
}

// toolSearchActivatedPrefix starts the line of a tool-search result naming
// the tools it activated. restoreActivations reads it back from a carried
// conversation, so a later turn advertises the same tools.
const toolSearchActivatedPrefix = "Activated: "

// parseToolSearchActivated returns the tools a tool-search result activated.
func parseToolSearchActivated(output string) []string {
	for line := range strings.Lines(output) {
		rest, ok := strings.CutPrefix(line, toolSearchActivatedPrefix)
		if !ok {
			continue
		}
		rest, _, _ = strings.Cut(rest, " (")
		var names []string
		for name := range strings.SplitSeq(rest, ",") {
			if name = strings.TrimSpace(name); name != "" {
				names = append(names, name)
			}
		}
		return names
	}
	return nil
}

// canonicalName is the tool a call under name runs: the canonical tool for a
// retired built-in name, else name itself.
func (r *toolRuntime) canonicalName(name string) string {
	if r.userToolNamed(name) || r.unfoldedLSPTool(name) {
		return name
	}
	return CanonicalToolName(name)
}

// noteCalls activates the deferred tools called by name (the call ran: the
// tool is allowed). Once the model uses a tool it gets its schema.
func (r *toolRuntime) noteCalls(names []string) {
	if r.surface == nil {
		return
	}
	for _, name := range names {
		r.surface.activate(r.canonicalName(name))
	}
}

// restoreActivations re-activates the deferred tools an earlier turn of the
// carried conversation activated, by calling one or through tool-search, so
// the tool list the conversation was using carries over.
func (r *toolRuntime) restoreActivations(msgs []provider.Message) {
	if r.surface == nil {
		return
	}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			r.surface.activate(r.canonicalName(tc.Name))
		}
		for _, res := range m.ToolResults {
			if res.Name == "tool-search" && !res.IsErr {
				r.surface.activate(parseToolSearchActivated(res.Output)...)
			}
		}
	}
}

// buildToolSurface builds the run's tool surface from its allow-list.
func (r *toolRuntime) buildToolSurface(allowedTools []string) *toolSurface {
	specs := buildToolSpecs(allowedTools)
	specs = append(specs, r.unfoldedLSPToolSpecs(allowedTools)...)
	hidden := map[string]bool{}
	if slices.ContainsFunc(specs, func(t provider.ToolSpec) bool { return slices.Contains(lspBuiltinTools, t.Name) }) && !lspAvailable(r.cwd) {
		for _, name := range lspBuiltinTools {
			if !r.userToolNamed(name) {
				hidden[name] = true
			}
		}
	}
	// A skills catalog in the system prompt tells the model to call
	// skill-read, so it is advertised whenever there is one.
	hasSkills := len(r.skillsCatalog.Active()) > 0
	isCore := func(name string) bool {
		return coreTools[name] || (name == "skill-read" && hasSkills)
	}
	return newToolSurface(specs, isCore, hidden)
}

// toolSurfacePrompt is the system prompt's note on the tools a run holds but
// does not advertise up front: the deferred tools, and the language-server
// tools left out for want of a server. Empty when there are none.
func toolSurfacePrompt(deferred []string, lspHidden bool) string {
	var parts []string
	if len(deferred) > 0 {
		parts = append(parts, "# More tools\n\nThese tools are available but their schemas are not loaded: "+strings.Join(deferred, ", ")+
			". Call tool-search with a tool name or keyword to load a tool's description and schema; it can be called from the next step on. Most tasks never need them.")
	}
	if lspHidden {
		parts = append(parts, "No language server is available in this workspace, so the lsp tool is disabled and edit results carry no language-server diagnostics: use the project's build, type-checker or tests instead.")
	}
	if len(parts) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(parts, "\n\n")
}

// toolCallNames returns the names of calls.
func toolCallNames(calls []provider.NativeTool) []string {
	names := make([]string, len(calls))
	for i, tc := range calls {
		names[i] = tc.Name
	}
	return names
}
