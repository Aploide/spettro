package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/config"
	"spettro/internal/workflow"
)

func TestWorkflowsSizeCommand(t *testing.T) {
	// /workflows size persists through config.Update: keep it off the real
	// ~/.spettro/config.json.
	t.Setenv("HOME", t.TempDir())
	m := NewModelForTesting()

	nm, _ := m.handleCommand("/workflows size")
	got := nm.(Model)
	for _, want := range []string{"workflow size: medium (default)", "small", "large", "unbounded", "~30 agents per run", "not a hard limit"} {
		if !hasSystemMsg(got, want) {
			t.Errorf("bare /workflows size is missing %q", want)
		}
	}
	if got.cfg.WorkflowSize != "" {
		t.Fatalf("a bare /workflows size must not persist anything, got %q", got.cfg.WorkflowSize)
	}

	nm, _ = got.handleCommand("/workflows size LARGE")
	got = nm.(Model)
	if got.cfg.WorkflowSize != "large" || got.cfg.WorkflowSizeTier() != "large" {
		t.Fatalf("size not set: %q", got.cfg.WorkflowSize)
	}
	if saved, err := config.Load(); err != nil || saved.WorkflowSize != "large" {
		t.Fatalf("size not persisted: %q (%v)", saved.WorkflowSize, err)
	}
	if !strings.Contains(got.banner, "large") {
		t.Fatalf("banner = %q", got.banner)
	}

	nm, _ = got.handleCommand("/workflows size huge")
	got = nm.(Model)
	if got.cfg.WorkflowSize != "large" || got.bannerKind != "error" {
		t.Fatalf("a bad tier must be refused and change nothing: size=%q banner=%q", got.cfg.WorkflowSize, got.banner)
	}
	nm, _ = got.handleCommand("/workflows size")
	if got := nm.(Model); !hasSystemMsg(got, "workflow size: large\n") {
		t.Fatal("the listing should show the configured tier without (default)")
	}
}

func TestUltracodeCommandToggles(t *testing.T) {
	m := NewModelForTesting()
	m.width, m.height = 160, 40
	if strings.Contains(stripANSIForTest(m.viewHeader()), "ultracode") {
		t.Fatal("ultracode starts off")
	}
	cases := []struct {
		input string
		want  bool
	}{
		{"/ultracode", true},
		{"/ultracode", false},
		{"/ultracode on", true},
		{"/ultracode ON", true},
		{"/ultracode off", false},
	}
	for _, c := range cases {
		nm, _ := m.handleCommand(c.input)
		m = nm.(Model)
		if m.ultracode != c.want {
			t.Fatalf("%s: ultracode = %v, want %v", c.input, m.ultracode, c.want)
		}
		if tagged := strings.Contains(stripANSIForTest(m.viewHeader()), "ultracode"); tagged != c.want {
			t.Fatalf("%s: status tag shown = %v, want %v", c.input, tagged, c.want)
		}
	}
	nm, _ := m.handleCommand("/ultracode maybe")
	m = nm.(Model)
	if m.ultracode || m.bannerKind != "error" {
		t.Fatalf("a bad argument must be refused: ultracode=%v banner=%q", m.ultracode, m.banner)
	}
}

// The workflow tool refuses to run under ask-first, so /ultracode there is
// kept but suspended — like /ultra — rather than injecting guidance whose
// every workflow call fails. The effective permission decides: under the
// default ask-first the "coding" agent still runs restricted by its spec.
func TestUltracodeSuspendedUnderAskFirst(t *testing.T) {
	m := NewModelForTesting()
	m.width, m.height = 160, 40
	m.manifest = config.DefaultAgentManifest()
	m.mode = "plan"
	m.cfg.Permission = config.PermissionAskFirst

	nm, _ := m.handleCommand("/ultracode on")
	m = nm.(Model)
	if !m.ultracode {
		t.Fatal("the toggle is kept under ask-first, only suspended")
	}
	if m.bannerKind != "warn" || !strings.Contains(m.banner, "restricted or yolo") || !strings.Contains(m.banner, "suspended") {
		t.Fatalf("banner = %q (%s)", m.banner, m.bannerKind)
	}
	if header := stripANSIForTest(m.viewHeader()); !strings.Contains(header, "ultracode:suspended") {
		t.Fatalf("the tag should say ultracode is suspended: %q", header)
	}
	plan, _ := m.manifest.AgentByID("plan")
	coding, _ := m.manifest.AgentByID("coding")
	if m.ultracodeActive() || m.ultracodeActiveFor(plan) {
		t.Fatal("an ask-first plan run must not get Ultracode")
	}
	if !m.ultracodeActiveFor(coding) {
		t.Fatal("the coding agent runs restricted by its own spec: ultracode engages there")
	}
	if m.budgetDirectivesLive() {
		t.Fatal("a suspended toggle does not make budget directives live")
	}

	m.mode = "coding"
	if header := stripANSIForTest(m.viewHeader()); strings.Contains(header, "suspended") || !strings.Contains(header, "ultracode") {
		t.Fatalf("on the coding agent the tag is plain: %q", header)
	}
	m.mode = "plan"
	m.cfg.Permission = config.PermissionYOLO
	if !m.ultracodeActive() || !m.ultracodeActiveFor(plan) {
		t.Fatal("a user level other than ask-first overrides the agent's own")
	}
	nm, _ = m.handleCommand("/ultracode on")
	if got := nm.(Model); got.bannerKind != "success" {
		t.Fatalf("under yolo the toggle engages: %q (%s)", got.banner, got.bannerKind)
	}
	m.ultracode = false
	if m.ultracodeActiveFor(coding) {
		t.Fatal("ultracode off is off everywhere")
	}
	if got := effectiveRunPermission(config.PermissionAskFirst, config.AgentSpec{}); got != config.PermissionAskFirst {
		t.Fatalf("an agent naming no permission reads as ask-first, got %q", got)
	}
}

