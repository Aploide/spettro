package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFactLine(t *testing.T) {
	// Legacy bare bullet: text only, no metadata.
	f, ok := parseFactLine("- prefers tabs")
	if !ok || f.Text != "prefers tabs" || f.ID != "" || f.Added != "" {
		t.Fatalf("legacy bullet: %+v ok=%v", f, ok)
	}
	// Stamped bullet.
	f, ok = parseFactLine("- prefers tabs <!-- id:m-a1b2c3 added:2026-01-02 used:2026-03-04 -->")
	if !ok || f.Text != "prefers tabs" || f.ID != "m-a1b2c3" || f.Added != "2026-01-02" || f.Used != "2026-03-04" {
		t.Fatalf("stamped bullet: %+v ok=%v", f, ok)
	}
	// Malformed comment: dropped, text kept, bogus dates ignored.
	f, ok = parseFactLine("- keep me <!-- id:m-x added:not-a-date whatever -->")
	if !ok || f.Text != "keep me" || f.ID != "m-x" || f.Added != "" {
		t.Fatalf("malformed comment: %+v ok=%v", f, ok)
	}
	// Headers and blanks are not facts.
	if _, ok := parseFactLine("# Spettro memory (user)"); ok {
		t.Fatal("header parsed as fact")
	}
	if _, ok := parseFactLine("   "); ok {
		t.Fatal("blank parsed as fact")
	}
}

func TestSaveExactDupeBumpsUsed(t *testing.T) {
	s := testStore(t)
	restore := today
	today = func() string { return "2026-01-01" }
	defer func() { today = restore }()

	if _, err := s.Save(ScopeUser, "prefers tabs over spaces"); err != nil {
		t.Fatal(err)
	}
	today = func() string { return "2026-06-15" }
	res, err := s.Save(ScopeUser, "  Prefers  TABS over spaces ")
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != SavedDuplicate {
		t.Fatalf("outcome = %v, want SavedDuplicate", res.Outcome)
	}
	data, _ := os.ReadFile(s.UserFile)
	got := string(data)
	if strings.Count(got, "prefers tabs") != 1 {
		t.Fatalf("duplicate appended: %q", got)
	}
	if !strings.Contains(got, "added:2026-01-01") || !strings.Contains(got, "used:2026-06-15") {
		t.Fatalf("used not bumped: %q", got)
	}
}

