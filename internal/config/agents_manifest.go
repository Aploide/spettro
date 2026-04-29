package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

const AgentManifestFilename = "spettro.agents.toml"

type SandboxMode string

const (
	SandboxWorkspaceWrite SandboxMode = "workspace-write"
	SandboxReadOnly       SandboxMode = "read-only"
	SandboxFullAccess     SandboxMode = "full-access"
)

type AgentRole string

const (
	AgentRolePrimary      AgentRole = "primary"
	AgentRoleSubagent     AgentRole = "subagent"
	AgentRoleOrchestrator AgentRole = "orchestrator"
	AgentRoleWorker       AgentRole = "worker"
)

type RuleAction string

const (
	RuleAllow RuleAction = "allow"
	RuleAsk   RuleAction = "ask"
	RuleDeny  RuleAction = "deny"
)

type PermissionRule struct {
	Permission string     `toml:"permission"`
	Pattern    string     `toml:"pattern"`
	Action     RuleAction `toml:"action"`
}

type DelegationPolicy struct {
	MaxParallelWorkers  int `toml:"max_parallel_workers"`
	MaxDepth            int `toml:"max_depth"`
	MaxToolCallsPerStep int `toml:"max_tool_calls_per_step"`
}

type AgentManifest struct {
	Version      int           `toml:"version"`
	DefaultAgent string        `toml:"default_agent"`
	Metadata     AgentMetadata `toml:"metadata"`
	Runtime      RuntimePolicy `toml:"runtime"`
	Tools        []ToolSpec    `toml:"tools"`
	Agents       []AgentSpec   `toml:"agents"`
}

type AgentMetadata struct {
	Name        string `toml:"name"`
	Description string `toml:"description"`
}

type RuntimePolicy struct {
	DefaultPermission PermissionLevel  `toml:"default_permission"`
	DefaultTimeoutSec int              `toml:"default_timeout_sec"`
	LogToolCalls      bool             `toml:"log_tool_calls"`
	SandboxMode       SandboxMode      `toml:"sandbox_mode"`
	Delegation        DelegationPolicy `toml:"delegation"`
	PermissionRules   []PermissionRule `toml:"permission_rules"`
	AllowNetworkTools bool             `toml:"allow_network_tools"` // legacy field; ignored
}

type ToolSpec struct {
	ID               string           `toml:"id"`
	Name             string           `toml:"name"`
	Description      string           `toml:"description"`
	Kind             string           `toml:"kind"`
	Enabled          bool             `toml:"enabled"`
	EntryPoint       string           `toml:"entry_point"`
	TimeoutSec       int              `toml:"timeout_sec"`
	RequiresApproval bool             `toml:"requires_approval"`
	PermittedActions []string         `toml:"permitted_actions"`
	Aliases          []string         `toml:"aliases"`
	InputSchema      map[string]any   `toml:"input_schema"`
	RiskLevel        string           `toml:"risk_level"`
	PrimaryOnly      bool             `toml:"primary_only"`
	PermissionRules  []PermissionRule `toml:"permission_rules"`
}

type AgentSpec struct {
	ID               string           `toml:"id"`
	Name             string           `toml:"name"`
	Description      string           `toml:"description"`
	Skill            string           `toml:"skill"`
	Mode             string           `toml:"mode"`
	Role             AgentRole        `toml:"role"`
	Color            string           `toml:"color"`
	ModelProvider    string           `toml:"model_provider"`
	Model            string           `toml:"model"`
	SystemPrompt     string           `toml:"system_prompt"`
	PromptFile       string           `toml:"prompt_file"`
	AllowedTools     []string         `toml:"allowed_tools"`
	PermittedActions []string         `toml:"permitted_actions"`
	Permission       PermissionLevel  `toml:"permission"`
	PermissionRules  []PermissionRule `toml:"permission_rules"`
	Temperature      float64          `toml:"temperature"`
	MaxTokens        int              `toml:"max_tokens"`
	MaxSteps         int              `toml:"max_steps"`
	Handoffs         []string         `toml:"handoffs"`
	Enabled          bool             `toml:"enabled"`
}

