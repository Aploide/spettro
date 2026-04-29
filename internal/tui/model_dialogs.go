package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/session"
)

var planApprovalOptions = []string{
	"Execute plan  (switch to coding agent)",
	"Don't execute",
	"Edit — tell me what to change",
}

func (m Model) updatePlanApproval(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(planApprovalOptions)
	switch msg.String() {
	case "up", "ctrl+p":
		if m.planApprovalCursor > 0 {
			m.planApprovalCursor--
		}
		return m, nil
	case "down", "ctrl+n":
		if m.planApprovalCursor < n-1 {
			m.planApprovalCursor++
		}
		return m, nil
	case "enter":
		choice := m.planApprovalCursor
		m.showPlanApproval = false
		m.planApprovalCursor = 0
		switch choice {
		case 0:
			spec, ok := m.manifest.AgentByID("coding")
			if !ok {
				m.showBanner("coding agent not found", "error")
				return m, nil
			}
			m.mode = "coding"
			m.persistUIState()
			plan := m.pendingPlan
			m.pendingPlan = ""
			return m.runAgentApproved(spec, plan, nil, nil, true)
		case 1:
			m.pendingPlan = ""
			m.showBanner("plan saved to .spettro/PLAN.md — use /approve later to execute", "info")
			return m, nil
		case 2:
			m.showBanner("describe your changes and press enter", "info")
			m.ta.Focus()
			return m, nil
		}
		return m, nil
	case "esc":
		m.showPlanApproval = false
		m.planApprovalCursor = 0
		m.showBanner("plan saved — use /approve to execute later", "info")
		return m, nil
	}
	return m, nil
}

func (m Model) handlePlanEdit(editInstruction string) (tea.Model, tea.Cmd) {
	if m.pendingPlan == "" {
		m.showBanner("no pending plan to edit", "warn")
		return m, nil
	}
	spec, ok := m.manifest.AgentByID("plan")
	if !ok {
		m.showBanner("plan agent not found", "error")
		return m, nil
	}
	task := m.pendingPlan + "\n\n---\nUser requested the following changes to the plan:\n" + editInstruction
	m.pendingPlan = ""
	return m.runAgent(spec, task, nil, nil)
}

var shellApprovalOptions = []string{
	"Allow once",
	"Allow always  (remember this command)",
	"Deny",
	"Tell the agent what to do instead",
}

const askUserFreeResponseOption = "Type my own answer"

func (m Model) updateShellApproval(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.pendingAuth == nil {
		return m, nil
	}
	if m.approvalCursor == 3 {
		switch msg.String() {
		case "enter":
			raw := strings.TrimSpace(m.ta.Value())
			if raw == "" {
				m.showBanner("type what the agent should do instead, then press enter", "warn")
				return m, nil
			}
			m = m.resolveShellApproval(agent.ShellApprovalDeny, "command denied")
			m.interruptRun("Command denied by user.", true)
			m.ta.SetValue(raw)
			return m, nil
		case "esc":
			m.approvalCursor = 0
			m.ta.Reset()
			return m, nil
		default:
			var taCmd tea.Cmd
			m.ta, taCmd = m.ta.Update(msg)
			return m, taCmd
		}
	}
	n := len(shellApprovalOptions)
	switch msg.String() {
	case "up", "ctrl+p":
		if m.approvalCursor > 0 {
			m.approvalCursor--
		}
		return m, nil
	case "down", "ctrl+n":
		if m.approvalCursor < n-1 {
			m.approvalCursor++
		}
		return m, nil
	case "enter":
		switch m.approvalCursor {
		case 0:
			return m.resolveShellApproval(agent.ShellApprovalAllowOnce, "command approved once"), nil
		case 1:
			return m.resolveShellApproval(agent.ShellApprovalAllowAlways, "command approved and saved"), nil
		case 2:
			m = m.resolveShellApproval(agent.ShellApprovalDeny, "command denied")
			m.interruptRun("Command denied by user.", true)
			return m, nil
		case 3:
			m.ta.Reset()
			m.showBanner("type what the agent should do instead, then press enter", "info")
			return m, nil
		}
	case "esc":
		m = m.resolveShellApproval(agent.ShellApprovalDeny, "command denied")
		m.interruptRun("Command denied by user.", true)
		return m, nil
	}
	return m, nil
}

func (m Model) resolveShellApproval(decision agent.ShellApprovalDecision, banner string) Model {
	if m.pendingAuth != nil {
		select {
		case m.pendingAuth.response <- shellApprovalResponse{decision: decision}:
		default:
		}
	}
	m.pendingAuth = nil
	m.approvalCursor = 0
	m.ta.Reset()
	m.showBanner(banner, "info")
	m.refreshViewport()
	return m
}

