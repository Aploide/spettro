package memory

import (
	"path/filepath"
	"testing"
)

func testInbox(t *testing.T) Inbox {
	t.Helper()
	return Inbox{Path: filepath.Join(t.TempDir(), "memory-inbox.json")}
}

func TestInboxLoadMissingIsEmpty(t *testing.T) {
	in := testInbox(t)
	cands, err := in.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("expected empty inbox, got %d", len(cands))
	}
}

func TestInboxAddDedupes(t *testing.T) {
	in := testInbox(t)
	added, err := in.Add([]Candidate{
		{Fact: "Prefers tabs", Scope: ScopeUser},
		{Fact: "prefers  TABS", Scope: ScopeUser}, // same fact, different spacing/case
		{Fact: "run make lint", Scope: ScopeProject},
	}, "")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if added != 2 {
		t.Fatalf("added = %d, want 2", added)
	}
	// Re-adding the same facts is a no-op.
	added, err = in.Add([]Candidate{{Fact: "prefers tabs", Scope: ScopeUser}}, "")
	if err != nil || added != 0 {
		t.Fatalf("re-add: added=%d err=%v, want 0/nil", added, err)
	}
	cands, _ := in.Load()
	if len(cands) != 2 {
		t.Fatalf("inbox size = %d, want 2", len(cands))
	}
	for _, c := range cands {
		if c.ID == "" || c.CreatedAt.IsZero() {
			t.Fatalf("candidate missing id/timestamp: %+v", c)
		}
	}
}

func TestInboxAddSkipsFactsAlreadyInMemory(t *testing.T) {
	in := testInbox(t)
	existing := "# Memory\n\n- prefers tabs\n- run make lint\n"
	added, err := in.Add([]Candidate{
		{Fact: "Prefers Tabs", Scope: ScopeUser},
		{Fact: "new durable fact", Scope: ScopeUser},
	}, existing)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if added != 1 {
		t.Fatalf("added = %d, want 1", added)
	}
}

func TestInboxRemove(t *testing.T) {
	in := testInbox(t)
	if _, err := in.Add([]Candidate{{Fact: "fact one"}, {Fact: "fact two"}}, ""); err != nil {
		t.Fatal(err)
	}
	cands, _ := in.Load()
	got, ok, err := in.Remove(cands[0].ID)
	if err != nil || !ok {
		t.Fatalf("remove: ok=%v err=%v", ok, err)
	}
	if got.Fact != cands[0].Fact {
		t.Fatalf("removed %q, want %q", got.Fact, cands[0].Fact)
	}
	left, _ := in.Load()
	if len(left) != 1 || left[0].Fact != cands[1].Fact {
		t.Fatalf("unexpected remainder: %+v", left)
	}
	if _, ok, _ := in.Remove("mem-does-not-exist"); ok {
		t.Fatal("remove of unknown id reported ok")
	}
}
