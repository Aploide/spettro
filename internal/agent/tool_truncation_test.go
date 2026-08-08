package agent

import (
	"testing"

	"spettro/internal/provider"
)

func TestToolBatchArgsTruncated(t *testing.T) {
	if toolBatchArgsTruncated([]provider.NativeTool{
		{Name: "file-read", Args: []byte(`{"path":"a.go"}`)},
	}) {
		t.Fatal("valid args must not look truncated")
	}
	if !toolBatchArgsTruncated([]provider.NativeTool{
		{Name: "file-read", Args: []byte(`{"path":"a.go"`)}, // missing closing brace
	}) {
		t.Fatal("invalid JSON must look truncated")
	}
	if !toolBatchArgsTruncated([]provider.NativeTool{
		{Name: "file-read", Args: []byte{}},
	}) {
		t.Fatal("empty args must look truncated")
	}
	// Mixed batch: one bad call means the batch is unsafe under length-stop.
	if !toolBatchArgsTruncated([]provider.NativeTool{
		{Name: "file-read", Args: []byte(`{"path":"a.go"}`)},
		{Name: "file-edit", Args: []byte(`{"path":"a.go","old_string":"x"`)},
	}) {
		t.Fatal("batch with one truncated call must report truncated")
	}
}

func TestFailTruncatedToolCallsShapesErrors(t *testing.T) {
	got := failTruncatedToolCalls([]toolCall{
		{Tool: "file-read", Args: []byte(`{"path":`)},
	})
	if len(got) != 1 || got[0].status != "error" || got[0].name != "file-read" {
		t.Fatalf("unexpected results: %+v", got)
	}
}
