// Package acp exposes Spettro as an Agent Client Protocol (ACP) agent so
// ACP-capable editors (Zed, Neovim plugins, JetBrains, ...) can drive it as
// an external coding agent over stdio JSON-RPC.
//
// See https://agentclientprotocol.com for the protocol specification. The
// wire layer is provided by github.com/coder/acp-go-sdk; this package only
// bridges protocol calls onto the existing agent.LLMAgent runtime.
package acp

import (
	"context"
	"log/slog"
	"os"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/provider"
)

// Options carries the process-wide state the ACP bridge shares with the other
// front-ends (TUI, headless): provider catalog, manifest, sandbox policy.
type Options struct {
	// CWD is the process working directory, used as the fallback session cwd
	// when a client creates a session without one.
	CWD string
	// GlobalDir is the user-global ~/.spettro directory (for session media).
	GlobalDir string
	Cfg       config.UserConfig
	Providers *provider.Manager
	Manifest  config.AgentManifest
	// SandboxState is the process-wide OS sandbox policy shared by every
	// session, mirroring headless mode. nil disables the sandbox feature.
	SandboxState *agent.SandboxState
}

// Serve runs the ACP agent on stdin/stdout until the client disconnects or
// ctx is cancelled. stdout is reserved for JSON-RPC; all diagnostics go to
// stderr.
func Serve(ctx context.Context, opts Options) error {
	bridge := newBridge(opts)
	conn := acpsdk.NewAgentSideConnection(bridge, os.Stdout, os.Stdin)
	conn.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	bridge.conn = conn

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-conn.Done():
		return nil
	}
}
