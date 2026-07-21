package tui

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
)

func (m *Model) syncInputSuggestions() tea.Cmd {
	val := m.ta.Value()
	if strings.HasPrefix(val, "/") {
		if strings.HasPrefix(val, "/permission") && len(val) > len("/permission") {
			filter := strings.TrimPrefix(val, "/permission")
			filter = strings.TrimPrefix(filter, " ")
			var items []commandDef
			for _, c := range permissionCommands {
				if filter == "" || strings.Contains(c.name, filter) || strings.Contains(c.desc, filter) {
					items = append(items, c)
				}
			}
			m.cmdItems = items
			if m.cmdCursor >= len(m.cmdItems) {
				m.cmdCursor = 0
			}
			m.mentionItems = nil
			m.mentionCursor = 0
			return nil
		}
		if strings.HasPrefix(val, "/thinking") && len(val) > len("/thinking") && m.activeModelSupportsReasoning() {
			filter := strings.TrimPrefix(val, "/thinking")
			filter = strings.TrimPrefix(filter, " ")
			var items []commandDef
			for _, c := range thinkingCommands {
				if filter == "" || strings.Contains(c.name, filter) || strings.Contains(c.desc, filter) {
					items = append(items, c)
				}
			}
			m.cmdItems = items
			if m.cmdCursor >= len(m.cmdItems) {
				m.cmdCursor = 0
			}
			m.mentionItems = nil
			m.mentionCursor = 0
			return nil
		}
		if strings.HasPrefix(val, "/think") && !strings.HasPrefix(val, "/thinking") && len(val) > len("/think") && m.activeModelSupportsReasoning() {
			filter := strings.TrimPrefix(val, "/think")
			filter = strings.TrimPrefix(filter, " ")
			var items []commandDef
			for _, c := range thinkCommands {
				if filter == "" || strings.Contains(c.name, filter) || strings.Contains(c.desc, filter) {
					items = append(items, c)
				}
			}
			m.cmdItems = items
			if m.cmdCursor >= len(m.cmdItems) {
				m.cmdCursor = 0
			}
			m.mentionItems = nil
			m.mentionCursor = 0
			return nil
		}
		if strings.HasPrefix(val, "/skill") && !strings.HasPrefix(val, "/skills") && len(val) > len("/skill") {
			filter := strings.TrimPrefix(val, "/skill")
			filter = strings.TrimPrefix(filter, " ")
			var items []commandDef
			for _, c := range skillCommands {
				if filter == "" || strings.Contains(c.name, filter) || strings.Contains(c.desc, filter) {
					items = append(items, c)
				}
			}
			m.cmdItems = items
			if m.cmdCursor >= len(m.cmdItems) {
				m.cmdCursor = 0
			}
			m.mentionItems = nil
			m.mentionCursor = 0
			return nil
		}
		query := val[1:]
		m.cmdItems = m.filterCommands(query)
		if m.cmdCursor >= len(m.cmdItems) {
			m.cmdCursor = 0
		}
		m.mentionItems = nil
		m.mentionCursor = 0
		return nil
	}

	m.cmdItems = nil
	m.cmdCursor = 0

	query, ok := activeMentionQuery(val)
	if !ok {
		m.mentionItems = nil
		m.mentionCursor = 0
		return nil
	}

	m.mentionItems = filterMentionFiles(m.repoFiles, query, 8)
	if m.mentionCursor >= len(m.mentionItems) {
		m.mentionCursor = 0
	}
	// Trigger a background re-scan so newly added/removed files show up
	// in the @-mention list. Throttled by scheduleRepoScan.
	return m.scheduleRepoScan()
}

func activeMentionQuery(input string) (string, bool) {
	lastSpace := strings.LastIndexAny(input, " \n\t")
	token := input
	if lastSpace >= 0 {
		token = input[lastSpace+1:]
	}
	if !strings.HasPrefix(token, "@") {
		return "", false
	}
	return strings.TrimPrefix(token, "@"), true
}

