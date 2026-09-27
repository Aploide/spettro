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
	if val != m.cmdQuery {
		// A new query: highlight the best match again (see cmdQuery).
		m.cmdQuery = val
		m.cmdCursor = 0
		m.mentionCursor = 0
	}
	if strings.HasPrefix(val, "/") {
		if items, ok := m.slashSubMenu(val); ok {
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

	if query, ok := activeSkillMentionQuery(val); ok {
		m.mentionItems = m.filterSkillMentions(query, 8)
		m.mentionKind = mentionSkill
		if m.mentionCursor >= len(m.mentionItems) {
			m.mentionCursor = 0
		}
		return nil
	}

	query, ok := activeMentionQuery(val)
	if !ok {
		m.mentionItems = nil
		m.mentionCursor = 0
		return nil
	}

	m.mentionItems = filterMentionFiles(m.repoFiles, query, 8)
	m.mentionKind = mentionFile
	if m.mentionCursor >= len(m.mentionItems) {
		m.mentionCursor = 0
	}
	// Trigger a background re-scan so newly added/removed files show up
	// in the @-mention list. Throttled by scheduleRepoScan.
	return m.scheduleRepoScan()
}

// slashSubMenu returns the completion menu for a command whose argument has
// its own list of choices (/permission <level>, /thinking <level>, ...),
// filtered by what was typed after the command, and ok = true when val is
// such a command.
//
// A sub-menu opens only once the command is followed by a space. Matching
// the bare prefix would capture every name that merely starts with the
// command: /thinker, /thinking-partner and /permissions-audit (skills), or
// the built-in /permissions, would open a sub-menu instead of reaching the
// main menu where they are listed.
func (m *Model) slashSubMenu(val string) ([]commandDef, bool) {
	reasoning := m.activeModelSupportsReasoning()
	subMenus := []struct {
		command string
		items   []commandDef
		// enabled is false when the command itself is hidden: thinking
		// levels apply only to reasoning-capable models.
		enabled bool
	}{
		{"/permission", permissionCommands, true},
		{"/thinking", thinkingCommands, reasoning},
		{"/think", thinkCommands, reasoning},
		{"/skill", skillCommands, true},
	}
	for _, sub := range subMenus {
		filter, ok := strings.CutPrefix(val, sub.command+" ")
		if !ok || !sub.enabled {
			continue
		}
		var items []commandDef
		for _, c := range sub.items {
			if filter == "" || strings.Contains(c.name, filter) || strings.Contains(c.desc, filter) {
				items = append(items, c)
			}
		}
		return items, true
	}
	return nil, false
}

// mentionKind says what the mention palette is completing.
type mentionKind int

const (
	// mentionFile completes an @path mention from the repository files.
	mentionFile mentionKind = iota
	// mentionSkill completes a $skill-name mention from the skills the
	// user may run (Codex style; see skills.ExpandMentions).
	mentionSkill
)

// sigil is the character that starts a mention of this kind.
func (k mentionKind) sigil() string {
	if k == mentionSkill {
		return "$"
	}
	return "@"
}

// activeSkillMentionQuery reports whether the last token of the input is a
// $mention being typed, and returns what follows the "$".
//
// A bare "$" is not a mention yet: prompts about regexes and shells end
// with one ("lines that end with $", "echo $"), and an open palette would
// make Enter complete a skill name instead of sending the prompt. The
// palette opens from the first character after the "$".
func activeSkillMentionQuery(input string) (string, bool) {
	lastSpace := strings.LastIndexAny(input, " \n\t")
	token := input[lastSpace+1:]
	query, ok := strings.CutPrefix(token, "$")
	if !ok || query == "" {
		return "", false
	}
	return query, true
}

// filterSkillMentions returns up to limit names of user-invocable skills
// whose name contains query (case-insensitive), names starting with it
// first. An empty result closes the palette, so "$HOME" or "$5" typed in a
// prompt shows nothing unless a skill actually matches.
func (m Model) filterSkillMentions(query string, limit int) []string {
	q := strings.ToLower(query)
	var prefix, other []string
	for _, s := range m.skillCatalog().ForUser() {
		name := strings.ToLower(s.Name)
		switch {
		case strings.HasPrefix(name, q):
			prefix = append(prefix, s.Name)
		case strings.Contains(name, q):
			other = append(other, s.Name)
		}
	}
	out := append(prefix, other...)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
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
	m.ta.SetValue(prefix + m.mentionKind.sigil() + chosen + " ")
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
	for part := range strings.FieldsSeq(input) {
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