func askUserOptions(req agent.AskUserRequest) []string {
	options := append([]string(nil), req.Options...)
	if req.AllowFreeResponse {
		options = append(options, askUserFreeResponseOption)
	}
	return options
}

func askUserDefaultCursor(req agent.AskUserRequest) int {
	def := strings.TrimSpace(req.DefaultOption)
	if def == "" {
		return 0
	}
	for i, option := range askUserOptions(req) {
		if strings.EqualFold(option, def) {
			return i
		}
	}
	return 0
}

func (m Model) updateAskUserQuestion(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.pendingQuestion == nil {
		return m, nil
	}
	req := m.pendingQuestion.request
	options := askUserOptions(req)
	if m.questionFreeform {
		switch msg.String() {
		case "enter":
			answer := strings.TrimSpace(m.ta.Value())
			if answer == "" {
				m.showBanner("type your answer, then press enter", "warn")
				return m, nil
			}
			return m.resolveAskUser(answer, "answer sent"), nil
		case "esc":
			if len(options) > 0 {
				m.questionFreeform = false
				m.ta.Reset()
				m.showBanner("choose an option or press esc again to decline", "info")
				return m, nil
			}
			return m.rejectAskUser("question declined"), nil
		default:
			var taCmd tea.Cmd
			m.ta, taCmd = m.ta.Update(msg)
			return m, taCmd
		}
	}
	if len(options) == 0 {
		m.questionFreeform = true
		m.ta.Reset()
		return m, nil
	}
	switch msg.String() {
	case "up", "ctrl+p":
		if m.questionCursor > 0 {
			m.questionCursor--
		}
		return m, nil
	case "down", "ctrl+n":
		if m.questionCursor < len(options)-1 {
			m.questionCursor++
		}
		return m, nil
	case "enter":
		choice := options[m.questionCursor]
		if choice == askUserFreeResponseOption {
			m.questionFreeform = true
			m.ta.Reset()
			m.showBanner("type your answer and press enter", "info")
			return m, nil
		}
		return m.resolveAskUser(choice, "answer sent"), nil
	case "esc":
		return m.rejectAskUser("question declined"), nil
	}
	return m, nil
}

func (m Model) resolveAskUser(answer, banner string) Model {
	if m.pendingQuestion != nil {
		select {
		case m.pendingQuestion.response <- askUserResponse{answer: answer}:
		default:
		}
	}
	m.pendingQuestion = nil
	m.questionCursor = 0
	m.questionFreeform = false
	m.ta.Reset()
	m.showBanner(banner, "info")
	m.refreshViewport()
	return m
}

func (m Model) rejectAskUser(banner string) Model {
	if m.pendingQuestion != nil {
		select {
		case m.pendingQuestion.response <- askUserResponse{err: fmt.Errorf("user declined to answer")}:
		default:
		}
	}
	m.pendingQuestion = nil
	m.questionCursor = 0
	m.questionFreeform = false
	m.ta.Reset()
	m.showBanner(banner, "warn")
	m.refreshViewport()
	return m
}

