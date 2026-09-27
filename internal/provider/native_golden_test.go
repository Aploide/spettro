package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"spettro/internal/models"
)

// Golden tests for the native chat-completions client: for every fixture
// request, the body it sends must be semantically equal (same JSON value)
// to the body the fantasy SDK sends for the same Request.

// okSSE is a minimal successful streamed reply.
const okSSE = "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n"

// bodyRecorder is an OpenAI-compatible server that records request bodies
// and headers and answers okSSE.
type bodyRecorder struct {
	srv     *httptest.Server
	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
	paths   []string
}

func newBodyRecorder(t *testing.T) *bodyRecorder {
	t.Helper()
	rec := &bodyRecorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.bodies = append(rec.bodies, raw)
		rec.headers = append(rec.headers, r.Header.Clone())
		rec.paths = append(rec.paths, r.URL.Path)
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, okSSE)
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (r *bodyRecorder) last(t *testing.T) any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.bodies) == 0 {
		t.Fatal("no request recorded")
	}
	var v any
	if err := json.Unmarshal(r.bodies[len(r.bodies)-1], &v); err != nil {
		t.Fatalf("request body is not JSON: %v\n%s", err, r.bodies[len(r.bodies)-1])
	}
	return v
}

// wireManager is a Manager for one local vision-capable model "m" (or the
// given name) on rec, using the given wire mode.
func wireManager(rec *bodyRecorder, mode WireMode, modelNames ...string) *Manager {
	pm := NewManager()
	pm.SetStreamAll(true)
	pm.SetWireMode(string(mode))
	if len(modelNames) == 0 {
		modelNames = []string{"m"}
	}
	var mods []Model
	for _, name := range modelNames {
		mods = append(mods, Model{Provider: rec.srv.URL, Name: name, Local: true, Vision: true, ToolCall: true, Reasoning: true})
	}
	pm.AddLocalModels(mods)
	return pm
}

// sentBody sends req through pm and returns the recorded body.
func sentBody(t *testing.T, pm *Manager, rec *bodyRecorder, providerName, modelName string, req Request) any {
	t.Helper()
	if _, err := pm.Send(context.Background(), providerName, modelName, req); err != nil {
		t.Fatalf("send: %v", err)
	}
	return rec.last(t)
}

func assertSameJSON(t *testing.T, what string, native, fantasy any) {
	t.Helper()
	if reflect.DeepEqual(native, fantasy) {
		return
	}
	n, _ := json.MarshalIndent(native, "", "  ")
	f, _ := json.MarshalIndent(fantasy, "", "  ")
	t.Fatalf("%s: native body differs from fantasy's\nnative:\n%s\nfantasy:\n%s", what, n, f)
}

