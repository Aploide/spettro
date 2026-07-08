package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

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
		return "", fmt.Errorf("no lsp servers configured (add .spettro/lsp.json with a servers entry)")
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
		return "", fmt.Errorf("no lsp servers configured (add .spettro/lsp.json with a servers entry)")
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

func (r *toolRuntime) runLSPRestart(rawArgs []byte) (string, error) {
	var args struct {
		Server string `json:"server"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("lsp-restart args: %w", err)
	}
	m := lsp.ForWorkspace(r.cwd)
	if m == nil {
		return "", fmt.Errorf("no lsp servers configured (add .spettro/lsp.json with a servers entry)")
	}
	return m.Restart(strings.TrimSpace(args.Server)), nil
}