func TestWorkflowCommandsAreInstantAndListed(t *testing.T) {
	for input, want := range map[string]bool{
		"/ultracode":                 true,
		"/ultracode off":             true,
		"/workflows size":            true,
		"/workflows size large":      true,
		"/workflows run audit":       false,
		"/workflows run audit {}":    false,
		"/workflows show audit":      true,
		"/workflows":                 true,
		"/workflow size unbounded":   true,
		"/workflows run audit focus": false,
	} {
		if got := isInstantCommand(input); got != want {
			t.Errorf("isInstantCommand(%q) = %v, want %v", input, got, want)
		}
	}

	names := map[string]bool{}
	for _, c := range allCommands {
		names[c.name] = true
	}
	for _, want := range []string{"/ultracode", "/workflows size", "/workflows run"} {
		if !names[want] {
			t.Errorf("%s missing from the command catalog", want)
		}
		if !strings.Contains(helpText, want) {
			t.Errorf("%s missing from /help", want)
		}
	}

	m := NewModelForTesting()
	items, ok := m.slashSubMenu("/workflows size ")
	if !ok || len(items) != len(config.WorkflowSizes) {
		t.Fatalf("/workflows size sub-menu = %v (ok=%v)", items, ok)
	}
	for i, tier := range config.WorkflowSizes {
		if items[i].name != "/workflows size "+tier || items[i].desc == "" {
			t.Fatalf("sub-menu item %d = %+v", i, items[i])
		}
	}
	if items, _ := m.slashSubMenu("/workflows size larg"); len(items) != 1 || items[0].name != "/workflows size large" {
		t.Fatalf("typing filters the tiers: %v", items)
	}
}

func TestWorkflowsHelpDescribesDynamicWorkflows(t *testing.T) {
	for _, want := range []string{"/workflows size", "/ultracode", "checkpoint()", "+500k", "templates", "[json | task]"} {
		if !strings.Contains(workflowsHelp, want) {
			t.Errorf("workflowsHelp is missing %q", want)
		}
	}
	if strings.Contains(workflowsHelp, "exactly as written") {
		t.Error("workflowsHelp still says saved scripts run exactly as written")
	}
}

func TestSplitWorkflowRunInput(t *testing.T) {
	cases := []struct {
		rest, args, task string
		err              bool
	}{
		{"", "", "", false},
		{`{"base": "main"}`, `{"base": "main"}`, "", false},
		{`  {"focus":  "two  spaces"}  `, `{"focus":  "two  spaces"}`, "", false},
		{"audit the auth package", "", "audit the auth package", false},
		{"42", "42", "", false},
		{`{"base": `, "", "", true},
		{"[1, 2", "", "", true},
	}
	for _, c := range cases {
		args, task, err := splitWorkflowRunInput(c.rest)
		if (err != nil) != c.err || args != c.args || task != c.task {
			t.Errorf("split(%q) = (%q, %q, %v), want (%q, %q, err=%v)", c.rest, args, task, err, c.args, c.task, c.err)
		}
	}
}

func TestWorkflowRunPromptTreatsTheScriptAsATemplate(t *testing.T) {
	meta := workflow.Meta{
		Name:        "audit",
		Description: "audit a package",
		Params: []workflow.ParamMeta{
			{Name: "target", Type: "string", Description: "package to audit", Required: true},
			{Name: "depth", Type: "number", Default: float64(2)},
		},
	}
	path := "/repo/.spettro/workflows/audit.js"

	prompt := workflowRunPrompt("audit", path, meta, "", "look at the auth package")
	for _, want := range []string{
		"ultracode: run the saved workflow \"audit\"",
		"Task: look at the auth package",
		`script_path "/repo/.spettro/workflows/audit.js"`,
		"discovered at runtime",
		"run the adapted script inline",
		`{"name": "audit"}`,
		"Declared params: target (string, required) — package to audit; depth (number, default 2)",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "Do not rewrite") {
		t.Errorf("the template prompt must not forbid adapting the script:\n%s", prompt)
	}

	withArgs := workflowRunPrompt("audit", path, workflow.Meta{Name: "audit"}, `{"target":"auth"}`, "")
	for _, want := range []string{`{"name": "audit", "args": {"target":"auth"}}`, "Declared params: none.", "Use these args"} {
		if !strings.Contains(withArgs, want) {
			t.Errorf("prompt with args is missing %q:\n%s", want, withArgs)
		}
	}
	if strings.Contains(withArgs, "Task:") {
		t.Errorf("JSON args are not a task:\n%s", withArgs)
	}
}

func TestWorkflowsListAndShowParams(t *testing.T) {
	m := NewModelForTesting()
	m.cwd = t.TempDir()
	dir := filepath.Join(m.cwd, ".spettro", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `export const meta = {
  name: 'audit',
  description: 'audit a package',
  phases: [{title: 'Scan'}],
  params: {target: {type: 'string', description: 'package to audit', required: true}, focus: 'what to look at'},
}
log(args.target)
`
	if err := os.WriteFile(filepath.Join(dir, "audit.js"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	nm, _ := m.handleCommand("/workflows")
	got := nm.(Model)
	if !hasSystemMsg(got, "params: target (string, required) — package to audit; focus (string) — what to look at") {
		t.Fatalf("the listing should show the template's params:\n%v", got.messages)
	}
	nm, _ = got.handleCommand("/workflows show audit")
	got = nm.(Model)
	if !hasSystemMsg(got, "params:\n  · target (string, required) — package to audit") {
		t.Fatalf("show should list the params:\n%v", got.messages)
	}
}
