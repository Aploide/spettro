package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spettro/internal/provider"
)

func writeFileAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGitInfoBranchFromSubdirectory(t *testing.T) {
	root := t.TempDir()
	writeFileAt(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/feature/x\n")
	sub := filepath.Join(root, "pkg", "inner")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	gotRoot, branch, ok := gitInfo(sub)
	if !ok || gotRoot != root || branch != "feature/x" {
		t.Fatalf("gitInfo(sub) = (%q, %q, %v), want (%q, feature/x, true)", gotRoot, branch, ok, root)
	}
}

func TestGitInfoWorktreeFileAndDetachedHead(t *testing.T) {
	base := t.TempDir()
	gitDir := filepath.Join(base, "main", ".git", "worktrees", "wt")
	writeFileAt(t, filepath.Join(gitDir, "HEAD"), "0123456789abcdef0123456789abcdef01234567\n")
	wt := filepath.Join(base, "wt")
	writeFileAt(t, filepath.Join(wt, ".git"), "gitdir: ../main/.git/worktrees/wt\n")
	root, branch, ok := gitInfo(wt)
	if !ok || root != wt || branch != "detached at 0123456789ab" {
		t.Fatalf("gitInfo(worktree) = (%q, %q, %v)", root, branch, ok)
	}
}

func TestGitInfoNotARepo(t *testing.T) {
	if _, _, ok := gitInfo(t.TempDir()); ok {
		t.Skip("temp dir is inside a git repository on this machine")
	}
}

func TestEnvironmentSectionContents(t *testing.T) {
	cwd := t.TempDir()
	writeFileAt(t, filepath.Join(cwd, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFileAt(t, filepath.Join(cwd, "go.mod"), "module x\n")
	if err := os.MkdirAll(filepath.Join(cwd, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	got := buildSessionContext(cwd, t.TempDir(), now)
	for _, want := range []string{
		"# Environment",
		"- Working directory: " + cwd,
		"- Today's date: 2026-09-26",
		"- Git repository: yes (root: " + cwd + "; branch: main)",
		"- Top-level entries: go.mod, internal/",
		"Shell for the bash tool:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("environment missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, ".git/") {
		t.Errorf(".git must be hidden from the listing:\n%s", got)
	}
	if strings.Contains(got, "# Project instructions") {
		t.Errorf("no instruction files exist, section must be omitted:\n%s", got)
	}
}

func TestTopLevelListingCapped(t *testing.T) {
	cwd := t.TempDir()
	for i := range 12 {
		writeFileAt(t, filepath.Join(cwd, fmt.Sprintf("f%02d.txt", i)), "x")
	}
	got := topLevelListing(cwd, 5)
	if !strings.Contains(got, "(first 5 of 12)") || strings.Contains(got, "f05.txt") || !strings.HasSuffix(got, "…") {
		t.Fatalf("listing not capped as expected: %q", got)
	}
}

func TestInstructionsLoadedRootToCwdWithGlobalFirst(t *testing.T) {
	home := t.TempDir()
	writeFileAt(t, filepath.Join(home, ".spettro", "AGENTS.md"), "GLOBAL RULE")
	root := t.TempDir()
	writeFileAt(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFileAt(t, filepath.Join(root, "AGENTS.md"), "ROOT RULE")
	// CLAUDE.md duplicating AGENTS.md (a common setup) must load once.
	writeFileAt(t, filepath.Join(root, "CLAUDE.md"), "ROOT RULE\n")
	writeFileAt(t, filepath.Join(root, "SPETTRO.md"), "SPETTRO OVERVIEW")
	sub := filepath.Join(root, "svc")
	writeFileAt(t, filepath.Join(sub, "AGENTS.md"), "SUB RULE")
	writeFileAt(t, filepath.Join(sub, "CLAUDE.md"), "   ") // blank: skipped

	got := buildSessionContext(sub, home, time.Now())
	if !strings.Contains(got, "# Project instructions") {
		t.Fatalf("missing instructions section:\n%s", got)
	}
	order := []string{
		`<instructions file="~/.spettro/AGENTS.md">` + "\nGLOBAL RULE\n</instructions>",
		`<instructions file="../AGENTS.md">` + "\nROOT RULE\n</instructions>",
		`<instructions file="../SPETTRO.md">` + "\nSPETTRO OVERVIEW\n</instructions>",
		`<instructions file="AGENTS.md">` + "\nSUB RULE\n</instructions>",
	}
	last := -1
	for _, want := range order {
		i := strings.Index(got, want)
		if i < 0 {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
		if i < last {
			t.Fatalf("%q out of order in:\n%s", want, got)
		}
		last = i
	}
	if strings.Count(got, "ROOT RULE") != 1 {
		t.Errorf("duplicate CLAUDE.md content must be loaded once:\n%s", got)
	}
	if strings.Contains(got, `file="CLAUDE.md"`) {
		t.Errorf("blank instruction file must be skipped:\n%s", got)
	}
}

func TestInstructionsOutsideGitUseCwdOnly(t *testing.T) {
	parent := t.TempDir()
	if _, _, ok := gitInfo(parent); ok {
		t.Skip("temp dir is inside a git repository on this machine")
	}
	writeFileAt(t, filepath.Join(parent, "AGENTS.md"), "PARENT RULE")
	cwd := filepath.Join(parent, "proj")
	writeFileAt(t, filepath.Join(cwd, "CLAUDE.md"), "PROJECT RULE")
	got := buildSessionContext(cwd, "", time.Now())
	if !strings.Contains(got, "PROJECT RULE") || strings.Contains(got, "PARENT RULE") {
		t.Fatalf("outside a repo only cwd instruction files load:\n%s", got)
	}
	if !strings.Contains(got, "- Git repository: no") {
		t.Fatalf("expected non-git environment:\n%s", got)
	}
}

func TestInstructionsSizeCaps(t *testing.T) {
	cwd := t.TempDir()
	big := strings.Repeat("line of project guidance\n", 2*instructionFileMaxBytes/25)
	writeFileAt(t, filepath.Join(cwd, "AGENTS.md"), big)
	writeFileAt(t, filepath.Join(cwd, "CLAUDE.md"), big+"variant A")
	writeFileAt(t, filepath.Join(cwd, "SPETTRO.md"), big+"variant B")
	got := instructionsSection(cwd, "", "")
	if strings.Count(got, "[... truncated:") != 2 {
		t.Errorf("both loaded files are oversized and must be truncated with a note")
	}
	// The budget goes to the most specific files first; among files in one
	// directory, the last in load order (SPETTRO.md) is served first.
	if !strings.Contains(got, "Not loaded (over the size cap; read them with file-read if relevant): AGENTS.md") {
		t.Errorf("file past the total cap must be listed by path:\n%s", got[len(got)-300:])
	}
	if len(got) > instructionTotalMaxBytes+2048 {
		t.Errorf("instructions section is %d bytes, cap is %d", len(got), instructionTotalMaxBytes)
	}
}

// TestInstructionsBudgetFavorsSpecificFiles pins that large global and root
// files can't starve the cwd's own instructions, and that notes about files
// outside the working directory point at something that can read them.
func TestInstructionsBudgetFavorsSpecificFiles(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	writeFileAt(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n")
	big := strings.Repeat("line of project guidance\n", 17*1024/25)
	writeFileAt(t, filepath.Join(home, ".spettro", "AGENTS.md"), "GLOBAL\n"+big)
	writeFileAt(t, filepath.Join(root, "AGENTS.md"), "ROOT\n"+big+"root tail")
	writeFileAt(t, filepath.Join(root, "CLAUDE.md"), "ROOT CLAUDE\n"+big)
	sub := filepath.Join(root, "svc")
	writeFileAt(t, filepath.Join(sub, "SPETTRO.md"), "SVC: use pnpm test:svc")

	got := instructionsSection(sub, root, home)
	if !strings.Contains(got, `<instructions file="SPETTRO.md">`+"\nSVC: use pnpm test:svc\n</instructions>") {
		t.Fatalf("the cwd's instruction file must load whole:\n%s", got[len(got)-min(len(got), 600):])
	}
	if !strings.Contains(got, `<instructions file="../AGENTS.md">`) || !strings.Contains(got, `<instructions file="../CLAUDE.md">`) {
		t.Errorf("the root files are next in line for the budget")
	}
	rootAbs := filepath.Join(root, "AGENTS.md")
	if !strings.Contains(got, "file-read cannot open it: read the rest of "+rootAbs+" with a read-only shell command") {
		t.Errorf("truncation note for a file above cwd must name its absolute path and the shell, not file-read")
	}
	globalAbs := filepath.Join(home, ".spettro", "AGENTS.md")
	if strings.Contains(got, `file="~/.spettro/AGENTS.md"`) || !strings.Contains(got, "file-read cannot open them: read them with a read-only shell command such as sed -n if relevant): "+globalAbs) {
		t.Errorf("the global file is the least specific: it is the one left out, listed by absolute path:\n%s", got[len(got)-min(len(got), 600):])
	}
	if len(got) > instructionTotalMaxBytes+2048 {
		t.Errorf("instructions section is %d bytes, cap is %d", len(got), instructionTotalMaxBytes)
	}
}

func TestInstructionsSmallFileFitsInLeftoverBudget(t *testing.T) {
	cwd := t.TempDir()
	// Fill the budget to under 1 KiB left with two cwd files, then check a
	// tiny global file (least specific, served last) still loads whole.
	home := t.TempDir()
	writeFileAt(t, filepath.Join(home, ".spettro", "SPETTRO.md"), "TINY GLOBAL")
	writeFileAt(t, filepath.Join(cwd, "AGENTS.md"), strings.Repeat("a", instructionFileMaxBytes))
	writeFileAt(t, filepath.Join(cwd, "CLAUDE.md"), strings.Repeat("b", instructionFileMaxBytes-600))
	got := instructionsSection(cwd, "", home)
	if !strings.Contains(got, "TINY GLOBAL") || strings.Contains(got, "Not loaded") {
		t.Fatalf("a file that fits whole in the leftover budget must load:\n%s", got[len(got)-min(len(got), 400):])
	}
}

func TestGitInfoReftableHeadPlaceholder(t *testing.T) {
	root := t.TempDir()
	writeFileAt(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/.invalid\n")
	gotRoot, branch, ok := gitInfo(root)
	if !ok || gotRoot != root || branch != "" {
		t.Fatalf("reftable HEAD placeholder must read as an unknown branch, got (%q, %q, %v)", gotRoot, branch, ok)
	}
}

func TestCapInstructionTextKeepsValidUTF8(t *testing.T) {
	text := strings.Repeat("é", 100) // no newlines, 2-byte runes
	got := capInstructionText(text, 51, "hint")
	body, _, _ := strings.Cut(got, "\n[... truncated")
	if !strings.HasPrefix(text, body) || len(body)%2 != 0 {
		t.Fatalf("cut split a rune: %q", body)
	}
}

// TestSystemStringCarriesFrozenSessionContext pins the cache contract: the
// environment/instructions land in the system prompt, and editing an
// instruction file mid-conversation does not change it — the snapshot rides
// on the conversation's messages.
func TestSystemStringCarriesFrozenSessionContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	writeFileAt(t, filepath.Join(cwd, "AGENTS.md"), "Run make check before finishing.")
	cfg := toolLoopConfig{SystemPrompt: "You are a coder.", UserTask: "fix it", CWD: cwd}

	snap := sessionContextFor(cfg)
	first := buildSystemStringWith(cfg, snap)
	if !strings.HasPrefix(first, "You are a coder.") || !strings.Contains(first, "Run make check before finishing.") || !strings.Contains(first, "- Working directory: "+cwd) {
		t.Fatalf("system prompt missing session context:\n%s", first)
	}
	writeFileAt(t, filepath.Join(cwd, "AGENTS.md"), "CHANGED")
	cont := cfg
	cont.Messages = []provider.Message{{Role: provider.RoleUser, Content: "earlier turn", SessionContext: snap}}
	if again := buildSystemString(cont); again != first {
		t.Fatalf("system prompt must stay byte-stable within a conversation")
	}
	sub := cfg
	sub.DelegationDepth = 1
	sub.parentSnapshot, sub.parentCWD = snap, cwd
	if got := buildSystemString(sub); got != first {
		t.Fatalf("a sub-agent in the same directory must reuse its parent's snapshot")
	}

	user := buildInitialUserMessage(cfg)
	if strings.Contains(user, "Working directory") || strings.Contains(user, "Environment") {
		t.Fatalf("environment moved to the system prompt; the first user turn must not repeat it:\n%s", user)
	}
}

func TestBuildSystemStringNoCommentNudge(t *testing.T) {
	cfg := toolLoopConfig{SystemPrompt: "base", AllowedTools: []string{"comment", "bash"}}
	if got := buildSystemString(cfg); strings.Contains(got, "comment tool") {
		t.Fatalf("the system prompt must not push the comment tool on every step:\n%s", got)
	}
}

// TestSessionContextIsPerConversation covers long-lived hosts running
// several conversations in one directory (ACP sessions): a new conversation
// takes a fresh snapshot (instruction files written since, a new file), and
// that must not leak into another conversation that is still going — its
// system prompt, and so its provider prompt cache, stays as it began.
func TestSessionContextIsPerConversation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	pm, url, ls := newLoopServer(t, loopReply{content: "a1"}, loopReply{content: "b1"}, loopReply{content: "a2"})
	cfg := loopCfg(t, pm, url)
	writeFileAt(t, filepath.Join(cfg.CWD, "a.txt"), "a")
	resA, err := runToolLoop(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, filepath.Join(cfg.CWD, "b.txt"), "b")
	writeFileAt(t, filepath.Join(cfg.CWD, "SPETTRO.md"), "Written by /init.")
	if _, err := runToolLoop(context.Background(), cfg); err != nil { // session B starts
		t.Fatal(err)
	}
	contA := cfg
	contA.Messages = resA.messages
	contA.UserTask = "next"
	if _, err := runToolLoop(context.Background(), contA); err != nil {
		t.Fatal(err)
	}
	reqs := ls.requests()
	system := func(i int) string {
		msgs, _ := reqs[i]["messages"].([]any)
		first, _ := msgs[0].(map[string]any)
		s, _ := first["content"].(string)
		return s
	}
	if !strings.Contains(system(1), "Written by /init.") || !strings.Contains(system(1), "b.txt") {
		t.Fatalf("a new conversation must take a fresh snapshot:\n%s", system(1))
	}
	if system(2) != system(0) {
		t.Fatalf("conversation A's system prompt changed after B started:\n--- A1\n%s\n--- A2\n%s", system(0), system(2))
	}
}

// A sub-agent in another directory (a worktree) builds its own snapshot.
func TestSubAgentInOtherDirectoryBuildsItsOwnSnapshot(t *testing.T) {
	parent, worktree := t.TempDir(), t.TempDir()
	cfg := toolLoopConfig{SystemPrompt: "base", CWD: worktree, DelegationDepth: 1}
	cfg.parentSnapshot, cfg.parentCWD = sessionContextFor(toolLoopConfig{CWD: parent}), parent
	if got := sessionContextFor(cfg); !strings.Contains(got, "- Working directory: "+worktree) {
		t.Fatalf("worktree sub-agent snapshot:\n%s", got)
	}
}
