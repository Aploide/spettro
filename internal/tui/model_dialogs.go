package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

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

func (m Model) updatePlanApproval(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	n := len(planApprovalOptions)
	switch msg.String() {
	case "up":
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

func (m Model) updateShellApproval(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
	case "up":
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

func (m Model) updateAskUserQuestion(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
	case "up":
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
	m.telegramClearAnswerExpectations()
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
	m.telegramClearAnswerExpectations()
	m.showBanner(banner, "warn")
	m.refreshViewport()
	return m
}

func (m Model) updateSetup(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
	m.connectActionCursor = 0
	m.connectEditMode = false
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

// localProbeDoneMsg delivers the result of an asynchronous ProbeLocalServer
// round-trip (see probeLocalServerCmd) back to the Update loop.
type localProbeDoneMsg struct {
	endpoint string
	models   []provider.Model
	err      error
}

// probeLocalServerCmd probes a local OpenAI-compatible endpoint off the UI
// thread. The 10s timeout bounds the blocking HTTP call so the TUI never hangs.
func probeLocalServerCmd(endpoint string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		models, err := provider.ProbeLocalServer(ctx, endpoint)
		return localProbeDoneMsg{endpoint: endpoint, models: models, err: err}
	}
}

// handleLocalProbeDone finishes the local-endpoint connect once the async probe
// resolves: it registers the discovered models, persists the endpoint, and
// closes the connect dialog. A failure leaves the dialog open with an error
// banner so the user can correct the URL.
func (m Model) handleLocalProbeDone(msg localProbeDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.showBanner("local endpoint error: "+msg.err.Error(), "error")
		return m, nil
	}
	if len(msg.models) == 0 {
		m.showBanner("local endpoint returned no models", "error")
		return m, nil
	}
	m.providers.AddLocalModels(msg.models)
	normalized := msg.models[0].Provider
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

var connectManageOptions = []string{
	"Edit key",
	"Remove provider",
	"Cancel",
}

var connectConfirmOptions = []string{
	"Yes, remove",
	"Cancel",
}

func (m Model) updateConnect(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.connectStep {
	case 0:
		switch msg.String() {
		case "esc", "ctrl+c":
			m.showConnect = false
			return m, nil
		case "up", "shift+tab":
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
				isAPIConnected := m.connectProvider != localConnectProviderID &&
					m.cfg.APIKeys[m.connectProvider] != ""
				if isAPIConnected {
					m.connectStep = 2
					m.connectActionCursor = 0
				} else {
					m.connectStep = 1
					m.connectEditMode = false
					m.ta.Reset()
					m.ta.Focus()
				}
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
			if m.connectEditMode {
				m.connectStep = 2
				m.connectEditMode = false
			} else {
				m.connectStep = 0
			}
			m.ta.Reset()
		case "enter":
			if m.connectProvider == localConnectProviderID {
				endpoint := strings.TrimSpace(m.ta.Value())
				if endpoint == "" {
					m.showBanner("endpoint cannot be empty", "error")
					return m, nil
				}
				// Probe the endpoint off the UI thread: ProbeLocalServer does a
				// blocking HTTP round-trip that previously froze the TUI for up
				// to 5s inside this key handler. Keep the dialog open and show a
				// progress banner; localProbeDoneMsg finishes the connect.
				m.showBanner("probing local endpoint…", "info")
				return m, probeLocalServerCmd(endpoint)
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
	case 2:
		switch msg.String() {
		case "esc":
			m.connectStep = 0
			m.connectActionCursor = 0
		case "up", "shift+tab":
			if m.connectActionCursor > 0 {
				m.connectActionCursor--
			}
		case "down", "ctrl+n", "tab":
			if m.connectActionCursor < len(connectManageOptions)-1 {
				m.connectActionCursor++
			}
		case "enter":
			switch m.connectActionCursor {
			case 0: // Edit key
				m.connectStep = 1
				m.connectEditMode = true
				m.ta.Reset()
				m.ta.Focus()
			case 1: // Remove provider
				m.connectStep = 3
				m.connectActionCursor = 0
			case 2: // Cancel
				m.connectStep = 0
				m.connectActionCursor = 0
			}
		}
	case 3:
		switch msg.String() {
		case "esc":
			m.connectStep = 2
			m.connectActionCursor = 0
		case "up", "shift+tab":
			if m.connectActionCursor > 0 {
				m.connectActionCursor--
			}
		case "down", "ctrl+n", "tab":
			if m.connectActionCursor < len(connectConfirmOptions)-1 {
				m.connectActionCursor++
			}
		case "enter":
			switch m.connectActionCursor {
			case 0: // Yes, remove
				removedProvider := m.connectProvider
				wasActive := m.cfg.ActiveProvider == removedProvider
				_ = config.RemoveAPIKey(removedProvider)
				_ = m.updateConfig(func(cfg *config.UserConfig) error {
					if cfg.ActiveProvider == removedProvider {
						connected := m.providers.ConnectedModels(cfg.APIKeys)
						if len(connected) > 0 {
							cfg.ActiveProvider = connected[0].Provider
							cfg.ActiveModel = connected[0].Name
						} else {
							cfg.ActiveProvider = ""
							cfg.ActiveModel = ""
						}
					}
					return nil
				})
				m.showConnect = false
				switch {
				case wasActive && m.cfg.ActiveProvider != "":
					m.showBanner(fmt.Sprintf("removed %s — switched to %s:%s", removedProvider, m.cfg.ActiveProvider, m.cfg.ActiveModel), "info")
				case wasActive:
					m.showBanner(fmt.Sprintf("removed %s — no providers connected", removedProvider), "warn")
				default:
					m.showBanner(fmt.Sprintf("removed %s", removedProvider), "info")
				}
				return m, nil
			case 1: // Cancel
				m.connectStep = 2
				m.connectActionCursor = 0
			}
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
			return nil
		}
		if strings.HasPrefix(val, "/think") && !strings.HasPrefix(val, "/thinking") && len(val) > len("/think") {
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
		m.cmdItems = filterCommands(query)
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

func (m Model) updateSelector(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.showSelector = false
		return m, nil
	case "up", "shift+tab":
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

	dialogWidth := 70
	if m.width < dialogWidth+4 {
		dialogWidth = m.width - 4
	}
	if dialogWidth < 30 {
		dialogWidth = 30
	}
	innerW := dialogInnerWidth(dialogWidth)

	titleLabel := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("◈ select model")
	title := diagFillTitle(titleLabel, innerW)

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
			Width(dialogWidth+2).
			Padding(2, 4).
			Render(msg)
		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			dialog,
			lipgloss.WithWhitespaceChars(" "),
			lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(colorDim)),
		)
	}

	cursor := lipgloss.NewStyle().Foreground(mc).Render("▊")
	promptStyle := lipgloss.NewStyle().Foreground(mc).Bold(true)
	filterLine := promptStyle.Render(">") + " " +
		lipgloss.NewStyle().Foreground(colorText).Render(m.selFilter) +
		cursor

	var rows []string
	selectedRow := 0
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

		displayName := mod.DisplayName
		if displayName == "" {
			displayName = mod.Name
		}
		tag := mod.Tag()

		if isSelected {
			selectedRow = len(rows)
			prefix := "› "
			if isFav {
				prefix += "★ "
			}
			if isCurrent {
				prefix += "● "
			}
			label := prefix + displayName
			if tag != "" {
				label += "  " + tag
			}
			rows = append(rows, lipgloss.NewStyle().
				Background(colorSelBg).
				Foreground(colorText).
				Bold(true).
				Width(innerW).
				Render(label))
		} else {
			prefix := "  "
			nameStyle := lipgloss.NewStyle().Foreground(colorMuted)
			tagStyle := lipgloss.NewStyle().Foreground(colorDim)
			var badges string
			if isFav {
				badges += lipgloss.NewStyle().Foreground(lipgloss.Color("#FBBF24")).Render("★ ")
			}
			if isCurrent {
				badges += lipgloss.NewStyle().Foreground(mc).Render("● ")
			}
			row := prefix + badges + nameStyle.Render(displayName)
			if tag != "" {
				row += "  " + tagStyle.Render(tag)
			}
			rows = append(rows, row)
		}
	}
	if len(m.selItems) == 0 {
		rows = append(rows, styleMuted.Render("  no matches"))
	}

	hint := styleMuted.Render("↑↓ navigate  enter select  f favorite  c connect  esc close")

	maxRows := m.height - 12
	if maxRows < 4 {
		maxRows = 4
	}
	start := 0
	if len(rows) > maxRows {
		start = selectedRow - maxRows/2
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
		Width(dialogWidth+2).
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
		lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(colorDim)),
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
	innerW := dialogInnerWidth(dialogWidth)

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

		titleLabel := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("◈ connect " + provName)
		title := diagFillTitle(titleLabel, innerW)
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
			Width(dialogWidth+2).
			Padding(1, 2).
			Render(inner)

		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			dialog,
			lipgloss.WithWhitespaceChars(" "),
			lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(colorDim)),
		)
	}

	if m.connectStep == 2 {
		provName := m.connectProvider
		for _, pi := range m.providers.AllProviderInfos() {
			if pi.ID == m.connectProvider && pi.Name != "" {
				provName = pi.Name
				break
			}
		}
		titleLabel := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("◈ manage " + provName)
		title := diagFillTitle(titleLabel, innerW)
		var rows []string
		for i, opt := range connectManageOptions {
			if i == m.connectActionCursor {
				rows = append(rows, lipgloss.NewStyle().
					Background(colorSelBg).Foreground(colorText).Bold(true).
					Width(innerW).Render("› "+opt))
			} else {
				rows = append(rows, lipgloss.NewStyle().Foreground(colorMuted).Render("  "+opt))
			}
		}
		inner := lipgloss.JoinVertical(lipgloss.Left,
			title, "",
			strings.Join(rows, "\n"),
			"",
			styleMuted.Render("↑↓ navigate  enter select  esc back"),
		)
		dialog := lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(mc).
			Width(dialogWidth+2).
			Padding(1, 2).
			Render(inner)
		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			dialog,
			lipgloss.WithWhitespaceChars(" "),
			lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(colorDim)),
		)
	}

	if m.connectStep == 3 {
		provName := m.connectProvider
		for _, pi := range m.providers.AllProviderInfos() {
			if pi.ID == m.connectProvider && pi.Name != "" {
				provName = pi.Name
				break
			}
		}
		titleLabel := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF5555")).Render("◈ remove " + provName)
		title := diagFillTitle(titleLabel, innerW)
		warning := lipgloss.NewStyle().Foreground(colorText).Render("Remove this provider and delete its API key?")
		var rows []string
		for i, opt := range connectConfirmOptions {
			if i == m.connectActionCursor {
				rows = append(rows, lipgloss.NewStyle().
					Background(colorSelBg).Foreground(colorText).Bold(true).
					Width(innerW).Render("› "+opt))
			} else {
				rows = append(rows, lipgloss.NewStyle().Foreground(colorMuted).Render("  "+opt))
			}
		}
		inner := lipgloss.JoinVertical(lipgloss.Left,
			title, "",
			warning, "",
			strings.Join(rows, "\n"),
			"",
			styleMuted.Render("↑↓ navigate  enter confirm  esc back"),
		)
		dialog := lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#FF5555")).
			Width(dialogWidth+2).
			Padding(1, 2).
			Render(inner)
		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			dialog,
			lipgloss.WithWhitespaceChars(" "),
			lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(colorDim)),
		)
	}

	titleLabel := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("◈ connect provider")
	title := diagFillTitle(titleLabel, innerW)
	cursor := lipgloss.NewStyle().Foreground(mc).Render("▊")
	promptStyle := lipgloss.NewStyle().Foreground(mc).Bold(true)
	filterLine := promptStyle.Render(">") + " " +
		lipgloss.NewStyle().Foreground(colorText).Render(m.connectFilter) +
		cursor

	var rows []string
	selectedRow := 0
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

		if isSelected {
			selectedRow = len(rows)
			label := "› " + name
			if isConnected {
				label += "  ✓ connected"
			}
			rows = append(rows, lipgloss.NewStyle().
				Background(colorSelBg).
				Foreground(colorText).
				Bold(true).
				Width(innerW).
				Render(label))
		} else {
			nameStyle := lipgloss.NewStyle().Foreground(colorMuted)
			suffix := ""
			if isConnected {
				suffix = "  " + lipgloss.NewStyle().Foreground(colorSuccess).Render("✓ connected")
			}
			rows = append(rows, "  "+nameStyle.Render(name)+suffix)
		}
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
		start = selectedRow - maxRows/2
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
		Width(dialogWidth+2).
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
		lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(colorDim)),
	)
}