func (m Model) updateSetup(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "esc" || strings.ToLower(m.ta.Value()) == "/cancel" {
		m.showSetup = false
		m.ta.Reset()
		m.showBanner("setup cancelled", "info")
		return m, nil
	}
	if msg.String() != "enter" {
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(msg)
		return m, cmd
	}

	input := strings.TrimSpace(m.ta.Value())
	m.ta.Reset()

	switch m.setup.step {
	case 0:
		providerIDs := m.providers.ProviderNames()
		if n, err := fmt.Sscanf(input, "%d", new(int)); n == 1 && err == nil {
			var idx int
			fmt.Sscanf(input, "%d", &idx)
			idx--
			if idx < 0 || idx >= len(providerIDs) {
				m.pushSystemMsg(fmt.Sprintf("invalid choice — enter 1-%d or provider name", len(providerIDs)))
				m.refreshViewport()
				return m, nil
			}
			m.setup.provider = providerIDs[idx]
		} else {
			found := false
			for _, id := range providerIDs {
				if strings.EqualFold(id, input) {
					m.setup.provider = id
					found = true
					break
				}
			}
			if !found {
				m.pushSystemMsg(fmt.Sprintf("unknown provider — enter 1-%d or provider name", len(providerIDs)))
				m.refreshViewport()
				return m, nil
			}
		}
		m.setup.step = 1
		var names []string
		for _, mod := range m.providers.Models() {
			if mod.Provider == m.setup.provider {
				displayName := mod.DisplayName
				if displayName == "" {
					displayName = mod.Name
				}
				tag := mod.Tag()
				line := "  " + mod.Name
				if displayName != mod.Name {
					line += " (" + displayName + ")"
				}
				if tag != "" {
					line += "  " + tag
				}
				names = append(names, line)
			}
		}
		m.pushSystemMsg("choose model:\n" + strings.Join(names, "\n"))
	case 1:
		if !m.providers.HasModel(m.setup.provider, input) {
			m.pushSystemMsg("unknown model for " + m.setup.provider + " — try again")
			m.refreshViewport()
			return m, nil
		}
		m.setup.model = input
		m.setup.step = 2
		m.pushSystemMsg("paste API key:")
	case 2:
		if input == "" {
			m.pushSystemMsg("key cannot be empty")
			m.refreshViewport()
			return m, nil
		}
		_ = config.SaveAPIKey(m.setup.provider, input)
		m.cfg.ActiveProvider = m.setup.provider
		m.cfg.ActiveModel = m.setup.model
		m.setup.step = 3
		m.pushSystemMsg("choose permission:\n  1) ask-first\n  2) restricted\n  3) yolo")
	case 3:
		switch input {
		case "1", "ask-first":
			m.cfg.Permission = config.PermissionAskFirst
		case "2", "restricted":
			m.cfg.Permission = config.PermissionRestricted
		case "3", "yolo":
			m.cfg.Permission = config.PermissionYOLO
		default:
			m.pushSystemMsg("invalid — enter 1, 2 or 3")
			m.refreshViewport()
			return m, nil
		}
		_ = m.updateConfig(func(cfg *config.UserConfig) error {
			cfg.ActiveProvider = m.setup.provider
			cfg.ActiveModel = m.setup.model
			cfg.Permission = m.cfg.Permission
			return nil
		})
		m.showSetup = false
		m.pushSystemMsg(fmt.Sprintf("setup complete ✓  %s:%s  perm:%s",
			m.cfg.ActiveProvider, m.cfg.ActiveModel, m.cfg.Permission))
	}

	m.refreshViewport()
	return m, nil
}

func (m Model) openConnect() Model {
	m.showConnect = true
	m.connectFilter = ""
	m.connectCursor = 0
	m.connectStep = 0
	m.connectProvider = ""
	m.connectItems = m.filterProviders("")
	return m
}

var suggestedProviderIDs = []string{localConnectProviderID, "anthropic", "openai", "mistral", "x-ai", "zai"}

func isSuggested(id string) bool {
	for _, s := range suggestedProviderIDs {
		if s == id {
			return true
		}
	}
	return false
}

func (m Model) filterProviders(filter string) []provider.ProviderInfo {
	all := m.providers.AllProviderInfos()
	all = append([]provider.ProviderInfo{{
		ID:   localConnectProviderID,
		Name: "Local endpoint (LM Studio/Ollama)",
	}}, all...)

	if filter != "" {
		q := strings.ToLower(filter)
		var out []provider.ProviderInfo
		for _, pi := range all {
			if strings.Contains(strings.ToLower(pi.ID), q) || strings.Contains(strings.ToLower(pi.Name), q) {
				out = append(out, pi)
			}
		}
		all = out
	}

	suggOrder := make(map[string]int, len(suggestedProviderIDs))
	for i, id := range suggestedProviderIDs {
		suggOrder[id] = i
	}
	sugg := make([]provider.ProviderInfo, len(suggestedProviderIDs))
	suggFilled := make([]bool, len(suggestedProviderIDs))
	var rest []provider.ProviderInfo
	for _, pi := range all {
		if idx, ok := suggOrder[pi.ID]; ok {
			sugg[idx] = pi
			suggFilled[idx] = true
		} else {
			rest = append(rest, pi)
		}
	}
	var out []provider.ProviderInfo
	for i, pi := range sugg {
		if suggFilled[i] {
			out = append(out, pi)
		}
	}
	return append(out, rest...)
}

func (m Model) localEndpointConnected() bool {
	return len(m.cfg.LocalEndpoints) > 0
}

func (m Model) hasLocalEndpoint(endpoint string) bool {
	for _, existing := range m.cfg.LocalEndpoints {
		if existing == endpoint {
			return true
		}
	}
	return false
}

