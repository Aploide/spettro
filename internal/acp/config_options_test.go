package acp

import (
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/config"
	"spettro/internal/provider"
)

func configTestSession(t *testing.T) *acpSession {
	t.Helper()
	// applyConfigOption persists via config.Update; sandbox HOME so tests never
	// touch the developer's real config.
	t.Setenv("HOME", t.TempDir())
	return &acpSession{
		agentID: "plan",
		manifest: config.AgentManifest{
			Agents: []config.AgentSpec{
				{ID: "plan", Name: "Plan", Mode: "orchestrator", Enabled: true},
				{ID: "coding", Name: "Coding", Mode: "orchestrator", Enabled: true},
				{ID: "ask", Name: "Ask", Mode: "orchestrator", Enabled: true},
				{ID: "explore", Name: "Explore", Mode: "worker", Enabled: true},
				{ID: "git", Name: "Git", Mode: "worker", Enabled: true},
			},
		},
	}
}

func TestBuildConfigOptions_ModeSelectorHidesWorkers(t *testing.T) {
	s := configTestSession(t)
	cfg := config.UserConfig{ActiveProvider: "openai", ActiveModel: "gpt-4o", Permission: config.PermissionRestricted}
	opts := buildConfigOptions(s, &cfg, provider.NewManager())

	var found bool
	for _, o := range opts {
		if o.Select == nil || o.Select.Id != configIDMode {
			continue
		}
		found = true
		if o.Select.CurrentValue != "plan" {
			t.Fatalf("expected mode currentValue plan, got %q", o.Select.CurrentValue)
		}
		if o.Select.Options.Ungrouped == nil {
			t.Fatalf("expected ungrouped mode options")
		}
		var names []string
		for _, opt := range *o.Select.Options.Ungrouped {
			names = append(names, string(opt.Value))
		}
		if len(names) != 3 {
			t.Fatalf("expected only the 3 orchestrator modes, got %v", names)
		}
		for _, n := range names {
			if n == "explore" || n == "git" {
				t.Fatalf("worker %q leaked into the mode selector: %v", n, names)
			}
		}
	}
	if !found {
		t.Fatal("no mode config option was produced")
	}
}

func TestBuildConfigOptions_PermissionCurrentValue(t *testing.T) {
	s := configTestSession(t)
	cfg := config.UserConfig{Permission: config.PermissionYOLO}
	opts := buildConfigOptions(s, &cfg, provider.NewManager())
	for _, o := range opts {
		if o.Select != nil && o.Select.Id == configIDPermission {
			if o.Select.CurrentValue != "yolo" {
				t.Fatalf("expected permission currentValue yolo, got %q", o.Select.CurrentValue)
			}
			return
		}
	}
	t.Fatal("no permission config option was produced")
}

// TestBuildConfigOptions_ThinkingAlwaysPresent pins that the thinking
// selector is offered even when the active model is unknown or not
// reasoning-capable: the client toolbar must never lose the control, and it
// shows "off" as the current value when no level is set.
func TestBuildConfigOptions_ThinkingAlwaysPresent(t *testing.T) {
	s := configTestSession(t)
	cfg := config.UserConfig{ActiveProvider: "openai", ActiveModel: "gpt-4o"}
	opts := buildConfigOptions(s, &cfg, provider.NewManager())
	for _, o := range opts {
		if o.Select != nil && o.Select.Id == configIDThinking {
			if o.Select.CurrentValue != "off" {
				t.Fatalf("expected thinking currentValue off, got %q", o.Select.CurrentValue)
			}
			return
		}
	}
	t.Fatal("no thinking config option was produced")
}

func TestApplyConfigOption_Mode(t *testing.T) {
	s := configTestSession(t)
	cfg := config.UserConfig{}
	b := &bridge{opts: Options{Providers: provider.NewManager()}}

	if err := b.applyConfigOption(s, &cfg, configIDMode, "coding"); err != nil {
		t.Fatalf("apply mode=coding: %v", err)
	}
	if s.agentID != "coding" {
		t.Fatalf("expected agentID coding, got %q", s.agentID)
	}
	if err := b.applyConfigOption(s, &cfg, configIDMode, "nonsense"); err == nil {
		t.Fatal("expected error for unknown mode")
	}
	if s.agentID != "coding" {
		t.Fatalf("agentID should be unchanged after invalid mode, got %q", s.agentID)
	}
}