// writeImage writes a small fake image and returns its path.
func writeImage(t *testing.T, dir, name string, seed byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	data := make([]byte, 300)
	for i := range data {
		data[i] = byte(i) + seed
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// goldenFixtures returns the fixture requests. provider is the endpoint
// the reasoning blocks are stamped with.
func goldenFixtures(t *testing.T, provider string) map[string]struct {
	model string
	req   Request
} {
	dir := t.TempDir()
	png := writeImage(t, dir, "a.png", 1)
	jpg := writeImage(t, dir, "b.JPG", 2)
	webp := writeImage(t, dir, "c.webp", 3)
	missing := filepath.Join(dir, "missing.png")
	schemaTool := func(name, schema string) ToolSpec {
		return ToolSpec{Name: name, Description: "tool " + name, Schema: json.RawMessage(schema)}
	}
	toolLoop := []Message{
		{Role: RoleUser, Content: "Task: fix the bug"},
		{Role: RoleAssistant, Content: "Looking.", ToolCalls: []NativeTool{
			{ID: "call_1", Name: "file-read", Args: json.RawMessage(`{"path":"a.go"}`)},
			{ID: "call_2", Name: "shell", Args: nil},
		}},
		{Role: RoleUser, ToolResults: []ToolResult{
			{ID: "call_1", Name: "file-read", Output: "package main\n\"quoted\" \\ back"},
			{ID: "call_2", Name: "shell", Output: "", IsErr: true, SpoolID: "spool:1"},
		}, Content: "also consider this"},
		{Role: RoleAssistant, ToolCalls: []NativeTool{{ID: "call_3", Name: "file-write", Args: json.RawMessage(`{"path":"b.go","content":"x"}`)}}},
		{Role: RoleUser, ToolResults: []ToolResult{{ID: "call_3", Name: "file-write", Output: "ok"}}},
	}
	mine := func(text string) ReasoningBlock { return ReasoningBlock{Text: text, Provider: provider, Model: "m"} }
	other := ReasoningBlock{Text: "other model", Provider: provider, Model: "other"}
	fixtures := map[string]struct {
		model string
		req   Request
	}{
		"system and user": {"m", Request{System: "You are Spettro.", Messages: []Message{{Role: RoleUser, Content: "hi"}}}},
		"blank system":    {"m", Request{System: " \n\t", Messages: []Message{{Role: RoleUser, Content: "hi"}}}},
		"no system":       {"m", Request{Messages: []Message{{Role: RoleUser, Content: ""}}}},
		"multi turn": {"m", Request{System: "s", Messages: []Message{
			{Role: RoleUser, Content: "one"}, {Role: RoleAssistant, Content: "two"}, {Role: RoleUser, Content: "three"},
			{Role: RoleAssistant}, {Role: RoleUser, Content: "four"},
		}}},
		"tool loop": {"m", Request{System: "s", Messages: toolLoop}},
		"reasoning replay": {"m", Request{Messages: []Message{
			{Role: RoleUser, Content: "q"},
			{Role: RoleAssistant, Content: "a", Reasoning: []ReasoningBlock{mine("first"), other, mine("last")}},
			{Role: RoleAssistant, Reasoning: []ReasoningBlock{mine("only reasoning")}},
			{Role: RoleAssistant, Content: "text", Reasoning: []ReasoningBlock{other}},
			{Role: RoleAssistant, Reasoning: []ReasoningBlock{mine("with call")}, ToolCalls: []NativeTool{{ID: "c9", Name: "grep", Args: json.RawMessage(`{"q":"x"}`)}}},
			{Role: RoleUser, ToolResults: []ToolResult{{ID: "c9", Output: "none"}}},
			{Role: RoleAssistant, Reasoning: []ReasoningBlock{mine("")}},
			{Role: RoleUser, Content: "again"},
		}}},
		"images": {"m", Request{
			Messages: []Message{
				{Role: RoleUser, Content: "look", Images: []string{png, missing, jpg}},
				{Role: RoleAssistant, Content: "seen", ToolCalls: []NativeTool{{ID: "s1", Name: "screenshot", Args: json.RawMessage(`{}`)}}},
				{Role: RoleUser, ToolResults: []ToolResult{{ID: "s1", Output: "shot", Images: []string{webp, missing}}}},
				{Role: RoleUser, Content: "now this", Images: []string{missing}},
				{Role: RoleAssistant, Content: "ok"},
			},
			Images: []string{png},
		}},
		"only missing images":   {"m", Request{Messages: []Message{{Role: RoleUser, Content: "look", Images: []string{missing}}}}},
		"prompt only":           {"m", Request{System: "ignored without messages", Prompt: "legacy prompt"}},
		"prompt with images":    {"m", Request{Prompt: "legacy", Images: []string{png, missing}}},
		"max tokens":            {"m", Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}, MaxTokens: 1234}},
		"max completion tokens": {"gpt-5-mini", Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}, MaxTokens: 777}},
		"oss max tokens":        {"gpt-oss-20b", Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}, MaxTokens: 555}},
		"tool schemas": {"m", Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}, Tools: []ToolSpec{
			schemaTool("nested", `{"type":"object","properties":{"path":{"type":"string","description":"a <b> & c"},"n":{"type":"integer","minimum":0,"maximum":1e3}},"required":["path"]}`),
			schemaTool("pretty", "{\n  \"type\": \"object\",\n  \"properties\": {}\n}"),
			schemaTool("empty", `{}`),
			schemaTool("null", `null`),
			schemaTool("array", `[1,2]`),
			schemaTool("invalid", `{"type":`),
			schemaTool("missing", ``),
			{Name: "nodesc", Schema: json.RawMessage(`{"type":"object"}`)},
		}}},
		"strange strings": {"m", Request{System: "sys     \x00 \x1f", Messages: []Message{
			{Role: RoleUser, Content: "invalid \xff\xfe utf8, emoji \U0001F600, tab\t, cr\r, quote \" backslash \\ slash / <html> &amp;"},
			{Role: RoleAssistant, Content: "x", ToolCalls: []NativeTool{{ID: "id\"1", Name: "n\\m", Args: json.RawMessage("{\"a\":\"\\u00e9\\n\"}")}}},
			{Role: RoleUser, ToolResults: []ToolResult{{ID: "id\"1", Output: "\x7f\x80 bytes"}}},
		}}},
		"not sent fields": {"m", Request{Messages: []Message{{
			Role: RoleUser, Content: "hi", SessionContext: "env", LoadedTools: []string{"x"},
			FileStamps: []FileStamp{{Path: "/a", Seen: "h"}},
		}}}},
	}
	for level, name := range map[ThinkingLevel]string{
		"": "unset", ThinkingOff: "off", ThinkingLow: "low", ThinkingMedium: "medium",
		ThinkingHigh: "high", ThinkingXHigh: "x-high", ThinkingMax: "max",
	} {
		fixtures["thinking "+name] = struct {
			model string
			req   Request
		}{"m", Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}, Thinking: level}}
	}
	return fixtures
}

