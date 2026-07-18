package app

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/indexer"
)

func (a *App) handleCommand(line string) error {
	fields := strings.Fields(line)
	switch fields[0] {
	case "/help":
		a.printLine(a.ui.Panel(string(a.mode), "Commands", "/setup, /next (Shift+Tab), /mode, /models [provider:model] [api_key], /permission <yolo|restricted|ask-first>, /index, /approve, /exit\nUse /models with no args for interactive picker."))
	case "/exit", "/quit":
		return io.EOF
	case "/setup":
		return a.startSetup()
	case "/login":
		a.printLine("deprecated: use /setup")
		return a.startSetup()
	case "/mode":
		a.mode = a.mode.Next()
		a.persistUIState()
		a.printLine(a.ui.Info(a.ui.Stage(string(a.mode))))
		a.printStatus()
	case "/models":
		if len(fields) < 2 {
			a.startModelPicker("")
			return nil
		}
		if !strings.Contains(fields[1], ":") {
			a.startModelPicker(fields[1])
			return nil
		}
		pair := strings.SplitN(fields[1], ":", 2)
		if len(pair) != 2 {
			return fmt.Errorf("invalid model selector")
		}
		if !a.providers.HasModel(pair[0], pair[1]) {
			return fmt.Errorf("unknown provider/model pair: %s", fields[1])
		}
		if len(fields) >= 3 {
			if err := config.SaveAPIKey(pair[0], fields[2]); err != nil {
				return err
			}
		}
		if err := a.updateConfig(func(cfg *config.UserConfig) error {
			cfg.ActiveProvider = pair[0]
			cfg.ActiveModel = pair[1]
			return nil
		}); err != nil {
			return err
		}
		a.printStatus()
	case "/permission":
		if len(fields) < 2 {
			return fmt.Errorf("usage: /permission <yolo|restricted|ask-first>")
		}
		level := config.PermissionLevel(fields[1])
		switch level {
		case config.PermissionYOLO, config.PermissionRestricted, config.PermissionAskFirst:
		default:
			return fmt.Errorf("invalid permission level")
		}
		return a.updateConfig(func(cfg *config.UserConfig) error {
			cfg.Permission = level
			return nil
		})
	case "/approve":
		if strings.TrimSpace(a.pendingPlan) == "" {
			a.printLine("no pending plan to approve")
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
		result, err := ag.Run(context.Background(), a.pendingPlan)
		if err != nil {
			return err
		}
		a.printLine(a.ui.Panel(string(a.mode), "Assistant", result.Content))
		a.pendingPlan = ""
	case "/index":
		snapshot, err := indexer.Build(a.cwd)
		if err != nil {
			return err
		}
		if err := indexer.WriteJSON(snapshot, filepath.Join(a.store.ProjectDir, "index.json")); err != nil {
			return err
		}
		a.printLine(a.ui.Panel(string(a.mode), "Indexer", fmt.Sprintf("Indexed %d files into .spettro/index.json", len(snapshot.Entries))))
	default:
		return fmt.Errorf("unknown command: %s", fields[0])
	}
	return nil
}
