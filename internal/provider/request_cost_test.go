package provider

// Guards for the fixed per-request costs: the token estimate, attached
// images and tool schemas. Wall-clock numbers are in the benchmarks
// (provider_bench_test.go); these pin allocation and work counts.

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spettro/internal/budget"
)

// estimateRequestTokensOracle is EstimateRequestTokens as it was before it
// counted in place: the texts collected into a slice for
// budget.EstimateTokens.
func estimateRequestTokensOracle(req Request) int {
	parts := []string{req.System, req.Prompt}
	for _, m := range req.Messages {
		parts = append(parts, m.Content)
		for _, r := range m.Reasoning {
			parts = append(parts, r.Text)
		}
		for _, tc := range m.ToolCalls {
			parts = append(parts, tc.Name, string(tc.Args))
		}
		for _, tr := range m.ToolResults {
			parts = append(parts, tr.Output)
		}
	}
	tools := 0
	if len(req.Tools) > 0 {
		var tp []string
		for _, t := range req.Tools {
			tp = append(tp, t.Name, t.Description, string(t.Schema))
		}
		tools = budget.EstimateTokens(tp...)
	}
	return budget.EstimateTokens(parts...) + tools
}

func randomText(r *rand.Rand) string {
	alphabet := []string{"a", "é", "\U0001F600", "\xff", " ", "\n", "日本", ""}
	var sb strings.Builder
	for range r.IntN(40) {
		sb.WriteString(alphabet[r.IntN(len(alphabet))])
	}
	return sb.String()
}

// The compaction and budget thresholds depend on the estimate: it must not
// change by a single token.
func TestEstimateRequestTokensMatchesBudget(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := range 2000 {
		req := Request{System: randomText(r), Prompt: randomText(r)}
		for range r.IntN(6) {
			m := Message{Role: RoleUser, Content: randomText(r)}
			for range r.IntN(3) {
				m.Reasoning = append(m.Reasoning, ReasoningBlock{Text: randomText(r)})
				m.ToolCalls = append(m.ToolCalls, NativeTool{Name: randomText(r), Args: json.RawMessage(randomText(r))})
				m.ToolResults = append(m.ToolResults, ToolResult{Output: randomText(r)})
			}
			req.Messages = append(req.Messages, m)
		}
		for range r.IntN(3) {
			req.Tools = append(req.Tools, ToolSpec{Name: randomText(r), Description: randomText(r), Schema: json.RawMessage(randomText(r))})
		}
		if got, want := EstimateRequestTokens(req), estimateRequestTokensOracle(req); got != want {
			t.Fatalf("case %d: estimate %d, want %d", i, got, want)
		}
	}
}

func TestEstimateRequestTokensDoesNotAllocate(t *testing.T) {
	req := guardRequest(500)
	if n := testing.AllocsPerRun(20, func() { _ = EstimateRequestTokens(req) }); n != 0 {
		t.Fatalf("EstimateRequestTokens allocated %v times", n)
	}
}

// guardRequest is an agent-like request with n messages of history.
func guardRequest(n int) Request {
	msgs := []Message{{Role: RoleUser, Content: "Task: refactor the handlers"}}
	for i := 0; len(msgs) < n-1; i++ {
		id := fmt.Sprintf("call_%d", i)
		msgs = append(msgs,
			Message{Role: RoleAssistant, Content: "Looking at the next file.", ToolCalls: []NativeTool{{ID: id, Name: "file-read", Args: json.RawMessage(fmt.Sprintf(`{"path":"pkg/h%d.go"}`, i))}}},
			Message{Role: RoleUser, ToolResults: []ToolResult{{ID: id, Name: "file-read", Output: strings.Repeat(fmt.Sprintf("func handler%d() {} // ünïcode\n", i), 40)}}})
	}
	msgs = append(msgs, Message{Role: RoleAssistant, Content: "done"})
	tools := make([]ToolSpec, 30)
	for i := range tools {
		tools[i] = ToolSpec{Name: fmt.Sprint("tool-", i), Description: "does things", Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}
	}
	return Request{System: strings.Repeat("You are Spettro. ", 400), Messages: msgs, Tools: tools, MaxTokens: 1000}
}

// Re-sending a request with images reads each file once, then serves it
// from the media cache until the file changes.
func TestMediaCacheRereadsChangedFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &mediaCache{limit: 1 << 20}
	url1, _, ok := c.dataURL(path)
	if !ok || url1 != "data:image/png;base64,Zmlyc3Q=" {
		t.Fatalf("data URL %q", url1)
	}
	url2, _, _ := c.dataURL(path)
	if url2 != url1 {
		t.Fatal("cached data URL changed")
	}
	if n := testing.AllocsPerRun(20, func() { _, _, _ = c.dataURL(path) }); n > 3 {
		t.Fatalf("a cached lookup allocated %v times (stat only expected)", n)
	}
	later := time.Now().Add(time.Minute)
	if err := os.WriteFile(path, []byte("second!"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(path, later, later)
	if b64, _ := c.base64(path); b64 != "c2Vjb25kIQ==" {
		t.Fatalf("changed file served stale: %q", b64)
	}
	if raw, _ := c.bytes(path); string(raw) != "second!" {
		t.Fatalf("bytes %q", raw)
	}
	if _, ok := c.bytes(filepath.Join(dir, "missing.png")); ok {
		t.Fatal("missing file reported readable")
	}
}

func TestMediaCacheEvictsToItsLimit(t *testing.T) {
	dir := t.TempDir()
	c := &mediaCache{limit: 3000}
	for i := range 10 {
		p := filepath.Join(dir, fmt.Sprintf("%d.png", i))
		if err := os.WriteFile(p, make([]byte, 1000), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, ok := c.dataURL(p); !ok {
			t.Fatal("unreadable")
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.total > c.limit || c.lru.Len() == 0 || c.lru.Len() != len(c.entries) {
		t.Fatalf("cache holds %d bytes in %d entries (map %d), limit %d", c.total, c.lru.Len(), len(c.entries), c.limit)
	}
}

// Tool schemas are parsed once per distinct schema.
func TestToolSchemasParsedOnce(t *testing.T) {
	req := guardRequest(10)
	buildFantasyCall("http://x", "", "m", req)
	if n := testing.AllocsPerRun(20, func() { _ = toolSchemas.parse(req.Tools[0].Schema) }); n != 0 {
		t.Fatalf("a cached schema lookup allocated %v times", n)
	}
	if toolSchemas.parse(json.RawMessage(`[1]`)) != nil || toolSchemas.parse(json.RawMessage(`null`)) != nil {
		t.Fatal("a non-object schema parsed as an object")
	}
}
