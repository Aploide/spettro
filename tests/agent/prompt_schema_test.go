package agent_test

import (
	"strings"
	"testing"

	"spettro/internal/agent"
)

// The system prompt must advertise per-tool argument schemas so the model
// uses the same field names the runtime decoder expects. Without this, the
// LLM tends to guess (e.g. {"text": ...} instead of {"message": ...} for the
// comment tool), and decodeJSONStrict rejects unknown fields.
func TestBuildToolSchemaSection_ListsAllowedToolsOnly(t *testing.T) {
	got := agent.BuildToolSchemaSectionForTesting([]string{"comment", "file-read", "ls"})
	if !strings.Contains(got, "Tool argument schemas") {
		t.Fatalf("missing schema header:\n%s", got)
	}
	if !strings.Contains(got, `comment arguments: {"message": string}`) {
		t.Fatalf("comment schema not advertised:\n%s", got)
	}
	if !strings.Contains(got, `file-read arguments: {"path": string`) {
		t.Fatalf("file-read schema not advertised:\n%s", got)
	}
	if !strings.Contains(got, "ls arguments:") {
		t.Fatalf("ls schema not advertised:\n%s", got)
	}
	if strings.Contains(got, "shell-exec") || strings.Contains(got, "file-write") {
		t.Fatalf("non-allowed tools leaked into prompt:\n%s", got)
	}
}

func TestBuildToolSchemaSection_SkipsUnknownAndDuplicates(t *testing.T) {
	got := agent.BuildToolSchemaSectionForTesting([]string{"comment", "comment", "custom-mcp-tool", ""})
	occurrences := strings.Count(got, "comment arguments")
	if occurrences != 1 {
		t.Fatalf("expected exactly one comment line, got %d:\n%s", occurrences, got)
	}
	if strings.Contains(got, "custom-mcp-tool") {
		t.Fatalf("expected unknown tool to be skipped (manifest tools without schema):\n%s", got)
	}
}

func TestBuildToolSchemaSection_EmptyWhenNoMatches(t *testing.T) {
	got := agent.BuildToolSchemaSectionForTesting([]string{"unknown-1", "unknown-2"})
	if got != "" {
		t.Fatalf("expected empty section when no allowed tool has a known schema, got:\n%q", got)
	}
}
