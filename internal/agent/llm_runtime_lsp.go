package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"spettro/internal/diff"
	"spettro/internal/lsp"
)

// lspDiagnosticsWait bounds how long a file-write/file-edit result waits for
// fresh diagnostics, server start included. Short on purpose: post-edit
// diagnostics are a bonus and must never make edits feel slow or block the
// run when a server is wedged. The server itself starts once per session in
// the background (warmed when a file is first read), so only the first edit
// of a session can find it still starting.
const lspDiagnosticsWait = 3 * time.Second

// withLSPDiagnostics appends the errors a language server reports for the
// just-written file to a mutating tool's result (see lsp.PostEditDiagnostics
// for the block's shape); also lists the other files the call wrote. The edit
// has already landed: no server for the file type, a crashed or slow server
// all leave the result as it was, bar a one-line note when the server could
// not answer in time.
func (r *toolRuntime) withLSPDiagnostics(ctx context.Context, absPath, result string, also ...string) string {
	m := lsp.ForWorkspace(r.cwd)
	if m == nil {
		return result
	}
	dctx, cancel := context.WithTimeout(ctx, lspDiagnosticsWait)
	defer cancel()
	if block := m.PostEditDiagnostics(dctx, absPath, also...); block != "" {
		return result + "\n\n" + block
	}
	return result
}

// lspTools are the tools that use a language server: the edits get post-edit
// diagnostics, the rest query it.
var lspTools = []string{"file-write", "file-edit", "multi-edit", "rename-symbol", "lsp"}

// usesLanguageServer reports whether an agent with these tools would ever use
// a language server. A read-only agent would not, and a server started for it
// indexes the whole workspace for nothing.
func usesLanguageServer(allowed map[string]struct{}) bool {
	for _, t := range lspTools {
		if _, ok := allowed[t]; ok {
			return true
		}
	}
	return false
}

// warmLSP starts, in the background, the language server for a file the model
// is looking at, so it is running by the time the file is edited and the
// first edit's diagnostics do not pay for the server's start. Only agents
// that can use the server warm it.
func (r *toolRuntime) warmLSP(absPath string) {
	if !r.lspWarm {
		return
	}
	if m := lsp.ForWorkspace(r.cwd); m != nil {
		m.Warm(absPath)
	}
}

// noLSPServer is the error an lsp call gets when no language server is
// configured or installed for the workspace.
const noLSPServer = "no lsp server available (install one on PATH, e.g. gopls or typescript-language-server, or configure .spettro/lsp.json)"

// lspOps lists the lsp tool's operations, in the order errors name them.
const lspOps = "diagnostics, references, definition, hover or restart"

// lspCallOp returns the op an lsp call asks for, normalized; "" when the
// arguments do not name one.
func lspCallOp(raw []byte) string {
	var args struct {
		Op string `json:"op"`
	}
	if json.Unmarshal(raw, &args) != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(args.Op))
}

// lspPosArgs are the arguments of the ops that look at one position
// (references, definition, hover).
type lspPosArgs struct {
	Path      string `json:"path"`
	Symbol    string `json:"symbol"`
	Line      int    `json:"line"`
	Character int    `json:"character"`
	// Kind is the references tool's lookup mode, still honoured by op
	// references: "definition" makes it op definition.
	Kind string `json:"kind"`
}