func (m Model) updateConnect(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.connectStep {
	case 0:
		switch msg.String() {
		case "esc", "ctrl+c":
			m.showConnect = false
			return m, nil
		case "up", "ctrl+p", "shift+tab":
			if m.connectCursor > 0 {
				m.connectCursor--
			}
		case "down", "ctrl+n", "tab":
			if m.connectCursor < len(m.connectItems)-1 {
				m.connectCursor++
			}
		case "enter":
			if len(m.connectItems) > 0 {
				m.connectProvider = m.connectItems[m.connectCursor].ID
				m.connectStep = 1
				m.ta.Reset()
				m.ta.Focus()
			}
		case "backspace":
			if len(m.connectFilter) > 0 {
				m.connectFilter = m.connectFilter[:len(m.connectFilter)-1]
				m.connectItems = m.filterProviders(m.connectFilter)
				m.connectCursor = 0
			}
		default:
			if len(msg.String()) == 1 {
				m.connectFilter += msg.String()
				m.connectItems = m.filterProviders(m.connectFilter)
				m.connectCursor = 0
			}
		}
	case 1:
		switch msg.String() {
		case "esc":
			m.connectStep = 0
			m.ta.Reset()
		case "enter":
			if m.connectProvider == localConnectProviderID {
				endpoint := strings.TrimSpace(m.ta.Value())
				if endpoint == "" {
					m.showBanner("endpoint cannot be empty", "error")
					return m, nil
				}
				localModels, err := provider.ProbeLocalServer(context.Background(), endpoint)
				if err != nil {
					m.showBanner("local endpoint error: "+err.Error(), "error")
					return m, nil
				}
				m.providers.AddLocalModels(localModels)
				normalized := localModels[0].Provider
				_ = m.updateConfig(func(cfg *config.UserConfig) error {
					for _, endpoint := range cfg.LocalEndpoints {
						if endpoint == normalized {
							return nil
						}
					}
					cfg.LocalEndpoints = append(cfg.LocalEndpoints, normalized)
					return nil
				})
				m.showConnect = false
				m.ta.Reset()
				m.ta.Focus()
				m.showBanner(fmt.Sprintf("connected %s ✓", provider.LocalProviderName(normalized)), "success")
				return m, nil
			}
			key := strings.TrimSpace(m.ta.Value())
			if key == "" {
				m.showBanner("key cannot be empty", "error")
				return m, nil
			}
			_ = config.SaveAPIKey(m.connectProvider, key)
			_ = m.updateConfig(nil)
			m.showConnect = false
			m.ta.Reset()
			m.ta.Focus()
			m.showBanner(fmt.Sprintf("connected %s ✓", m.connectProvider), "success")
			return m, nil
		default:
			var cmd tea.Cmd
			m.ta, cmd = m.ta.Update(msg)
			return m, cmd
		}
	}

	return m, nil
}

func (m Model) openSelector(prefix string) Model {
	m.showSelector = true
	m.selFilter = strings.ToLower(strings.TrimSpace(prefix))
	m.selCursor = 0
	m.selItems = m.filterModels(m.selFilter)
	return m
}

func (m Model) filterModels(prefix string) []provider.Model {
	all := m.providers.ConnectedModels(m.cfg.APIKeys)
	if len(all) == 0 {
		all = nil
	}

	var favs, rest []provider.Model
	for _, mod := range all {
		if m.favorites[mod.Provider+":"+mod.Name] {
			favs = append(favs, mod)
		} else {
			rest = append(rest, mod)
		}
	}
	combined := append(favs, rest...)

	if prefix == "" {
		return combined
	}
	q := strings.ToLower(prefix)
	var out []provider.Model
	for _, mod := range combined {
		hay := strings.ToLower(mod.Provider + " " + mod.ProviderName + " " + mod.Name + " " + mod.DisplayName)
		if strings.Contains(hay, q) {
			out = append(out, mod)
		}
	}
	return out
}

func (m Model) favoriteModels() []provider.Model {
	all := m.providers.ConnectedModels(m.cfg.APIKeys)
	out := make([]provider.Model, 0, len(all))
	for _, mod := range all {
		if m.favorites[mod.Provider+":"+mod.Name] {
			out = append(out, mod)
		}
	}
	return out
}

func (m *Model) saveFavorites() {
	favList := make([]string, 0, len(m.favorites))
	for k, v := range m.favorites {
		if v {
			favList = append(favList, k)
		}
	}
	m.cfg.Favorites = favList
	_ = m.updateConfig(func(cfg *config.UserConfig) error {
		cfg.Favorites = append([]string(nil), favList...)
		return nil
	})
}

