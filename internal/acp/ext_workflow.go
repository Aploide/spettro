package acp

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/session"
	"spettro/internal/workflow"
)

// The workflow authoring surface.
//
// Core ACP can carry a workflow *run* — it turns into tool calls like any
// other work — but it has no vocabulary at all for the thing a user actually
// wants to do with workflows, which is write one, see whether it compiles,
// keep it, and run it again tomorrow. Until now a GUI client's only route was
// to type "/workflows run <name>" as ordinary prose and let the CLI rewrite it
// into a prompt: the client could not list what a repo had, could not read a
// script, could not save one, and could not tell a typo from a missing file
// until a model had already spent a turn finding out.
//
// These methods close that gap. They are deliberately thin over the same
// internal/workflow primitives the TUI's /workflows command uses — Discover,
// Load, Save, Validate — so a script written in the desktop app is the same
// file the TUI runs, in the same .spettro/workflows folder, in the repo it
// belongs to. Nothing here is a second source of truth.
//
// Running is intentionally NOT here. A run needs the model, the manifest, the
// permission level and the sub-agent machinery, all of which hang off a prompt
// turn; the client starts one through the normal Prompt path and watches the
// tool calls it already knows how to render. What the client could not do
// before, and now can, is everything around the run.
const (
	extWorkflowList     = "_spettro/workflow/list"
	extWorkflowRead     = "_spettro/workflow/read"
	extWorkflowWrite    = "_spettro/workflow/write"
	extWorkflowDelete   = "_spettro/workflow/delete"
	extWorkflowValidate = "_spettro/workflow/validate"
	extWorkflowRuns     = "_spettro/workflow/runs"
)

// workflowScopeArgs carries the project a call is about. Every method takes it
// the same way: an explicit sessionId (so the call lands in the project that
// session was opened on), or an explicit cwd, or neither — in which case the
// process working directory stands in, exactly as NewSession does it.
type workflowScopeArgs struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
}

// resolveCwd turns the scope into a project root. A sessionId that names no
// session is an error rather than a silent fall back to the process cwd:
// writing a workflow into the wrong repo is not a mistake worth being quiet
// about.
func (b *bridge) resolveCwd(scope workflowScopeArgs) (string, error) {
	if id := strings.TrimSpace(scope.SessionID); id != "" {
		b.mu.Lock()
		s, ok := b.sessions[id]
		var cwd string
		if ok {
			cwd = s.cwd
		}
		b.mu.Unlock()
		if !ok {
			return "", extError("session %s not found", id)
		}
		return cwd, nil
	}
	if cwd := strings.TrimSpace(scope.Cwd); cwd != "" {
		if !filepath.IsAbs(cwd) {
			return "", acpsdk.NewInvalidParams(map[string]any{"error": "cwd must be an absolute path"})
		}
		return cwd, nil
	}
	return b.opts.CWD, nil
}

// WorkflowPhaseInfo is one declared phase.
type WorkflowPhaseInfo struct {
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
}

// WorkflowInfo describes a saved workflow. Error is set when the file is
// present but does not compile: a listing that hides a broken script sends the
// user to find out the hard way, and the editor wants to show the problem
// against the file it belongs to.
type WorkflowInfo struct {
	Name        string              `json:"name"`
	Path        string              `json:"path"`
	Scope       string              `json:"scope"`
	Description string              `json:"description,omitempty"`
	WhenToUse   string              `json:"whenToUse,omitempty"`
	Phases      []WorkflowPhaseInfo `json:"phases"`
	Error       string              `json:"error,omitempty"`
}

func phaseInfos(meta workflow.Meta) []WorkflowPhaseInfo {
	out := make([]WorkflowPhaseInfo, 0, len(meta.Phases))
	for _, p := range meta.Phases {
		out = append(out, WorkflowPhaseInfo{Title: p.Title, Detail: p.Detail})
	}
	return out
}

func workflowInfo(s workflow.Saved) WorkflowInfo {
	info := WorkflowInfo{
		Name:        s.Name,
		Path:        s.Path,
		Scope:       s.Scope,
		Description: s.Meta.Description,
		WhenToUse:   s.Meta.WhenToUse,
		Phases:      phaseInfos(s.Meta),
	}
	if s.Err != nil {
		info.Error = s.Err.Error()
	}
	return info
}

// WorkflowListResult is the reply to `_spettro/workflow/list`. SearchPaths is
// included so a client can show *where* it would save, and can offer to create
// the folder for a project that has none yet.
type WorkflowListResult struct {
	Workflows   []WorkflowInfo `json:"workflows"`
	SearchPaths []string       `json:"searchPaths"`
	Cwd         string         `json:"cwd"`
}