// runLSP is the lsp tool: the read-only language-server operations. Writing
// (rename-symbol) is a tool of its own, with its own approval.
//
// Each op decodes only its own arguments and ignores the rest, as the tool
// it replaced did, so a stray argument of another op never fails the call.
// calledAs is the retired tool the model called (the lsp call's CalledAs, or
// the tool's own name when it was left unfolded); errors are then worded as
// that tool's were.
func (r *toolRuntime) runLSP(ctx context.Context, rawArgs []byte, calledAs string) (string, error) {
	var head struct {
		Op string `json:"op"`
	}
	if err := decodeJSONStrict(rawArgs, &head); err != nil {
		return "", fmt.Errorf("%s args: %w", lspLabel(calledAs, ""), err)
	}
	op := strings.ToLower(strings.TrimSpace(head.Op))
	label := lspLabel(calledAs, op)
	decode := func(target any) error {
		if err := decodeJSONStrict(rawArgs, target); err != nil {
			return fmt.Errorf("%s args: %w", lspLabel(calledAs, ""), err)
		}
		return nil
	}
	switch op {
	case "diagnostics":
		var args struct {
			Path string `json:"path"`
		}
		if err := decode(&args); err != nil {
			return "", err
		}
		return r.lspDiagnostics(ctx, args.Path)
	case "references", "definition", "hover":
		var args lspPosArgs
		if err := decode(&args); err != nil {
			return "", err
		}
		if strings.TrimSpace(args.Path) == "" {
			return "", fmt.Errorf("%s: path is required", label)
		}
		if strings.TrimSpace(args.Symbol) == "" && args.Line <= 0 {
			return "", fmt.Errorf("%s: symbol or line is required", label)
		}
		if op == "hover" {
			return r.lspHover(ctx, args)
		}
		if op == "references" {
			switch args.Kind {
			case "", "references":
			case "definition":
				op = "definition"
			default:
				if calledAs != "" {
					return "", fmt.Errorf("%s: kind must be \"references\" or \"definition\"", label)
				}
				return "", fmt.Errorf("%s: kind must be \"references\" or \"definition\" (or use op \"definition\")", label)
			}
		}
		return r.lspLookup(ctx, op, args)
	case "restart":
		var args struct {
			Server string `json:"server"`
		}
		if err := decode(&args); err != nil {
			return "", err
		}
		return r.lspRestart(args.Server)
	case "":
		return "", fmt.Errorf("lsp: op is required (%s)", lspOps)
	default:
		return "", fmt.Errorf("lsp: unknown op %q (want %s)", head.Op, lspOps)
	}
}

// lspLabel names an lsp call in its errors: the retired tool the model
// called, else "lsp <op>" (or "lsp" alone).
func lspLabel(calledAs, op string) string {
	switch {
	case calledAs != "":
		return calledAs
	case op == "":
		return "lsp"
	}
	return "lsp " + op
}

// lspDiagnostics is op diagnostics (the former diagnostics tool): one file's
// diagnostics, or with no path everything published so far this session.
func (r *toolRuntime) lspDiagnostics(ctx context.Context, path string) (string, error) {
	m := lsp.ForWorkspace(r.cwd)
	if m == nil {
		return "", errors.New(noLSPServer)
	}
	if strings.TrimSpace(path) == "" {
		out := m.WorkspaceDiagnostics()
		if strings.TrimSpace(out) == "" {
			return "no diagnostics (across files opened so far this session)", nil
		}
		return truncate(out, r.historyLimit("lsp")), nil
	}
	abs, rel, err := r.resolvePath(path)
	if err != nil {
		return "", err
	}
	out, err := m.DiagnosticsForFile(ctx, abs)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Sprintf("no diagnostics for %s", rel), nil
	}
	return truncate(out, r.historyLimit("lsp")), nil
}

// lspLookup is ops references and definition (the former references tool,
// kind "references" or "definition").
func (r *toolRuntime) lspLookup(ctx context.Context, op string, args lspPosArgs) (string, error) {
	m := lsp.ForWorkspace(r.cwd)
	if m == nil {
		return "", errors.New(noLSPServer)
	}
	abs, _, err := r.resolvePath(args.Path)
	if err != nil {
		return "", err
	}
	out, err := m.Lookup(ctx, abs, strings.TrimSpace(args.Symbol), op, args.Line, args.Character)
	if err != nil {
		return "", err
	}
	return truncate(out, r.historyLimit("lsp")), nil
}

