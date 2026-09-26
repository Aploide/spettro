package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	agentprompts "spettro/agents"
	"spettro/internal/config"
	"spettro/internal/provider"
)

// stubLSPAvailable makes the workspace look like it has (or lacks) a language
// server for the duration of a test.
func stubLSPAvailable(t *testing.T, ok bool) {
	t.Helper()
	saved := lspAvailable
	lspAvailable = func(string) bool { return ok }
	t.Cleanup(func() { lspAvailable = saved })
}

// codingSurface builds the default coding agent's tool surface.
func codingSurface(t *testing.T) (*toolRuntime, map[string]struct{}) {
	t.Helper()
	return agentSurface(t, "coding")
}

// agentSurface builds a default agent's tool surface, with its built-in
// prompt.
func agentSurface(t *testing.T, id string) (*toolRuntime, map[string]struct{}) {
	t.Helper()
	m := config.DefaultAgentManifest()
	spec, ok := m.AgentByID(id)
	if !ok {
		t.Fatalf("%s agent missing", id)
	}
	prompt, ok := agentprompts.Prompt(spec.PromptFile)
	if !ok {
		t.Fatalf("%s agent prompt %q missing", id, spec.PromptFile)
	}
	allowedTools, policies := resolveToolPolicies(spec, &m)
	r := &toolRuntime{cwd: t.TempDir(), toolPolicies: policies, manifest: &m}
	r.surface = r.buildToolSurface(allowedTools, prompt)
	allowed := map[string]struct{}{}
	for _, id := range allowedTools {
		allowed[id] = struct{}{}
	}
	return r, allowed
}

func specNames(specs []provider.ToolSpec) []string {
	names := make([]string, len(specs))
	for i, s := range specs {
		names[i] = s.Name
	}
	return names
}

// The coding agent advertises its core tools up front and defers the rest.
func TestCodingAgentAdvertisesCoreToolsOnly(t *testing.T) {
	stubLSPAvailable(t, true)
	r, _ := codingSurface(t)
	wantCore := []string{
		"agent", "glob", "grep", "file-read", "file-write", "file-edit", "lsp", "bash",
		"job-output", "job-kill", "tool-search", "todo-write", "ask-user", "comment",
		"web-fetch", "tool-output",
		// Named by the coding prompt.
		"view-image",
	}
	wantDeferred := []string{
		"task-stop", "config", "send-message", "skill-read", "skill-list", "save-memory",
		"download", "rename-symbol", "pty-start", "pty-write", "pty-kill",
	}
	got := specNames(r.surface.specs())
	if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(wantCore))) {
		t.Errorf("advertised %v, want %v", got, wantCore)
	}
	deferred := r.surface.deferredNames()
	if !slices.Equal(slices.Sorted(slices.Values(deferred)), slices.Sorted(slices.Values(wantDeferred))) {
		t.Errorf("deferred %v, want %v", deferred, wantDeferred)
	}
	// Every held tool is either advertised or deferred: none is lost.
	if n := len(got) + len(deferred); n != len(codingAgentAdvertisedTools) {
		t.Errorf("core %d + deferred %d = %d tools, want the %d the agent holds", len(got), len(deferred), n, len(codingAgentAdvertisedTools))
	}
}

// Without a language server the lsp tools are neither advertised, deferred
// nor found, and the prompt says so.
func TestLSPToolsHiddenWithoutServer(t *testing.T) {
	stubLSPAvailable(t, false)
	r, allowed := codingSurface(t)
	for _, name := range []string{"lsp", "rename-symbol"} {
		if slices.Contains(specNames(r.surface.specs()), name) || slices.Contains(r.surface.deferredNames(), name) {
			t.Errorf("%s offered without a language server", name)
		}
	}
	out, err := r.runToolSearch(allowed, []byte(`{"query":"lsp, rename-symbol"}`))
	if err != nil {
		t.Fatal(err)
	}
	if out != "no tools matched" {
		t.Errorf("tool-search found lsp tools without a server:\n%s", out)
	}
	note := toolSurfacePrompt(r.surface.deferredNames(), len(r.surface.droppedNames()) > 0)
	if !strings.Contains(note, "No language server") || strings.Contains(note, "rename-symbol") {
		t.Errorf("prompt note = %q", note)
	}
	// An agent that never held the lsp tools gets no such note.
	plain := &toolRuntime{cwd: t.TempDir()}
	plain.surface = plain.buildToolSurface([]string{"file-read", "bash"}, "")
	if len(plain.surface.droppedNames()) != 0 {
		t.Errorf("dropped %v from an agent without lsp tools", plain.surface.droppedNames())
	}
}

