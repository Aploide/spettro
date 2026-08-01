package hooks

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type Event string

const (
	EventPreToolUse        Event = "PreToolUse"
	EventPostToolUse       Event = "PostToolUse"
	EventPermissionRequest Event = "PermissionRequest"
	EventSessionStart      Event = "SessionStart"
)

var supportedEvents = map[Event]struct{}{
	EventPreToolUse:        {},
	EventPostToolUse:       {},
	EventPermissionRequest: {},
	EventSessionStart:      {},
}

type Rule struct {
	ID         string `json:"id"`
	Event      Event  `json:"event"`
	Matcher    string `json:"matcher,omitempty"`
	Command    string `json:"command"`
	TimeoutSec int    `json:"timeout_sec,omitempty"`
	Enabled    *bool  `json:"enabled,omitempty"`
}

type EffectiveRule struct {
	Rule
	Source  string `json:"source"`
	Enabled bool   `json:"enabled"`
}

type ValidationIssue struct {
	Source  string `json:"source"`
	ID      string `json:"id"`
	Message string `json:"message"`
}

type EffectiveConfig struct {
	Rules  []EffectiveRule   `json:"rules"`
	Issues []ValidationIssue `json:"issues"`
}

type decisionEnvelope struct {
	Decision    string          `json:"decision,omitempty"`
	Message     string          `json:"message,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	UpdatedArgs json.RawMessage `json:"updated_args,omitempty"`
}

type RunInput struct {
	Event      Event           `json:"event"`
	ToolID     string          `json:"tool_id,omitempty"`
	ToolArgs   json.RawMessage `json:"tool_args,omitempty"`
	ToolOutput string          `json:"tool_output,omitempty"`
	Command    string          `json:"command,omitempty"`
}

type RunResult struct {
	Decision    string
	Message     string
	Reason      string
	UpdatedArgs json.RawMessage
	Stdout      string
	Stderr      string
}

func LoadEffective(cwd string) (EffectiveConfig, error) {
	globalPath, err := globalHooksPath()
	if err != nil {
		return EffectiveConfig{}, err
	}
	projectPath := filepath.Join(cwd, ".spettro", "hooks.json")

	globalRules, globalIssues, err := loadConfigFile(globalPath, "global")
	if err != nil {
		return EffectiveConfig{}, err
	}
	projectRules, projectIssues, err := loadConfigFile(projectPath, "project")
	if err != nil {
		return EffectiveConfig{}, err
	}

	merged := map[string]EffectiveRule{}
	ordered := make([]string, 0, len(globalRules)+len(projectRules))
	for _, r := range globalRules {
		k := mergeKey(r)
		if _, ok := merged[k]; !ok {
			ordered = append(ordered, k)
		}
		merged[k] = r
	}
	for _, r := range projectRules {
		k := mergeKey(r)
		if _, ok := merged[k]; !ok {
			ordered = append(ordered, k)
		}
		merged[k] = r
	}
	rules := make([]EffectiveRule, 0, len(ordered))
	for _, k := range ordered {
		rules = append(rules, merged[k])
	}
	sort.SliceStable(rules, func(i, j int) bool {
		if rules[i].Event != rules[j].Event {
			return rules[i].Event < rules[j].Event
		}
		return rules[i].ID < rules[j].ID
	})

	issues := append(globalIssues, projectIssues...)
	return EffectiveConfig{Rules: rules, Issues: issues}, nil
}

func Match(rule EffectiveRule, toolID string) bool {
	m := strings.TrimSpace(rule.Matcher)
	if m == "" || m == "*" {
		return true
	}
	if after, ok := strings.CutPrefix(m, "re:"); ok {
		re, err := regexp.Compile(after)
		if err != nil {
			return false
		}
		return re.MatchString(toolID)
	}
	ok, err := filepath.Match(m, toolID)
	if err != nil {
		return false
	}
	return ok
}

func Run(ctx context.Context, rule EffectiveRule, input RunInput) (RunResult, error) {
	timeout := rule.TimeoutSec
	if timeout <= 0 {
		timeout = 15
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	raw, _ := json.Marshal(input)
	cmd := exec.CommandContext(runCtx, "bash", "-lc", rule.Command)
	cmd.Stdin = bytes.NewReader(raw)
	cmd.Env = append(os.Environ(),
		"SPETTRO_HOOK_EVENT="+string(input.Event),
		"SPETTRO_HOOK_TOOL_ID="+input.ToolID,
		"SPETTRO_HOOK_COMMAND="+input.Command,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := RunResult{Stdout: strings.TrimSpace(stdout.String()), Stderr: strings.TrimSpace(stderr.String())}
	if err != nil {
		return result, fmt.Errorf("hook %s failed: %w", rule.ID, err)
	}
	if result.Stdout == "" {
		return result, nil
	}
	last := lastNonEmptyLine(result.Stdout)
	if last == "" {
		return result, nil
	}
	var env decisionEnvelope
	if uerr := json.Unmarshal([]byte(last), &env); uerr != nil {
		return result, nil
	}
	result.Decision = strings.ToLower(strings.TrimSpace(env.Decision))
	result.Message = strings.TrimSpace(env.Message)
	result.Reason = strings.TrimSpace(env.Reason)
	if len(env.UpdatedArgs) > 0 {
		result.UpdatedArgs = env.UpdatedArgs
	}
	return result, nil
}

func loadConfigFile(path, source string) ([]EffectiveRule, []ValidationIssue, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("read hooks config %s: %w", path, err)
	}
	type wrapper struct {
		Hooks []Rule `json:"hooks"`
	}
	var parsed wrapper
	if err := json.Unmarshal(data, &parsed); err != nil {
		var arr []Rule
		if err2 := json.Unmarshal(data, &arr); err2 != nil {
			return nil, nil, fmt.Errorf("decode hooks config %s: %w", path, err)
		}
		parsed.Hooks = arr
	}

	rules := make([]EffectiveRule, 0, len(parsed.Hooks))
	issues := make([]ValidationIssue, 0)
	for i, r := range parsed.Hooks {
		id := strings.TrimSpace(r.ID)
		if id == "" {
			id = fmt.Sprintf("%s-%d", strings.ToLower(source), i+1)
		}
		enabled := true
		if r.Enabled != nil {
			enabled = *r.Enabled
		}
		er := EffectiveRule{Rule: r, Source: source, Enabled: enabled}
		er.ID = id
		if _, ok := supportedEvents[er.Event]; !ok {
			issues = append(issues, ValidationIssue{Source: source, ID: er.ID, Message: fmt.Sprintf("unsupported event %q", er.Event)})
			continue
		}
		if strings.TrimSpace(er.Command) == "" {
			issues = append(issues, ValidationIssue{Source: source, ID: er.ID, Message: "command is required"})
			continue
		}
		rules = append(rules, er)
	}
	return rules, issues, nil
}

func mergeKey(r EffectiveRule) string {
	id := strings.TrimSpace(r.ID)
	if id == "" {
		id = strings.TrimSpace(r.Command)
	}
	return string(r.Event) + "|" + strings.TrimSpace(r.Matcher) + "|" + id
}

func globalHooksPath() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	return filepath.Join(h, ".spettro", "hooks.json"), nil
}

func lastNonEmptyLine(text string) string {
	s := bufio.NewScanner(strings.NewReader(text))
	last := ""
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line != "" {
			last = line
		}
	}
	return last
}