// buildConversationHistory renders a bounded, oldest-first transcript of prior
// turns for the model (EFF-2). It includes user and assistant turns plus any
// /compact summary, and excludes the trailing user message when present (that
// is the current request, sent separately as the task — including it here would
// duplicate it). The result is capped at maxConversationHistoryBytes with
// most-recent turns winning, so token cost stays bounded. Returns "" when there
// is no prior context (first turn), preserving the pre-EFF-2 behavior exactly.
func (m Model) buildConversationHistory() string {
	msgs := m.messages
	// Drop the trailing user turn: it is the request being sent as the task.
	if n := len(msgs); n > 0 && msgs[n-1].Role == RoleUser {
		msgs = msgs[:n-1]
	}

	// Collect eligible turns as formatted lines, oldest-first.
	var lines []string
	for _, msg := range msgs {
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue
		}
		switch msg.Role {
		case RoleUser:
			lines = append(lines, "user: "+singleLineHistory(content))
		case RoleAssistant:
			// Skip transient progress comments; keep substantive replies/plans.
			if msg.Kind == "comment" {
				continue
			}
			lines = append(lines, "assistant: "+singleLineHistory(content))
		case RoleSystem:
			// Only carry forward a compaction summary, not routine notices.
			if strings.HasPrefix(content, compactSummaryPrefix) {
				lines = append(lines, "summary: "+singleLineHistory(content))
			}
		}
	}
	if len(lines) == 0 {
		return ""
	}

	// Keep the most recent turns within the byte cap (oldest dropped first).
	kept := make([]string, 0, len(lines))
	total := 0
	for i := len(lines) - 1; i >= 0; i-- {
		size := len(lines[i]) + 1 // +1 for the joining newline
		if total+size > maxConversationHistoryBytes && len(kept) > 0 {
			break
		}
		kept = append(kept, lines[i])
		total += size
	}
	// kept is most-recent-first; reverse to oldest-first for the prompt.
	for l, r := 0, len(kept)-1; l < r; l, r = l+1, r-1 {
		kept[l], kept[r] = kept[r], kept[l]
	}
	return strings.Join(kept, "\n")
}