func TestSaveNearDupeRoutesToInbox(t *testing.T) {
	s := testStore(t)
	inbox := Inbox{Path: filepath.Join(t.TempDir(), "inbox.json")}
	s.Inbox = &inbox

	if _, err := s.Save(ScopeUser, "prefers tabs for indentation everywhere"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Save(ScopeUser, "prefers tabs for indentation in Go only")
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != SavedToInbox {
		t.Fatalf("outcome = %v, want SavedToInbox", res.Outcome)
	}
	if res.Near != "prefers tabs for indentation everywhere" {
		t.Fatalf("near = %q", res.Near)
	}
	cands, err := inbox.Load()
	if err != nil || len(cands) != 1 {
		t.Fatalf("inbox: %v %v", cands, err)
	}
	if cands[0].Supersedes != "prefers tabs for indentation everywhere" {
		t.Fatalf("supersedes = %q", cands[0].Supersedes)
	}
	// The memory file itself is unchanged.
	data, _ := os.ReadFile(s.UserFile)
	if strings.Contains(string(data), "Go only") {
		t.Fatalf("near-dupe was appended anyway: %s", data)
	}
	// SaveApproved bypasses routing.
	if res, err := s.SaveApproved(ScopeUser, "prefers tabs for indentation in Go only"); err != nil || res.Outcome != SavedNew {
		t.Fatalf("SaveApproved: %+v %v", res, err)
	}
}

func TestSupersedeReplacesFact(t *testing.T) {
	s := testStore(t)
	if _, err := s.Save(ScopeUser, "prefers tabs"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Supersede(ScopeUser, "prefers tabs", "prefers spaces"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(s.UserFile)
	got := string(data)
	if strings.Contains(got, "prefers tabs") || !strings.Contains(got, "prefers spaces") {
		t.Fatalf("supersede did not replace: %q", got)
	}
}

func TestCurateProposesAndAppliesOps(t *testing.T) {
	s := testStore(t)
	if _, err := s.Save(ScopeUser, "likes tabs"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ScopeUser, "obsolete fact about removed tooling"); err != nil {
		t.Fatal(err)
	}
	facts := s.Facts(ScopeUser)
	if len(facts) != 2 {
		t.Fatalf("facts = %d", len(facts))
	}
	staleID := facts[1].ID

	ops, err := Curate(context.Background(), facts, nil,
		func(ctx context.Context, prompt string) (string, error) {
			if !strings.Contains(prompt, "likes tabs") {
				t.Fatalf("prompt missing facts: %q", prompt)
			}
			return `[{"action":"delete","ids":["` + staleID + `"],"reason":"stale"},
			        {"action":"merge","ids":["bogus-id"],"text":"x"}]`, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	// The op referencing an unknown id is filtered out.
	if len(ops) != 1 || ops[0].Action != "delete" {
		t.Fatalf("ops = %+v", ops)
	}
	// Rejected (not applied) ops leave the file untouched.
	before, _ := os.ReadFile(s.UserFile)
	if err := s.ApplyOp(ScopeUser, ops[0]); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(s.UserFile)
	if strings.Contains(string(after), "obsolete fact") {
		t.Fatalf("delete op not applied: %s", after)
	}
	if !strings.Contains(string(after), "likes tabs") {
		t.Fatalf("unrelated fact lost: %s", after)
	}
	if string(before) == string(after) {
		t.Fatal("apply did not rewrite the file")
	}
}

func TestApplyOpMerge(t *testing.T) {
	s := testStore(t)
	restore := today
	today = func() string { return "2026-01-01" }
	if _, err := s.Save(ScopeUser, "runs make lint"); err != nil {
		t.Fatal(err)
	}
	today = func() string { return "2026-02-01" }
	if _, err := s.Save(ScopeUser, "runs make test after lint"); err != nil {
		t.Fatal(err)
	}
	today = restore
	facts := s.Facts(ScopeUser)
	op := CurateOp{Action: "merge", IDs: []string{facts[0].ID, facts[1].ID}, Text: "runs make lint then make test"}
	if err := s.ApplyOp(ScopeUser, op); err != nil {
		t.Fatal(err)
	}
	got := s.Facts(ScopeUser)
	if len(got) != 1 || got[0].Text != "runs make lint then make test" {
		t.Fatalf("merge result: %+v", got)
	}
	if got[0].Added != "2026-01-01" {
		t.Fatalf("merged fact should keep earliest added: %+v", got[0])
	}
}

func TestLoadCapDropsStalestFirst(t *testing.T) {
	s := testStore(t)
	var sb strings.Builder
	sb.WriteString(userHeader + "\n")
	sb.WriteString("- fresh important fact <!-- id:m-aaaaaa added:2026-01-01 used:2026-07-01 -->\n")
	for i := 0; i < 2000; i++ {
		sb.WriteString("- stale padding fact repeated for size <!-- id:m-bbbbbb added:2025-01-01 used:2025-01-01 -->\n")
	}
	if err := os.MkdirAll(filepath.Dir(s.UserFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.UserFile, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got := s.Load()
	if len(got) > maxFileBytes+1024 {
		t.Fatalf("not capped: %d bytes", len(got))
	}
	// The recently-used fact survives even though it sits at the head of the
	// file; the cap eats the stale tail instead.
	if !strings.Contains(got, "- fresh important fact") {
		t.Fatal("cap dropped the recently-used fact")
	}
}

func TestStaleHints(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "Makefile"), []byte("all:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	facts := []Fact{
		{ID: "m-1", Text: "build with scripts/old-build.sh", Added: "2025-01-01", Used: "2025-01-01"},
		{ID: "m-2", Text: "Makefile drives the build", Added: "2025-01-01", Used: "2025-01-01"},
		{ID: "m-3", Text: "uses scripts/gone.sh daily", Added: "2026-07-20", Used: "2026-07-20"},
	}
	hints := StaleHints(facts, cwd)
	if len(hints) != 1 || !strings.Contains(hints[0], "m-1") {
		t.Fatalf("hints = %v", hints)
	}
}
