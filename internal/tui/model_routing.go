package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"spettro/internal/config"
)

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	newModel, cmd := m.update(msg)
	if nm, ok := newModel.(Model); ok {
		nm = nm.recalcLayout()
		return nm, cmd
	}
	return newModel, cmd
}

// resetRunState clears every per-run field when an agent or plan run ends, so
// no channel, cursor, live-tool, or progress state leaks into the next run.
// Both the agentDoneMsg and planDoneMsg handlers begin with this identical
// teardown; keeping it in one place means a new per-run field only has to be
// reset once.
func (m *Model) resetRunState() {
	m.thinking = false
	m.cancelAgent = nil
	m.toolCh = nil
	m.usageCh = nil
	m.approvalCh = nil
	m.askUserCh = nil
	m.liveTools = nil
	m.currentTool = nil
	m.pendingAuth = nil
	m.pendingQuestion = nil
	m.questionCursor = 0
	m.questionFreeform = false
	m.parallelAgents = nil
	m.progressNote = ""
	m.activePrompt = nil
	m.activeAgentID = ""
	m.refreshModifiedFiles()
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m = m.recalcLayout()
		if !m.ready {
			m.ready = true
			if !config.IsTrusted(m.cwd) {
				m.showTrust = true
			} else {
				msg := "spettro ready — /help for commands, shift+tab to switch mode"
				m.pushSystemMsg(msg)
			}
			m.refreshViewport()
		}
	case tickMsg:
		m.eyeFrame++
		// Auto-clear expired banners so the status bar falls back to
		// goal info (or empty) after 5 seconds.
		if m.banner != "" && !m.bannerClearAt.IsZero() && time.Since(m.bannerClearAt) >= 0 {
			m.banner = ""
			m.bannerKind = ""
			m.bannerClearAt = time.Time{}
		}
		cmds = append(cmds, tick())
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		cmds = append(cmds, cmd)
	case agentDoneMsg:
		if !m.thinking {
			break
		}
		m.resetRunState()
		// Live usage events already applied part (usually all) of this run's
		// cost; only the remainder is added here.
		if msg.tokensUsed > m.liveRunTokens {
			m.totalTokensUsed += msg.tokensUsed - m.liveRunTokens
		}
		m.liveRunTokens = 0
		if msg.contextTokens > 0 {
			m.contextTokens = msg.contextTokens
		}
		if msg.tokensUsed > 0 || msg.contextTokens > 0 {
			m.updateCompactWarningState()
		}
		if msg.err != nil {
			m.clearStreamMessages()
			// Adopt the run's partial conversation as the carried history: the
			// turn failed or was cancelled, but its tool calls and results are
			// still valid context for the next prompt.
			if len(msg.messages) > 0 {
				m.convHistory = msg.messages
			}
			m.finishAgentActivity(m.mode, "failed", msg.err.Error(), "")
			m.showBanner("error: "+msg.err.Error(), "error")
			m.publishRemote("assistant_error", map[string]interface{}{"error": msg.err.Error()})
		} else {
			m.syncTodosFromSession()
			// Adopt the run's structured conversation as the next turn's carried
			// history: reusing it verbatim is what keeps the prompt-cache prefix
			// stable and preserves tool calls/results for the model.
			if len(msg.messages) > 0 {
				m.convHistory = msg.messages
			}
			// Fold the live-streamed reasoning into the final message and drop
			// the transient draft blocks before appending the authoritative
			// result (which always supersedes whatever was streamed live).
			streamedThinking := m.collectStreamThinking()
			m.clearStreamMessages()
			main, thinking := stripThinking(msg.content)
			if thinking == "" {
				thinking = streamedThinking
			}
			m.messages = append(m.messages, ChatMessage{
				Role:     RoleAssistant,
				Content:  main,
				Thinking: thinking,
				Meta:     msg.meta,
				Tools:    toToolItems(msg.tools),
				At:       time.Now(),
			})
			m.finishAgentActivity(m.mode, "done", main, thinking)
			m.publishRemote("assistant_message", map[string]interface{}{
				"content":     main,
				"thinking":    thinking,
				"meta":        msg.meta,
				"tools_count": len(msg.tools),
				"tokens_used": msg.tokensUsed,
			})
		}
		m.publishRemoteState("agent_done")
		m.maybeNotify(msg.err)
		// Force a save at run completion: the debounced in-run saves may have
		// skipped the final assistant message if it landed inside the window.
		m.autoSave()
		m.refreshViewport()
		// Steering messages the run never got to (pushed after its last step
		// boundary) must not be lost: with no goal continuing, requeue them as
		// ordinary prompts. A live goal keeps them in the queue — the next
		// iteration shares it and delivers them.
		if m.activeGoal == nil && m.steering.Len() > 0 {
			for _, s := range m.steering.Drain() {
				m.queuePrompt(s, s, nil, nil)
				m.pushSystemMsg(fmt.Sprintf("undelivered steering re-queued as request: %s", truncateLabel(s, 140)))
			}
		}
		// Goal orchestration seam: if a goal is active, the loop decides
		// whether to continue, stall, or report completion — BEFORE the
		// queued-prompt / auto-compact fallback. Non-goal runs are unaffected.
		if m.activeGoal != nil && msg.err == nil {
			if next := m.advanceGoal(msg); next != nil {
				cmds = append(cmds, next)
			}
		} else if m.activeGoal != nil && msg.err != nil {
			// A hard run error (provider failure, etc.) — retry the iteration a
			// bounded number of times, else stall.
			if next := m.advanceGoalOnError(msg); next != nil {
				cmds = append(cmds, next)
			}
		} else if cmd := m.autoCompactIfNeeded(); cmd != nil {
			cmds = append(cmds, cmd)
		} else if newModel, nextCmd := m.maybeRunNextQueuedPrompt(); nextCmd != nil {
			nm, _ := newModel.(Model)
			m = nm
			cmds = append(cmds, nextCmd)
		}
		// If goal advancement cleared the active goal (completion/stall/error),
		// persist the cleared state so resume doesn't offer the finished goal.
		if m.activeGoal == nil {
			m.autoSave()
		}
	case planDoneMsg:
		if !m.thinking {
			break
		}
		m.resetRunState()
		// Plans run through the same streaming path; drop any live draft blocks
		// before rendering the plan card.
		m.clearStreamMessages()
		if msg.tokensUsed > m.liveRunTokens {
			m.totalTokensUsed += msg.tokensUsed - m.liveRunTokens
		}
		m.liveRunTokens = 0
		if msg.contextTokens > 0 {
			m.contextTokens = msg.contextTokens
		}
		if msg.tokensUsed > 0 || msg.contextTokens > 0 {
			m.updateCompactWarningState()
		}
		if msg.err != nil {
			m.finishAgentActivity(m.mode, "failed", msg.err.Error(), "")
			m.showBanner("plan error: "+msg.err.Error(), "error")
			m.publishRemote("plan_error", map[string]interface{}{"error": msg.err.Error()})
		} else {
			m.syncTodosFromSession()
			if len(msg.messages) > 0 {
				m.convHistory = msg.messages
			}
			m.pendingPlan = msg.plan
			m.messages = append(m.messages, ChatMessage{
				Role:    RoleAssistant,
				Kind:    "plan",
				Content: msg.plan,
				Tools:   toToolItems(msg.tools),
				At:      time.Now(),
			})
			m.finishAgentActivity(m.mode, "done", msg.plan, "")
			m.showPlanApproval = true
			m.planApprovalCursor = 0
			m.publishRemote("plan", map[string]interface{}{
				"plan":        msg.plan,
				"tools_count": len(msg.tools),
				"tokens_used": msg.tokensUsed,
			})
		}
		m.publishRemoteState("plan_done")
		m.maybeNotify(msg.err)
		m.autoSave()
		m.refreshViewport()
		if cmd := m.autoCompactIfNeeded(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case commitDoneMsg:
		if !m.thinking {
			break
		}
		m.thinking = false
		m.cancelAgent = nil
		m.refreshModifiedFiles()
		if msg.err != nil {
			m.showBanner("commit error: "+msg.err.Error(), "error")
			m.publishRemote("commit_error", map[string]interface{}{"error": msg.err.Error()})
		} else {
			m.messages = append(m.messages, ChatMessage{
				Role:    RoleSystem,
				Content: fmt.Sprintf("committed: %s\n\n%s", msg.commitMsg, coAuthorInfo),
				At:      time.Now(),
			})
			m.publishRemote("commit", map[string]interface{}{"message": msg.commitMsg})
		}
		m.publishRemoteState("commit_done")
		m.autoSave()
		m.refreshViewport()
	case searchDoneMsg:
		if !m.thinking {
			break
		}
		m.thinking = false
		m.cancelAgent = nil
		if msg.err != nil {
			m.showBanner("search error: "+msg.err.Error(), "error")
			m.publishRemote("search_error", map[string]interface{}{"error": msg.err.Error()})
		} else {
			m.messages = append(m.messages, ChatMessage{
				Role:    RoleSystem,
				Content: msg.result,
				At:      time.Now(),
			})
			m.publishRemote("search", map[string]interface{}{"result": msg.result})
		}
		m.publishRemoteState("search_done")
		m.autoSave()
		m.refreshViewport()
	case memoryMineDoneMsg:
		switch {
		case msg.err != nil:
			m.showBanner("memory mining failed: "+msg.err.Error(), "error")
		case msg.added == 0:
			m.showBanner(fmt.Sprintf("memory mining done — no new candidates (%d session(s) scanned)", msg.scanned), "info")
		default:
			m.showBanner(fmt.Sprintf("memory mining done — %d candidate(s) drafted, review with /memory review", msg.added), "success")
		}
	case memoryEditDoneMsg:
		if msg.err != nil {
			m.showBanner("memory edit failed: "+msg.err.Error(), "error")
		} else {
			m.showBanner("memory updated — applies from the next session", "success")
		}
	case compactDoneMsg:
		if !m.thinking {
			break
		}
		m.thinking = false
		m.cancelAgent = nil
		wasAutoCompact := m.autoCompactInFlight
		m.autoCompactInFlight = false
		if msg.err != nil {
			if wasAutoCompact {
				m.autoCompactFailures++
			}
			m.showBanner("compact error: "+msg.err.Error(), "error")
		} else {
			m.autoCompactFailures = 0
			m.autoSave()
			m.sessionID = ""
			m.todos = nil
			m.totalTokensUsed = 0
			m.contextTokens = 0
			m.compactWarningLevel = 0
			m.messages = []ChatMessage{{
				Role:    RoleSystem,
				Content: compactSummaryPrefix + "\n\n" + msg.summary,
				At:      time.Now(),
			}}
			// Reseed the carried structured history from the summary. The old
			// prefix is gone (one deliberate cache miss); every turn after this
			// extends the new prefix and caches again.
			m.convHistory = compactedHistorySeed(msg.summary)
		}
		m.publishRemoteState("compact_done")
		m.refreshViewport()
		// If a goal is active and this compaction was triggered by the goal
		// loop's inter-iteration compaction, resume the loop now.
		if m.goalResumeAfterCompact && m.activeGoal != nil && msg.err == nil {
			m.goalResumeAfterCompact = false
			_, cmd := m.dispatchGoalIteration()
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case agentTickMsg:
		m.tickCount++
		for _, a := range m.parallelAgents {
			if a.Status == "running" {
				cmds = append(cmds, agentTickCmd())
				break
			}
		}
		m.vp.SetContent(m.renderMessages())
	case toolProgressMsg:
		if m.thinking {
			t := msg.trace
			m.applyToolTraceToObservability(t)
			m.publishRemoteToolTrace(t)
			// When an agent finishes generating an image/video, push the
			// produced files into every bound Telegram chat. The
			// dispatcher is a no-op when the relay is offline or nobody
			// is subscribed, so it stays cheap on the hot path.
			m.dispatchTelegramMedia(t)
			if t.Name == "comment" {
				if t.Status == "success" {
					if message := extractCommentMessage(t.Args, t.Output); message != "" {
						m.setProgressNote(message)
					}
				}
				if m.toolCh != nil {
					cmds = append(cmds, waitForTool(m.toolCh))
				}
				m.vp.SetContent(m.renderMessages())
				m.vp.GotoBottom()
				break
			}
			switch t.Name {
			case "todo-write", "task-create", "task-update", "task-delete":
				if t.Status != "running" {
					m.syncTodosFromSession()
				}
			}
			m.trackSessionEditFromTrace(t)
			if t.Status != "running" {
				switch t.Name {
				case "file-write", "shell-exec", "bash", "agent":
					// Refresh the side-panel file list off the Update
					// goroutine, throttled so a burst of traces does not
					// spawn git serially on the hot path.
					if cmd := m.scheduleModifiedRefresh(); cmd != nil {
						cmds = append(cmds, cmd)
					}
					// Re-scan repo files so @-mention suggestions pick
					// up files created or deleted by the tool.
					if cmd := m.scheduleRepoScan(); cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			}
			if t.Status == "running" {
				item := ToolItem{Name: t.Name, Args: t.Args, Status: "running"}
				m.currentTool = &item
				m.appendToolStreamMessage(item)
			} else {
				m.toolSeq++
				completed := ToolItem{
					Name:   t.Name,
					Status: t.Status,
					Args:   t.Args,
					Output: t.Output,
					Seq:    m.toolSeq,
				}
				// Compute the diff off the Update goroutine: computeFileDiff
				// shells out to git, which used to block Update per edit. The
				// result is attached later via toolDiffMsg keyed on Seq.
				cmds = append(cmds, computeFileDiffCmd(completed.Seq, m.cwd, t.Name, t.Args, t.Status))
				// Cap m.liveTools to bound memory and the run summary built
				// at interrupt time. When the LLM emits very large tool
				// batches we keep the most recent maxLiveTools entries so
				// the most useful context (what just happened) survives.
				m.liveTools = append(m.liveTools, completed)
				if len(m.liveTools) > maxLiveTools {
					m.liveTools = append([]ToolItem(nil), m.liveTools[len(m.liveTools)-maxLiveTools:]...)
				}
				m.currentTool = nil
				m.updateToolStreamMessage(completed)
			}
			if m.toolCh != nil {
				cmds = append(cmds, waitForTool(m.toolCh))
			}
			m.vp.SetContent(m.renderMessages())
			m.vp.GotoBottom()
		}
	case streamChunkMsg:
		if m.thinking {
			m.applyStreamChunk(msg.chunk)
			if m.streamCh != nil {
				cmds = append(cmds, waitForStream(m.streamCh))
			}
			m.vp.SetContent(m.renderMessages())
			m.vp.GotoBottom()
		}
	case usageEventMsg:
		if m.thinking {
			if msg.event.StepTokens > 0 {
				m.totalTokensUsed += msg.event.StepTokens
				m.liveRunTokens += msg.event.StepTokens
			}
			if msg.event.ContextTokens > m.contextTokens {
				m.contextTokens = msg.event.ContextTokens
			}
			m.updateCompactWarningState()
			if m.usageCh != nil {
				cmds = append(cmds, waitForUsage(m.usageCh))
			}
		}
	case modifiedFilesMsg:
		m.gitBranch = msg.branch
		m.modifiedFiles = msg.files
	case toolDiffMsg:
		if msg.seq > 0 && strings.TrimSpace(msg.diff) != "" {
			m.attachToolDiff(msg.seq, msg.diff)
			m.vp.SetContent(m.renderMessages())
		}
	case shellApprovalRequestMsg:
		if m.thinking {
			m.pendingAuth = &msg
			m.approvalCursor = 0
			m.ta.Reset()
			m.showBanner("command approval required", "warn")
			m.publishRemote("approval_request", map[string]interface{}{
				"command":  msg.request.Command,
				"tool_id":  msg.request.ToolID,
				"segments": msg.request.Segments,
				"reason":   msg.request.Reason,
			})
			if m.approvalCh != nil {
				cmds = append(cmds, waitForShellApproval(m.approvalCh))
			}
			m.refreshViewport()
		}
	case askUserRequestMsg:
		if m.thinking {
			m.pendingQuestion = &msg
			m.questionCursor = askUserDefaultCursor(msg.request)
			m.questionFreeform = len(msg.request.Options) == 0
			m.ta.Reset()
			m.showBanner("agent is waiting for your answer", "info")
			m.publishRemote("ask_user", map[string]interface{}{
				"question":            msg.request.Question,
				"options":             msg.request.Options,
				"context":             msg.request.Context,
				"default":             msg.request.DefaultOption,
				"allow_free_response": msg.request.AllowFreeResponse,
			})
			if m.askUserCh != nil {
				cmds = append(cmds, waitForAskUser(m.askUserCh))
			}
			m.refreshViewport()
		}
	case pasteImageMsg:
		if msg.err != nil {
			m.clipboardCounter-- // keep numbering gap-free on failure
			m.showBanner("paste image: "+msg.err.Error(), "error")
			return m, tea.Batch(cmds...)
		}
		m.attachments = append(m.attachments, attachmentItem{
			Kind:    "image",
			Path:    msg.path,
			RelPath: fmt.Sprintf("Image #%d", msg.counter),
		})
		m.showBanner(fmt.Sprintf("pasted Image #%d", msg.counter), "success")
		m.refreshViewport()
		return m, tea.Batch(cmds...)
	case verifyKeyDoneMsg:
		newModel, cmd := m.handleVerifyKeyDone(msg)
		return newModel, cmd
	case localProbeDoneMsg:
		newModel, cmd := m.handleLocalProbeDone(msg)
		return newModel, cmd
	case loginInitiatedMsg:
		return m.handleLoginInitiated(msg)
	case loginPolledMsg:
		return m.handleLoginPolled(msg)
	case spettroLoadedMsg:
		return m.handleSpettroLoaded(msg)
	case updateCheckMsg:
		return m.handleUpdateCheck(msg)
	case updateAppliedMsg:
		return m.handleUpdateApplied(msg)
	case repoFilesScannedMsg:
		m.repoFiles = msg.files
		m.lastRepoScanAt = time.Now()
		if cmd := m.syncInputSuggestions(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case tea.FocusMsg:
		m.terminalFocused = true
	case tea.BlurMsg:
		m.terminalFocused = false
	case bannerClearMsg:
		m.banner = ""
		m.bannerKind = ""
		m.bannerClearAt = time.Time{}
	case remoteSubmitMsg:
		newModel, cmd := m.handleRemoteSubmission(msg.req)
		nm, _ := newModel.(Model)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		if nm.remoteServer != nil {
			cmds = append(cmds, waitForRemoteSubmit(nm.remoteServer))
		}
		return nm, tea.Batch(cmds...)
	case remoteInterruptMsg:
		if m.thinking {
			m.interruptRun("Interrupted by remote client.", false)
			m.publishRemote("remote_interrupt", map[string]interface{}{"thinking": true})
			m.refreshViewport()
		} else {
			m.publishRemote("remote_interrupt", map[string]interface{}{"thinking": false})
		}
		if m.remoteServer != nil {
			cmds = append(cmds, waitForRemoteInterrupt(m.remoteServer))
		}
	case telegramAutostartDoneMsg:
		return m.handleTelegramAutostartDone(msg)
	case telegramSubmitMsg:
		newModel, cmd := m.handleTelegramSubmission(msg.req)
		nm, _ := newModel.(Model)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		if nm.telegramRelay != nil {
			cmds = append(cmds, waitForTelegramSubmit(nm.telegramRelay))
		}
		return nm, tea.Batch(cmds...)
	case telegramInterruptMsg:
		if m.thinking {
			m.interruptRun("Interrupted via Telegram.", false)
			m.publishRemote("telegram_interrupt", map[string]interface{}{"thinking": true})
			m.refreshViewport()
		} else {
			m.publishRemote("telegram_interrupt", map[string]interface{}{"thinking": false})
		}
		if m.telegramRelay != nil {
			cmds = append(cmds, waitForTelegramInterrupt(m.telegramRelay))
		}
	case quitWarningMsg:
		if m.banner == "press again ctrl C to exit" {
			m.banner = ""
			m.bannerKind = ""
			m.bannerClearAt = time.Time{}
			m.ctrlCAt = time.Time{}
		}
	case tea.MouseMsg:
		if m.mouseCaptureOff {
			return m, tea.Batch(cmds...)
		}
		// v2 splits mouse events into distinct message types; only wheel and
		// click (press) events drive the UI here — motion and release are
		// ignored, matching the v1 behavior of matching on wheel/left buttons.
		switch msg.(type) {
		case tea.MouseWheelMsg, tea.MouseClickMsg:
		default:
			return m, tea.Batch(cmds...)
		}
		mouse := msg.Mouse()
		if m.showResume {
			switch mouse.Button {
			case tea.MouseWheelUp:
				if m.resumeCursor > 0 {
					m.resumeCursor--
				}
				m.ensureResumeWindow()
				return m, tea.Batch(cmds...)
			case tea.MouseWheelDown:
				if m.resumeCursor < len(m.resumeItems)-1 {
					m.resumeCursor++
				}
				m.ensureResumeWindow()
				return m, tea.Batch(cmds...)
			}
		}
		sideW := m.sidePanelWidth()
		onSidePanel := sideW > 0 && mouse.X >= m.paneWidth()+1
		if onSidePanel {
			items := m.sidePanelItems()
			innerHeight := m.sidePanelInnerHeight()
			reserved := m.sidePanelReservedRows(sideW)
			_, _, rows := m.sidePanelWindow(items, innerHeight, reserved)
			maxStart := max(0, len(items)-rows)
			switch mouse.Button {
			case tea.MouseWheelUp:
				if m.sideDetailScroll > 0 {
					m.sideDetailScroll--
					return m, tea.Batch(cmds...)
				}
				if m.sideScroll > 0 {
					m.sideScroll--
				}
				if m.sideCursor > 0 {
					m.sideCursor--
					m.sideDetailScroll = 0
				}
				return m, tea.Batch(cmds...)
			case tea.MouseWheelDown:
				detailMax := m.sidePanelDetailMaxScroll(sideW)
				if m.sideDetailScroll < detailMax {
					m.sideDetailScroll++
					return m, tea.Batch(cmds...)
				}
				if m.sideScroll < maxStart {
					m.sideScroll++
				}
				if m.sideCursor < len(items)-1 {
					m.sideCursor++
					m.sideDetailScroll = 0
				}
				return m, tea.Batch(cmds...)
			case tea.MouseLeft:
				startY, _ := m.sideListGeometry()
				row := mouse.Y - startY
				if row >= 0 {
					cursor, start, rows := m.sidePanelWindow(items, innerHeight, reserved)
					_, rowToItem := m.sidePanelLines(items, sideW, cursor, start, rows)
					if row >= 0 && row < len(rowToItem) {
						idx := rowToItem[row]
						if idx >= 0 && idx < len(items) {
							if m.sideCursor != idx {
								m.sideDetailScroll = 0
							}
							m.sideCursor = idx
						}
					}
					if len(rowToItem) == 0 {
						idx := m.sideScroll + row
						if idx >= 0 && idx < len(items) {
							if m.sideCursor != idx {
								m.sideDetailScroll = 0
							}
							m.sideCursor = idx
						}
					}
				}
				return m, tea.Batch(cmds...)
			}
		}
		switch mouse.Button {
		case tea.MouseWheelUp:
			switch {
			case m.showOnboarding && m.onboarding.step == 0:
				if m.onboarding.cursor > 0 {
					m.onboarding.cursor--
				}
			case m.showSelector:
				if m.selCursor > 0 {
					m.selCursor--
				}
			case m.showConnect:
				if m.connectCursor > 0 {
					m.connectCursor--
				}
			default:
				m.vp.ScrollUp(3)
			}
		case tea.MouseWheelDown:
			switch {
			case m.showOnboarding && m.onboarding.step == 0:
				if m.onboarding.cursor < len(m.onboarding.items)-1 {
					m.onboarding.cursor++
				}
			case m.showSelector:
				if m.selCursor < len(m.selItems)-1 {
					m.selCursor++
				}
			case m.showConnect:
				if m.connectCursor < len(m.connectItems)-1 {
					m.connectCursor++
				}
			default:
				m.vp.ScrollDown(3)
			}
		}
		return m, tea.Batch(cmds...)
	case tea.PasteMsg:
		// Bracketed paste is not a KeyPressMsg, so it bypasses the modal key
		// routing below. Forward it to the textarea when the active modal is
		// in a text-entry step (API key / endpoint / setup input); otherwise
		// swallow it so pasted text can't leak into list filters. With no
		// modal active it falls through to the passthrough guard below.
		if modal := m.activeModal(); modal != modalNone {
			inTextEntry := (modal == modalConnect && m.connectStep == 1) ||
				(modal == modalOnboarding && m.onboarding.step == 1) ||
				modal == modalSetup
			if !inTextEntry {
				return m, tea.Batch(cmds...)
			}
			var taCmd tea.Cmd
			m.ta, taCmd = m.ta.Update(msg)
			cmds = append(cmds, taCmd)
			return m, tea.Batch(cmds...)
		}
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+t" {
			// View() reads mouseCaptureOff to pick the view's MouseMode; no
			// imperative enable/disable command exists in v2.
			m.mouseCaptureOff = !m.mouseCaptureOff
			if m.mouseCaptureOff {
				m.showBanner("text-select mode — mouse off, ctrl+t to re-enable", "info")
			} else {
				m.showBanner("mouse on — scroll wheel and side panel clicks active", "info")
			}
			return m, tea.Batch(cmds...)
		}
		if h, ok := modalHandlers[m.activeModal()]; ok {
			return h.update(m, msg)
		}
		return m.updateMain(msg)
	}

	// Only forward passthrough (non-key) messages to the textarea/viewport
	// when no overlay owns the UI. Consulting activeModal() keeps this guard
	// in lockstep with the routing above (it previously omitted onboarding).
	if m.activeModal() == modalNone {
		var taCmd tea.Cmd
		m.ta, taCmd = m.ta.Update(msg)
		cmds = append(cmds, taCmd)
		if cmd := m.syncInputSuggestions(); cmd != nil {
			cmds = append(cmds, cmd)
		}

		var vpCmd tea.Cmd
		m.vp, vpCmd = m.vp.Update(msg)
		cmds = append(cmds, vpCmd)
	}

	return m, tea.Batch(cmds...)
}