// compactedHistorySeed builds the structured conversation that replaces the
// carried history after a /compact: a user turn holding the summary and a
// short assistant acknowledgment. It starts with a user turn (a provider
// requirement) and gives every subsequent turn a stable prefix to extend, so
// prompt caching resumes immediately after the one unavoidable compaction miss.
func compactedHistorySeed(summary string) []provider.Message {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return nil
	}
	return []provider.Message{
		{Role: provider.RoleUser, Content: "Context from the conversation so far (compacted summary):\n" + summary},
		{Role: provider.RoleAssistant, Content: "Understood. I have the compacted context and will continue from there."},
	}
}

// singleLineHistory collapses a turn to a single line so the transcript stays
// compact and unambiguous in the prompt. Very long turns are truncated to keep
// any single entry from dominating the budget.
func singleLineHistory(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const maxPerTurn = 4000
	if len(s) > maxPerTurn {
		s = s[:maxPerTurn] + " …(truncated)"
	}
	return s
}

func (m Model) runAgent(spec config.AgentSpec, input string, mentionedFiles []string, images []string) (tea.Model, tea.Cmd) {
	return m.runAgentApproved(spec, input, mentionedFiles, images, false)
}

func (m Model) runAgentApproved(spec config.AgentSpec, input string, mentionedFiles []string, images []string, approved bool) (tea.Model, tea.Cmd) {
	m.thinking = true
	m.agentStartAt = time.Now()
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
	streamCh := make(chan agent.StreamChunk, 256)
	m.streamCh = streamCh
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
	// Structured cross-turn history: the previous run's conversation is passed
	// back verbatim so the provider sees a byte-stable, growing prefix (prompt
	// caching) and keeps every tool call/result. The flattened transcript is
	// only the degraded fallback for the first turn after resuming a session,
	// where no structured context exists yet.
	convHistory := m.convHistory
	history := ""
	if len(convHistory) == 0 {
		history = m.buildConversationHistory()
	}
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
		History:         history,
		Messages:        convHistory,
		Manifest:        &manifest,
		SandboxState:    m.sandboxState,
		SessionDir:      session.SessionDir(store.GlobalDir, m.sessionID),
		DelegationDepth: 0,
		// Goal-mode fields: set when a goal is active so the runtime uses
		// generous tool timeouts and recognizes goal-complete.
		GoalMode:        m.activeGoal != nil,
		ContextWindow:   resolveGoalContextWindow(m),
		ShellTimeoutSec: m.cfg.GoalShellTimeoutSec,
		ToolCallback: func(t agent.ToolTrace) {
			// Guard the send against a cancelled run: after stopAgent() the TUI
			// stops draining toolCh, so an unguarded send from an in-flight
			// step could block the agent goroutine forever once the 64-slot
			// buffer fills.
			select {
			case toolCh <- t:
			case <-ctx.Done():
			}
		},
		StreamCallback: func(c agent.StreamChunk) {
			// Same cancellation guard as ToolCallback: never block the agent
			// goroutine on a stream send once the TUI stops draining.
			select {
			case streamCh <- c:
			case <-ctx.Done():
			}
		},
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
		waitForStream(streamCh),
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
			close(streamCh)
			close(approvalCh)
			close(askUserCh)
			if err != nil {
				return agentDoneMsg{err: err}
			}
			if agentID == "plan" || spec.Mode == "planning" {
				_ = store.WriteProjectFile("PLAN.md", result.Content)
				return planDoneMsg{plan: result.Content, tools: result.Tools, tokensUsed: result.TokensUsed, contextTokens: result.ContextTokens, messages: result.Messages}
			}
			return agentDoneMsg{content: result.Content, tools: result.Tools, tokensUsed: result.TokensUsed, contextTokens: result.ContextTokens, meta: "", goalComplete: result.GoalComplete, goalSummary: result.GoalSummary, messages: result.Messages}
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
	// "docs" agent is read-only (no file-write); use "coding" so the file actually gets written.
	spec, ok := m.manifest.AgentByID("coding")
	if !ok {
		spec, ok = m.manifest.AgentByID("code")
		if !ok {
			m.showBanner("coding/code agent not found in manifest", "error")
			return m, nil
		}
	}
	task := `Analyze this codebase and write a SPETTRO.md file to the repository root.

Use glob, grep, file-read, and ls to explore the codebase first, then write the file.

SPETTRO.md must contain these sections:
- **Project overview**: what the project does in 2–3 sentences
- **Architecture**: key packages/directories and their roles (list each with a one-line description)
- **Entry points**: main binaries, primary types, and the startup flow
- **Agent system**: how agents are defined (spettro.agents.toml), loaded, and executed (internal/agent/)
- **TUI**: how the bubbletea TUI is structured (internal/tui/), key models and update paths
- **Configuration**: how config is loaded and what settings are available
- **Build & run**: how to build, run, and test the project (Makefile targets, go commands)
- **Conventions**: code style, naming, and patterns used in this codebase

CRITICAL: You MUST write the file to disk using file-write at path "SPETTRO.md" in the repository root. Do not just output the content — the file must exist after you finish.`
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
