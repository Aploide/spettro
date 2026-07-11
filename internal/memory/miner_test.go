package memory

import (
	"context"
	"strings"
	"testing"
)

func fakeComplete(out string, err error) CompleteFunc {
	return func(context.Context, string) (string, error) { return out, err }
}

func TestMineParsesFencedJSONWithProse(t *testing.T) {
	out := "Here are the candidates:\n```json\n[\n {\"fact\":\"prefers table-driven tests\",\"scope\":\"project\"},\n {\"fact\":\"answers in Italian\",\"scope\":\"user\"}\n]\n```\nDone."
	cands, err := Mine(context.Background(), []Transcript{{SessionID: "s1", Text: "user: hi"}}, "", fakeComplete(out, nil))
	if err != nil {
		t.Fatalf("mine: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want 2", len(cands))
	}
	if cands[0].Scope != ScopeProject || cands[1].Scope != ScopeUser {
		t.Fatalf("scopes wrong: %+v", cands)
	}
	if cands[0].ID == "" || len(cands[0].Sources) != 1 || cands[0].Sources[0] != "s1" {
		t.Fatalf("id/sources wrong: %+v", cands[0])
	}
}

func TestMineInvalidScopeDefaultsToUser(t *testing.T) {
	out := `[{"fact":"something","scope":"global"}]`
	cands, err := Mine(context.Background(), []Transcript{{SessionID: "s1", Text: "user: hi"}}, "", fakeComplete(out, nil))
	if err != nil || len(cands) != 1 {
		t.Fatalf("mine: %v (%d)", err, len(cands))
	}
	if cands[0].Scope != ScopeUser {
		t.Fatalf("scope = %q, want user", cands[0].Scope)
	}
}

func TestMineRejectsGarbageOutput(t *testing.T) {
	if _, err := Mine(context.Background(), []Transcript{{SessionID: "s1", Text: "user: hi"}}, "", fakeComplete("no json here", nil)); err == nil {
		t.Fatal("garbage output accepted")
	}
}

func TestMineEmptyTranscriptsIsNoop(t *testing.T) {
	called := false
	complete := func(context.Context, string) (string, error) { called = true; return "[]", nil }
	cands, err := Mine(context.Background(), nil, "", complete)
	if err != nil || cands != nil {
		t.Fatalf("expected noop, got %v/%v", cands, err)
	}
	if called {
		t.Fatal("completion called with no transcripts")
	}
	// Whitespace-only transcripts also skip the LLM call.
	cands, err = Mine(context.Background(), []Transcript{{SessionID: "s1", Text: "  "}}, "", complete)
	if err != nil || cands != nil || called {
		t.Fatalf("expected noop for empty text, got %v/%v called=%v", cands, err, called)
	}
}

func TestMinePromptIncludesExistingMemoryAndTruncates(t *testing.T) {
	var gotPrompt string
	complete := func(_ context.Context, p string) (string, error) { gotPrompt = p; return "[]", nil }
	long := strings.Repeat("x", maxTranscriptChars+500)
	if _, err := Mine(context.Background(), []Transcript{{SessionID: "s1", Text: long}}, "- already saved fact", complete); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotPrompt, "already saved fact") {
		t.Fatal("existing memory not in prompt")
	}
	if !strings.Contains(gotPrompt, "[truncated]") {
		t.Fatal("oversized transcript not truncated")
	}
}
