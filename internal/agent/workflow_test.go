package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/workflow"
)

func TestWorkflowRequested_WholeWordOnly(t *testing.T) {
	on := []string{
		"ultracode",
		"please ULTRACODE this refactor",
		"do it, ultracode.",
		"(ultracode)",
		"refactor the parser — ultracode",
	}
	for _, s := range on {
		if !WorkflowRequested(s) {
			t.Fatalf("%q should activate workflows", s)
		}
	}
	off := []string{
		"",
		"just do the thing",
		"open src/ultracoder.go",
		"rename ultracodes to something else",
		"the ultracode_helper function",
	}
	for _, s := range off {
		if WorkflowRequested(s) {
			t.Fatalf("%q should not activate workflows", s)
		}
	}
}

func TestResolveWorkflowTarget(t *testing.T) {
	manifest := config.DefaultAgentManifest()
	spec, err := resolveWorkflowTarget(&manifest, "")
	if err != nil {
		t.Fatalf("default target: %v", err)
	}
	if spec.ID != "general-purpose" {
		t.Fatalf("default target = %q, want general-purpose", spec.ID)
	}
	if spec, err = resolveWorkflowTarget(&manifest, "review"); err != nil || spec.ID != "review" {
		t.Fatalf("explicit target = %q, %v", spec.ID, err)
	}
	// An orchestrator would let a script nest a swarm inside a phase.
	if _, err := resolveWorkflowTarget(&manifest, "coding"); err == nil {
		t.Fatal("want an error for an orchestrator target")
	}
	if _, err := resolveWorkflowTarget(&manifest, "nope"); err == nil ||
		!strings.HasPrefix(err.Error(), "workflow:") {
		t.Fatalf("want a workflow-prefixed error, got %v", err)
	}
}

// A manifest that predates the general-purpose agent must still resolve a
// default rather than failing every unqualified agent() call.
func TestResolveWorkflowTargetFallsBackWithoutGeneralPurpose(t *testing.T) {
	manifest := config.DefaultAgentManifest()
	kept := manifest.Agents[:0]
	for _, a := range manifest.Agents {
		if a.ID != "general-purpose" {
			kept = append(kept, a)
		}
	}
	manifest.Agents = kept
	spec, err := resolveWorkflowTarget(&manifest, "")
	if err != nil {
		t.Fatalf("fallback target: %v", err)
	}
	if spec.ID != "code" {
		t.Fatalf("fallback target = %q, want code", spec.ID)
	}
}