func (m *Model) syncInputSuggestions() {
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
			return
		}
		if strings.HasPrefix(val, "/thinking") && len(val) > len("/thinking") {
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
			return
		}
		query := val[1:]
		m.cmdItems = filterCommands(query)
		if m.cmdCursor >= len(m.cmdItems) {
			m.cmdCursor = 0
		}
		m.mentionItems = nil
		m.mentionCursor = 0
		return
	}

	m.cmdItems = nil
	m.cmdCursor = 0

	query, ok := activeMentionQuery(val)
	if !ok {
		m.mentionItems = nil
		m.mentionCursor = 0
		return
	}

	m.mentionItems = filterMentionFiles(m.repoFiles, query, 8)
	if m.mentionCursor >= len(m.mentionItems) {
		m.mentionCursor = 0
	}
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

func scanRepoFiles(root string) ([]string, error) {
	gi := newGitignoreMatcher(root)
	var entries []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".spettro":
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

func (m Model) updateSelector(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.showSelector = false
		return m, nil
	case "up", "ctrl+p", "shift+tab":
		if m.selCursor > 0 {
			m.selCursor--
		}
	case "down", "ctrl+n", "tab":
		if m.selCursor < len(m.selItems)-1 {
			m.selCursor++
		}
	case "enter":
		if len(m.selItems) > 0 {
			sel := m.selItems[m.selCursor]
			_ = m.updateConfig(func(cfg *config.UserConfig) error {
				cfg.ActiveProvider = sel.Provider
				cfg.ActiveModel = sel.Name
				return nil
			})
			m.showSelector = false
			m.showBanner(fmt.Sprintf("model → %s:%s", sel.Provider, sel.Name), "success")
		}
	case "f":
		if len(m.selItems) > 0 {
			sel := m.selItems[m.selCursor]
			key := sel.Provider + ":" + sel.Name
			if m.favorites == nil {
				m.favorites = map[string]bool{}
			}
			m.favorites[key] = !m.favorites[key]
			m.saveFavorites()
			m.selItems = m.filterModels(m.selFilter)
			if m.selCursor >= len(m.selItems) {
				m.selCursor = len(m.selItems) - 1
			}
			if m.selCursor < 0 {
				m.selCursor = 0
			}
		}
	case "c":
		m.showSelector = false
		m = m.openConnect()
		return m, nil
	case "backspace":
		if len(m.selFilter) > 0 {
			m.selFilter = m.selFilter[:len(m.selFilter)-1]
			m.selItems = m.filterModels(m.selFilter)
			m.selCursor = 0
		}
	default:
		if len(msg.String()) == 1 {
			m.selFilter += msg.String()
			m.selItems = m.filterModels(m.selFilter)
			m.selCursor = 0
		}
	}

	return m, nil
}

func (m Model) viewSelector() string {
	mc := m.currentColor()
	title := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("◈ select model")

	if len(m.providers.ConnectedModels(m.cfg.APIKeys)) == 0 {
		msg := lipgloss.JoinVertical(lipgloss.Left,
			title,
			"",
			styleMuted.Render("no providers connected yet"),
			"",
			styleSuccess.Render("press c to connect a provider"),
			styleMuted.Render("or use /connect from the main screen"),
		)
		dialog := lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(mc).
			Width(50).
			Padding(2, 4).
			Render(msg)
		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			dialog,
			lipgloss.WithWhitespaceChars(" "),
			lipgloss.WithWhitespaceForeground(colorDim),
		)
	}

	cursor := lipgloss.NewStyle().Foreground(mc).Render("_")
	filterLine := styleMuted.Render("search  ") +
		lipgloss.NewStyle().Foreground(colorText).Render(m.selFilter) +
		cursor

	var rows []string
	currentProvider := ""
	for i, mod := range m.selItems {
		if mod.Provider != currentProvider {
			currentProvider = mod.Provider
			if len(rows) > 0 {
				rows = append(rows, "")
			}
			provLabel := mod.ProviderName
			if provLabel == "" {
				provLabel = mod.Provider
			}
			rows = append(rows, lipgloss.NewStyle().
				Foreground(colorMuted).Bold(true).
				Render("  ─ "+provLabel))
		}

		isSelected := i == m.selCursor
		isCurrent := mod.Provider == m.cfg.ActiveProvider && mod.Name == m.cfg.ActiveModel
		isFav := m.favorites[mod.Provider+":"+mod.Name]

		var prefix string
		var nameStyle, tagStyle lipgloss.Style
		if isSelected {
			prefix = lipgloss.NewStyle().Foreground(mc).Bold(true).Render("› ")
			nameStyle = lipgloss.NewStyle().Foreground(colorText).Bold(true)
			tagStyle = lipgloss.NewStyle().Foreground(colorMuted)
		} else {
			prefix = "  "
			nameStyle = lipgloss.NewStyle().Foreground(colorMuted)
			tagStyle = lipgloss.NewStyle().Foreground(colorDim)
		}

		displayName := mod.DisplayName
		if displayName == "" {
			displayName = mod.Name
		}

		var badges string
		if isFav {
			badges += lipgloss.NewStyle().Foreground(lipgloss.Color("#FBBF24")).Render("★ ")
		}
		if isCurrent {
			badges += lipgloss.NewStyle().Foreground(mc).Render("● ")
		}

		tag := tagStyle.Render(mod.Tag())
		row := prefix + badges + nameStyle.Render(displayName)
		if tag != "" {
			row += "  " + tag
		}
		rows = append(rows, row)
	}
	if len(m.selItems) == 0 {
		rows = append(rows, styleMuted.Render("  no matches"))
	}

	hint := styleMuted.Render("↑↓ navigate  enter select  f favorite  c connect  esc close")
	dialogWidth := 70
	if m.width < dialogWidth+4 {
		dialogWidth = m.width - 4
	}
	if dialogWidth < 30 {
		dialogWidth = 30
	}

	maxRows := m.height - 12
	if maxRows < 4 {
		maxRows = 4
	}
	start := 0
	if len(rows) > maxRows {
		start = m.selCursor - maxRows/2
		if start < 0 {
			start = 0
		}
		if start+maxRows > len(rows) {
			start = len(rows) - maxRows
		}
		rows = rows[start : start+maxRows]
	}

	dialog := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(mc).
		Width(dialogWidth).
		Padding(1, 2).
		Render(lipgloss.JoinVertical(lipgloss.Left,
			title,
			"",
			filterLine,
			"",
			strings.Join(rows, "\n"),
			"",
			hint,
		))

	return lipgloss.Place(m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		dialog,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceForeground(colorDim),
	)
}

