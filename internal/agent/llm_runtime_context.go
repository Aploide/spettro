package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"spettro/internal/homedir"
	"spettro/internal/provider"
	"spettro/internal/shell"
)

// Project instruction files, loaded from every directory between the git root
// and the working directory (root first, so deeper files read as more
// specific). SPETTRO.md is what /init writes; AGENTS.md and CLAUDE.md are the
// conventions other harnesses use, so repos set up for them work unchanged.
var projectInstructionFiles = []string{"AGENTS.md", "CLAUDE.md", "SPETTRO.md"}

// globalInstructionFiles live under ~/.spettro, next to the global memory
// file and workflows, and apply to every project.
var globalInstructionFiles = []string{"AGENTS.md", "SPETTRO.md"}

const (
	// instructionFileMaxBytes caps one instruction file; the rest is dropped
	// with a note telling the model how to read it.
	instructionFileMaxBytes = 16 * 1024
	// instructionTotalMaxBytes caps all instruction files together; files past
	// the cap are listed by path only.
	instructionTotalMaxBytes = 32 * 1024
	// envListingMaxEntries caps the top-level directory listing.
	envListingMaxEntries = 40
)

// sessionContextFor returns the environment and project-instructions sections
// appended to cfg's system prompt. They are a snapshot taken when a
// conversation starts and reused for the rest of it: the system prompt must
// stay byte-stable across every step and turn (the provider prompt cache keys
// on it), so the date, branch, listing and instruction files are not
// refreshed mid-conversation.
//
//   - A top-level run continuing a conversation reuses the snapshot carried
//     on its messages (provider.Message.SessionContext, attached by the run
//     that started it). Each conversation carries its own, so several
//     conversations in one directory (ACP sessions) never disturb each other.
//   - A top-level run with no carried snapshot — a new session, /clear, an
//     ACP session/new, or a history from before snapshots were carried —
//     takes a fresh one, so instruction files written since (e.g. by /init),
//     a branch switch or a new day show up without restarting the process.
//   - Sub-agents reuse their parent's snapshot when they run in the parent's
//     directory (keeping sibling prompts identical), and build their own for
//     another directory (a worktree).
func sessionContextFor(cfg toolLoopConfig) string {
	if cfg.DelegationDepth > 0 {
		if cfg.parentSnapshot != "" && filepath.Clean(cfg.CWD) == filepath.Clean(cfg.parentCWD) {
			return cfg.parentSnapshot
		}
		return freshSessionContext(cfg.CWD)
	}
	if snap := carriedSessionContext(cfg.Messages); snap != "" {
		return snap
	}
	return freshSessionContext(cfg.CWD)
}

// carriedSessionContext returns the snapshot a conversation carries, if any.
func carriedSessionContext(msgs []provider.Message) string {
	for _, m := range msgs {
		if m.SessionContext != "" {
			return m.SessionContext
		}
	}
	return ""
}

// freshSessionContext builds a new snapshot for cwd.
func freshSessionContext(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return ""
	}
	home, _ := homedir.Dir()
	return buildSessionContext(cwd, home, time.Now())
}

func buildSessionContext(cwd, home string, now time.Time) string {
	root, branch, isGit := gitInfo(cwd)
	return environmentSection(cwd, root, branch, isGit, now) + instructionsSection(cwd, root, home)
}