func DefaultAgentManifest() AgentManifest {
	m := AgentManifest{
		Version:      2,
		DefaultAgent: "plan",
		Metadata: AgentMetadata{
			Name:        "Spettro default agents",
			Description: "Built-in fallback manifest when no spettro.agents.toml is present.",
		},
		Runtime: RuntimePolicy{
			DefaultPermission: PermissionAskFirst,
			DefaultTimeoutSec: 120,
			LogToolCalls:      true,
			SandboxMode:       SandboxWorkspaceWrite,
			Delegation:        DelegationPolicy{MaxParallelWorkers: 4, MaxDepth: 2},
		},
		Tools: []ToolSpec{
			{ID: "glob", Name: "Glob", Description: "Find files by name pattern.", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: false, PermittedActions: []string{"read", "search"}, RiskLevel: "low"},
			{ID: "grep", Name: "Grep", Description: "Search file contents with regex.", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: false, PermittedActions: []string{"read", "search"}, RiskLevel: "low"},
			{ID: "file-read", Name: "File Reader", Description: "Reads file contents in the workspace.", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: false, PermittedActions: []string{"read"}, RiskLevel: "low"},
			{ID: "file-write", Name: "File Writer", Description: "Creates and edits files in the workspace.", Kind: "builtin", Enabled: true, TimeoutSec: 60, RequiresApproval: true, PermittedActions: []string{"write"}, RiskLevel: "high"},
			{ID: "file-edit", Name: "File Edit", Description: "Apply targeted edits to existing files.", Kind: "builtin", Enabled: true, TimeoutSec: 60, RequiresApproval: true, PermittedActions: []string{"write"}, RiskLevel: "high"},
			{ID: "shell-exec", Name: "Shell Executor", Description: "Runs shell commands in the project directory.", Kind: "builtin", Enabled: true, TimeoutSec: 120, RequiresApproval: true, PermittedActions: []string{"execute", "git"}, RiskLevel: "high"},
			{ID: "repo-search", Name: "Repository Search", Description: "Searches file names and content inside the project.", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: false, PermittedActions: []string{"read", "search"}, RiskLevel: "low"},
			{ID: "tool-search", Name: "Tool Search", Description: "Search available tools for current agent.", Kind: "builtin", Enabled: true, TimeoutSec: 20, RequiresApproval: false, PermittedActions: []string{"read", "search"}, RiskLevel: "low"},
			{ID: "ls", Name: "List Directory", Description: "List directory contents.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"read", "search"}, RiskLevel: "low"},
			{ID: "todo-write", Name: "Todo Write", Description: "Write a list of todos to track task progress.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"write"}, RiskLevel: "medium"},
			{ID: "task-create", Name: "Task Create", Description: "Create a structured task in session state.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"write"}, RiskLevel: "low"},
			{ID: "task-get", Name: "Task Get", Description: "Fetch one task by id.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"read"}, RiskLevel: "low"},
			{ID: "task-update", Name: "Task Update", Description: "Update a task by id.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"write"}, RiskLevel: "low"},
			{ID: "task-list", Name: "Task List", Description: "List tasks in current session.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"read"}, RiskLevel: "low"},
			{ID: "task-stop", Name: "Task Stop", Description: "Request stopping the current agent run.", Kind: "builtin", Enabled: true, TimeoutSec: 5, RequiresApproval: false, PermittedActions: []string{"plan", "ask"}, RiskLevel: "low"},
			{ID: "config", Name: "Config", Description: "Read or update selected runtime settings.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"read", "write", "plan"}, RiskLevel: "medium"},
			{ID: "bash", Name: "Bash", Description: "Execute a bash command and return output.", Kind: "builtin", Enabled: true, TimeoutSec: 120, RequiresApproval: true, PermittedActions: []string{"execute", "git"}, RiskLevel: "high"},
			{ID: "comment", Name: "Comment", Description: "Emit a progress comment or note.", Kind: "builtin", Enabled: true, TimeoutSec: 5, RequiresApproval: false, PermittedActions: []string{"read"}, RiskLevel: "low"},
			{ID: "ask-user", Name: "Ask User", Description: "Prompt the user for a decision.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"ask"}, RiskLevel: "low"},
			{ID: "enter-plan-mode", Name: "Enter Plan Mode", Description: "Switch execution into planning mode.", Kind: "builtin", Enabled: true, TimeoutSec: 5, RequiresApproval: false, PermittedActions: []string{"plan"}, RiskLevel: "low"},
			{ID: "exit-plan-mode", Name: "Exit Plan Mode", Description: "Exit planning mode.", Kind: "builtin", Enabled: true, TimeoutSec: 5, RequiresApproval: false, PermittedActions: []string{"plan"}, RiskLevel: "low"},
			{ID: "web-search", Name: "Web Search", Description: "Search the web and return result links.", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: true, PermittedActions: []string{"search", "network"}, RiskLevel: "medium"},
			{ID: "mcp-list-resources", Name: "MCP List Resources", Description: "List resources exposed by MCP servers.", Kind: "builtin", Enabled: true, TimeoutSec: 30, RequiresApproval: true, PermittedActions: []string{"read", "network"}, RiskLevel: "medium"},
			{ID: "mcp-read-resource", Name: "MCP Read Resource", Description: "Read one MCP resource.", Kind: "builtin", Enabled: true, TimeoutSec: 45, RequiresApproval: true, PermittedActions: []string{"read", "network"}, RiskLevel: "medium"},
			{ID: "mcp-auth", Name: "MCP Auth", Description: "Store MCP server auth credentials.", Kind: "builtin", Enabled: true, TimeoutSec: 20, RequiresApproval: true, PermittedActions: []string{"write", "network"}, RiskLevel: "high"},
			{ID: "enter-worktree", Name: "Enter Worktree", Description: "Create and enter a git worktree.", Kind: "builtin", Enabled: true, TimeoutSec: 120, RequiresApproval: true, PermittedActions: []string{"git", "write"}, RiskLevel: "high"},
			{ID: "exit-worktree", Name: "Exit Worktree", Description: "Remove a git worktree.", Kind: "builtin", Enabled: true, TimeoutSec: 120, RequiresApproval: true, PermittedActions: []string{"git", "write"}, RiskLevel: "high"},
			{ID: "send-message", Name: "Send Message", Description: "Send a structured coordination message.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"ask", "plan"}, RiskLevel: "low"},
			{ID: "agent", Name: "Agent", Description: "Spawn a sub-agent to handle a subtask.", Kind: "builtin", Enabled: true, TimeoutSec: 300, RequiresApproval: false, PermittedActions: []string{"read", "write", "execute", "git", "search", "plan", "ask"}, RiskLevel: "medium", PrimaryOnly: true},
			{ID: "devin-session", Name: "Devin Session", Description: "Delegate a self-contained subtask to a fresh Cognition Devin session and wait for its result.", Kind: "builtin", Enabled: true, TimeoutSec: 1800, RequiresApproval: true, PermittedActions: []string{"plan", "network", "execute", "write"}, RiskLevel: "high", PrimaryOnly: true},
			{ID: "skill-read", Name: "Skill Read", Description: "Activate an installed Agent Skill and load its SKILL.md instructions.", Kind: "builtin", Enabled: true, TimeoutSec: 15, RequiresApproval: false, PermittedActions: []string{"read"}, Aliases: []string{"activate-skill", "skill-activate"}, RiskLevel: "low"},
			{ID: "skill-list", Name: "Skill List", Description: "List installed Agent Skills with name + description.", Kind: "builtin", Enabled: true, TimeoutSec: 10, RequiresApproval: false, PermittedActions: []string{"read"}, RiskLevel: "low"},
		},
		Agents: []AgentSpec{
			{ID: "plan", Name: "Plan", Description: "Planning orchestrator", Skill: "planning", Mode: "orchestrator", Role: AgentRoleOrchestrator, Color: "blue", AllowedTools: []string{"agent", "devin-session", "glob", "grep", "file-read", "tool-search", "task-create", "task-get", "task-update", "task-list", "task-stop", "config", "ask-user", "enter-plan-mode", "exit-plan-mode", "send-message", "todo-write", "comment", "skill-read", "skill-list"}, PermittedActions: []string{"read", "search", "plan", "write"}, Permission: PermissionAskFirst, MaxSteps: 30, Enabled: true, Handoffs: []string{"explore", "review", "docs"}, PromptFile: "agents/planning.md"},
			{ID: "coding", Name: "Coding", Description: "Coding orchestrator", Skill: "implementation", Mode: "orchestrator", Role: AgentRolePrimary, Color: "green", AllowedTools: []string{"agent", "devin-session", "glob", "grep", "file-read", "file-write", "file-edit", "shell-exec", "bash", "ls", "tool-search", "task-create", "task-get", "task-update", "task-list", "task-stop", "config", "send-message", "todo-write", "comment", "skill-read", "skill-list"}, PermittedActions: []string{"read", "search", "plan", "write", "execute", "git"}, Permission: PermissionRestricted, MaxSteps: 32, Enabled: true, Handoffs: []string{"code", "git", "test", "review", "docs", "explore"}, PromptFile: "agents/coding.md"},
			{ID: "ask", Name: "Ask", Description: "Read-only orchestrator for Q&A", Skill: "conversation", Mode: "orchestrator", Role: AgentRolePrimary, Color: "cyan", AllowedTools: []string{"agent", "devin-session", "glob", "grep", "file-read", "tool-search", "web-search", "mcp-list-resources", "mcp-read-resource", "comment", "skill-read", "skill-list"}, PermittedActions: []string{"ask", "read", "search"}, Permission: PermissionAskFirst, MaxSteps: 16, Enabled: true, Handoffs: []string{"explore", "docs"}, PromptFile: "agents/chat.md"},
			{ID: "explore", Name: "Explore", Description: "Read-only code exploration worker", Skill: "analysis", Mode: "worker", Role: AgentRoleWorker, Color: "blue", AllowedTools: []string{"glob", "grep", "file-read", "ls", "comment", "skill-read", "skill-list"}, PermittedActions: []string{"read", "search"}, Permission: PermissionAskFirst, MaxSteps: 24, Enabled: true, Handoffs: []string{"explore", "review", "docs"}, PromptFile: "agents/explore.md"},
			{ID: "code", Name: "Code", Description: "Implementation worker", Skill: "implementation", Mode: "worker", Role: AgentRoleWorker, Color: "green", AllowedTools: []string{"agent", "glob", "grep", "file-read", "file-write", "file-edit", "shell-exec", "bash", "ls", "task-create", "task-get", "task-update", "task-list", "task-stop", "config", "enter-worktree", "exit-worktree", "comment", "todo-write", "skill-read", "skill-list"}, PermittedActions: []string{"read", "search", "write", "execute", "git"}, Permission: PermissionRestricted, MaxSteps: 24, Enabled: true, Handoffs: []string{"explore", "review", "test", "docs"}, PromptFile: "agents/coding.md"},
			{ID: "git", Name: "Git", Description: "Git operations worker", Skill: "git", Mode: "worker", Role: AgentRoleWorker, Color: "yellow", AllowedTools: []string{"glob", "grep", "file-read", "shell-exec", "bash", "ls", "comment", "skill-read", "skill-list"}, PermittedActions: []string{"read", "search", "execute", "git"}, Permission: PermissionRestricted, MaxSteps: 20, Enabled: true, Handoffs: []string{"review", "docs"}, PromptFile: "agents/git.md"},
			{ID: "test", Name: "Test", Description: "Test execution worker", Skill: "testing", Mode: "worker", Role: AgentRoleWorker, Color: "yellow", AllowedTools: []string{"glob", "grep", "file-read", "shell-exec", "bash", "ls", "comment", "skill-read", "skill-list"}, PermittedActions: []string{"read", "search", "execute"}, Permission: PermissionRestricted, MaxSteps: 20, Enabled: true, Handoffs: []string{"review", "explore"}, PromptFile: "agents/tester.md"},
			{ID: "review", Name: "Review", Description: "Code review worker", Skill: "review", Mode: "worker", Role: AgentRoleSubagent, Color: "red", AllowedTools: []string{"glob", "grep", "file-read", "shell-exec", "bash", "ls", "comment", "skill-read", "skill-list"}, PermittedActions: []string{"read", "search", "execute", "plan"}, Permission: PermissionAskFirst, MaxSteps: 20, Enabled: true, Handoffs: []string{"explore", "docs"}, PromptFile: "agents/reviewer.md"},
			{ID: "docs", Name: "Docs", Description: "Read-only documentation worker", Skill: "documentation", Mode: "worker", Role: AgentRoleSubagent, Color: "cyan", AllowedTools: []string{"glob", "grep", "file-read", "comment", "skill-read", "skill-list"}, PermittedActions: []string{"read", "search", "ask"}, Permission: PermissionAskFirst, MaxSteps: 16, Enabled: true, Handoffs: []string{"explore"}, PromptFile: "agents/docs-writer.md"},
		},
	}
	_ = m.normalizeFromVersion()
	return m
}

func AgentManifestPath(cwd string) string {
	return filepath.Join(cwd, AgentManifestFilename)
}

func LoadAgentManifestForProject(cwd string) (AgentManifest, error) {
	p := AgentManifestPath(cwd)
	m, originalVersion, changed, err := loadAgentManifestWithMigrationInfo(p)
	if err == nil {
		if changed || originalVersion == 1 {
			if werr := backupAndWriteManifest(p, m); werr != nil {
				return AgentManifest{}, werr
			}
		}
		return m, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return DefaultAgentManifest(), nil
	}
	return AgentManifest{}, err
}

func LoadAgentManifest(path string) (AgentManifest, error) {
	m, _, _, err := loadAgentManifestWithMigrationInfo(path)
	if err != nil {
		return AgentManifest{}, err
	}
	return m, nil
}

func loadAgentManifestWithMigrationInfo(path string) (AgentManifest, int, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return AgentManifest{}, 0, false, err
	}
	defer f.Close()
	return DecodeAgentManifestWithMigrationInfo(f)
}

func DecodeAgentManifest(r io.Reader) (AgentManifest, error) {
	m, _, _, err := DecodeAgentManifestWithMigrationInfo(r)
	return m, err
}

func DecodeAgentManifestWithMigrationInfo(r io.Reader) (AgentManifest, int, bool, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return AgentManifest{}, 0, false, fmt.Errorf("read agent manifest: %w", err)
	}
	var versionOnly struct {
		Version int `toml:"version"`
	}
	if err := toml.Unmarshal(data, &versionOnly); err != nil {
		return AgentManifest{}, 0, false, fmt.Errorf("decode manifest version: %w", err)
	}
	if versionOnly.Version <= 0 {
		versionOnly.Version = 1
	}

	var manifest AgentManifest
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&manifest); err != nil {
		return AgentManifest{}, versionOnly.Version, false, fmt.Errorf("decode agent manifest: %w", err)
	}
	originalVersion := manifest.Version
	if originalVersion == 0 {
		originalVersion = versionOnly.Version
	}
	changed := manifest.normalizeFromVersion()
	if err := manifest.Validate(); err != nil {
		return AgentManifest{}, originalVersion, changed, err
	}
	return manifest, originalVersion, changed, nil
}

func backupAndWriteManifest(path string, m AgentManifest) error {
	if _, err := os.Stat(path); err == nil {
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return fmt.Errorf("read manifest before migration backup: %w", rerr)
		}
		backup := fmt.Sprintf("%s.migrated-%s.bak", path, time.Now().UTC().Format("20060102-150405"))
		if werr := os.WriteFile(backup, data, 0o644); werr != nil {
			return fmt.Errorf("write migration backup: %w", werr)
		}
	}
	raw, err := toml.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode migrated manifest: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("write migrated manifest: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace migrated manifest: %w", err)
	}
	return nil
}

func (m *AgentManifest) normalizeFromVersion() bool {
	changed := false
	if m.Version <= 0 {
		m.Version = 1
	}
	if m.Runtime.DefaultTimeoutSec <= 0 {
		m.Runtime.DefaultTimeoutSec = 120
		changed = true
	}
	if m.Runtime.SandboxMode == "" {
		m.Runtime.SandboxMode = SandboxWorkspaceWrite
		changed = true
	}
	if m.Runtime.Delegation.MaxParallelWorkers <= 0 {
		m.Runtime.Delegation.MaxParallelWorkers = 4
		changed = true
	}
	if m.Runtime.Delegation.MaxDepth <= 0 {
		m.Runtime.Delegation.MaxDepth = 2
		changed = true
	}
	if m.Runtime.Delegation.MaxToolCallsPerStep <= 0 {
		m.Runtime.Delegation.MaxToolCallsPerStep = 32
		changed = true
	}
	for i := range m.Tools {
		if m.Tools[i].RiskLevel == "" {
			m.Tools[i].RiskLevel = "medium"
			changed = true
		}
		if len(m.Tools[i].Aliases) == 0 {
			switch m.Tools[i].ID {
			case "bash":
				m.Tools[i].Aliases = []string{"bash-output"}
				changed = true
			}
		}
	}
	for i := range m.Agents {
		if m.Agents[i].Role == "" {
			switch strings.ToLower(strings.TrimSpace(m.Agents[i].Mode)) {
			case "orchestrator":
				m.Agents[i].Role = AgentRoleOrchestrator
			case "worker":
				m.Agents[i].Role = AgentRoleWorker
			default:
				m.Agents[i].Role = AgentRoleWorker
			}
			changed = true
		}
	}
	if m.Version < 2 {
		m.Version = 2
		changed = true
	}
	return changed
}

func (m AgentManifest) Validate() error {
	_ = m.normalizeFromVersion()
	if m.Version <= 0 {
		return fmt.Errorf("agent manifest: version must be > 0")
	}
	if len(m.Tools) == 0 {
		return fmt.Errorf("agent manifest: at least one tool is required")
	}
	if len(m.Agents) == 0 {
		return fmt.Errorf("agent manifest: at least one agent is required")
	}
	if strings.TrimSpace(m.DefaultAgent) == "" {
		return fmt.Errorf("agent manifest: default_agent is required")
	}
	if m.Runtime.DefaultTimeoutSec <= 0 {
		return fmt.Errorf("agent manifest: runtime.default_timeout_sec must be > 0")
	}
	if err := validatePermissionLevel(m.Runtime.DefaultPermission); err != nil {
		return fmt.Errorf("agent manifest: invalid runtime.default_permission: %w", err)
	}
	if err := validateSandboxMode(m.Runtime.SandboxMode); err != nil {
		return fmt.Errorf("agent manifest: invalid runtime.sandbox_mode: %w", err)
	}
	if m.Runtime.Delegation.MaxParallelWorkers <= 0 {
		return fmt.Errorf("agent manifest: runtime.delegation.max_parallel_workers must be > 0")
	}
	if m.Runtime.Delegation.MaxDepth <= 0 {
		return fmt.Errorf("agent manifest: runtime.delegation.max_depth must be > 0")
	}
	if m.Runtime.Delegation.MaxToolCallsPerStep <= 0 {
		return fmt.Errorf("agent manifest: runtime.delegation.max_tool_calls_per_step must be > 0")
	}
	if err := validatePermissionRules(m.Runtime.PermissionRules, "runtime.permission_rules"); err != nil {
		return err
	}

	toolIDs := map[string]struct{}{}
	for _, tool := range m.Tools {
		id := strings.TrimSpace(tool.ID)
		if id == "" {
			return fmt.Errorf("agent manifest: tool id is required")
		}
		if _, exists := toolIDs[id]; exists {
			return fmt.Errorf("agent manifest: duplicate tool id %q", id)
		}
		toolIDs[id] = struct{}{}
		if strings.TrimSpace(tool.Name) == "" {
			return fmt.Errorf("agent manifest: tool %q name is required", id)
		}
		switch tool.Kind {
		case "builtin", "mcp", "script", "http":
		default:
			return fmt.Errorf("agent manifest: tool %q has unsupported kind %q", id, tool.Kind)
		}
		if tool.TimeoutSec <= 0 {
			return fmt.Errorf("agent manifest: tool %q timeout_sec must be > 0", id)
		}
		if len(tool.PermittedActions) == 0 {
			return fmt.Errorf("agent manifest: tool %q must declare permitted_actions", id)
		}
		if (tool.Kind == "script" || tool.Kind == "http" || tool.Kind == "mcp") && strings.TrimSpace(tool.EntryPoint) == "" {
			return fmt.Errorf("agent manifest: tool %q requires entry_point for kind %q", id, tool.Kind)
		}
		for _, alias := range tool.Aliases {
			if strings.TrimSpace(alias) == "" {
				return fmt.Errorf("agent manifest: tool %q aliases cannot be blank", id)
			}
		}
		if err := validateRiskLevel(tool.RiskLevel); err != nil {
			return fmt.Errorf("agent manifest: invalid risk_level for tool %q: %w", id, err)
		}
		if err := validatePermissionRules(tool.PermissionRules, fmt.Sprintf("tool %q permission_rules", id)); err != nil {
			return err
		}
	}

	agentIDs := map[string]struct{}{}
	for _, ag := range m.Agents {
		id := strings.TrimSpace(ag.ID)
		if id == "" {
			return fmt.Errorf("agent manifest: agent id is required")
		}
		if _, exists := agentIDs[id]; exists {
			return fmt.Errorf("agent manifest: duplicate agent id %q", id)
		}
		agentIDs[id] = struct{}{}
		if strings.TrimSpace(ag.Name) == "" {
			return fmt.Errorf("agent manifest: agent %q name is required", id)
		}
		if strings.TrimSpace(ag.Mode) == "" {
			return fmt.Errorf("agent manifest: agent %q mode is required", id)
		}
		if ag.MaxSteps <= 0 {
			return fmt.Errorf("agent manifest: agent %q max_steps must be > 0", id)
		}
		if len(ag.AllowedTools) == 0 {
			return fmt.Errorf("agent manifest: agent %q must declare allowed_tools", id)
		}
		if err := validatePermissionLevel(ag.Permission); err != nil {
			return fmt.Errorf("agent manifest: invalid permission for agent %q: %w", id, err)
		}
		if err := validateAgentRole(ag.Role); err != nil {
			return fmt.Errorf("agent manifest: invalid role for agent %q: %w", id, err)
		}
		if strings.TrimSpace(ag.Color) != "" {
			if err := validateAgentColor(ag.Color); err != nil {
				return fmt.Errorf("agent manifest: invalid color for agent %q: %w", id, err)
			}
		}
		for _, toolID := range ag.AllowedTools {
			if _, exists := toolIDs[toolID]; !exists {
				return fmt.Errorf("agent manifest: agent %q references unknown tool %q", id, toolID)
			}
		}
		if err := validatePermissionRules(ag.PermissionRules, fmt.Sprintf("agent %q permission_rules", id)); err != nil {
			return err
		}
	}
	if _, exists := agentIDs[m.DefaultAgent]; !exists {
		return fmt.Errorf("agent manifest: default_agent %q not found in agents", m.DefaultAgent)
	}
	for _, ag := range m.Agents {
		for _, handoff := range ag.Handoffs {
			if _, exists := agentIDs[handoff]; !exists {
				return fmt.Errorf("agent manifest: agent %q handoff references unknown agent %q", ag.ID, handoff)
			}
		}
	}
	return nil
}

func validatePermissionLevel(level PermissionLevel) error {
	switch level {
	case PermissionYOLO, PermissionRestricted, PermissionAskFirst:
		return nil
	default:
		return fmt.Errorf("unsupported permission level %q", level)
	}
}

func validateSandboxMode(mode SandboxMode) error {
	switch mode {
	case SandboxWorkspaceWrite, SandboxReadOnly, SandboxFullAccess:
		return nil
	default:
		return fmt.Errorf("unsupported sandbox mode %q", mode)
	}
}

func validateRiskLevel(level string) error {
	switch strings.TrimSpace(level) {
	case "", "low", "medium", "high":
		return nil
	default:
		return fmt.Errorf("unsupported risk level %q; must be low, medium, or high", level)
	}
}

func validateAgentRole(role AgentRole) error {
	switch role {
	case AgentRolePrimary, AgentRoleSubagent, AgentRoleOrchestrator, AgentRoleWorker:
		return nil
	default:
		return fmt.Errorf("unsupported role %q; must be primary, subagent, orchestrator, or worker", role)
	}
}

func validatePermissionRules(rules []PermissionRule, field string) error {
	for i, r := range rules {
		if strings.TrimSpace(r.Permission) == "" {
			return fmt.Errorf("agent manifest: %s[%d].permission is required", field, i)
		}
		if strings.TrimSpace(r.Pattern) == "" {
			return fmt.Errorf("agent manifest: %s[%d].pattern is required", field, i)
		}
		switch r.Action {
		case RuleAllow, RuleAsk, RuleDeny:
		default:
			return fmt.Errorf("agent manifest: %s[%d].action must be allow, ask, or deny", field, i)
		}
	}
	return nil
}

var validAgentColors = map[string]struct{}{
	"blue":    {},
	"green":   {},
	"cyan":    {},
	"yellow":  {},
	"magenta": {},
	"red":     {},
	"white":   {},
}

func validateAgentColor(color string) error {
	if _, ok := validAgentColors[color]; !ok {
		return fmt.Errorf("unsupported color %q; must be one of: blue, green, cyan, yellow, magenta, red, white", color)
	}
	return nil
}

func (m AgentManifest) AgentByID(id string) (AgentSpec, bool) {
	for _, a := range m.Agents {
		if a.ID == id {
			return a, true
		}
	}
	return AgentSpec{}, false
}

func (m AgentManifest) EnabledAgents() []AgentSpec {
	out := make([]AgentSpec, 0, len(m.Agents))
	for _, a := range m.Agents {
		if a.Enabled {
			out = append(out, a)
		}
	}
	return out
}

func (m AgentManifest) EnabledToolsForAgent(agentID string) []ToolSpec {
	agent, ok := m.AgentByID(agentID)
	if !ok {
		return nil
	}
	allowed := map[string]struct{}{}
	for _, id := range agent.AllowedTools {
		allowed[id] = struct{}{}
	}
	out := make([]ToolSpec, 0)
	for _, t := range m.Tools {
		if !t.Enabled {
			continue
		}
		if _, ok := allowed[t.ID]; ok {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b ToolSpec) int {
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func (m AgentManifest) ToolByID(id string) (ToolSpec, bool) {
	for _, t := range m.Tools {
		if t.ID == id {
			return t, true
		}
		for _, alias := range t.Aliases {
			if alias == id {
				return t, true
			}
		}
	}
	return ToolSpec{}, false
}

func (a AgentSpec) IsPrimaryRole() bool {
	return a.Role == AgentRolePrimary || a.Role == AgentRoleOrchestrator
}