func (b *bridge) workflowList(_ context.Context, args workflowScopeArgs) (WorkflowListResult, error) {
	cwd, err := b.resolveCwd(args)
	if err != nil {
		return WorkflowListResult{}, err
	}
	saved := workflow.Discover(cwd)
	out := make([]WorkflowInfo, 0, len(saved))
	for _, s := range saved {
		out = append(out, workflowInfo(s))
	}
	return WorkflowListResult{Workflows: out, SearchPaths: workflow.SearchPaths(cwd), Cwd: cwd}, nil
}

type workflowReadArgs struct {
	workflowScopeArgs
	Name string `json:"name"`
}

// WorkflowReadResult carries the script itself alongside the parsed header, so
// an editor can open a file and draw its phase tree in one round trip.
type WorkflowReadResult struct {
	WorkflowInfo
	Script string `json:"script"`
}

func (b *bridge) workflowRead(_ context.Context, args workflowReadArgs) (WorkflowReadResult, error) {
	cwd, err := b.resolveCwd(args.workflowScopeArgs)
	if err != nil {
		return WorkflowReadResult{}, err
	}
	script, path, err := workflow.Load(cwd, args.Name)
	if err != nil {
		return WorkflowReadResult{}, extError("%s", err.Error())
	}
	info := WorkflowInfo{Name: strings.TrimSpace(args.Name), Path: path, Scope: scopeOf(cwd, path)}
	// A script that does not compile is still readable, and reading it is
	// exactly what you do next — so the error travels with the source rather
	// than replacing it.
	if meta, err := workflow.Validate(script); err != nil {
		info.Error = err.Error()
		info.Description, info.WhenToUse, info.Phases = meta.Description, meta.WhenToUse, phaseInfos(meta)
	} else {
		info.Description, info.WhenToUse, info.Phases = meta.Description, meta.WhenToUse, phaseInfos(meta)
	}
	return WorkflowReadResult{WorkflowInfo: info, Script: script}, nil
}

// scopeOf reports whether a resolved path lives in the project's folder or the
// global one, by asking where it actually is rather than trusting the caller.
func scopeOf(cwd, path string) string {
	paths := workflow.SearchPaths(cwd)
	if len(paths) > 0 && strings.HasPrefix(path, paths[0]) {
		return "project"
	}
	return "global"
}

type workflowWriteArgs struct {
	workflowScopeArgs
	Name   string `json:"name"`
	Scope  string `json:"scope"`
	Script string `json:"script"`
}

// WorkflowWriteResult is the reply to a save. The parsed header comes back
// because saving is when the client learns the canonical phase list — the
// script is the source of truth for it, not whatever the editor was showing.
type WorkflowWriteResult struct {
	WorkflowInfo
}

func (b *bridge) workflowWrite(_ context.Context, args workflowWriteArgs) (WorkflowWriteResult, error) {
	cwd, err := b.resolveCwd(args.workflowScopeArgs)
	if err != nil {
		return WorkflowWriteResult{}, err
	}
	// Validate before Save does, so the client gets the compile error as the
	// error for *this* call rather than wrapped in a refusal-to-save.
	meta, err := workflow.Validate(args.Script)
	if err != nil {
		return WorkflowWriteResult{}, extError("%s", err.Error())
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		// meta.name is the workflow's own idea of what it is called; falling
		// back to it means "save" works on a script the user has only ever
		// named inside its own header.
		name = meta.Name
	}
	path, err := workflow.Save(cwd, name, args.Scope, args.Script)
	if err != nil {
		return WorkflowWriteResult{}, extError("%s", err.Error())
	}
	return WorkflowWriteResult{WorkflowInfo{
		Name:        name,
		Path:        path,
		Scope:       scopeOf(cwd, path),
		Description: meta.Description,
		WhenToUse:   meta.WhenToUse,
		Phases:      phaseInfos(meta),
	}}, nil
}

type workflowDeleteArgs struct {
	workflowScopeArgs
	Name  string `json:"name"`
	Scope string `json:"scope"`
}

// WorkflowDeleteResult reports what was removed, so a client can tell "deleted"
// from "there was nothing there".
type WorkflowDeleteResult struct {
	Deleted bool   `json:"deleted"`
	Path    string `json:"path,omitempty"`
}