func environmentSection(cwd, gitRoot, branch string, isGit bool, now time.Time) string {
	var sb strings.Builder
	sb.WriteString("\n\n# Environment\nSnapshot taken when this conversation started; it is not refreshed as you work.\n")
	sb.WriteString("- Working directory: " + cwd + "\n")
	sb.WriteString(environmentBrief() + "\n")
	sb.WriteString("- Today's date: " + now.Format("2006-01-02") + "\n")
	switch {
	case !isGit:
		sb.WriteString("- Git repository: no\n")
	case branch != "":
		fmt.Fprintf(&sb, "- Git repository: yes (root: %s; branch: %s)\n", gitRoot, branch)
	default:
		fmt.Fprintf(&sb, "- Git repository: yes (root: %s)\n", gitRoot)
	}
	if listing := topLevelListing(cwd, envListingMaxEntries); listing != "" {
		sb.WriteString(listing + "\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// hiddenTopLevelEntries are left out of the Environment listing: git's own
// metadata, and Spettro's per-project state dir, which says nothing about the
// task and only invites the model to probe it.
var hiddenTopLevelEntries = map[string]bool{".git": true, ".spettro": true}

// topLevelListing renders the working directory's entries on one line,
// directories marked with a trailing slash, capped at max entries.
func topLevelListing(cwd string, max int) string {
	entries, err := os.ReadDir(cwd)
	if err != nil {
		return ""
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if hiddenTopLevelEntries[e.Name()] {
			continue
		}
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return "- Top-level entries: (empty directory)"
	}
	if len(names) > max {
		return fmt.Sprintf("- Top-level entries (first %d of %d): %s, …", max, len(names), strings.Join(names[:max], ", "))
	}
	return "- Top-level entries: " + strings.Join(names, ", ")
}

// gitInfo finds the repository containing dir by walking up to the nearest
// .git entry, and reads the current branch straight from HEAD (no git
// subprocess). A .git file (linked worktree or submodule) is followed to its
// gitdir. branch is "detached at <sha>" for a detached HEAD, "" if unreadable
// (or kept in a reftable, which this doesn't parse).
func gitInfo(dir string) (root, branch string, ok bool) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", "", false
	}
	for d := abs; ; {
		dotGit := filepath.Join(d, ".git")
		if fi, err := os.Stat(dotGit); err == nil {
			gitDir := dotGit
			if !fi.IsDir() {
				gitDir = resolveGitDirFile(dotGit)
			}
			return d, readGitHead(gitDir), true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", "", false
		}
		d = parent
	}
}

func resolveGitDirFile(dotGit string) string {
	data, err := os.ReadFile(dotGit)
	if err != nil {
		return ""
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return ""
	}
	target = strings.TrimSpace(target)
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(dotGit), target)
	}
	return target
}

func readGitHead(gitDir string) string {
	if gitDir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(data))
	if ref, ok := strings.CutPrefix(head, "ref:"); ok {
		branch := strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")
		if branch == ".invalid" {
			// Reftable repositories keep this placeholder in HEAD and the
			// real ref in the binary table; report the branch as unknown
			// rather than name a branch that doesn't exist.
			return ""
		}
		return branch
	}
	if len(head) >= 12 {
		return "detached at " + head[:12]
	}
	return ""
}

// instructionDirs lists the directories searched for project instruction
// files: the git root down to cwd, or just cwd outside a repository.
func instructionDirs(cwd, gitRoot string) []string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return []string{cwd}
	}
	if gitRoot == "" {
		return []string{abs}
	}
	rel, err := filepath.Rel(gitRoot, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return []string{abs}
	}
	dirs := []string{gitRoot}
	if rel == "." {
		return dirs
	}
	d := gitRoot
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		d = filepath.Join(d, part)
		dirs = append(dirs, d)
	}
	return dirs
}

type instructionFile struct {
	path    string // as shown to the model
	abs     string
	content string
	// inWorkspace is false for files file-read cannot open (global files and
	// those in directories above cwd); notes about them point at the shell.
	inWorkspace bool
}