func TestWorkflowEffortMapping(t *testing.T) {
	cases := map[string]provider.ThinkingLevel{
		"low": provider.ThinkingLow, "medium": provider.ThinkingMedium,
		"high": provider.ThinkingHigh, "xhigh": provider.ThinkingXHigh,
		"x-high": provider.ThinkingXHigh, "max": provider.ThinkingMax,
		"off": provider.ThinkingOff, "NONE": provider.ThinkingOff,
	}
	for in, want := range cases {
		got, ok := workflowEffort(in)
		if !ok || got != want {
			t.Fatalf("workflowEffort(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	if _, ok := workflowEffort("turbo"); ok {
		t.Fatal("an unknown effort must fall through to the session level")
	}
	if _, ok := workflowEffort(""); ok {
		t.Fatal("an unset effort must fall through to the session level")
	}
}

func TestRunWorkflowRejectsSubagentsAndAskFirst(t *testing.T) {
	manifest := config.DefaultAgentManifest()
	nested := &toolRuntime{delegationDepth: 1, manifest: &manifest}
	if _, err := nested.runWorkflow(context.Background(), json.RawMessage(`{"script":"x"}`)); err == nil ||
		!strings.Contains(err.Error(), "only the top-level agent") {
		t.Fatalf("want a depth error, got %v", err)
	}
	top := &toolRuntime{manifest: &manifest, providerMgr: &provider.Manager{}, permission: config.PermissionAskFirst}
	if _, err := top.runWorkflow(context.Background(), json.RawMessage(`{"script":"x"}`)); err == nil ||
		!strings.Contains(err.Error(), "restricted or yolo") {
		t.Fatalf("want a permission error, got %v", err)
	}
}

func TestRunWorkflowRequiresAScript(t *testing.T) {
	manifest := config.DefaultAgentManifest()
	rt := &toolRuntime{manifest: &manifest, providerMgr: &provider.Manager{}, permission: config.PermissionYOLO}
	_, err := rt.runWorkflow(context.Background(), json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "pass script") {
		t.Fatalf("want a missing-script error, got %v", err)
	}
	_, err = rt.runWorkflow(context.Background(), json.RawMessage(`{"script":"return 1"}`))
	if err == nil || !strings.Contains(err.Error(), "export const meta") {
		t.Fatalf("want a missing-header error, got %v", err)
	}
}

func TestRenderWorkflowResult(t *testing.T) {
	out := renderWorkflowResult("wf_1", "/tmp/run", "inline", workflow.Meta{Name: "audit"}, workflow.Result{
		Value:  map[string]any{"confirmed": []any{"a", "b"}},
		Agents: 7, Failed: 2, Cached: 1, Tokens: 4200,
		Logs:   []string{"round 1: 3 found"},
		Phases: []string{"Find", "Verify"},
	}, []string{`general-purpose#3: workspace merge conflict — branch "spettro/gp-3" kept at /w/gp-3`})
	for _, want := range []string{
		`name="audit"`, `run_id="wf_1"`,
		"7 agents · 2 failed · 1 replayed from journal · 4200 tokens",
		"Find → Verify", "round 1: 3 found", `"confirmed"`,
		"resume_from_run_id", "Some agents failed",
		"<unmerged>", `branch "spettro/gp-3"`, "did not merge back",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("result missing %q:\n%s", want, out)
		}
	}
}

func TestRenderWorkflowResultCleanRun(t *testing.T) {
	out := renderWorkflowResult("wf_2", "/tmp/run", "inline", workflow.Meta{Name: "sweep"}, workflow.Result{
		Value: "all clear", Agents: 3,
	}, nil)
	if strings.Contains(out, "Some agents failed") {
		t.Fatalf("a clean run must not warn about failures:\n%s", out)
	}
	if strings.Contains(out, "unmerged") {
		t.Fatalf("a clean run must not mention unmerged work:\n%s", out)
	}
	if !strings.Contains(out, "all clear") {
		t.Fatalf("a string return value should pass through verbatim:\n%s", out)
	}
}

func TestWorkflowRunIDsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		id := newWorkflowRunID()
		if seen[id] {
			t.Fatalf("duplicate run id %q", id)
		}
		seen[id] = true
	}
}

// The tool must be injected by the keyword alone, without a host flag, so
// every surface gets workflows for free.
func TestWorkflowToolHasDescriptionAndSchema(t *testing.T) {
	specs := buildToolSpecs([]string{workflowToolID})
	if len(specs) != 1 {
		t.Fatalf("workflow tool is not exposed to the model: %+v", specs)
	}
	var schema map[string]any
	if err := json.Unmarshal(specs[0].Schema, &schema); err != nil {
		t.Fatalf("schema does not parse: %v", err)
	}
	props, _ := schema["properties"].(map[string]any)
	for _, key := range []string{"script", "script_path", "name", "args", "resume_from_run_id"} {
		if _, ok := props[key]; !ok {
			t.Fatalf("schema is missing %q", key)
		}
	}
}