func (b *bridge) workflowDelete(_ context.Context, args workflowDeleteArgs) (WorkflowDeleteResult, error) {
	cwd, err := b.resolveCwd(args.workflowScopeArgs)
	if err != nil {
		return WorkflowDeleteResult{}, err
	}
	name := strings.TrimSpace(args.Name)
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return WorkflowDeleteResult{}, acpsdk.NewInvalidParams(map[string]any{"error": "invalid workflow name"})
	}
	// Delete resolves by scope rather than by shadowing order: a project
	// script shadows a global one of the same name, so "delete the one I can
	// see" would silently remove the wrong file for anyone who has both.
	paths := workflow.SearchPaths(cwd)
	var dirs []string
	switch args.Scope {
	case "project":
		if len(paths) > 0 {
			dirs = paths[:1]
		}
	case "global":
		if len(paths) > 1 {
			dirs = paths[1:]
		}
	default:
		dirs = paths
	}
	for _, dir := range dirs {
		path := filepath.Join(dir, name+".js")
		if err := os.Remove(path); err == nil {
			return WorkflowDeleteResult{Deleted: true, Path: path}, nil
		} else if !os.IsNotExist(err) {
			return WorkflowDeleteResult{}, extError("delete %s: %s", path, err)
		}
	}
	return WorkflowDeleteResult{Deleted: false}, nil
}

type workflowValidateArgs struct {
	Script string `json:"script"`
}

// WorkflowValidateResult is the editor's live feedback: does this compile, and
// what does its header declare. Ok=false is a normal answer, not a call
// failure — an editor validates on every keystroke and most keystrokes leave
// the script mid-edit.
type WorkflowValidateResult struct {
	Ok          bool                `json:"ok"`
	Error       string              `json:"error,omitempty"`
	Name        string              `json:"name,omitempty"`
	Description string              `json:"description,omitempty"`
	WhenToUse   string              `json:"whenToUse,omitempty"`
	Phases      []WorkflowPhaseInfo `json:"phases"`
}

func (b *bridge) workflowValidate(_ context.Context, args workflowValidateArgs) (WorkflowValidateResult, error) {
	meta, err := workflow.Validate(args.Script)
	res := WorkflowValidateResult{
		Ok:          err == nil,
		Name:        meta.Name,
		Description: meta.Description,
		WhenToUse:   meta.WhenToUse,
		Phases:      phaseInfos(meta),
	}
	if err != nil {
		res.Error = err.Error()
	}
	return res, nil
}

// WorkflowRunInfo is one past run's transcript directory. Runs are what makes
// resume possible — an edited script replays every unchanged call from the
// journal — so a client that cannot list them cannot offer the feature.
type WorkflowRunInfo struct {
	RunID string `json:"runId"`
	Dir   string `json:"dir"`
	// ModifiedAt is unix millis, matching how the app carries every other
	// timestamp across this boundary.
	ModifiedAt int64 `json:"modifiedAt"`
}

type workflowRunsArgs struct {
	workflowScopeArgs
	// Limit caps the reply, newest first. Zero means a sensible default
	// rather than everything: a busy project accumulates these quickly.
	Limit int `json:"limit"`
}

// WorkflowRunsResult lists recent run transcripts under the session store.
type WorkflowRunsResult struct {
	Runs []WorkflowRunInfo `json:"runs"`
}

// workflowRuns lists the runs of the sessions opened on the scope's project,
// like every other workflow method: a client showing "recent runs" for one
// repo must not offer to resume a run another repo's script produced.
func (b *bridge) workflowRuns(_ context.Context, args workflowRunsArgs) (WorkflowRunsResult, error) {
	cwd, err := b.resolveCwd(args.workflowScopeArgs)
	if err != nil {
		return WorkflowRunsResult{}, err
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 50
	}
	root := session.SessionsDir(b.opts.GlobalDir)
	var runs []WorkflowRunInfo
	sessions, err := os.ReadDir(root)
	if err != nil {
		// No sessions yet is not a failure; it is an empty list.
		return WorkflowRunsResult{Runs: runs}, nil
	}
	for _, se := range sessions {
		if !se.IsDir() || !b.sessionInProject(se.Name(), cwd) {
			continue
		}
		dir := filepath.Join(root, se.Name(), "workflows")
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			runs = append(runs, WorkflowRunInfo{
				RunID:      e.Name(),
				Dir:        filepath.Join(dir, e.Name()),
				ModifiedAt: info.ModTime().UnixMilli(),
			})
		}
	}
	// Newest first: the run a client wants is almost always the last one.
	for i := 0; i < len(runs); i++ {
		for j := i + 1; j < len(runs); j++ {
			if runs[j].ModifiedAt > runs[i].ModifiedAt {
				runs[i], runs[j] = runs[j], runs[i]
			}
		}
	}
	if len(runs) > limit {
		runs = runs[:limit]
	}
	return WorkflowRunsResult{Runs: runs}, nil
}

// sessionInProject reports whether the stored session id was opened on the
// project cwd. The metadata says so when it has been written (session/list
// matches the same way); a session still in its first turn has none yet, so
// an open session's own cwd counts too.
func (b *bridge) sessionInProject(id, cwd string) bool {
	if meta, err := session.LoadMetadata(b.opts.GlobalDir, id); err == nil {
		return meta.ProjectPath == cwd || meta.ProjectHash == session.ProjectHash(cwd)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[id]
	return ok && s.cwd == cwd
}