func TestApplyConfigOption_Permission(t *testing.T) {
	s := configTestSession(t)
	cfg := config.UserConfig{}
	b := &bridge{opts: Options{Providers: provider.NewManager()}}

	if err := b.applyConfigOption(s, &cfg, configIDPermission, "yolo"); err != nil {
		t.Fatalf("apply permission=yolo: %v", err)
	}
	if cfg.Permission != config.PermissionYOLO {
		t.Fatalf("expected cfg.Permission yolo, got %q", cfg.Permission)
	}
	if err := b.applyConfigOption(s, &cfg, configIDPermission, "bogus"); err == nil {
		t.Fatal("expected error for invalid permission")
	}
}

func TestApplyConfigOption_Thinking(t *testing.T) {
	s := configTestSession(t)
	cfg := config.UserConfig{ThinkingLevel: "high"}
	b := &bridge{opts: Options{Providers: provider.NewManager()}}

	if err := b.applyConfigOption(s, &cfg, configIDThinking, "off"); err != nil {
		t.Fatalf("apply thinking=off: %v", err)
	}
	if cfg.ThinkingLevel != "off" {
		t.Fatalf("expected thinking off to be stored explicitly, got %q", cfg.ThinkingLevel)
	}
	if err := b.applyConfigOption(s, &cfg, configIDThinking, "bogus"); err == nil {
		t.Fatal("expected error for invalid thinking level")
	}
}

func TestApplyConfigOption_ModelInvalid(t *testing.T) {
	s := configTestSession(t)
	cfg := config.UserConfig{}
	b := &bridge{opts: Options{Providers: provider.NewManager()}}

	if err := b.applyConfigOption(s, &cfg, configIDModel, "nocolon"); err == nil {
		t.Fatal("expected error for malformed model value")
	}
	if err := b.applyConfigOption(s, &cfg, configIDModel, "bogus:model"); err == nil {
		t.Fatal("expected error for unknown model")
	}
}

func TestApplyConfigOption_UnknownID(t *testing.T) {
	s := configTestSession(t)
	cfg := config.UserConfig{}
	b := &bridge{opts: Options{Providers: provider.NewManager()}}
	if err := b.applyConfigOption(s, &cfg, "nope", "x"); err == nil {
		t.Fatal("expected error for unknown config option id")
	}
}

// TestBuildConfigOptions_UltraIsBoolean pins that Ultra renders as an on/off
// toggle (boolean variant), not a dropdown: clients draw a select as a menu,
// which was the reported rendering bug.
func TestBuildConfigOptions_UltraIsBoolean(t *testing.T) {
	s := configTestSession(t)
	cfg := config.UserConfig{Ultra: true}
	opts := buildConfigOptions(s, &cfg, provider.NewManager())
	for _, o := range opts {
		if o.Select != nil && o.Select.Id == configIDUltra {
			t.Fatal("ultra must not be a select option; clients render it as a dropdown")
		}
		if o.Boolean != nil && o.Boolean.Id == configIDUltra {
			if !o.Boolean.CurrentValue {
				t.Fatal("expected ultra currentValue true")
			}
			return
		}
	}
	t.Fatal("no ultra boolean config option was produced")
}

func TestApplyConfigOption_Ultra(t *testing.T) {
	s := configTestSession(t)
	cfg := config.UserConfig{Permission: config.PermissionRestricted}
	b := &bridge{opts: Options{Providers: provider.NewManager()}}

	// Boolean wire values.
	if err := b.applyConfigOption(s, &cfg, configIDUltra, "true"); err != nil {
		t.Fatalf("apply ultra=true: %v", err)
	}
	if !cfg.Ultra {
		t.Fatal("expected cfg.Ultra true")
	}
	if err := b.applyConfigOption(s, &cfg, configIDUltra, "false"); err != nil {
		t.Fatalf("apply ultra=false: %v", err)
	}
	if cfg.Ultra {
		t.Fatal("expected cfg.Ultra false")
	}

	// Legacy select values still work.
	if err := b.applyConfigOption(s, &cfg, configIDUltra, "on"); err != nil {
		t.Fatalf("apply ultra=on: %v", err)
	}
	if !cfg.Ultra {
		t.Fatal("expected cfg.Ultra true after on")
	}

	if err := b.applyConfigOption(s, &cfg, configIDUltra, "bogus"); err == nil {
		t.Fatal("expected error for invalid ultra value")
	}

	// Ask-first permission must reject enabling (approval prompts would flood).
	cfg.Permission = config.PermissionAskFirst
	if err := b.applyConfigOption(s, &cfg, configIDUltra, "true"); err == nil {
		t.Fatal("expected error enabling ultra under ask-first permission")
	}
}