// tool-search loads a deferred tool: its schema comes back and it is
// advertised from then on, after the core tools. Tools the agent does not
// hold are never found.
func TestToolSearchLoadsDeferredTools(t *testing.T) {
	stubLSPAvailable(t, true)
	r, allowed := codingSurface(t)
	out, err := r.runToolSearch(allowed, []byte(`{"query":"select:save-memory,download"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := parseToolSearchActivated(out); !slices.Equal(got, []string{"download", "save-memory"}) {
		t.Fatalf("activated %v:\n%s", got, out)
	}
	if !strings.Contains(out, "## download") || !strings.Contains(out, `"url"`) {
		t.Errorf("schema missing from the result:\n%s", out)
	}
	// Activated tools follow the core ones in allow-list order.
	specs := specNames(r.surface.specs())
	if tail := specs[len(specs)-2:]; !slices.Equal(tail, []string{"save-memory", "download"}) {
		t.Errorf("advertised tail = %v", tail)
	}
	// A second search lists it as available instead of loading it again.
	again, _ := r.runToolSearch(allowed, []byte(`{"query":"download"}`))
	if parseToolSearchActivated(again) != nil || !strings.Contains(again, "- download (available") {
		t.Errorf("second search:\n%s", again)
	}
	// A keyword finds the deferred tools it describes.
	kw, _ := r.runToolSearch(allowed, []byte(`{"query":"pty"}`))
	if got := parseToolSearchActivated(kw); !slices.Equal(got, []string{"pty-kill", "pty-start", "pty-write"}) {
		t.Errorf("keyword activated %v:\n%s", got, kw)
	}
	// web-search is deferred for agents that hold it, but coding does not.
	ws, _ := r.runToolSearch(allowed, []byte(`{"query":"web-search"}`))
	if strings.Contains(ws, "web-search") {
		t.Errorf("tool-search offered a tool the agent does not hold:\n%s", ws)
	}
	// An empty query lists without loading.
	all, _ := r.runToolSearch(allowed, []byte(`{"query":""}`))
	if parseToolSearchActivated(all) != nil || !strings.Contains(all, "- send-message (not loaded") {
		t.Errorf("empty query:\n%s", all)
	}
}

// Without tool-search there is no way to find a deferred tool, so nothing is
// deferred.
func TestNoDeferralWithoutToolSearch(t *testing.T) {
	stubLSPAvailable(t, true)
	r := &toolRuntime{cwd: t.TempDir()}
	r.surface = r.buildToolSurface([]string{"file-read", "save-memory", "view-image"}, "")
	if got := specNames(r.surface.specs()); !slices.Equal(got, []string{"file-read", "save-memory", "view-image"}) {
		t.Errorf("advertised %v", got)
	}
	if d := r.surface.deferredNames(); len(d) != 0 {
		t.Errorf("deferred %v", d)
	}
}

// requestToolNames returns the tool names of a recorded chat-completions body.
func requestToolNames(body map[string]any) []string {
	raw, _ := body["tools"].([]any)
	var names []string
	for _, tool := range raw {
		fn, _ := tool.(map[string]any)["function"].(map[string]any)
		names = append(names, fmt.Sprint(fn["name"]))
	}
	return names
}

// End to end: the first request advertises only the core tools and names the
// deferred ones in the system prompt; tool-search activates one for the next
// request; a deferred tool called by name runs and is advertised too; and a
// later turn of the conversation keeps both.
func TestRunToolLoopDeferredTools(t *testing.T) {
	stubLSPAvailable(t, true)
	pm, url, ls := newLoopServer(t,
		loopReply{toolName: "tool-search", toolArgs: `{"query":"save-memory"}`},
		loopReply{toolName: "skill-list", toolArgs: `{}`},
		loopReply{content: "done"},
		loopReply{content: "again"},
	)
	cfg := loopCfg(t, pm, url)
	cfg.AllowedTools = []string{"file-read", "tool-search", "skill-list", "save-memory", "comment"}
	res, err := runToolLoop(context.Background(), cfg)
	if err != nil || res.content != "done" {
		t.Fatalf("res = %q, err = %v", res.content, err)
	}
	reqs := ls.requests()
	if got := requestToolNames(reqs[0]); !slices.Equal(got, []string{"file-read", "tool-search", "comment"}) {
		t.Errorf("first request tools = %v", got)
	}
	system := fmt.Sprint(requestMessages(reqs[0])[0]["content"])
	if !strings.Contains(system, "# More tools") || !strings.Contains(system, "skill-list, save-memory") {
		t.Errorf("system prompt does not name the deferred tools:\n%s", system)
	}
	if got := requestToolNames(reqs[1]); !slices.Equal(got, []string{"file-read", "tool-search", "comment", "save-memory"}) {
		t.Errorf("after tool-search tools = %v", got)
	}
	for _, tr := range res.traces {
		if tr.Name == "skill-list" && tr.Status != "success" {
			t.Errorf("deferred tool called by name was refused: %s", tr.Output)
		}
	}
	// Activated tools stand in allow-list order, whatever order they were
	// loaded in, and the conversation records them.
	if got := requestToolNames(reqs[2]); !slices.Equal(got, []string{"file-read", "tool-search", "comment", "skill-list", "save-memory"}) {
		t.Errorf("after the direct call tools = %v", got)
	}
	if got := res.messages[0].LoadedTools; !slices.Equal(got, []string{"skill-list", "save-memory"}) {
		t.Errorf("recorded loaded tools = %v", got)
	}
	if s2 := fmt.Sprint(requestMessages(reqs[2])[0]["content"]); s2 != system {
		t.Error("system prompt changed between steps")
	}
	cfg.Messages = res.messages
	cfg.UserTask = "next"
	if _, err := runToolLoop(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := requestToolNames(ls.requests()[3]); !slices.Equal(got, requestToolNames(reqs[2])) {
		t.Errorf("next turn tools = %v, want the last request's %v", got, requestToolNames(reqs[2]))
	}
}

// A later turn rebuilds the exact tool list the last request carried, however
// the tools were loaded: a call by name and a tool-search in one step (whose
// live order used to differ from the replayed one), or loads a compaction
// has since summarized away.
func TestRestoredToolListMatchesLiveOne(t *testing.T) {
	stubLSPAvailable(t, true)
	live, allowed := codingSurface(t)
	var msgs []provider.Message
	msgs = append(msgs, provider.Message{Role: provider.RoleUser, Content: "task"})
	// One step: download called by name, and a tool-search for save-memory
	// (which activates while it runs, before noteCalls).
	out, err := live.runToolSearch(allowed, []byte(`{"query":"select:save-memory"}`))
	if err != nil {
		t.Fatal(err)
	}
	live.noteCalls([]string{"download", "tool-search"})
	live.recordActivations(msgs)
	msgs = append(msgs,
		provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.NativeTool{{ID: "1", Name: "download"}, {ID: "2", Name: "tool-search"}}},
		provider.Message{Role: provider.RoleUser, ToolResults: []provider.ToolResult{{ID: "1", Name: "download", Output: "ok"}, {ID: "2", Name: "tool-search", Output: out}}},
	)
	want := specNames(live.surface.specs())

	// Replayed from the calls and results alone (a conversation saved
	// before the record existed), the load order differs from the live one.
	unrecorded := slices.Clone(msgs)
	unrecorded[0].LoadedTools = nil
	restored, _ := codingSurface(t)
	restored.restoreActivations(unrecorded)
	if got := specNames(restored.surface.specs()); !slices.Equal(got, want) {
		t.Errorf("restored %v, live %v", got, want)
	}
	// After a compaction only the first message and a summary remain.
	compacted := []provider.Message{msgs[0], {Role: provider.RoleUser, Content: "summary"}}
	fromRecord, _ := codingSurface(t)
	fromRecord.restoreActivations(compacted)
	if got := specNames(fromRecord.surface.specs()); !slices.Equal(got, want) {
		t.Errorf("restored after compaction %v, live %v", got, want)
	}
}

// A held tool the agent's prompt tells the model to use is advertised up
// front: the ask agent's prompt points at web-search.
func TestPromptNamedToolsAreCore(t *testing.T) {
	stubLSPAvailable(t, true)
	r, _ := agentSurface(t, "ask")
	if got := specNames(r.surface.specs()); !slices.Contains(got, "web-search") {
		t.Errorf("ask agent advertises %v, want web-search among them", got)
	}
	if slices.Contains(r.surface.deferredNames(), "web-search") {
		t.Error("web-search deferred for the ask agent")
	}
	// Without the prompt naming it, it is deferred.
	plain := &toolRuntime{cwd: t.TempDir()}
	plain.surface = plain.buildToolSurface([]string{"file-read", "tool-search", "web-search"}, "Use `file-read`.")
	if !slices.Equal(plain.surface.deferredNames(), []string{"web-search"}) {
		t.Errorf("deferred %v, want web-search", plain.surface.deferredNames())
	}
}
