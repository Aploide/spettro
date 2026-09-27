package provider

// Decoder-only benchmarks: the native client's stream handling without a
// network round trip, so the numbers are CPU cost alone.

import (
	"bytes"
	"strings"
	"testing"

	wire "spettro/internal/provider/wire/chatcompletions"
)

// decodeReply runs a complete SSE reply through the native stream state.
func decodeReply(b *testing.B, reply []byte) Response {
	c := newStreamCollector(func(StreamEvent) {})
	state := newCompatStream(c)
	events := wire.NewEventReader(bytes.NewReader(reply))
	defer events.Release()
	var chunk wire.Chunk
	for events.Next() {
		data := events.Data()
		if len(data) == 0 || bytes.HasPrefix(data, []byte("[DONE]")) {
			continue
		}
		if err := wire.DecodeChunk(data, &chunk); err != nil {
			b.Fatal(err)
		}
		if err := state.chunk(&chunk); err != nil {
			b.Fatal(err)
		}
	}
	state.end()
	return c.response("p", "m", 0)
}

func BenchmarkDecodeBigToolArgs(b *testing.B) {
	reply := []byte(bigToolCallSSE(50000, 12000))
	b.SetBytes(int64(len(reply)))
	b.ReportAllocs()
	for b.Loop() {
		if resp := decodeReply(b, reply); len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ArgsError != "" {
			b.Fatalf("%+v", resp.ToolCalls)
		}
	}
}

func BenchmarkDecodeStreamAnswer(b *testing.B) {
	var sb strings.Builder
	piece := strings.Repeat("x", 15) + " "
	for range 20000 {
		sb.WriteString(sseData(chunkWith(map[string]any{"content": piece}, nil)))
	}
	sb.WriteString(sseData(chunkWith(map[string]any{}, "stop")))
	reply := []byte(sb.String())
	b.SetBytes(int64(len(reply)))
	b.ReportAllocs()
	for b.Loop() {
		if resp := decodeReply(b, reply); len(resp.Content) != 20000*16 {
			b.Fatalf("content %d bytes", len(resp.Content))
		}
	}
}