func (m Model) viewConnect() string {
	mc := m.currentColor()
	dialogWidth := 60
	if m.width < dialogWidth+4 {
		dialogWidth = m.width - 4
	}
	if dialogWidth < 30 {
		dialogWidth = 30
	}

	if m.connectStep == 1 {
		provName := m.connectProvider
		envHint := ""
		prompt := "paste your API key and press enter:"
		if m.connectProvider == localConnectProviderID {
			provName = "Local endpoint"
			prompt = "enter local endpoint (e.g. localhost:1234) and press enter:"
		}
		for _, pi := range m.providers.AllProviderInfos() {
			if pi.ID == m.connectProvider {
				if pi.Name != "" {
					provName = pi.Name
				}
				if pi.Env != "" {
					envHint = styleMuted.Render("env var: " + pi.Env)
				}
				break
			}
		}

		title := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("◈ connect " + provName)
		inner := lipgloss.JoinVertical(lipgloss.Left,
			title, "",
			envHint,
			styleMuted.Render(prompt),
			"",
			m.ta.View(),
			"",
			styleMuted.Render("esc: back"),
		)
		dialog := lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(mc).
			Width(dialogWidth).
			Padding(1, 2).
			Render(inner)

		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			dialog,
			lipgloss.WithWhitespaceChars(" "),
			lipgloss.WithWhitespaceForeground(colorDim),
		)
	}

	title := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("◈ connect provider")
	cursor := lipgloss.NewStyle().Foreground(mc).Render("_")
	filterLine := styleMuted.Render("search  ") +
		lipgloss.NewStyle().Foreground(colorText).Render(m.connectFilter) +
		cursor

	var rows []string
	inSuggested := true
	for i, pi := range m.connectItems {
		nowSugg := isSuggested(pi.ID)
		if i == 0 {
			if nowSugg {
				rows = append(rows, lipgloss.NewStyle().Foreground(colorMuted).Bold(true).Render("  ─ suggested"))
			} else {
				rows = append(rows, lipgloss.NewStyle().Foreground(colorMuted).Bold(true).Render("  ─ all providers"))
				inSuggested = false
			}
		} else if inSuggested && !nowSugg {
			inSuggested = false
			rows = append(rows, "")
			rows = append(rows, lipgloss.NewStyle().Foreground(colorMuted).Bold(true).Render("  ─ all providers"))
		}

		isSelected := i == m.connectCursor
		isConnected := m.cfg.APIKeys[pi.ID] != ""
		if pi.ID == localConnectProviderID {
			isConnected = m.localEndpointConnected()
		}

		name := pi.Name
		if name == "" {
			name = pi.ID
		}

		var prefix string
		var nameStyle lipgloss.Style
		if isSelected {
			prefix = lipgloss.NewStyle().Foreground(mc).Bold(true).Render("› ")
			nameStyle = lipgloss.NewStyle().Foreground(colorText).Bold(true)
		} else {
			prefix = "  "
			nameStyle = lipgloss.NewStyle().Foreground(colorMuted)
		}

		suffix := ""
		if isConnected {
			suffix = "  " + lipgloss.NewStyle().Foreground(colorSuccess).Render("✓ connected")
		}

		rows = append(rows, prefix+nameStyle.Render(name)+suffix)
	}
	if len(m.connectItems) == 0 {
		rows = append(rows, styleMuted.Render("  no matches"))
	}

	hint := styleMuted.Render("↑↓ navigate  enter connect  esc close")
	maxRows := m.height - 12
	if maxRows < 4 {
		maxRows = 4
	}
	start := 0
	if len(rows) > maxRows {
		start = m.connectCursor - maxRows/2
		if start < 0 {
			start = 0
		}
		if start+maxRows > len(rows) {
			start = len(rows) - maxRows
		}
		rows = rows[start : start+maxRows]
	}

	dialog := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(mc).
		Width(dialogWidth).
		Padding(1, 2).
		Render(lipgloss.JoinVertical(lipgloss.Left,
			title, "",
			filterLine, "",
			strings.Join(rows, "\n"),
			"",
			hint,
		))

	return lipgloss.Place(m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		dialog,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceForeground(colorDim),
	)
}

