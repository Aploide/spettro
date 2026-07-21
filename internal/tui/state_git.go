package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/agent"
)

func parseNumstat(text string, totals map[string][2]int) {
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		added, _ := strconv.Atoi(parts[0])
		deleted, _ := strconv.Atoi(parts[1])
		path := strings.TrimSpace(parts[2])
		if strings.Contains(path, " -> ") {
			segs := strings.Split(path, " -> ")
			path = strings.TrimSpace(segs[len(segs)-1])
		}
		if path == "" {
			continue
		}
		curr := totals[path]
		totals[path] = [2]int{curr[0] + added, curr[1] + deleted}
	}
}

func normalizeWorkspacePath(cwd, p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		rel, err := filepath.Rel(cwd, p)
		if err == nil && !strings.HasPrefix(rel, "..") {
			p = rel
		}
	}
	p = filepath.ToSlash(filepath.Clean(p))
	if p == "." || strings.HasPrefix(p, "../") {
		return ""
	}
	return p
}

func (m *Model) markSessionEdit(path string) {
	path = normalizeWorkspacePath(m.cwd, path)
	if path == "" {
		return
	}
	if m.sessionEdits == nil {
		m.sessionEdits = map[string]struct{}{}
	}
	m.sessionEdits[path] = struct{}{}
}

func (m *Model) trackSessionEditFromTrace(t agent.ToolTrace) {
	if t.Name != "file-write" || t.Status == "running" {
		return
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(t.Args), &args); err != nil {
		return
	}
	m.markSessionEdit(args.Path)
}

// minModifiedRefreshInterval throttles how often the modified-files git query
// runs from the Update hot path. A tool-heavy run emits many traces; without
// this guard each one would spawn up to four git subprocesses synchronously on
// the Bubble Tea Update goroutine, serializing the whole UI.
const minModifiedRefreshInterval = time.Second

// minRepoScanInterval throttles how often the repo-file scan runs from the
// Update hot path. The scan walks the working directory tree to build the
// @-mention suggestion list; without throttling, every keystroke after "@"
// would re-walk the tree.
const minRepoScanInterval = 2 * time.Second

// scheduleRepoScan returns a tea.Cmd that re-walks the working directory off
// the Update goroutine, refreshing m.repoFiles so @-mention suggestions stay
// in sync with files added or removed since startup. Returns nil when a scan
// ran too recently (the suggestion list is eventually consistent, so dropping
// a redundant scan is safe).
func (m *Model) scheduleRepoScan() tea.Cmd {
	if !m.lastRepoScanAt.IsZero() && time.Since(m.lastRepoScanAt) < minRepoScanInterval {
		return nil
	}
	m.lastRepoScanAt = time.Now()
	return scanRepoFilesCmd(m.cwd)
}

// queryModifiedFiles runs the git commands needed to compute the side-panel
// branch + modified-file list. It is a pure function (no Model state) so it can
// run inside a tea.Cmd off the Update goroutine. An empty branch with nil files
// means "not a git work tree".
func queryModifiedFiles(cwd string) (branch string, files []modifiedFileEntry) {
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return "", nil
	}

	branch = readGitBranch(cwd)

	cmd = exec.Command("git", "status", "--porcelain")
	cmd.Dir = cwd
	out, err = cmd.Output()
	if err != nil {
		return branch, nil
	}

	stat := make(map[string]modifiedFileEntry)
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) < 4 {
			continue
		}
		x, y := line[0], line[1]
		path := strings.TrimSpace(line[3:])
		if strings.Contains(path, " -> ") {
			segs := strings.Split(path, " -> ")
			path = strings.TrimSpace(segs[len(segs)-1])
		}
		if path == "" {
			continue
		}
		normPath := normalizeWorkspacePath(cwd, path)
		if normPath == "" {
			continue
		}
		entry := stat[normPath]
		entry.Path = normPath
		entry.Untracked = x == '?' && y == '?'
		entry.Staged = !entry.Untracked && x != ' '
		entry.Unstaged = !entry.Untracked && y != ' '
		stat[normPath] = entry
	}

	numTotals := make(map[string][2]int)
	for _, args := range [][]string{{"diff", "--numstat"}, {"diff", "--cached", "--numstat"}} {
		d := exec.Command("git", args...)
		d.Dir = cwd
		data, derr := d.Output()
		if derr == nil {
			parseNumstat(string(data), numTotals)
		}
	}

	for path, entry := range stat {
		if v, ok := numTotals[path]; ok {
			entry.Added, entry.Deleted = v[0], v[1]
		}
		files = append(files, entry)
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return branch, files
}

// refreshModifiedFiles synchronously updates the side-panel git state. It is
// used for the one-time startup query in New() and as the message applier for
// modifiedFilesMsg. Hot-path callers should prefer scheduleModifiedRefresh.
func (m *Model) refreshModifiedFiles() {
	m.gitBranch, m.modifiedFiles = queryModifiedFiles(m.cwd)
	m.lastModifiedRefreshAt = time.Now()
}