func filterMentionFiles(files []string, query string, limit int) []string {
	q := strings.ToLower(strings.TrimSpace(query))
	var dirs, regular []string
	for _, f := range files {
		if q != "" && !strings.Contains(strings.ToLower(f), q) {
			continue
		}
		if strings.HasSuffix(f, "/") {
			dirs = append(dirs, f)
		} else {
			regular = append(regular, f)
		}
	}
	out := append(dirs, regular...)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (m Model) acceptMention() Model {
	if len(m.mentionItems) == 0 {
		return m
	}
	chosen := m.mentionItems[m.mentionCursor]
	current := m.ta.Value()
	lastSpace := strings.LastIndexAny(current, " \n\t")
	prefix := ""
	if lastSpace >= 0 {
		prefix = current[:lastSpace+1]
	}
	m.ta.SetValue(prefix + "@" + chosen + " ")
	m.mentionItems = nil
	m.mentionCursor = 0
	return m
}

func (m *Model) pushInputHistory(input string) {
	if strings.TrimSpace(input) == "" {
		return
	}
	m.inputHistory = append(m.inputHistory, input)
	m.historyBrowsing = false
	m.historyIndex = -1
	m.historyDraft = ""
}

func (m *Model) recallPreviousInput() bool {
	if len(m.inputHistory) == 0 {
		return false
	}
	if !m.historyBrowsing {
		m.historyDraft = m.ta.Value()
		m.historyIndex = len(m.inputHistory) - 1
		m.historyBrowsing = true
	} else if m.historyIndex > 0 {
		m.historyIndex--
	}
	m.ta.SetValue(m.inputHistory[m.historyIndex])
	return true
}

func (m *Model) recallNextInput() bool {
	if !m.historyBrowsing || len(m.inputHistory) == 0 {
		return false
	}
	if m.historyIndex < len(m.inputHistory)-1 {
		m.historyIndex++
		m.ta.SetValue(m.inputHistory[m.historyIndex])
		return true
	}
	m.ta.SetValue(m.historyDraft)
	m.historyBrowsing = false
	m.historyIndex = -1
	m.historyDraft = ""
	return true
}

// Caps for scanRepoFiles so that launching spettro in a huge directory (e.g.
// $HOME) cannot walk millions of entries. Vars so tests can shrink them.
var (
	scanMaxEntries = 20_000  // collected entries
	scanMaxVisited = 100_000 // visited paths, including ignored ones
)

func scanRepoFiles(root string) ([]string, error) {
	gi := newGitignoreMatcher(root)
	var entries []string
	visited := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		visited++
		if visited > scanMaxVisited || len(entries) >= scanMaxEntries {
			return filepath.SkipAll
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".spettro", "node_modules":
				return filepath.SkipDir
			}
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		if rel == "." {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if gi.Ignored(relSlash, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			entries = append(entries, relSlash+"/")
		} else {
			entries = append(entries, relSlash)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(entries)
	return entries, nil
}

// repoFilesScannedMsg delivers the result of the background repo file scan.
type repoFilesScannedMsg struct{ files []string }

// scanRepoFilesCmd runs scanRepoFiles off the UI thread so startup never
// blocks on the size of the working directory.
func scanRepoFilesCmd(root string) tea.Cmd {
	return func() tea.Msg {
		files, _ := scanRepoFiles(root)
		return repoFilesScannedMsg{files: files}
	}
}

func (m Model) extractMentionedFiles(input string) []string {
	seen := map[string]struct{}{}
	for _, part := range strings.Fields(input) {
		if !strings.HasPrefix(part, "@") {
			continue
		}
		p := strings.TrimPrefix(part, "@")
		p = strings.TrimSpace(strings.Trim(p, `"'.,;:!?()[]{}<>`))
		if p == "" {
			continue
		}
		resolved := resolveMentionPaths(m.cwd, p)
		for _, rel := range resolved {
			seen[rel] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for rel := range seen {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}

func resolveMentionPaths(cwd, p string) []string {
	var abs string
	if filepath.IsAbs(p) {
		abs = filepath.Clean(p)
	} else {
		abs = filepath.Clean(filepath.Join(cwd, strings.TrimSuffix(p, "/")))
	}
	rel, err := filepath.Rel(cwd, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil
	}
	if !info.IsDir() {
		return []string{filepath.ToSlash(rel)}
	}
	gi := newGitignoreMatcher(cwd)
	var files []string
	_ = filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		frel, err := filepath.Rel(cwd, path)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(frel)
		if gi.Ignored(relSlash, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			files = append(files, relSlash)
		}
		return nil
	})
	return files
}

func injectMentionGuidance(input string, mentionedFiles []string) string {
	if len(mentionedFiles) == 0 {
		return input
	}
	var sb strings.Builder
	sb.WriteString(input)
	sb.WriteString("\n\nReferenced paths from @mentions (read these before making decisions):\n")
	for _, p := range mentionedFiles {
		sb.WriteString("- ")
		sb.WriteString(p)
		sb.WriteString("\n")
	}
	return sb.String()
}