func (m Model) runAgent(spec config.AgentSpec, input string, mentionedFiles []string, images []string) (tea.Model, tea.Cmd) {
	return m.runAgentApproved(spec, input, mentionedFiles, images, false)
}

func (m Model) runAgentApproved(spec config.AgentSpec, input string, mentionedFiles []string, images []string, approved bool) (tea.Model, tea.Cmd) {
	m.thinking = true
	m.activeAgentID = spec.ID
	m.publishRemoteState("agent_start")
	m.refreshModifiedFiles()
	m.liveTools = nil
	m.currentTool = nil
	m.pendingAuth = nil
	m.progressNote = fmt.Sprintf("Okay, let me work on that with the %s agent.", spec.ID)
	m.activePrompt = &queuedPrompt{
		Input:          input,
		Prompt:         input,
		MentionedFiles: append([]string(nil), mentionedFiles...),
		Images:         append([]string(nil), images...),
	}
	m.startAgentActivity(spec.ID, input)
	toolCh := make(chan agent.ToolTrace, 64)
	m.toolCh = toolCh
	approvalCh := make(chan shellApprovalRequestMsg, 8)
	m.approvalCh = approvalCh
	askUserCh := make(chan askUserRequestMsg, 4)
	m.askUserCh = askUserCh
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelAgent = cancel
	m.ensureSession()
	pm := m.providers
	providerName := m.cfg.ActiveProvider
	modelName := m.cfg.ActiveModel
	cwd := m.cwd
	store := m.store
	perm := m.cfg.Permission
	agentID := spec.ID

	manifest := m.manifest
	a := agent.LLMAgent{
		Spec:            spec,
		ProviderManager: pm,
		ProviderName:    func() string { return providerName },
		ModelName:       func() string { return modelName },
		CWD:             cwd,
		MaxTokens:       m.cfg.TokenBudget,
		Thinking:        provider.ThinkingLevel(m.cfg.ThinkingLevel),
		RequiredReads:   mentionedFiles,
		Images:          images,
		Manifest:        &manifest,
		SessionDir:      session.SessionDir(store.GlobalDir, m.sessionID),
		DelegationDepth: 0,
		ToolCallback:    func(t agent.ToolTrace) { toolCh <- t },
		ShellApproval: func(ctx context.Context, req agent.ShellApprovalRequest) (agent.ShellApprovalDecision, error) {
			respCh := make(chan shellApprovalResponse, 1)
			select {
			case approvalCh <- shellApprovalRequestMsg{request: req, response: respCh}:
			case <-ctx.Done():
				return agent.ShellApprovalDeny, ctx.Err()
			}
			select {
			case resp := <-respCh:
				if resp.err != nil {
					return agent.ShellApprovalDeny, resp.err
				}
				return resp.decision, nil
			case <-ctx.Done():
				return agent.ShellApprovalDeny, ctx.Err()
			}
		},
		AskUser: func(ctx context.Context, req agent.AskUserRequest) (string, error) {
			respCh := make(chan askUserResponse, 1)
			select {
			case askUserCh <- askUserRequestMsg{request: req, response: respCh}:
			case <-ctx.Done():
				return "", ctx.Err()
			}
			select {
			case resp := <-respCh:
				return resp.answer, resp.err
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	}

	return m, tea.Batch(
		m.spin.Tick,
		waitForTool(toolCh),
		waitForShellApproval(approvalCh),
		waitForAskUser(askUserCh),
		func() tea.Msg {
			runSpec := spec
			if approved || perm != config.PermissionAskFirst {
				if perm != config.PermissionAskFirst {
					runSpec.Permission = perm
				}
			}
			a.Spec = runSpec
			result, err := a.Run(ctx, input)
			close(toolCh)
			close(approvalCh)
			close(askUserCh)
			if err != nil {
				return agentDoneMsg{err: err}
			}
			if agentID == "plan" || spec.Mode == "planning" {
				_ = store.WriteProjectFile("PLAN.md", result.Content)
				return planDoneMsg{plan: result.Content, tools: result.Tools, tokensUsed: result.TokensUsed}
			}
			return agentDoneMsg{content: result.Content, tools: result.Tools, tokensUsed: result.TokensUsed, meta: ""}
		},
	)
}

func (m Model) runCommitter() (tea.Model, tea.Cmd) {
	m.thinking = true
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelAgent = cancel
	cwd := m.cwd
	pm := m.providers
	providerName := m.cfg.ActiveProvider
	modelName := m.cfg.ActiveModel
	committer := agent.LLMCommitter{
		ProviderManager: pm,
		ProviderName:    func() string { return providerName },
		ModelName:       func() string { return modelName },
	}
	return m, tea.Batch(
		m.spin.Tick,
		func() tea.Msg {
			msg, err := committer.Commit(ctx, cwd)
			return commitDoneMsg{commitMsg: msg, err: err}
		},
	)
}

func (m Model) runSearcher(query string) (tea.Model, tea.Cmd) {
	m.thinking = true
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelAgent = cancel
	searcher := m.searcher
	cwd := m.cwd
	return m, tea.Batch(
		m.spin.Tick,
		func() tea.Msg {
			result, err := searcher.Search(ctx, cwd, query)
			return searchDoneMsg{result: result, err: err}
		},
	)
}

func (m Model) runCompact(focus string) (tea.Model, tea.Cmd) {
	return m.runCompactWithMode(focus, false)
}

func (m Model) runCompactWithMode(focus string, auto bool) (tea.Model, tea.Cmd) {
	if len(m.messages) == 0 {
		m.showBanner("nothing to compact", "info")
		return m, nil
	}
	m.thinking = true
	m.autoCompactInFlight = auto
	pm := m.providers
	providerName := m.cfg.ActiveProvider
	modelName := m.cfg.ActiveModel
	var sb strings.Builder
	for _, msg := range m.messages {
		if msg.Role == RoleSystem {
			continue
		}
		sb.WriteString(string(msg.Role))
		sb.WriteString(": ")
		sb.WriteString(msg.Content)
		sb.WriteString("\n\n")
	}
	transcript := sb.String()
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelAgent = cancel
	return m, tea.Batch(
		m.spin.Tick,
		func() tea.Msg {
			compactPrompt := "Summarize the following conversation concisely, preserving all key decisions, facts, code snippets, and action items. Output only the summary, no preamble."
			if focus != "" {
				compactPrompt += " Focus especially on: " + focus + "."
			}
			resp, err := pm.Send(ctx, providerName, modelName, provider.Request{
				Prompt: compactPrompt + "\n\n" + transcript,
			})
			if err != nil {
				return compactDoneMsg{err: err}
			}
			return compactDoneMsg{summary: resp.Content}
		},
	)
}

func (m Model) runInit() (tea.Model, tea.Cmd) {
	spec, ok := m.manifest.AgentByID("docs")
	if !ok {
		spec, ok = m.manifest.AgentByID("coding")
		if !ok {
			m.showBanner("docs/coding agent not found in manifest", "error")
			return m, nil
		}
	}
	task := "Analyze this codebase and create (or improve) a SPETTRO.md file in the repository root."
	return m.runAgent(spec, task, nil, nil)
}

func (m Model) runExplore(task string) (tea.Model, tea.Cmd) {
	if strings.TrimSpace(task) == "" {
		task = "Explore this codebase: understand the architecture, key types, conventions, and entry points."
	}
	spec, ok := m.manifest.AgentByID("explore")
	if !ok {
		m.showBanner("explore agent not found in manifest", "error")
		return m, nil
	}
	return m.runAgent(spec, task, nil, nil)
}
