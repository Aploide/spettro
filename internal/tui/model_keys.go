package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"

	"spettro/internal/config"
)

func (m Model) updateMain(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.showPlanApproval {
		return m.updatePlanApproval(msg)
	}
	if m.showSteerChoice {
		return m.updateSteerChoice(msg)
	}
	if m.pendingAuth != nil {
		return m.updateShellApproval(msg)
	}
	if m.pendingQuestion != nil {
		return m.updateAskUserQuestion(msg)
	}

	switch msg.String() {
	case "ctrl+c":
		if !m.ctrlCAt.IsZero() && time.Since(m.ctrlCAt) < 5*time.Second {
			return m, tea.Quit
		}
		m.ctrlCAt = time.Now()
		m.showBanner("press again ctrl C to exit", "warn")
		return m, tea.Tick(5*time.Second, func(time.Time) tea.Msg { return quitWarningMsg{} })
	case "ctrl+q":
		return m, tea.Quit
	case "up":
		if len(m.cmdItems) > 0 || len(m.mentionItems) > 0 {
			if m.cmdCursor > 0 {
				m.cmdCursor--
			}
			if m.mentionCursor > 0 {
				m.mentionCursor--
			}
			return m, nil
		}
		if m.recallPreviousInput() {
			if cmd := m.syncInputSuggestions(); cmd != nil {
				return m, cmd
			}
			return m, nil
		}
	case "ctrl+y":
		for i := len(m.messages) - 1; i >= 0; i-- {
			if m.messages[i].Role == RoleAssistant {
				if err := clipboard.WriteAll(m.messages[i].Content); err != nil {
					m.showBanner("clipboard error: "+err.Error(), "error")
				} else {
					m.showBanner("last response copied to clipboard", "success")
				}
				return m, nil
			}
		}
		m.showBanner("no response to copy yet", "info")
		return m, nil
	case "ctrl+v":
		if m.showAttachPrompt {
			break // let textarea handle text paste in attach mode
		}
		if !m.providers.SupportsVision(m.cfg.ActiveProvider, m.cfg.ActiveModel) {
			m.showBanner("current model does not support vision", "error")
			return m, nil
		}
		if err := m.ensureClipboardTempDir(); err != nil {
			m.showBanner("clipboard temp dir: "+err.Error(), "error")
			return m, nil
		}
		m.clipboardCounter++
		return m, readClipboardImageCmd(m.clipboardTempDir, m.clipboardCounter)
	case "ctrl+f":
		if m.showAttachPrompt {
			m.ta.Reset()
			m.ta.SetValue(m.attachDraft)
			m.ta.Placeholder = "enter message…"
			m.showAttachPrompt = false
			return m, nil
		}
		m.attachDraft = m.ta.Value()
		m.ta.Reset()
		m.ta.Placeholder = "file path to attach…"
		m.showAttachPrompt = true
		m.showBanner("enter file path and press enter (esc cancels)", "info")
		return m, nil
	case "ctrl+r":
		if m.showAttachPrompt {
			m.ta.Reset()
			m.ta.SetValue(m.attachDraft)
			m.ta.Placeholder = "enter message…"
			m.showAttachPrompt = false
			return m, nil
		}
		if len(m.attachments) > 0 {
			removed := m.attachments[len(m.attachments)-1]
			m.attachments = m.attachments[:len(m.attachments)-1]
			m.showBanner("removed attachment: "+removed.RelPath, "info")
		} else {
			m.showBanner("no attachments to remove", "info")
		}
		return m, nil
	case "down", "ctrl+n":
		if len(m.cmdItems) > 0 || len(m.mentionItems) > 0 {
			if m.cmdCursor < len(m.cmdItems)-1 {
				m.cmdCursor++
			}
			if m.mentionCursor < len(m.mentionItems)-1 {
				m.mentionCursor++
			}
			return m, nil
		}
		if m.recallNextInput() {
			if cmd := m.syncInputSuggestions(); cmd != nil {
				return m, cmd
			}
			return m, nil
		}
	case "tab":
		if len(m.cmdItems) > 0 {
			m.cmdCursor = (m.cmdCursor + 1) % len(m.cmdItems)
			return m, nil
		}
		if len(m.mentionItems) > 0 {
			m.mentionCursor = (m.mentionCursor + 1) % len(m.mentionItems)
			return m, nil
		}
	case "shift+tab":
		m.mode = nextAgent(m.manifest, m.mode)
		m.persistUIState()
		m.showBanner(fmt.Sprintf("switched to %s mode", m.mode), "info")
		m.publishRemoteState("mode_change")
		return m, nil
	case "ctrl+o":
		m.showTools = !m.showTools
		if !m.showTools {
			m.showFullOutput = false
		}
		m.sideDetailScroll = 0
		m.refreshViewport()
		return m, nil
	case "ctrl+g":
		// Toggle full (untrimmed) tool outputs; implies details visible.
		m.showFullOutput = !m.showFullOutput
		if m.showFullOutput {
			m.showTools = true
		}
		m.sideDetailScroll = 0
		m.refreshViewport()
		return m, nil
	case "ctrl+b":
		m.showSidePanel = !m.showSidePanel
		m.persistUIState()
		m.refreshModifiedFiles()
		m.refreshViewport()
		if m.showSidePanel {
			m.showBanner("activity panel enabled", "info")
		} else {
			m.showBanner("activity panel hidden", "info")
		}
		return m, nil
	case "f2":
		models := m.favoriteModels()
		if len(models) > 0 {
			current := -1
			for i, mod := range models {
				if mod.Provider == m.cfg.ActiveProvider && mod.Name == m.cfg.ActiveModel {
					current = i
					break
				}
			}
			next := models[(current+1)%len(models)]
			_ = m.updateConfig(func(cfg *config.UserConfig) error {
				cfg.ActiveProvider = next.Provider
				cfg.ActiveModel = next.Name
				return nil
			})
			m.showBanner(fmt.Sprintf("model → %s:%s", next.Provider, next.Name), "success")
		} else {
			m.showBanner("no favorite models — mark one with f in /models", "info")
		}
		return m, nil
	case "shift+f2":
		models := m.favoriteModels()
		if len(models) > 0 {
			current := -1
			for i, mod := range models {
				if mod.Provider == m.cfg.ActiveProvider && mod.Name == m.cfg.ActiveModel {
					current = i
					break
				}
			}
			prev := (current - 1 + len(models)) % len(models)
			_ = m.updateConfig(func(cfg *config.UserConfig) error {
				cfg.ActiveProvider = models[prev].Provider
				cfg.ActiveModel = models[prev].Name
				return nil
			})
			m.showBanner(fmt.Sprintf("model → %s:%s", models[prev].Provider, models[prev].Name), "success")
		} else {
			m.showBanner("no favorite models — mark one with f in /models", "info")
		}
		return m, nil
	case "enter":
		if m.showAttachPrompt {
			path := strings.TrimSpace(m.ta.Value())
			m.ta.Reset()
			m.ta.SetValue(m.attachDraft)
			m.ta.Placeholder = "enter message…"
			m.showAttachPrompt = false
			if path != "" {
				m.addAttachment(path)
			}
			return m, nil
		}
		if len(m.cmdItems) > 0 {
			chosen := m.cmdItems[m.cmdCursor].name
			// Commands that require a parameter (including sub-commands like
			// "/jobs kill <id>") complete into the input and wait instead of
			// executing.
			if requiresParam(chosen) {
				m.ta.SetValue(chosen + " ")
				m.cmdItems = nil
				m.cmdCursor = 0
				if cmd := m.syncInputSuggestions(); cmd != nil {
					return m, cmd
				}
				return m, nil
			}
			// Self-contained sub-commands (e.g. "/think high") execute immediately.
			if strings.Contains(chosen[1:], " ") {
				m.ta.Reset()
				m.cmdItems = nil
				m.cmdCursor = 0
				m.mentionItems = nil
				if m.thinking && !isInstantCommand(chosen) {
					m.showBanner("commands cannot be queued while an agent is running", "warn")
					return m, nil
				}
				return m.handleCommand(chosen)
			}
			// Other commands: execute if textarea already matches, else complete.
			current := strings.TrimSpace(m.ta.Value())
			if current == chosen {
				m.ta.Reset()
				m.cmdItems = nil
				m.cmdCursor = 0
				m.mentionItems = nil
				if m.thinking && !isInstantCommand(chosen) {
					m.showBanner("commands cannot be queued while an agent is running", "warn")
					return m, nil
				}
				return m.handleCommand(chosen)
			}
			m.ta.SetValue(chosen + " ")
			m.cmdItems = nil
			m.cmdCursor = 0
			if cmd := m.syncInputSuggestions(); cmd != nil {
				return m, cmd
			}
			return m, nil
		}
		if len(m.mentionItems) > 0 {
			m = m.acceptMention()
			if cmd := m.syncInputSuggestions(); cmd != nil {
				return m, cmd
			}
			return m, nil
		}
		input := strings.TrimSpace(m.ta.Value())
		if input == "" {
			return m, nil
		}
		isCmd := strings.HasPrefix(input, "/")
		instant := isCmd && isInstantCommand(input)
		if !instant {
			m.pushInputHistory(input)
		}
		m.ta.Reset()
		m.cmdItems = nil
		m.mentionItems = nil
		if m.pendingPlan != "" && !isCmd {
			return m.handlePlanEdit(input)
		}
		if isCmd {
			if m.thinking && !instant {
				m.showBanner("commands cannot be queued while an agent is running", "warn")
				return m, nil
			}
			return m.handleCommand(input)
		}
		if m.thinking {
			// A run is active: let the user choose between steering the
			// current run and queueing the message for after it.
			m.showSteerChoice = true
			m.steerPending = input
			m.steerCursor = 0
			m.refreshViewport()
			return m, nil
		}
		return m.handlePrompt(input)
	case "esc":
		if m.showAttachPrompt {
			m.ta.Reset()
			m.ta.SetValue(m.attachDraft)
			m.ta.Placeholder = "enter message…"
			m.showAttachPrompt = false
			return m, nil
		}
		if m.thinking {
			m.interruptRun("Stopped by user.", true)
			m.refreshViewport()
			return m, nil
		}
		if len(m.cmdItems) > 0 {
			m.ta.Reset()
			m.cmdItems = nil
			m.cmdCursor = 0
			return m, nil
		}
		if len(m.mentionItems) > 0 {
			m.mentionItems = nil
			m.mentionCursor = 0
			if cmd := m.syncInputSuggestions(); cmd != nil {
				return m, cmd
			}
			return m, nil
		}
		// Double-esc while idle opens the rewind picker (like /rewind).
		if !m.lastEscAt.IsZero() && time.Since(m.lastEscAt) < 500*time.Millisecond {
			m.lastEscAt = time.Time{}
			return m.openRewind()
		}
		m.lastEscAt = time.Now()
		m.ta.Reset()
		m.banner = ""
		m.bannerKind = ""
		m.bannerClearAt = time.Time{}
		return m, nil
	}

	var taCmd tea.Cmd
	m.ta, taCmd = m.ta.Update(msg)
	if cmd := m.syncInputSuggestions(); cmd != nil {
		return m, tea.Batch(taCmd, cmd)
	}
	return m, taCmd
}