func TestFanOutToolsGrantsByModeAndDepth(t *testing.T) {
	base := []string{"file-read", "bash"}

	tools, prompt := fanOutTools(base, false, false, 0)
	if len(tools) != 2 || prompt != "" {
		t.Fatalf("no mode on should grant nothing: %v %q", tools, prompt)
	}

	tools, prompt = fanOutTools(base, false, true, 0)
	if !contains(tools, workflowToolID) || contains(tools, ultraToolID) {
		t.Fatalf("workflows alone should grant only the workflow tool: %v", tools)
	}
	if !strings.Contains(prompt, "WORKFLOWS are available") || strings.Contains(prompt, "ULTRA MODE") {
		t.Fatalf("wrong guidance: %q", prompt)
	}

	tools, prompt = fanOutTools(base, true, true, 0)
	if !contains(tools, workflowToolID) || !contains(tools, ultraToolID) {
		t.Fatalf("both modes should grant both tools: %v", tools)
	}
	if !strings.Contains(prompt, "ULTRA MODE") || !strings.Contains(prompt, "WORKFLOWS are available") {
		t.Fatalf("both guidance sections expected: %q", prompt)
	}

	// A sub-agent never orchestrates: that is what stops swarms of swarms.
	tools, prompt = fanOutTools(base, true, true, 1)
	if len(tools) != 2 || prompt != "" {
		t.Fatalf("sub-agents must get neither tool: %v %q", tools, prompt)
	}

	// Granting twice must not duplicate an already allow-listed tool.
	tools, _ = fanOutTools([]string{"workflow"}, false, true, 0)
	if len(tools) != 1 {
		t.Fatalf("duplicate grant: %v", tools)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestFindWorkflowRunDir(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	thisSession := filepath.Join(sessions, "session-a")
	otherSession := filepath.Join(sessions, "session-b")

	mkRun := func(sessionDir, runID string) string {
		dir := filepath.Join(sessionDir, "workflows", runID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "journal.jsonl"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	mine := mkRun(thisSession, "wf_mine")
	theirs := mkRun(otherSession, "wf_theirs")

	r := &toolRuntime{sessionDir: thisSession}

	if got, err := r.findWorkflowRunDir("wf_mine", ""); err != nil || got != mine {
		t.Fatalf("own session: %q %v", got, err)
	}
	// The common case: an editor opens a fresh session per prompt, so the run
	// being resumed lives under a sibling.
	if got, err := r.findWorkflowRunDir("wf_theirs", ""); err != nil || got != theirs {
		t.Fatalf("sibling session: %q %v", got, err)
	}
	// Re-running <run>/script.js is the documented resume path, so the
	// script's own directory counts even outside the session tree.
	loose := filepath.Join(root, "elsewhere", "wf_loose")
	if err := os.MkdirAll(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(loose, "journal.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := r.findWorkflowRunDir("wf_loose", filepath.Join(loose, "script.js")); err != nil || got != loose {
		t.Fatalf("script-adjacent run: %q %v", got, err)
	}
	// A run that does not exist must be an error, not a silent full re-run.
	if _, err := r.findWorkflowRunDir("wf_missing", ""); err == nil ||
		!strings.Contains(err.Error(), "no journal found") {
		t.Fatalf("want a not-found error, got %v", err)
	}
	for _, bad := range []string{"", "../escape", "a/b"} {
		if _, err := r.findWorkflowRunDir(bad, ""); err == nil ||
			!strings.Contains(err.Error(), "invalid resume_from_run_id") {
			t.Fatalf("%q: want an invalid-id error, got %v", bad, err)
		}
	}
}

func TestWorkflowActivationSpans(t *testing.T) {
	// Spans are rune indices, not byte offsets: the TUI highlights cells.
	spans := WorkflowActivationSpans("héllo ultracode wörld ULTRACODE")
	if len(spans) != 2 {
		t.Fatalf("spans = %v, want 2", spans)
	}
	text := []rune("héllo ultracode wörld ULTRACODE")
	if got := string(text[spans[0][0]:spans[0][1]]); got != "ultracode" {
		t.Fatalf("first span = %q", got)
	}
	if got := string(text[spans[1][0]:spans[1][1]]); got != "ULTRACODE" {
		t.Fatalf("second span = %q", got)
	}
	if WorkflowActivationSpans("ultracoded") != nil {
		t.Fatal("a non-activating word must produce no span")
	}
	// The highlight and the runtime must never disagree.
	for _, s := range []string{"", "ultracode", "ultracoded", "x ultracode.", "ULTRAcode!", "src/ultracode.go"} {
		if (len(WorkflowActivationSpans(s)) > 0) != WorkflowRequested(s) {
			t.Fatalf("%q: spans and WorkflowRequested disagree", s)
		}
	}
}

// Asking for a workflow in plain English has to work: making people learn a
// magic word to reach the feature would be a worse tool.
func TestWorkflowRequestedFromPlainEnglish(t *testing.T) {
	on := []string{
		"use a workflow to modernise these handlers",
		"Use a workflow for this",
		"can you write a workflow that reviews the diff",
		"run a multi-agent workflow over the packages",
		"set up a workflow to migrate the call sites",
		"do this as a workflow please",
		"handle it with workflows",
		"reach for the workflow tool here",
		"fan this out across sub-agents",
		"fan it out over several agents",
		"orchestrate this with subagents",
		"orchestrate the migration using sub-agents",
		"I want a multi-agent orchestration for this",
		"kick off a workflow",
	}
	for _, s := range on {
		if !WorkflowRequested(s) {
			t.Fatalf("%q should activate workflows", s)
		}
	}

	// And ordinary uses of the word "workflow" must stay quiet, or every
	// conversation about CI would silently pay for the guidance.
	off := []string{
		"our deploy workflow is broken",
		"fix the GitHub workflow in .github/workflows/ci.yml",
		"what is the workflow for releasing?",
		"the workflow diagram needs updating",
		"add a step to the release workflow",
		"run the workflow again",
		"trigger the workflow on push",
		"agents.md documents the agent manifest",
		"the agent tool delegates one subtask",
		"this workflow of ours predates the rewrite",
	}
	for _, s := range off {
		if WorkflowRequested(s) {
			t.Fatalf("%q must not activate workflows", s)
		}
	}
}

// Patterns overlap on purpose; a renderer handed overlapping ranges would
// style the same cell twice.
func TestWorkflowActivationSpansAreMergedAndOrdered(t *testing.T) {
	text := "please use a workflow tool here, then ultracode the rest"
	spans := WorkflowActivationSpans(text)
	if len(spans) != 2 {
		t.Fatalf("spans = %v, want 2 merged spans", spans)
	}
	runes := []rune(text)
	if got := string(runes[spans[0][0]:spans[0][1]]); got != "use a workflow tool" {
		t.Fatalf("first span = %q, want the merged phrase", got)
	}
	if got := string(runes[spans[1][0]:spans[1][1]]); got != "ultracode" {
		t.Fatalf("second span = %q", got)
	}
	if spans[0][1] > spans[1][0] {
		t.Fatalf("spans overlap: %v", spans)
	}
}

func TestWorkflowPreapprovedOnlyByKeyword(t *testing.T) {
	// The keyword is a standing yes.
	for _, s := range []string{"ultracode", "ULTRACODE do the thing", "ok, ultracode."} {
		if !WorkflowPreapproved(s) {
			t.Fatalf("%q should be pre-approved", s)
		}
	}
	// Asking in plain English turns the tool on but still has to be confirmed.
	for _, s := range []string{"use a workflow for this", "fan this out across agents", ""} {
		if WorkflowPreapproved(s) {
			t.Fatalf("%q must not be pre-approved", s)
		}
	}
	if !WorkflowRequested("use a workflow for this") {
		t.Fatal("a plain-English request must still activate the tool")
	}
}

// askUserFunc adapts a decision function to the callback shape.
func askUserFunc(pick string, err error) AskUserCallback {
	return func(context.Context, AskUserForm) ([]AskUserAnswer, error) {
		if err != nil {
			return nil, err
		}
		if pick == "" {
			return []AskUserAnswer{{Header: "Workflow", Skipped: true}}, nil
		}
		return []AskUserAnswer{{Header: "Workflow", Selected: []string{pick}}}, nil
	}
}

func TestConfirmWorkflow(t *testing.T) {
	meta := workflow.Meta{Name: "audit", Description: "Audit the repo"}

	// The keyword skips the prompt entirely.
	asked := false
	rt := &toolRuntime{workflowPreapproved: true, askUser: func(context.Context, AskUserForm) ([]AskUserAnswer, error) {
		asked = true
		return nil, nil
	}}
	if got := rt.confirmWorkflow(context.Background(), meta, workflowArgs{}); got != workflowRun {
		t.Fatalf("pre-approved decision = %v", got)
	}
	if asked {
		t.Fatal("a pre-approved run must not prompt")
	}

	cases := map[string]workflowDecision{
		"Run it":             workflowRun,
		"Save it, don't run": workflowSaveOnly,
		"Don't run it":       workflowDeclined,
	}
	for pick, want := range cases {
		rt := &toolRuntime{askUser: askUserFunc(pick, nil)}
		if got := rt.confirmWorkflow(context.Background(), meta, workflowArgs{}); got != want {
			t.Fatalf("%q → %v, want %v", pick, got, want)
		}
	}

	// Skipping the question is a refusal, not a yes: silence must never be
	// read as consent to spend.
	rt = &toolRuntime{askUser: askUserFunc("", nil)}
	if got := rt.confirmWorkflow(context.Background(), meta, workflowArgs{}); got != workflowDeclined {
		t.Fatalf("skipped question = %v, want declined", got)
	}

	// Nobody to ask (headless, goal loop, relay): the request that turned
	// workflows on this turn stands in for the answer.
	rt = &toolRuntime{askUser: nil}
	if got := rt.confirmWorkflow(context.Background(), meta, workflowArgs{}); got != workflowRun {
		t.Fatalf("no transport = %v, want run", got)
	}
	rt = &toolRuntime{askUser: askUserFunc("", errors.New("no ui"))}
	if got := rt.confirmWorkflow(context.Background(), meta, workflowArgs{}); got != workflowRun {
		t.Fatalf("failed transport = %v, want run", got)
	}
	// Except the chat exit, which is the user actively stepping away from it.
	rt = &toolRuntime{askUser: askUserFunc("", ErrAskUserReplyInChat)}
	if got := rt.confirmWorkflow(context.Background(), meta, workflowArgs{}); got != workflowDeclined {
		t.Fatalf("chat exit = %v, want declined", got)
	}
}

func TestRunWorkflowSavesAndAsksBeforeRunning(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	manifest := config.DefaultAgentManifest()
	script := "export const meta = {name: 'audit', description: 'Audit the repo', phases: [{title: 'Scan'}]}\nreturn 1"

	rt := &toolRuntime{
		cwd: cwd, manifest: &manifest, providerMgr: &provider.Manager{},
		permission: config.PermissionYOLO,
		askUser:    askUserFunc("Save it, don't run", nil),
	}
	args, _ := json.Marshal(workflowArgs{Script: script})
	out, err := rt.runWorkflow(context.Background(), args)
	if err != nil {
		t.Fatalf("save-only run: %v", err)
	}
	if !strings.Contains(out, "chose not to run") || !strings.Contains(out, "Do not run it anyway") {
		t.Fatalf("result must tell the model the user declined:\n%s", out)
	}
	saved := filepath.Join(cwd, ".spettro", "workflows", "audit.js")
	if data, err := os.ReadFile(saved); err != nil || string(data) != script {
		t.Fatalf("script not saved at %s: %v", saved, err)
	}

	// A refusal must not save anything either.
	cwd2 := t.TempDir()
	rt2 := &toolRuntime{
		cwd: cwd2, manifest: &manifest, providerMgr: &provider.Manager{},
		permission: config.PermissionYOLO,
		askUser:    askUserFunc("Don't run it", nil),
	}
	if _, err := rt2.runWorkflow(context.Background(), args); err == nil ||
		!strings.Contains(err.Error(), "declined") {
		t.Fatalf("want a declined error, got %v", err)
	}
	// Nothing at all, not just no saved script: consent has to come before the
	// run directory is created, or a refusal still leaves a transcript behind.
	var leftovers []string
	_ = filepath.WalkDir(filepath.Join(cwd2, ".spettro"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			leftovers = append(leftovers, path)
		}
		return nil
	})
	if len(leftovers) > 0 {
		t.Fatalf("a declined workflow wrote to disk: %v", leftovers)
	}
}

// Run transcripts and the user's reusable scripts must not share a directory:
// filling .spettro/workflows with run directories would bury the scripts the
// user actually keeps there.
func TestWorkflowRunDirIsSeparateFromSavedScripts(t *testing.T) {
	withSession := &toolRuntime{cwd: "/proj", sessionDir: "/home/u/.spettro/sessions/session-1"}
	if got, want := withSession.workflowRunDir("wf_1"),
		filepath.Join("/home/u/.spettro/sessions/session-1", "workflows", "wf_1"); got != want {
		t.Fatalf("session run dir = %q, want %q", got, want)
	}

	sessionless := &toolRuntime{cwd: "/proj"}
	got := sessionless.workflowRunDir("wf_1")
	if want := filepath.Join("/proj", ".spettro", "workflow-runs", "wf_1"); got != want {
		t.Fatalf("sessionless run dir = %q, want %q", got, want)
	}
	saved := filepath.Join("/proj", ".spettro", workflow.SavedWorkflowsDir)
	if strings.HasPrefix(got, saved+string(filepath.Separator)) {
		t.Fatalf("run transcripts land inside the saved-workflow folder: %q", got)
	}
}

// script_path is resolved like every other tool path: a relative path is
// relative to the agent's workspace (not spettro's process directory, which
// differs under ACP), and a path outside the workspace, the session's run
// directories and the saved-workflow folders is refused.
func TestResolveWorkflowScriptPath(t *testing.T) {
	ws := t.TempDir()
	sessions := t.TempDir()
	runDir := filepath.Join(sessions, "sess-1", "workflows", "wf_1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "export const meta = {name: 'x'}\nreturn 1\n"
	for _, p := range []string{filepath.Join(ws, "wf.js"), filepath.Join(runDir, "script.js")} {
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "secret.js")
	if err := os.WriteFile(outside, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := &toolRuntime{cwd: ws, sessionDir: filepath.Join(sessions, "sess-2")}
	for _, ok := range []string{"wf.js", filepath.Join(runDir, "script.js")} {
		if script, _, err := rt.resolveWorkflowScript(workflowArgs{ScriptPath: ok}); err != nil || script != src {
			t.Errorf("script_path %q: script=%q err=%v", ok, script, err)
		}
	}
	if _, _, err := rt.resolveWorkflowScript(workflowArgs{ScriptPath: outside}); err == nil {
		t.Errorf("script_path outside every allowed root was read")
	}
}
