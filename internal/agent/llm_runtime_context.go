package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"spettro/internal/homedir"
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
	// with a note pointing the model at file-read.
	instructionFileMaxBytes = 16 * 1024
	// instructionTotalMaxBytes caps all instruction files together; files past
	// the cap are listed by path only.
	instructionTotalMaxBytes = 32 * 1024
	// envListingMaxEntries caps the top-level directory listing.
	envListingMaxEntries = 40
)

var (
	sessionContextMu    sync.Mutex
	sessionContextCache = map[string]string{}
)

// sessionContext returns the environment and project-instructions sections
// appended to the system prompt, built once per process per working directory
// and then frozen. Freezing is what keeps the system prompt byte-stable across
// every step and turn of a session (the provider prompt cache keys on it), so
// the date, branch and listing are a session-start snapshot and edits to
// instruction files take effect in the next session.
func sessionContext(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return ""
	}
	sessionContextMu.Lock()
	defer sessionContextMu.Unlock()
	if v, ok := sessionContextCache[cwd]; ok {
		return v
	}
	home, _ := homedir.Dir()
	v := buildSessionContext(cwd, home, time.Now())
	sessionContextCache[cwd] = v
	return v
}

func buildSessionContext(cwd, home string, now time.Time) string {
	root, branch, isGit := gitInfo(cwd)
	return environmentSection(cwd, root, branch, isGit, now) + instructionsSection(cwd, root, home)
}

func environmentSection(cwd, gitRoot, branch string, isGit bool, now time.Time) string {
	var sb strings.Builder
	sb.WriteString("\n\n# Environment\nSnapshot taken when the session started; it is not refreshed as you work.\n")
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

// topLevelListing renders the working directory's entries on one line,
// directories marked with a trailing slash, capped at max entries.
func topLevelListing(cwd string, max int) string {
	entries, err := os.ReadDir(cwd)
	if err != nil {
		return ""
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Name() == ".git" {
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
// gitdir. branch is "detached at <sha>" for a detached HEAD, "" if unreadable.
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
		return strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")
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
	content string
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
		for _, name := range projectInstructionFiles {
			abs := filepath.Join(dir, name)
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
		out = append(out, instructionFile{path: c.shown, content: text})
	}
	return out
}

func instructionsSection(cwd, gitRoot, home string) string {
	files := collectInstructionFiles(cwd, gitRoot, home)
	if len(files) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n# Project instructions\n")
	sb.WriteString("Instructions from the user and the project's maintainers, loaded from the files below (global first, then from the repository root down to the working directory; later files are more specific). Follow them: they override the default guidance above, but not explicit requests the user makes in this conversation.\n")
	used := 0
	var skipped []string
	for _, f := range files {
		// A file that doesn't fit whole gets whatever budget is left, unless
		// that is too little to be useful.
		remaining := instructionTotalMaxBytes - used
		if remaining < 1024 {
			skipped = append(skipped, f.path)
			continue
		}
		body := capInstructionText(f.content, min(instructionFileMaxBytes, remaining))
		used += len(body)
		fmt.Fprintf(&sb, "\n<instructions file=%q>\n%s\n</instructions>\n", f.path, body)
	}
	if len(skipped) > 0 {
		fmt.Fprintf(&sb, "\nNot loaded (over the size cap; read them with file-read if relevant): %s\n", strings.Join(skipped, ", "))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// capInstructionText truncates text to at most max bytes at a line boundary,
// appending a note so the model knows to read the rest itself.
func capInstructionText(text string, max int) string {
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
	return cut + fmt.Sprintf("\n[... truncated: %d more bytes; read the file for the rest]", len(text)-len(cut))
}

// resetSessionContextForTesting clears the per-process snapshot.
func resetSessionContextForTesting() {
	sessionContextMu.Lock()
	defer sessionContextMu.Unlock()
	sessionContextCache = map[string]string{}
}
