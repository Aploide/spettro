package agent

import (
	"strings"
	"testing"
)

func TestToolOutputHistoryLimit(t *testing.T) {
	if got := toolOutputHistoryLimit("file-read"); got != 40000 {
		t.Errorf("file-read limit = %d, want 40000", got)
	}
	if got := toolOutputHistoryLimit("comment"); got != 2000 {
		t.Errorf("default limit = %d, want 2000", got)
	}
	if toolOutputHistoryLimit("file-read") <= toolOutputHistoryLimit("comment") {
		t.Error("read tools should get a larger budget than the default")
	}
}

func TestSummarizeLoopToolResultPreservesNewlines(t *testing.T) {
	out := "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}"
	got := summarizeLoopToolResult("file-read", `{"path":"main.go"}`, "ok", out, toolOutputHistoryLimit("file-read"))
	if !strings.Contains(got, "\n") {
		t.Fatalf("expected newlines preserved in file-read output, got %q", got)
	}
	if !strings.Contains(got, "func main()") {
		t.Fatalf("expected file content retained, got %q", got)
	}
}

func TestSummarizeLoopToolResultBoundsLength(t *testing.T) {
	huge := strings.Repeat("x", 50000)
	got := summarizeLoopToolResult("file-read", "", "ok", huge, toolOutputHistoryLimit("file-read"))
	// 40000 cap for file-read, plus the short "status=ok | output=" prefix and a
	// truncation marker.
	if len(got) > 40200 {
		t.Fatalf("file-read output not bounded: len=%d", len(got))
	}
	if len(got) < 20000 {
		t.Fatalf("file-read output truncated too aggressively: len=%d", len(got))
	}
}