// collectInstructionFiles reads the global and project instruction files in
// precedence order, skipping duplicates (CLAUDE.md is often a symlink to or a
// copy of AGENTS.md) and blank files.
func collectInstructionFiles(cwd, gitRoot, home string) []instructionFile {
	type candidate struct{ abs, shown string }
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	var cands []candidate
	if strings.TrimSpace(home) != "" {
		for _, name := range globalInstructionFiles {
			cands = append(cands, candidate{filepath.Join(home, ".spettro", name), "~/.spettro/" + name})
		}
	}
	for _, dir := range instructionDirs(cwd, gitRoot) {
		main, inWorktree := mainCheckoutPath(dir)
		for _, name := range projectInstructionFiles {
			abs := filepath.Join(dir, name)
			if inWorktree && !fileExists(abs) && fileExists(filepath.Join(main, name)) {
				// Uncommitted in the main checkout, so absent from this
				// agent worktree: the project's rules still apply.
				abs = filepath.Join(main, name)
			}
			shown := abs
			if rel, err := filepath.Rel(cwd, abs); err == nil {
				shown = filepath.ToSlash(rel)
			}
			cands = append(cands, candidate{abs, shown})
		}
	}
	seenPath := map[string]bool{}
	seenContent := map[string]bool{}
	var out []instructionFile
	for _, c := range cands {
		fi, err := os.Stat(c.abs)
		if err != nil || fi.IsDir() {
			continue
		}
		real := c.abs
		if r, err := filepath.EvalSymlinks(c.abs); err == nil {
			real = r
		}
		if seenPath[real] {
			continue
		}
		seenPath[real] = true
		data, err := os.ReadFile(c.abs)
		if err != nil {
			continue
		}
		text := strings.TrimSpace(string(data))
		if text == "" || seenContent[text] {
			continue
		}
		seenContent[text] = true
		rel, relErr := filepath.Rel(cwd, c.abs)
		inWorkspace := relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
		out = append(out, instructionFile{path: c.shown, abs: c.abs, content: text, inWorkspace: inWorkspace})
	}
	return out
}

func instructionsSection(cwd, gitRoot, home string) string {
	files := collectInstructionFiles(cwd, gitRoot, home)
	if len(files) == 0 {
		return ""
	}
	// The budget goes to the most specific files first (cwd, then up to the
	// root, then global), so a large global or root file can't crowd out the
	// package-level instructions that should win. A file that doesn't fit
	// whole gets whatever budget is left, unless that is too little to be
	// useful; the files are still rendered in precedence order.
	bodies := make([]string, len(files))
	loaded := make([]bool, len(files))
	used := 0
	for i := len(files) - 1; i >= 0; i-- {
		f := files[i]
		remaining := instructionTotalMaxBytes - used
		limit := min(instructionFileMaxBytes, remaining)
		if len(f.content) > limit && remaining < 1024 {
			continue
		}
		bodies[i] = capInstructionText(f.content, limit, readHint(f))
		loaded[i] = true
		used += len(bodies[i])
	}
	var sb strings.Builder
	sb.WriteString("\n\n# Project instructions\n")
	sb.WriteString("Instructions from the user and the project's maintainers, loaded from the files below (global first, then from the repository root down to the working directory; later files are more specific). Follow them: they override the default guidance above, but not explicit requests the user makes in this conversation.\n")
	var skippedIn, skippedOut []string
	for i, f := range files {
		if !loaded[i] {
			if f.inWorkspace {
				skippedIn = append(skippedIn, f.path)
			} else {
				skippedOut = append(skippedOut, f.abs)
			}
			continue
		}
		fmt.Fprintf(&sb, "\n<instructions file=%q>\n%s\n</instructions>\n", f.path, bodies[i])
	}
	if len(skippedIn) > 0 {
		fmt.Fprintf(&sb, "\nNot loaded (over the size cap; read them with file-read if relevant): %s\n", strings.Join(skippedIn, ", "))
	}
	if len(skippedOut) > 0 {
		fmt.Fprintf(&sb, "\nNot loaded (over the size cap; outside the working directory, so file-read cannot open them: read them with %s if relevant): %s\n", shellReadCommand(shell.Dialect()), strings.Join(skippedOut, ", "))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// readHint tells the model how to read the rest of a truncated file.
func readHint(f instructionFile) string {
	if f.inWorkspace {
		return "read the rest with file-read"
	}
	return "the file is outside the working directory, so file-read cannot open it: read the rest of " + f.abs + " with " + shellReadCommand(shell.Dialect())
}

// capInstructionText truncates text to at most max bytes at a line boundary,
// appending a note (ending in hint) so the model knows to read the rest.
func capInstructionText(text string, max int, hint string) string {
	if len(text) <= max {
		return text
	}
	n := max
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	cut := text[:n]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	return cut + fmt.Sprintf("\n[... truncated: %d more bytes; %s]", len(text)-len(cut), hint)
}