func TestNativeBodyMatchesFantasy(t *testing.T) {
	rec := newBodyRecorder(t)
	fixtures := goldenFixtures(t, rec.srv.URL)
	for name, fx := range fixtures {
		t.Run(name, func(t *testing.T) {
			models := []string{fx.model}
			native := sentBody(t, wireManager(rec, WireNative, models...), rec, rec.srv.URL, fx.model, fx.req)
			fantasy := sentBody(t, wireManager(rec, WireFantasy, models...), rec, rec.srv.URL, fx.model, fx.req)
			assertSameJSON(t, name, native, fantasy)
		})
	}
}

// The Spettro Subscription and catalog OpenAI-compatible providers take the
// native client too, to the same endpoint path and with the same body.
func TestNativeBodyMatchesFantasyForCatalogAndSpettro(t *testing.T) {
	rec := newBodyRecorder(t)
	req := Request{System: "s", Messages: []Message{{Role: RoleUser, Content: "hi"}}, Thinking: ThinkingHigh, Tools: []ToolSpec{{Name: "t", Schema: json.RawMessage(`{"type":"object"}`)}}}
	setup := func(mode WireMode) *Manager {
		pm := NewManager()
		pm.SetStreamAll(true)
		pm.SetWireMode(string(mode))
		pm.SetAPIKeys(map[string]string{"compat": "key-c", spettroProviderID: "key-s"})
		pm.SetCatalog(models.Catalog{Providers: map[string]models.CatalogProvider{
			"compat": {API: models.APIOpenAI, BaseURL: rec.srv.URL + "/v1", Models: map[string]models.CatalogModel{"thinker": {Reasoning: true, ToolCall: true}}},
		}})
		pm.SetSpettro(rec.srv.URL+"/api/inference/v1/", []Model{{Provider: spettroProviderID, Name: "flash", ToolCall: true}})
		return pm
	}
	for _, target := range []struct{ provider, model, path, auth string }{
		{"compat", "thinker", "/v1/chat/completions", "Bearer key-c"},
		{spettroProviderID, "flash", "/api/inference/v1/chat/completions", "Bearer key-s"},
	} {
		native := sentBody(t, setup(WireNative), rec, target.provider, target.model, req)
		rec.mu.Lock()
		nativePath, nativeAuth, nativeUA := rec.paths[len(rec.paths)-1], rec.headers[len(rec.headers)-1].Get("Authorization"), rec.headers[len(rec.headers)-1].Get("User-Agent")
		rec.mu.Unlock()
		fantasy := sentBody(t, setup(WireFantasy), rec, target.provider, target.model, req)
		rec.mu.Lock()
		fantasyPath, fantasyUA := rec.paths[len(rec.paths)-1], rec.headers[len(rec.headers)-1].Get("User-Agent")
		rec.mu.Unlock()
		if fantasyUA != fantasyUserAgent() {
			t.Fatalf("%s: the fantasy wire mode did not use fantasy (User-Agent %q)", target.provider, fantasyUA)
		}
		assertSameJSON(t, target.provider, native, fantasy)
		if nativePath != target.path || fantasyPath != target.path {
			t.Errorf("%s: paths native=%q fantasy=%q, want %q", target.provider, nativePath, fantasyPath, target.path)
		}
		if nativeAuth != target.auth {
			t.Errorf("%s: Authorization = %q, want %q", target.provider, nativeAuth, target.auth)
		}
		if nativeUA != nativeUserAgent() {
			t.Errorf("%s: User-Agent = %q", target.provider, nativeUA)
		}
	}
}

