package provider

// Benchmarks for the provider wire path. They correspond to the profiling
// harness of the performance plan (scratchpad perf/loop/harness,
// zz_perf_provider_test.go): BenchmarkSendStream is PerfSendStream,
// BenchmarkSendWithImages is PerfSendWithImages, BenchmarkLookup is
// PerfModelsLookup, BenchmarkBuildFantasyCall is PerfBuildFantasyCall and
// BenchmarkBigToolArgs is the agent-level TestPerfBigToolArgs reduced to
// the provider. Each Send benchmark runs on both clients ("native" and
// "fantasy") against an instant local server, so the numbers are the
// client's own cost.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var benchWires = []WireMode{WireNative, WireFantasy}

// instantSSE serves the given reply to every request, in one write with a
// Content-Length like the profiling harness's fake server (a chunked reply
// written 4 KB at a time would measure loopback round trips instead of
// the client).
func instantSSE(b *testing.B, reply string) *httptest.Server {
	b.Helper()
	payload := []byte(reply)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	b.Cleanup(srv.Close)
	return srv
}

func benchManager(srv *httptest.Server, mode WireMode, vision bool) *Manager {
	pm := NewManager()
	pm.SetStreamAll(true)
	pm.SetWireMode(string(mode))
	pm.AddLocalModels([]Model{{Provider: srv.URL, Name: "m", Local: true, Vision: vision, ToolCall: true}})
	return pm
}

