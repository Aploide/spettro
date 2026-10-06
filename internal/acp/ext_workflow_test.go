package acp

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"spettro/internal/session"
)

const goodScript = `export const meta = {
  name: 'demo',
  description: 'A demo workflow',
  whenToUse: 'when demonstrating',
  phases: [{ title: 'Scan', detail: 'look around' }, { title: 'Report' }],
}
phase('Scan')
return 'ok'
`

// projectBridge gives the extension a project to work in and a HOME that is
// not the developer's, so a test can never write into a real ~/.spettro.
func projectBridge(t *testing.T) (*bridge, string) {
	t.Helper()
	cwd := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	return newBridge(Options{CWD: cwd, GlobalDir: t.TempDir()}), cwd
}

func TestWorkflowWriteReadList(t *testing.T) {
	b, cwd := projectBridge(t)
	ctx := context.Background()

	written, err := b.workflowWrite(ctx, workflowWriteArgs{
		workflowScopeArgs: workflowScopeArgs{Cwd: cwd},
		Name:              "demo",
		Script:            goodScript,
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	// The repo is where a workflow belongs: it is versioned with the code it
	// automates, and a teammate who clones the project gets it.
	want := filepath.Join(cwd, ".spettro", "workflows", "demo.js")
	if written.Path != want {
		t.Fatalf("saved to %q, want %q", written.Path, want)
	}
	if written.Scope != "project" {
		t.Fatalf("scope = %q, want project", written.Scope)
	}
	if len(written.Phases) != 2 || written.Phases[0].Title != "Scan" ||
		written.Phases[0].Detail != "look around" {
		t.Fatalf("phases not parsed back: %+v", written.Phases)
	}

	read, err := b.workflowRead(ctx, workflowReadArgs{
		workflowScopeArgs: workflowScopeArgs{Cwd: cwd}, Name: "demo",
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if read.Script != goodScript {
		t.Fatalf("script did not round-trip:\n%s", read.Script)
	}
	if read.Description != "A demo workflow" || read.WhenToUse != "when demonstrating" {
		t.Fatalf("header lost on read: %+v", read.WorkflowInfo)
	}

	list, err := b.workflowList(ctx, workflowScopeArgs{Cwd: cwd})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Workflows) != 1 || list.Workflows[0].Name != "demo" {
		t.Fatalf("list = %+v", list.Workflows)
	}
	if list.Workflows[0].Error != "" {
		t.Fatalf("a valid script must not report an error: %q", list.Workflows[0].Error)
	}
	// The client needs somewhere to offer to save; a project with no folder
	// yet still has to be told where the folder would go.
	if len(list.SearchPaths) == 0 || !strings.HasPrefix(list.SearchPaths[0], cwd) {
		t.Fatalf("search paths = %v", list.SearchPaths)
	}
}

// A script that does not compile must never reach the folder: the failure
// would surface later, to whoever runs it, far from whoever wrote it.
func TestWorkflowWriteRefusesBrokenScript(t *testing.T) {
	b, cwd := projectBridge(t)
	_, err := b.workflowWrite(context.Background(), workflowWriteArgs{
		workflowScopeArgs: workflowScopeArgs{Cwd: cwd},
		Name:              "broken",
		Script:            "export const meta = { name: 'broken' }\nthis is not javascript(",
	})
	if err == nil {
		t.Fatal("a script that does not compile must not be saved")
	}
	if _, statErr := os.Stat(filepath.Join(cwd, ".spettro", "workflows", "broken.js")); statErr == nil {
		t.Fatal("the refused script was written anyway")
	}
}

// An editor validates on every keystroke, and most keystrokes leave the script
// mid-edit. "Does not compile" is an answer, not a call failure.
func TestWorkflowValidateReportsRatherThanFails(t *testing.T) {
	b, _ := projectBridge(t)
	ctx := context.Background()

	ok, err := b.workflowValidate(ctx, workflowValidateArgs{Script: goodScript})
	if err != nil {
		t.Fatalf("validate returned a call error: %v", err)
	}
	if !ok.Ok || ok.Name != "demo" || len(ok.Phases) != 2 {
		t.Fatalf("valid script misreported: %+v", ok)
	}

	bad, err := b.workflowValidate(ctx, workflowValidateArgs{Script: "export const meta = {"})
	if err != nil {
		t.Fatalf("an invalid script must not be a call error: %v", err)
	}
	if bad.Ok || bad.Error == "" {
		t.Fatalf("invalid script reported as fine: %+v", bad)
	}
}

// A listing that hides a broken file sends the user to find out the hard way.
func TestWorkflowListSurfacesBrokenFiles(t *testing.T) {
	b, cwd := projectBridge(t)
	dir := filepath.Join(cwd, ".spettro", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wrong.js"), []byte("export const meta = {"), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := b.workflowList(context.Background(), workflowScopeArgs{Cwd: cwd})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Workflows) != 1 {
		t.Fatalf("broken file dropped from listing: %+v", list.Workflows)
	}
	if list.Workflows[0].Error == "" {
		t.Fatal("broken file listed as if it were runnable")
	}
}

// A project script shadows a global one of the same name, so "delete the one I
// can see" would remove the wrong file for anyone who has both.
func TestWorkflowDeleteRespectsScope(t *testing.T) {
	b, cwd := projectBridge(t)
	ctx := context.Background()

	for _, scope := range []string{"project", "global"} {
		if _, err := b.workflowWrite(ctx, workflowWriteArgs{
			workflowScopeArgs: workflowScopeArgs{Cwd: cwd},
			Name:              "demo", Scope: scope, Script: goodScript,
		}); err != nil {
			t.Fatalf("write %s: %v", scope, err)
		}
	}

	res, err := b.workflowDelete(ctx, workflowDeleteArgs{
		workflowScopeArgs: workflowScopeArgs{Cwd: cwd}, Name: "demo", Scope: "global",
	})
	if err != nil || !res.Deleted {
		t.Fatalf("global delete: %+v %v", res, err)
	}
	// The project copy must have survived.
	if _, err := os.Stat(filepath.Join(cwd, ".spettro", "workflows", "demo.js")); err != nil {
		t.Fatalf("deleting the global copy removed the project one: %v", err)
	}

	// Deleting nothing is a normal answer, not an error.
	gone, err := b.workflowDelete(ctx, workflowDeleteArgs{
		workflowScopeArgs: workflowScopeArgs{Cwd: cwd}, Name: "nope", Scope: "project",
	})
	if err != nil {
		t.Fatalf("deleting a missing workflow errored: %v", err)
	}
	if gone.Deleted {
		t.Fatal("reported deleting a workflow that never existed")
	}
}

// Writing into the wrong repo is not a mistake worth being quiet about.
func TestWorkflowUnknownSessionIsAnError(t *testing.T) {
	b, _ := projectBridge(t)
	if _, err := b.workflowList(context.Background(), workflowScopeArgs{SessionID: "nope"}); err == nil {
		t.Fatal("an unknown sessionId must not fall back to the process cwd")
	}
}

// A name the header already carries is a name the user should not have to
// repeat.
func TestWorkflowWriteFallsBackToMetaName(t *testing.T) {
	b, cwd := projectBridge(t)
	res, err := b.workflowWrite(context.Background(), workflowWriteArgs{
		workflowScopeArgs: workflowScopeArgs{Cwd: cwd}, Script: goodScript,
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if res.Name != "demo" {
		t.Fatalf("name = %q, want the header's own name", res.Name)
	}
}

// Runs are listed for the scope's project only: a "recent runs" list in one
// repo must not offer to resume a run another repo's script produced. A
// session still in its first turn has no stored metadata yet, so its own cwd
// places it.
func TestWorkflowRunsHonourProjectScope(t *testing.T) {
	b, cwd := projectBridge(t)
	other := t.TempDir()
	ctx := context.Background()

	addRun := func(sessionID, runID string) {
		t.Helper()
		dir := filepath.Join(session.SessionDir(b.opts.GlobalDir, sessionID), "workflows", runID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	saveMeta := func(sessionID, project string) {
		t.Helper()
		if err := session.Save(b.opts.GlobalDir, session.State{Metadata: session.Metadata{
			ID: sessionID, ProjectPath: project, ProjectHash: session.ProjectHash(project),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	saveMeta("here", cwd)
	addRun("here", "run-here")
	saveMeta("there", other)
	addRun("there", "run-there")
	// Open on this project, nothing persisted but its run directory.
	b.sessions["live"] = &acpSession{id: "live", cwd: cwd}
	addRun("live", "run-live")
	// Neither stored nor open: no way to tell its project.
	addRun("orphan", "run-orphan")

	ids := func(res WorkflowRunsResult) string {
		var names []string
		for _, r := range res.Runs {
			names = append(names, r.RunID)
		}
		slices.Sort(names)
		return strings.Join(names, ",")
	}
	for _, tc := range []struct {
		name  string
		scope workflowScopeArgs
		want  string
	}{
		{"cwd", workflowScopeArgs{Cwd: cwd}, "run-here,run-live"},
		{"session", workflowScopeArgs{SessionID: "live"}, "run-here,run-live"},
		{"process cwd", workflowScopeArgs{}, "run-here,run-live"},
		{"other project", workflowScopeArgs{Cwd: other}, "run-there"},
	} {
		res, err := b.workflowRuns(ctx, workflowRunsArgs{workflowScopeArgs: tc.scope})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := ids(res); got != tc.want {
			t.Errorf("%s: runs = %q, want %q", tc.name, got, tc.want)
		}
	}
	if _, err := b.workflowRuns(ctx, workflowRunsArgs{workflowScopeArgs: workflowScopeArgs{SessionID: "nope"}}); err == nil {
		t.Error("an unknown sessionId must not fall back to the process cwd")
	}
}
