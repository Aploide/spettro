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

// setPermissionForTest sets the user permission the way /permission does:
// through the saved config, which /ultra reloads when it saves its own
// setting (an in-memory edit would be lost there).
func setPermissionForTest(t *testing.T, m *Model, level config.PermissionLevel) {
	t.Helper()
	if err := m.updateConfig(func(cfg *config.UserConfig) error {
		cfg.Permission = level
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// headerTagForTest is the ultra tag the header shows: "", "ultra" or
// "ultra:suspended".
func headerTagForTest(m Model) string {
	header := stripANSIForTest(m.viewHeader())
	switch {
	case strings.Contains(header, "ultra:suspended"):
		return "ultra:suspended"
	case strings.Contains(header, "ultra"):
		return "ultra"
	}
	return ""
}

// /ultra is the one switch for the standing ultracode mode: it flips or sets
// cfg.Ultra, saves it, and the header carries a single "ultra" tag while it
// is on.
func TestUltraCommandToggles(t *testing.T) {
	// /ultra persists through config.Update: keep it off the real
	// ~/.spettro/config.json.
	t.Setenv("HOME", t.TempDir())
	m := NewModelForTesting()
	m.width, m.height = 160, 40
	setPermissionForTest(t, &m, config.PermissionRestricted)
	if tag := headerTagForTest(m); tag != "" {
		t.Fatalf("ultra starts off, header tag %q", tag)
	}
	cases := []struct {
		input string
		want  bool
	}{
		{"/ultra", true},
		{"/ultra", false},
		{"/ultra on", true},
		{"/ultra ON", true},
		{"/ultra off", false},
		{"/ultra on", true},
	}
	for _, c := range cases {
		nm, _ := m.handleCommand(c.input)
		m = nm.(Model)
		if m.cfg.Ultra != c.want || m.ultraActive() != c.want {
			t.Fatalf("%s: Ultra = %v, active = %v, want %v", c.input, m.cfg.Ultra, m.ultraActive(), c.want)
		}
		if saved, err := config.Load(); err != nil || saved.Ultra != c.want {
			t.Fatalf("%s: not saved: %v (%v)", c.input, saved.Ultra, err)
		}
		wantTag := ""
		if c.want {
			wantTag = "ultra"
		}
		if tag := headerTagForTest(m); tag != wantTag {
			t.Fatalf("%s: header tag %q, want %q", c.input, tag, wantTag)
		}
		if m.bannerKind != "success" {
			t.Fatalf("%s: banner %q (%s)", c.input, m.banner, m.bannerKind)
		}
		for _, banned := range []string{"swarm", "Swarm"} {
			if strings.Contains(m.banner, banned) {
				t.Fatalf("%s: banner still talks about the swarm: %q", c.input, m.banner)
			}
		}
	}
	if !strings.Contains(m.banner, "dynamic workflows") {
		t.Fatalf("the on banner should say what ultra does: %q", m.banner)
	}
	nm, _ := m.handleCommand("/ultra maybe")
	m = nm.(Model)
	if !m.cfg.Ultra || m.bannerKind != "error" {
		t.Fatalf("a bad argument must be refused and change nothing: Ultra=%v banner=%q", m.cfg.Ultra, m.banner)
	}
}

// There is no /ultracode command, not even as an alias: /ultra is the switch
// and the keyword covers a single turn.
func TestUltracodeIsNotACommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := NewModelForTesting()
	nm, _ := m.handleCommand("/ultracode on")
	m = nm.(Model)
	if m.cfg.Ultra || !strings.Contains(m.banner, "unknown command") {
		t.Fatalf("/ultracode must be unknown: Ultra=%v banner=%q", m.cfg.Ultra, m.banner)
	}
	for _, c := range allCommands {
		if strings.HasPrefix(c.name, "/ultracode") {
			t.Fatalf("%s is still in the command catalog", c.name)
		}
	}
	if strings.Contains(helpText, "/ultracode") || strings.Contains(workflowsHelp, "/ultracode") {
		t.Fatal("/help or /workflows help still mentions /ultracode")
	}
	if isInstantCommand("/ultracode") {
		t.Fatal("/ultracode is no instant command any more")
	}
}

// The workflow tool refuses to run under ask-first, so /ultra there is
// saved but suspended rather than refused — the user can switch permission
// afterwards — and it injects no guidance whose every workflow call fails.
// The effective permission decides: under the default ask-first the
// "coding" agent still runs restricted by its spec, so ultra engages there.
func TestUltraSuspendedUnderAskFirst(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := NewModelForTesting()
	m.width, m.height = 160, 40
	m.manifest = config.DefaultAgentManifest()
	m.mode = "plan"
	setPermissionForTest(t, &m, config.PermissionAskFirst)

	nm, _ := m.handleCommand("/ultra on")
	m = nm.(Model)
	if !m.cfg.Ultra {
		t.Fatal("the toggle is kept under ask-first, only suspended")
	}
	if saved, err := config.Load(); err != nil || !saved.Ultra {
		t.Fatalf("a suspended ultra is still saved: %v (%v)", saved.Ultra, err)
	}
	if m.bannerKind != "warn" || !strings.Contains(m.banner, "restricted or yolo") || !strings.Contains(m.banner, "suspended") {
		t.Fatalf("banner = %q (%s)", m.banner, m.bannerKind)
	}
	if tag := headerTagForTest(m); tag != "ultra:suspended" {
		t.Fatalf("the tag should say ultra is suspended, got %q", tag)
	}
	plan, _ := m.manifest.AgentByID("plan")
	coding, _ := m.manifest.AgentByID("coding")
	if m.ultraActive() || m.ultraActiveFor(plan) {
		t.Fatal("an ask-first plan run must not get Ultracode")
	}
	if !m.ultraActiveFor(coding) {
		t.Fatal("the coding agent runs restricted by its own spec: ultra engages there")
	}
	m.SetTextareaValueForTesting("audit this +500k")
	if m.budgetDirectivesLive() || inputMayGlow(m.ta.Value(), m.ultraActive()) {
		t.Fatal("a suspended ultra does not make budget directives live")
	}

	// The tag, the glow and the run all follow the agent the next message
	// goes to.
	m.mode = "coding"
	if tag := headerTagForTest(m); tag != "ultra" {
		t.Fatalf("on the coding agent the tag is plain, got %q", tag)
	}
	if !m.budgetDirectivesLive() || !inputMayGlow(m.ta.Value(), m.ultraActive()) {
		t.Fatal("with ultra live, a bare budget directive is live and may glow")
	}

	m.mode = "plan"
	setPermissionForTest(t, &m, config.PermissionYOLO)
	if !m.ultraActive() || !m.ultraActiveFor(plan) {
		t.Fatal("a user level other than ask-first overrides the agent's own")
	}
	if tag := headerTagForTest(m); tag != "ultra" {
		t.Fatalf("under yolo the tag is plain, got %q", tag)
	}
	nm, _ = m.handleCommand("/ultra on")
	if got := nm.(Model); got.bannerKind != "success" {
		t.Fatalf("under yolo the toggle engages: %q (%s)", got.banner, got.bannerKind)
	}
	nm, _ = m.handleCommand("/ultra off")
	m = nm.(Model)
	if m.ultraActiveFor(coding) || headerTagForTest(m) != "" {
		t.Fatal("ultra off is off everywhere")
	}
	if got := effectiveRunPermission(config.PermissionAskFirst, config.AgentSpec{}); got != config.PermissionAskFirst {
		t.Fatalf("an agent naming no permission reads as ask-first, got %q", got)
	}
}

// The /ultra picker offers both directions, so nobody has to remember that a
// bare /ultra flips.
func TestUltraSubMenu(t *testing.T) {
	m := NewModelForTesting()
	items, ok := m.slashSubMenu("/ultra ")
	if !ok || len(items) != 2 || items[0].name != "/ultra on" || items[1].name != "/ultra off" {
		t.Fatalf("/ultra sub-menu = %v (ok=%v)", items, ok)
	}
	if items, _ := m.slashSubMenu("/ultra of"); len(items) != 1 || items[0].name != "/ultra off" {
		t.Fatalf("typing filters the choices: %v", items)
	}
	for _, c := range append(append([]commandDef{}, allCommands...), ultraCommands...) {
		if strings.HasPrefix(c.name, "/ultra") && strings.Contains(strings.ToLower(c.desc), "swarm") {
			t.Fatalf("%s still describes the swarm: %q", c.name, c.desc)
		}
	}
	if strings.Contains(strings.ToLower(helpText), "swarm") {
		t.Fatal("/help still describes the swarm")
	}
}

func TestWorkflowCommandsAreInstantAndListed(t *testing.T) {
	for input, want := range map[string]bool{
		"/ultra":                     true,
		"/ultra off":                 true,
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
	for _, want := range []string{"/ultra", "/workflows size", "/workflows run"} {
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
	for _, want := range []string{"/workflows size", "/ultra", "checkpoint()", "+500k", "templates", "[json | task]"} {
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
		`{"name": "audit", "show": true}`,
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