// OPENAI_ORG_ID and OPENAI_PROJECT_ID reach the server as the SDK's
// organization and project headers on both clients, so billing and routing
// by project do not change with the wire mode.
func TestNativeSendsOpenAIEnvHeadersLikeFantasy(t *testing.T) {
	t.Setenv("OPENAI_ORG_ID", "org-123")
	t.Setenv("OPENAI_PROJECT_ID", "proj-9")
	rec := newBodyRecorder(t)
	req := Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}
	sent := map[WireMode]string{}
	for _, mode := range []WireMode{WireNative, WireFantasy} {
		sentBody(t, wireManager(rec, mode), rec, rec.srv.URL, "m", req)
		rec.mu.Lock()
		h := rec.headers[len(rec.headers)-1]
		rec.mu.Unlock()
		sent[mode] = h.Get("OpenAI-Organization") + "|" + h.Get("OpenAI-Project")
	}
	if want := "org-123|proj-9"; sent[WireNative] != want || sent[WireFantasy] != want {
		t.Fatalf("organization|project headers: native %q, fantasy %q, want %q", sent[WireNative], sent[WireFantasy], want)
	}
}

// The encoder cache must never serve a stale encoding: a growing history,
// an in-place edit of a tool result, a compaction that rewrites the prefix,
// a model switch and interleaved conversations all still produce fantasy's
// body.
func TestNativeBodyCacheStaysCorrect(t *testing.T) {
	rec := newBodyRecorder(t)
	native := wireManager(rec, WireNative, "m", "other")
	fantasyPM := wireManager(rec, WireFantasy, "m", "other")
	check := func(what, model string, req Request) {
		t.Helper()
		got := sentBody(t, native, rec, rec.srv.URL, model, req)
		want := sentBody(t, fantasyPM, rec, rec.srv.URL, model, req)
		assertSameJSON(t, what, got, want)
	}
	tools := []ToolSpec{{Name: "file-read", Description: "read", Schema: json.RawMessage(`{"type":"object"}`)}}
	msgs := []Message{{Role: RoleUser, Content: "task A"}}
	other := []Message{{Role: RoleUser, Content: "task B"}}
	for step := range 6 {
		id := fmt.Sprintf("call_%d", step)
		msgs = append(msgs,
			Message{Role: RoleAssistant, Content: "step", ToolCalls: []NativeTool{{ID: id, Name: "file-read", Args: json.RawMessage(`{"path":"x"}`)}},
				Reasoning: []ReasoningBlock{{Text: "why", Provider: rec.srv.URL, Model: "m"}}},
			Message{Role: RoleUser, ToolResults: []ToolResult{{ID: id, Output: fmt.Sprintf("output %d", step)}}})
		check(fmt.Sprintf("step %d", step), "m", Request{System: "sys", Messages: msgs, Tools: tools})
		other = append(other, Message{Role: RoleAssistant, Content: fmt.Sprint("b", step)}, Message{Role: RoleUser, Content: "more"})
		check(fmt.Sprintf("interleaved %d", step), "m", Request{System: "sys B", Messages: other})
	}
	// In-place edits of the caller's slices.
	msgs[2].ToolResults[0].Output = "rewritten in place"
	msgs[1].ToolCalls[0].Args[2] = 'P'
	msgs[3].Reasoning[0].Text = "changed"
	check("in-place edit", "m", Request{System: "sys", Messages: msgs, Tools: tools})
	// Compaction: the prefix is replaced by a summary.
	compacted := append([]Message{{Role: RoleUser, Content: "summary of earlier work"}}, msgs[5:]...)
	check("compacted", "m", Request{System: "sys", Messages: compacted, Tools: tools})
	// A shorter history on the same lane, a new system prompt and tools.
	check("truncated", "m", Request{System: "sys 2", Messages: compacted[:2]})
	// Another model: reasoning replay changes.
	check("model switch", "other", Request{System: "sys", Messages: msgs, Tools: tools})
	check("back", "m", Request{System: "sys", Messages: msgs, Tools: tools})
}
