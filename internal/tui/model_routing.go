package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/config"
	"spettro/internal/theme"
)

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	newModel, cmd := m.update(msg)
	if nm, ok := newModel.(Model); ok {
		if nm.frameMemo == nil {
			nm.frameMemo = &frameMemo{}
		}
		if !transcriptOnly(msg) {
			nm.chromeSeq++
		}
		nm = nm.recalcLayout()
		if timers := nm.armTimers(); timers != nil {
			cmd = tea.Batch(cmd, timers)
		}
		if git := nm.gitRefreshCmd(); git != nil {
			cmd = tea.Batch(cmd, git)
		}
		if fill := nm.fillCmd(); fill != nil {
			cmd = tea.Batch(cmd, fill)
		}
		if save := nm.uiStateSaveCmd(); save != nil {
			cmd = tea.Batch(cmd, save)
		}
		return nm, cmd
	}
	return newModel, cmd
}

// isActiveRun reports whether a message from the run identified by queue
// belongs to the run in progress. A stopped run's messages do not, even
// when a new run has started since. A nil queue (tests build messages
// without one) matches whatever run is in progress.
func (m *Model) isActiveRun(queue *runEventQueue) bool {
	return m.thinking && (queue == nil || queue == m.runEvents)
}

// resetRunState clears every per-run field when an agent or plan run ends, so
// no channel, cursor, live-tool, or progress state leaks into the next run.
// Both the agentDoneMsg and planDoneMsg handlers begin with this identical
// teardown; keeping it in one place means a new per-run field only has to be
// reset once.
func (m *Model) resetRunState() {
	m.thinking = false
	m.cancelAgent = nil
	m.runEvents = nil
	m.usageCh = nil
	m.approvalCh = nil
	m.askUserCh = nil
	m.liveTools = nil
	m.currentTool = nil
	m.pendingAuth = nil
	m.pendingQuestion = nil
	m.discardQuestionQueue(fmt.Errorf("run ended"))
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
		oldTranscriptW := m.transcriptWidth()
		m.width = msg.Width
		m.height = msg.Height
		m = m.recalcLayout()
		if m.ready && m.transcriptWidth() != oldTranscriptW {
			// The transcript is rendered for one width, and the viewport
			// cuts rows wider than itself without a trace. Nothing else
			// re-renders it while the run is idle or waiting on an
			// approval, so a narrower terminal would keep showing rows
			// cut mid-word, and a wider one rows wrapped too early.
			m.refreshViewport()
		}
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
	case tea.BackgroundColorMsg:
		// The answer to Init's OSC 11 query, and occasionally an unsolicited
		// report after the user switches their terminal's own theme mid
		// session. Terminals may send it more than once and may send it long
		// after the first frames, so the handler stays cheap and idempotent —
		// and it only applies while the theme is on auto, since an explicit
		// selection outranks anything the terminal claims.
		if m.themeAuto {
			// A nil colour is the parser's documented "could not decode": the
			// OSC 11 body was malformed, or the terminal answered with
			// something that is not a colour. Resolve already treats it as
			// "nothing detected" and falls through to COLORFGBG or dark, so
			// the flag that records *why* must not claim a detection either —
			// /theme reads it to explain which source decided.
			m = m.applyTheme(theme.Resolve(theme.AutoKind, msg.Color))
			m.themeDetected = msg.Color != nil
		}
	case tickMsg:
		// Update re-arms the next tick only while something animates (see
		// armTimers).
		m.tickArmed = false
		m.eyeFrame++
		// Auto-clear expired banners so the status bar falls back to
		// goal info (or empty) after 5 seconds.
		if m.banner != "" && !m.bannerClearAt.IsZero() && time.Since(m.bannerClearAt) >= 0 {
			m.banner = ""
			m.bannerKind = ""
			m.bannerClearAt = time.Time{}
		}
		if m.transcriptHasLiveTail() {
			// A running pty tool's live tail comes from the pty session,
			// not from the message, so only a repaint shows it moving.
			m.refreshViewport()
		}
	case bannerExpiredMsg:
		if m.banner != "" && m.bannerClearAt.Equal(msg.at) {
			m.banner = ""
			m.bannerKind = ""
			m.bannerClearAt = time.Time{}
		}
	case agentDoneMsg:
		if !m.isActiveRun(msg.run) {
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
			m.publishRemote("assistant_error", map[string]any{"error": msg.err.Error()})
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
			m.publishRemote("assistant_message", map[string]any{
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
		if !m.isActiveRun(msg.run) {
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
			m.publishRemote("plan_error", map[string]any{"error": msg.err.Error()})
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
			m.publishRemote("plan", map[string]any{
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
			m.publishRemote("commit_error", map[string]any{"error": msg.err.Error()})
		} else {
			m.messages = append(m.messages, ChatMessage{
				Role:    RoleSystem,
				Content: fmt.Sprintf("committed: %s\n\n%s", msg.commitMsg, coAuthorInfo),
				At:      time.Now(),
			})
			m.publishRemote("commit", map[string]any{"message": msg.commitMsg})
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
			m.publishRemote("search_error", map[string]any{"error": msg.err.Error()})
		} else {
			m.messages = append(m.messages, ChatMessage{
				Role:    RoleSystem,
				Content: msg.result,
				At:      time.Now(),
			})
			m.publishRemote("search", map[string]any{"result": msg.result})
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
	case memoryCurateDoneMsg:
		switch {
		case msg.err != nil:
			m.showBanner("memory curation failed: "+msg.err.Error(), "error")
		case len(msg.items) == 0:
			m.showBanner("memory curation done — nothing needs changing", "info")
		default:
			m.showMemoryCurate = true
			m.memoryCurateItems = msg.items
			m.memoryCurateCursor = 0
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
		} else if msg.noop {
			if wasAutoCompact {
				// Nothing worth compacting yet (see autoCompactParams): stay
				// quiet, and don't try again until the history has grown.
				m.autoCompactNoopLen = len(m.convHistory)
			} else {
				m.showBanner("history is small enough already; nothing was compacted", "info")
			}
		} else {
			m.autoCompactFailures = 0
			m.autoCompactNoopLen = 0
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
			// Carry the compacted structured history (or, after summarizing
			// the flat transcript, reseed it from the summary). The old prefix
			// is gone (one deliberate cache miss); every turn after this
			// extends the new prefix and caches again.
			if msg.messages != nil {
				m.convHistory = msg.messages
			} else {
				m.convHistory = compactedHistorySeed(msg.summary)
			}
		}
		m.publishRemoteState("compact_done")
		m.refreshViewport()
		// If a goal is active and this compaction was triggered by the goal
		// loop's inter-iteration compaction, resume the loop now.
		if m.goalResumeAfterCompact && m.activeGoal != nil && msg.err == nil {
			m.goalResumeAfterCompact = false
			if cmd := m.dispatchNextGoalIteration(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case loopTickMsg:
		return m.handleLoopTick(msg)
	case agentTickMsg:
		m.tickCount++
		for _, a := range m.parallelAgents {
			if a.Status == "running" {
				cmds = append(cmds, agentTickCmd())
				break
			}
		}
		m.refreshViewport()
	case runEventsMsg:
		// A batch left over from a stopped run is dropped: its queue is no
		// longer the active one (tests deliver batches with no queue).
		if m.isActiveRun(msg.queue) {
			for _, ev := range msg.events {
				if ev.chunk != nil {
					m.applyStreamChunk(*ev.chunk)
				} else if ev.trace != nil {
					cmds = append(cmds, m.applyToolTrace(*ev.trace)...)
				}
			}
			// One refresh for the whole batch, however many events it held.
			m.refreshViewport()
			if m.runEvents != nil {
				cmds = append(cmds, waitForRunEvents(m.runEvents))
			}
		}
	case toolProgressMsg:
		// A single trace, as tests deliver them; a live run delivers
		// runEventsMsg batches.
		if m.thinking {
			cmds = append(cmds, m.applyToolTrace(msg.trace)...)
			m.refreshViewport()
		}
	case streamChunkMsg:
		if m.thinking {
			m.applyStreamChunk(msg.chunk)
			m.refreshViewport()
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
	case renderFillMsg:
		if m.renderCache != nil {
			m.renderCache.fillArmed = false
		}
		m.refreshViewport()
	case diffCommandMsg:
		m.applyDiffCommand(msg)
	case toolDiffMsg:
		if msg.seq > 0 && strings.TrimSpace(msg.diff) != "" {
			m.attachToolDiff(msg.seq, msg.diff)
			// The diff lands on an earlier tool row after later output may
			// already be on screen; refreshViewport keeps a view that was
			// following the latest output at the bottom as the row grows.
			m.refreshViewport()
		}
	case shellApprovalRequestMsg:
		if m.thinking {
			m.pendingAuth = &msg
			m.approvalCursor = 0
			m.approvalPreviewExpanded = false
			m.approvalScroll = 0
			m.ta.Reset()
			m.showBanner("command approval required", "warn")
			m.notifyIfUnfocused("Agent is waiting for command approval")
			m.publishRemote("approval_request", map[string]any{
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
		switch {
		case !m.thinking:
			// The run ended or was cancelled while this question was in
			// flight. Answer it so the tool call unblocks instead of hanging
			// on a reply that can never come.
			answerAskUser(msg, askUserResponse{err: fmt.Errorf("run ended before the question was answered")})
		case m.pendingQuestion == nil:
			m = m.presentQuestion(msg)
		default:
			// One question at a time owns the input, but the others are only
			// waiting their turn: dropping one would strand its tool call.
			m.questionQueue = append(m.questionQueue, msg)
			m.showBanner(fmt.Sprintf("another question arrived — %d waiting after this one", len(m.questionQueue)), "info")
		}
		// Keep draining regardless of what happened above, so a question that
		// arrives next is still seen.
		if m.askUserCh != nil {
			cmds = append(cmds, waitForAskUser(m.askUserCh))
		}
		m.refreshViewport()
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
	case skillInstalledMsg:
		m = m.finishSkillInstall(msg)
		m.refreshViewport()
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
			m.publishRemote("remote_interrupt", map[string]any{"thinking": true})
			m.refreshViewport()
		} else {
			m.publishRemote("remote_interrupt", map[string]any{"thinking": false})
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
			m.publishRemote("telegram_interrupt", map[string]any{"thinking": true})
			m.refreshViewport()
		} else {
			m.publishRemote("telegram_interrupt", map[string]any{"thinking": false})
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
		// v2 splits mouse events into distinct message types. Wheel and click
		// (press) events drive the UI below; motion and release implement
		// drag-to-select: a left drag highlights text and the release copies
		// it to the clipboard (OSC 52), so copying works without disabling
		// mouse capture.
		switch msg.(type) {
		case tea.MouseWheelMsg, tea.MouseClickMsg:
		case tea.MouseMotionMsg:
			mm := msg.Mouse()
			if m.textSel.active && (mm.Button == tea.MouseLeft || m.textSel.dragging) {
				m.textSel.dragging = true
				m.textSel.endX, m.textSel.endY = mm.X, mm.Y
			}
			return m, tea.Batch(cmds...)
		case tea.MouseReleaseMsg:
			if m.textSel.dragging {
				text := extractSelection(m.viewContent(), m.textSel)
				if strings.TrimSpace(text) != "" {
					cmds = append(cmds, tea.SetClipboard(text))
					m.showBanner(fmt.Sprintf("copied %d chars to clipboard", len(text)), "success")
				}
			}
			m.textSel = textSelection{}
			return m, tea.Batch(cmds...)
		default:
			return m, tea.Batch(cmds...)
		}
		mouse := msg.Mouse()
		if _, isWheel := msg.(tea.MouseWheelMsg); isWheel {
			// Scrolling moves content under a screen-space selection; drop it.
			m.textSel = textSelection{}
		} else if mouse.Button == tea.MouseLeft {
			// Arm a potential drag selection at the press cell. Plain clicks
			// (press+release with no motion) are unaffected.
			m.textSel = textSelection{active: true, startX: mouse.X, startY: mouse.Y, endX: mouse.X, endY: mouse.Y}
		}
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
			switch mouse.Button {
			case tea.MouseWheelUp:
				if m.sideDetailScroll > 0 {
					m.sideDetailScroll--
					return m, tea.Batch(cmds...)
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
				if m.sideCursor < len(items)-1 {
					m.sideCursor++
					m.sideDetailScroll = 0
				}
				return m, tea.Batch(cmds...)
			case tea.MouseLeft:
				startY, rows := m.sideListGeometry()
				row := mouse.Y - startY
				if row >= 0 {
					// Same window the view rendered, so screen rows map
					// one-to-one onto items (headers map to -1).
					_, rowToItem := m.sidePanelList(items, sideW, rows)
					if row < len(rowToItem) {
						idx := rowToItem[row]
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
			inTextEntry := (modal == modalConnect && (m.connectStep == 1 || m.connectStep == 5)) ||
				(modal == modalOnboarding && m.onboarding.step == 1) ||
				(modal == modalQuestion && m.pendingQuestion.textEntry()) ||
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
		// Typing invalidates a screen-space selection (content may move).
		m.textSel = textSelection{}
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
		// The transcript viewport needs no passthrough: keys reach it
		// through updateMain (pgup/pgdown) and the wheel through the
		// tea.MouseMsg case above.
	}

	return m, tea.Batch(cmds...)
}