// BenchmarkSendStream resends one unchanged request: for the native
// client that is the encoder's best case, nothing new to encode.
// BenchmarkSendStep measures a real step.
func BenchmarkSendStream(b *testing.B) {
	srv := instantSSE(b, okSSE)
	for _, n := range []int{10, 100, 500} {
		req := guardRequest(n)
		req.OnStream = func(StreamEvent) {}
		for _, mode := range benchWires {
			pm := benchManager(srv, mode, false)
			b.Run(fmt.Sprintf("%s/hist=%d", mode, n), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if _, err := pm.Send(context.Background(), srv.URL, "m", req); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkSendStep is Manager.Send for one agent step at n messages: the
// last two messages (a tool call and its result) are new on every
// iteration, as on every step of a run, so the native encoder encodes those
// two and reuses the rest. It is the plan's "Manager.Send at n msgs".
func BenchmarkSendStep(b *testing.B) {
	srv := instantSSE(b, okSSE)
	for _, n := range []int{10, 100, 500} {
		for _, mode := range benchWires {
			req := guardRequest(n)
			req.Messages = slices.Clone(req.Messages)
			req.OnStream = func(StreamEvent) {}
			pm := benchManager(srv, mode, false)
			b.Run(fmt.Sprintf("%s/hist=%d", mode, n), func(b *testing.B) {
				b.ReportAllocs()
				step := 0
				for b.Loop() {
					step++
					replaceLastStep(req.Messages, step)
					if _, err := pm.Send(context.Background(), srv.URL, "m", req); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// replaceLastStep overwrites the last two messages of msgs, in place, with
// a new tool call and its result.
func replaceLastStep(msgs []Message, step int) {
	id := "step_" + strconv.Itoa(step)
	msgs[len(msgs)-2] = Message{Role: RoleAssistant, Content: "Next file.", ToolCalls: []NativeTool{{ID: id, Name: "file-read", Args: json.RawMessage(`{"path":"pkg/step.go"}`)}}}
	msgs[len(msgs)-1] = Message{Role: RoleUser, ToolResults: []ToolResult{{ID: id, Name: "file-read", Output: "package step // " + id}}}
}

// BenchmarkEncodeStep is the request encoding of one agent step at n
// messages: the native encoder with its cache warm (the history grew by two
// messages since the last step) against fantasy's call construction.
func BenchmarkEncodeStep(b *testing.B) {
	for _, n := range []int{10, 100, 500} {
		req := guardRequest(n)
		short := req
		short.Messages = req.Messages[:len(req.Messages)-2]
		b.Run(fmt.Sprintf("native/hist=%d", n), func(b *testing.B) {
			enc := &chatEncoder{}
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				r := req
				if i%2 == 1 {
					r = short
				}
				i++
				enc.encode("p", "m", r)
			}
		})
		b.Run(fmt.Sprintf("fantasy-call/hist=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = buildFantasyCall("http://x", "", "m", req)
			}
		})
	}
}

func BenchmarkBuildFantasyCall(b *testing.B) {
	for _, n := range []int{10, 100, 500} {
		req := guardRequest(n)
		b.Run(fmt.Sprint("hist=", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = buildFantasyCall("http://x", "", "m", req)
			}
		})
	}
}

func BenchmarkLookup(b *testing.B) {
	pm := guardManager()
	b.ReportAllocs()
	for b.Loop() {
		_ = pm.ModelContext("http://127.0.0.1:1/some/long/endpoint/path", "a-long-local-model-name")
	}
}

// bigToolCallSSE is a reply with one file-write whose argsBytes of
// arguments arrive in fragments pieces.
func bigToolCallSSE(argsBytes, fragments int) string {
	line := `    fmt.Println(\"hello world from the generated file\") // filler\n`
	content := strings.Repeat(line, argsBytes/len(line)+1)[:argsBytes]
	args := `{"path":"big.go","content":"` + content + `"}`
	var sb strings.Builder
	sb.WriteString(sseData(chunkWith(toolDeltas(toolFrag(0, "call_1", "file-write", "")), nil)))
	for _, f := range fragmentArgs(args, fragments) {
		sb.WriteString(sseData(chunkWith(toolDeltas(toolFrag(0, "", "", f)), nil)))
	}
	sb.WriteString(sseData(chunkWith(map[string]any{}, "tool_calls")))
	sb.WriteString(sseData(usageChunk(100, 20, 0)))
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

func BenchmarkBigToolArgs(b *testing.B) {
	for _, frags := range []int{1000, 12000} {
		srv := instantSSE(b, bigToolCallSSE(50000, frags))
		req := guardRequest(10)
		for _, mode := range benchWires {
			pm := benchManager(srv, mode, false)
			b.Run(fmt.Sprintf("%s/fragments=%d", mode, frags), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					resp, err := pm.Send(context.Background(), srv.URL, "m", req)
					if err != nil || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ArgsError != "" {
						b.Fatalf("resp %+v, err %v", resp.ToolCalls, err)
					}
				}
			})
		}
	}
}

// BenchmarkStreamAnswer is a long streamed answer of 20,000 text chunks.
func BenchmarkStreamAnswer(b *testing.B) {
	var sb strings.Builder
	piece := strings.Repeat("x", 15) + " "
	for range 20000 {
		sb.WriteString(sseData(chunkWith(map[string]any{"content": piece}, nil)))
	}
	sb.WriteString(sseData(chunkWith(map[string]any{}, "stop")))
	sb.WriteString(sseData(usageChunk(100, 20000, 0)))
	sb.WriteString("data: [DONE]\n\n")
	srv := instantSSE(b, sb.String())
	req := guardRequest(10)
	req.OnStream = func(StreamEvent) {}
	for _, mode := range benchWires {
		pm := benchManager(srv, mode, false)
		b.Run(string(mode), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := pm.Send(context.Background(), srv.URL, "m", req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSendWithImages(b *testing.B) {
	srv := instantSSE(b, okSSE)
	dir := b.TempDir()
	req := guardRequest(100)
	for i := range 5 {
		p := filepath.Join(dir, fmt.Sprintf("shot%d.png", i))
		buf := make([]byte, 400_000)
		for j := range buf {
			buf[j] = byte(j*31 + i)
		}
		if err := os.WriteFile(p, buf, 0o644); err != nil {
			b.Fatal(err)
		}
		// Attached to tool-result turns, as view-image does.
		req.Messages[2+10*i].ToolResults[0].Images = []string{p}
	}
	req.Tools = append(req.Tools, ToolSpec{Name: "view-image", Schema: json.RawMessage(`{"type":"object"}`)})
	for _, mode := range benchWires {
		pm := benchManager(srv, mode, true)
		b.Run(fmt.Sprintf("%s/hist=100/images=5", mode), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := pm.Send(context.Background(), srv.URL, "m", req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
