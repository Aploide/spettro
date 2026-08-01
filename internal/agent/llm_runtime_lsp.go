package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"spettro/internal/diff"
	"spettro/internal/lsp"
)

// lspDiagnosticsWait bounds how long a file-write/file-edit result waits for
// fresh diagnostics. Short on purpose: post-edit diagnostics are a bonus and
// must never make edits feel slow or block the run when a server is wedged.
const lspDiagnosticsWait = 3 * time.Second

// withLSPDiagnostics appends fresh diagnostics for the just-written file to a
// mutating tool's result. Every failure path returns the result unchanged —
// no configured server, dead server, timeout — per the degrade-silently rule.
func (r *toolRuntime) withLSPDiagnostics(ctx context.Context, absPath, result string) string {
	m := lsp.ForWorkspace(r.cwd)
	if m == nil {
		return result
	}
	dctx, cancel := context.WithTimeout(ctx, lspDiagnosticsWait)
	defer cancel()
	diags, err := m.DiagnosticsForFile(dctx, absPath)
	if err != nil || strings.TrimSpace(diags) == "" {
		return result
	}
	return result + "\n\nlsp diagnostics:\n" + diags
}

func (r *toolRuntime) runLSPDiagnostics(ctx context.Context, rawArgs []byte) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("diagnostics args: %w", err)
	}
	m := lsp.ForWorkspace(r.cwd)
	if m == nil {
		return "", fmt.Errorf("no lsp server available (install one on PATH, e.g. gopls or typescript-language-server, or configure .spettro/lsp.json)")
	}
	if strings.TrimSpace(args.Path) == "" {
		out := m.WorkspaceDiagnostics()
		if strings.TrimSpace(out) == "" {
			return "no diagnostics (across files opened so far this session)", nil
		}
		return truncate(out, r.historyLimit("diagnostics")), nil
	}
	abs, rel, err := r.resolvePath(args.Path)
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
	return truncate(out, r.historyLimit("diagnostics")), nil
}

func (r *toolRuntime) runLSPReferences(ctx context.Context, rawArgs []byte) (string, error) {
	var args struct {
		Path      string `json:"path"`
		Symbol    string `json:"symbol"`
		Kind      string `json:"kind"`
		Line      int    `json:"line"`
		Character int    `json:"character"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("references args: %w", err)
	}
	if strings.TrimSpace(args.Path) == "" {
		return "", fmt.Errorf("references: path is required")
	}
	if strings.TrimSpace(args.Symbol) == "" && args.Line <= 0 {
		return "", fmt.Errorf("references: symbol or line is required")
	}
	switch args.Kind {
	case "", "references", "definition":
	default:
		return "", fmt.Errorf("references: kind must be \"references\" or \"definition\"")
	}
	m := lsp.ForWorkspace(r.cwd)
	if m == nil {
		return "", fmt.Errorf("no lsp server available (install one on PATH, e.g. gopls or typescript-language-server, or configure .spettro/lsp.json)")
	}
	abs, _, err := r.resolvePath(args.Path)
	if err != nil {
		return "", err
	}
	out, err := m.Lookup(ctx, abs, strings.TrimSpace(args.Symbol), args.Kind, args.Line, args.Character)
	if err != nil {
		return "", err
	}
	return truncate(out, r.historyLimit("references")), nil
}

func (r *toolRuntime) runLSPHover(ctx context.Context, rawArgs []byte) (string, error) {
	var args struct {
		Path      string `json:"path"`
		Symbol    string `json:"symbol"`
		Line      int    `json:"line"`
		Character int    `json:"character"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("hover args: %w", err)
	}
	if strings.TrimSpace(args.Path) == "" {
		return "", fmt.Errorf("hover: path is required")
	}
	if strings.TrimSpace(args.Symbol) == "" && args.Line <= 0 {
		return "", fmt.Errorf("hover: symbol or line is required")
	}
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
	return truncate(out, r.historyLimit("hover")), nil
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
	var applied []string
	for _, ch := range changes {
		if err := os.WriteFile(ch.Path, []byte(ch.New), 0o644); err != nil {
			return "", fmt.Errorf("rename-symbol: applied %d of %d files, then: %w", len(applied), len(changes), err)
		}
		applied = append(applied, ch.Rel)
		r.mu.Lock()
		r.readSet[ch.Rel] = struct{}{}
		r.mu.Unlock()
	}
	msg := fmt.Sprintf("renamed to %q in %d file(s):\n- %s", newName, len(applied), strings.Join(applied, "\n- "))
	return r.withLSPDiagnostics(ctx, abs, msg), nil
}

func (r *toolRuntime) runLSPRestart(rawArgs []byte) (string, error) {
	var args struct {
		Server string `json:"server"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("lsp-restart args: %w", err)
	}
	m := lsp.ForWorkspace(r.cwd)
	if m == nil {
		return "", fmt.Errorf("no lsp server available (install one on PATH, e.g. gopls or typescript-language-server, or configure .spettro/lsp.json)")
	}
	return m.Restart(strings.TrimSpace(args.Server)), nil
}