// scheduleModifiedRefresh returns a tea.Cmd that recomputes the modified-files
// list off the Update goroutine, throttled to minModifiedRefreshInterval.
// Returns nil when a refresh ran too recently (the side panel is eventually
// consistent, so dropping a redundant refresh is safe).
func (m *Model) scheduleModifiedRefresh() tea.Cmd {
	if !m.lastModifiedRefreshAt.IsZero() && time.Since(m.lastModifiedRefreshAt) < minModifiedRefreshInterval {
		return nil
	}
	m.lastModifiedRefreshAt = time.Now()
	return refreshModifiedFilesCmd(m.cwd)
}

// refreshModifiedFilesCmd runs queryModifiedFiles in the background.
func refreshModifiedFilesCmd(cwd string) tea.Cmd {
	return func() tea.Msg {
		branch, files := queryModifiedFiles(cwd)
		return modifiedFilesMsg{branch: branch, files: files}
	}
}

func readGitBranch(cwd string) string {
	cmd := exec.Command("git", "branch", "--show-current")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err == nil {
		branch := strings.TrimSpace(string(out))
		if branch != "" {
			return branch
		}
	}

	cmd = exec.Command("git", "rev-parse", "--short", "HEAD")
	cmd.Dir = cwd
	out, err = cmd.Output()
	if err != nil {
		return "(unknown)"
	}
	hash := strings.TrimSpace(string(out))
	if hash == "" {
		return "(unknown)"
	}
	return "detached@" + hash
}

func computeFileDiff(cwd, name, argsJSON, status string) string {
	if status != "success" {
		return ""
	}
	if name != "file-write" && name != "file-edit" && name != "multi-edit" {
		return ""
	}
	var args struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(argsJSON), &args) != nil || strings.TrimSpace(args.Path) == "" {
		return ""
	}
	return gitPathDiff(cwd, strings.TrimSpace(args.Path))
}

// gitPathDiff returns a unified diff for path against HEAD: working-tree
// first, then staged, then an all-additions pseudo-diff for untracked files.
func gitPathDiff(cwd, path string) string {
	// Try working-tree diff vs HEAD (covers modified tracked files).
	cmd := exec.Command("git", "diff", "HEAD", "--", path)
	cmd.Dir = cwd
	if out, err := cmd.Output(); err == nil && len(strings.TrimSpace(string(out))) > 0 {
		return string(out)
	}

	// Try staged diff vs HEAD (file was git-added before we see the trace).
	cmd2 := exec.Command("git", "diff", "--cached", "--", path)
	cmd2.Dir = cwd
	if out2, err2 := cmd2.Output(); err2 == nil && len(strings.TrimSpace(string(out2))) > 0 {
		return string(out2)
	}

	// New / untracked file: format entire content as additions.
	absPath := path
	if !filepath.IsAbs(path) {
		absPath = filepath.Join(cwd, path)
	}
	content, err := exec.Command("git", "ls-files", "--others", "--exclude-standard", "--", absPath).Output()
	if err != nil || len(strings.TrimSpace(string(content))) == 0 {
		return ""
	}
	data, readErr := os.ReadFile(absPath)
	if readErr != nil {
		return ""
	}
	return buildNewFileDiff(path, string(data))
}

// computeFileDiffCmd runs computeFileDiff off the Update goroutine and returns
// a toolDiffMsg carrying the diff plus the seq of the tool entry it belongs to.
// computeFileDiff itself early-returns "" for non file-write/file-edit tools,
// so the cmd is cheap to spawn for every completed tool. When the diff is empty
// the cmd still resolves (the handler simply finds nothing to attach).
func computeFileDiffCmd(seq int, cwd, name, argsJSON, status string) tea.Cmd {
	return func() tea.Msg {
		return toolDiffMsg{seq: seq, diff: computeFileDiff(cwd, name, argsJSON, status)}
	}
}

func buildNewFileDiff(path, content string) string {
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n", path, len(lines))
	for _, l := range lines {
		sb.WriteString("+")
		sb.WriteString(l)
		sb.WriteString("\n")
	}
	return sb.String()
}

// handleDiffCommand implements /diff [path…]: it pushes a colored diff view of
// the files modified this session (per git), or of the given paths, into the
// transcript as a Kind:"diff" system message.
func (m Model) handleDiffCommand(input string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(input)
	var paths []string
	if len(fields) > 1 {
		paths = fields[1:]
	} else {
		m.refreshModifiedFiles()
		for _, f := range m.modifiedFiles {
			paths = append(paths, f.Path)
		}
	}
	if len(paths) == 0 {
		m.showBanner("no modified files in the working tree", "info")
		return m, nil
	}
	var parts []string
	for _, p := range paths {
		if d := gitPathDiff(m.cwd, p); strings.TrimSpace(d) != "" {
			parts = append(parts, strings.TrimRight(d, "\n"))
		}
	}
	if len(parts) == 0 {
		m.showBanner("no diffs to show", "info")
		return m, nil
	}
	m.messages = append(m.messages, ChatMessage{
		Role:    RoleSystem,
		Kind:    "diff",
		Content: strings.Join(parts, "\n"),
		At:      time.Now(),
	})
	m.autoSaveDebounced()
	m.refreshViewport()
	return m, nil
}