// lspHover is op hover (the former hover tool): type signature and docs.
func (r *toolRuntime) lspHover(ctx context.Context, args lspPosArgs) (string, error) {
	m := lsp.ForWorkspace(r.cwd)
	if m == nil {
		return "", fmt.Errorf("no language server for this file type (install one on PATH, e.g. gopls or typescript-language-server, or configure .spettro/lsp.json)")
	}
	abs, rel, err := r.resolvePath(args.Path)
	if err != nil {
		return "", err
	}
	out, err := m.Hover(ctx, abs, strings.TrimSpace(args.Symbol), args.Line, args.Character)
	if err != nil {
		return "", err
	}
	if out == "" {
		return fmt.Sprintf("no hover info for that position in %s", rel), nil
	}
	return truncate(out, r.historyLimit("lsp")), nil
}

// lspRestart is op restart (the former lsp-restart tool): restart one server,
// or all of them, and reload .spettro/lsp.json.
func (r *toolRuntime) lspRestart(server string) (string, error) {
	m := lsp.ForWorkspace(r.cwd)
	if m == nil {
		return "", errors.New(noLSPServer)
	}
	return m.Restart(strings.TrimSpace(server)), nil
}

func (r *toolRuntime) runLSPRename(ctx context.Context, rawArgs []byte) (string, error) {
	var args struct {
		Path      string `json:"path"`
		Symbol    string `json:"symbol"`
		Line      int    `json:"line"`
		Character int    `json:"character"`
		NewName   string `json:"new_name"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("rename-symbol args: %w", err)
	}
	if strings.TrimSpace(args.Path) == "" {
		return "", fmt.Errorf("rename-symbol: path is required")
	}
	if strings.TrimSpace(args.Symbol) == "" && args.Line <= 0 {
		return "", fmt.Errorf("rename-symbol: symbol or line is required")
	}
	newName := strings.TrimSpace(args.NewName)
	if newName == "" {
		return "", fmt.Errorf("rename-symbol: new_name is required")
	}
	m := lsp.ForWorkspace(r.cwd)
	if m == nil {
		return "", fmt.Errorf("no language server for this file type (install one on PATH, e.g. gopls or typescript-language-server, or configure .spettro/lsp.json)")
	}
	abs, rel, err := r.resolvePath(args.Path)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	_, alreadyRead := r.readSet[rel]
	r.mu.Unlock()
	if !alreadyRead {
		return "", fmt.Errorf("refusing rename: read %q first", rel)
	}
	changes, err := m.RenameEdits(ctx, abs, strings.TrimSpace(args.Symbol), args.Line, args.Character, newName)
	if err != nil {
		return "", err
	}
	var combined strings.Builder
	for _, ch := range changes {
		// relPath falls back to the absolute path for files outside the
		// workspace root; a rename must never write outside the workspace.
		if filepath.IsAbs(ch.Rel) {
			return "", fmt.Errorf("rename-symbol: refusing to edit %s outside the workspace", ch.Rel)
		}
		combined.WriteString(diff.Unified(ch.Rel, ch.Old, ch.New))
	}
	// One approval covers the whole workspace edit: the combined diff shows
	// every file, and applying only part of a rename would break the build.
	if err := r.authorizeWriteAccess(ctx, "rename-symbol", rel, combined.String()); err != nil {
		return "", err
	}
	var applied, written []string
	for _, ch := range changes {
		if err := os.WriteFile(ch.Path, []byte(ch.New), 0o644); err != nil {
			return "", fmt.Errorf("rename-symbol: applied %d of %d files, then: %w", len(applied), len(changes), err)
		}
		applied = append(applied, ch.Rel)
		written = append(written, ch.Path)
		r.mu.Lock()
		r.readSet[ch.Rel] = struct{}{}
		r.mu.Unlock()
		r.recordFileStamp(ch.Rel, []byte(ch.New))
		recordFileChange(ctx, ch.Path, ch.Old, ch.New, false)
	}
	msg := fmt.Sprintf("renamed to %q in %d file(s):\n- %s", newName, len(applied), strings.Join(applied, "\n- "))
	return r.withLSPDiagnostics(ctx, abs, msg, written...), nil
}
