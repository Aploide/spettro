package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/session"
)

func (a *App) handlePlanning(ctx context.Context, prompt string) error {
	spec, ok := a.manifest.AgentByID("plan")
	if !ok {
		return fmt.Errorf("plan agent not found")
	}
	ag := agent.LLMAgent{
		Spec:            spec,
		ProviderManager: a.providers,
		ProviderName:    func() string { return a.cfg.ActiveProvider },
		ModelName:       func() string { return a.cfg.ActiveModel },
		CWD:             a.cwd,
		Ultra:           a.cfg.UltraActive(),
		ToolCallback:    a.printToolProgress,
		Manifest:        &a.manifest,
		SessionDir:      a.cliSessionDir(),
	}
	result, err := ag.Run(ctx, prompt)
	if err != nil {
		return err
	}
	if err := a.store.WriteProjectFile("PLAN.md", result.Content); err != nil {
		return err
	}
	a.pendingPlan = result.Content
	a.printLine(a.ui.Panel(string(a.mode), "Plan Generated", "Saved to .spettro/PLAN.md.\nUse /approve in coding flow to execute."))
	return nil
}

func (a *App) handleCoding(ctx context.Context, prompt string) error {
	if a.cfg.Permission == config.PermissionAskFirst {
		a.printLine("ask-first mode: generate plan in planning mode, then use /approve")
		return nil
	}
	spec, ok := a.manifest.AgentByID("coding")
	if !ok {
		return fmt.Errorf("coding agent not found")
	}
	ag := agent.LLMAgent{
		Spec:            spec,
		ProviderManager: a.providers,
		ProviderName:    func() string { return a.cfg.ActiveProvider },
		ModelName:       func() string { return a.cfg.ActiveModel },
		CWD:             a.cwd,
		Ultra:           a.cfg.UltraActive(),
		ToolCallback:    a.printToolProgress,
		ShellApproval:   a.promptShellApproval,
		Manifest:        &a.manifest,
		SessionDir:      a.cliSessionDir(),
	}
	ag.Spec.Permission = a.cfg.Permission
	result, err := ag.Run(ctx, prompt)
	if err != nil {
		return err
	}
	a.printLine(a.ui.Panel(string(a.mode), "Assistant", result.Content))
	return nil
}

func (a *App) handleChat(ctx context.Context, prompt string) error {
	spec, ok := a.manifest.AgentByID("ask")
	if !ok {
		return fmt.Errorf("ask agent not found")
	}
	ag := agent.LLMAgent{
		Spec:            spec,
		ProviderManager: a.providers,
		ProviderName:    func() string { return a.cfg.ActiveProvider },
		ModelName:       func() string { return a.cfg.ActiveModel },
		CWD:             a.cwd,
		Ultra:           a.cfg.UltraActive(),
		ToolCallback:    a.printToolProgress,
		Manifest:        &a.manifest,
		SessionDir:      a.cliSessionDir(),
	}
	result, err := ag.Run(ctx, prompt)
	if err != nil {
		return err
	}
	a.printLine(a.ui.Panel(string(a.mode), "Assistant", result.Content))
	return nil
}

func (a *App) printStatus() {
	a.printLine(a.ui.Status(string(a.mode), string(a.cfg.Permission)))
}

func (a *App) printLine(s string) {
	fmt.Fprintln(a.out, s)
}

func (a *App) promptShellApproval(ctx context.Context, req agent.ShellApprovalRequest) (agent.ShellApprovalDecision, error) {
	if a.reader == nil {
		a.reader = bufio.NewReader(a.in)
	}
	for {
		if err := ctx.Err(); err != nil {
			return agent.ShellApprovalDeny, err
		}
		a.printLine(FormatShellApprovalPrompt(req.Command))
		fmt.Fprint(a.out, "> ")
		line, err := a.reader.ReadString('\n')
		if err != nil {
			return agent.ShellApprovalDeny, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "1", "y", "yes":
			return agent.ShellApprovalAllowOnce, nil
		case "2", "a", "always", "yes and don't ask again", "yes and dont ask again":
			return agent.ShellApprovalAllowAlways, nil
		case "3", "n", "no":
			return agent.ShellApprovalDeny, nil
		case "4":
			a.printLine("type what the agent should do instead:")
			fmt.Fprint(a.out, "> ")
			instead, rerr := a.reader.ReadString('\n')
			if rerr != nil {
				return agent.ShellApprovalDeny, rerr
			}
			instead = strings.TrimSpace(instead)
			if instead == "" {
				a.printLine("empty alternative instruction; command denied")
				return agent.ShellApprovalDeny, nil
			}
			return agent.ShellApprovalDeny, fmt.Errorf("shell-exec denied by user; do this instead: %s", instead)
		default:
			text := strings.TrimSpace(line)
			if text != "" {
				return agent.ShellApprovalDeny, fmt.Errorf("shell-exec denied by user; do this instead: %s", text)
			}
			a.printLine("invalid choice; use 1, 2, 3, or 4")
		}
	}
}

func FormatShellApprovalPrompt(command string) string {
	return strings.Join([]string{
		"spettro wants to run this command:",
		"  Bash(" + command + ")",
		"",
		"choose an action:",
		"  1) yes",
		"  2) yes and don't ask again",
		"  3) no",
		"  4) tell the agent what to do instead",
	}, "\n")
}

func (a *App) printToolProgress(tr agent.ToolTrace) {
	if tr.Status == "running" {
		switch tr.Name {
		case "file-write", "shell-exec", "bash", "agent":
			a.printLine(a.ui.Info(fmt.Sprintf("running %s...", tr.Name)))
		}
		return
	}
	if tr.Name == "comment" && tr.Status == "success" {
		if msg := commentMessage(tr.Args, tr.Output); msg != "" {
			a.printLine(a.ui.Info(msg))
		}
		return
	}
	if tr.Status == "error" {
		a.printLine(a.ui.Panel(string(a.mode), "Tool Error", fmt.Sprintf("%s failed.\n%s", tr.Name, strings.TrimSpace(tr.Output))))
	}
}

func commentMessage(args, output string) string {
	var payload struct {
		Message string `json:"message"`
	}
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &payload); err == nil && strings.TrimSpace(payload.Message) != "" {
			return strings.TrimSpace(payload.Message)
		}
	}
	return strings.TrimSpace(output)
}

func (a *App) cliSessionDir() string {
	id := "cli-" + session.ProjectHash(a.cwd)
	return session.SessionDir(a.store.GlobalDir, id)
}