func workflowSizeOption(t *testing.T, opts []acpsdk.SessionConfigOption) *acpsdk.SessionConfigOptionSelect {
	t.Helper()
	for _, o := range opts {
		if o.Select != nil && o.Select.Id == configIDWorkflowSize {
			return o.Select
		}
	}
	t.Fatal("no workflow_size select option was produced")
	return nil
}

// The workflow size selector is always present, lists the tiers smallest
// first, and shows medium when no tier was ever chosen.
func TestBuildConfigOptions_WorkflowSize(t *testing.T) {
	s := configTestSession(t)
	cases := []struct {
		configured, current string
	}{
		{"", "medium"},
		{"small", "small"},
		{"unbounded", "unbounded"},
	}
	for _, tc := range cases {
		cfg := config.UserConfig{WorkflowSize: tc.configured}
		sel := workflowSizeOption(t, buildConfigOptions(s, &cfg, provider.NewManager()))
		if string(sel.CurrentValue) != tc.current {
			t.Errorf("configured %q: current = %q, want %q", tc.configured, sel.CurrentValue, tc.current)
		}
		if sel.Category != nil {
			t.Errorf("workflow_size must be ungrouped, got category %v", *sel.Category)
		}
		opts := *sel.Options.Ungrouped
		var values []string
		for _, o := range opts {
			values = append(values, string(o.Value))
			if o.Description == nil || *o.Description == "" {
				t.Errorf("tier %s has no description", o.Value)
			}
		}
		if strings.Join(values, ",") != strings.Join(config.WorkflowSizes, ",") {
			t.Errorf("tiers = %v", values)
		}
	}
}

func TestApplyConfigOption_WorkflowSize(t *testing.T) {
	s := configTestSession(t)
	cfg := config.UserConfig{}
	b := &bridge{opts: Options{Providers: provider.NewManager()}}

	before := sharedSettings(&cfg)
	for _, tc := range []struct{ value, want string }{
		{"large", "large"},
		{" SMALL ", "small"},
		{"medium", "medium"},
	} {
		if err := b.applyConfigOption(s, &cfg, configIDWorkflowSize, tc.value); err != nil {
			t.Fatalf("apply %q: %v", tc.value, err)
		}
		if cfg.WorkflowSize != tc.want {
			t.Fatalf("apply %q: cfg = %q", tc.value, cfg.WorkflowSize)
		}
		saved, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		if saved.WorkflowSize != tc.want {
			t.Fatalf("apply %q: persisted = %q", tc.value, saved.WorkflowSize)
		}
	}
	for _, bad := range []string{"huge", "", "medium-ish"} {
		if err := b.applyConfigOption(s, &cfg, configIDWorkflowSize, bad); err == nil {
			t.Fatalf("expected an error for workflow size %q", bad)
		}
	}
	if cfg.WorkflowSize != "medium" {
		t.Fatalf("a rejected value changed the tier: %q", cfg.WorkflowSize)
	}

	// The tier lives in the user config, so a change from one session must
	// reach the others' selectors (syncOtherSessions keys off this).
	cfg.WorkflowSize = "large"
	if sharedSettings(&cfg) == before {
		t.Fatal("sharedSettings must change with the workflow size")
	}
	// "" and "medium" are the same tier: switching between them is no change.
	a, c := config.UserConfig{}, config.UserConfig{WorkflowSize: "medium"}
	if sharedSettings(&a) != sharedSettings(&c) {
		t.Fatal("an unset tier and medium must fingerprint alike")
	}
}
