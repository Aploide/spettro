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
	"io"
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

	// In, Out and Log carry the newline-delimited JSON-RPC stream and the
	// diagnostics channel. A nil value means the process default — os.Stdin,
	// os.Stdout and os.Stderr respectively — which is exactly what the
	// `spettro --acp` CLI has always used. Hosts that embed the agent
	// in-process (the iOS bridge in spettro/mobile) pass in-memory pipes
	// instead so nothing touches the real stdio of the app.
	In  io.Reader
	Out io.Writer
	Log io.Writer

	// Ready, if non-nil, is called exactly once on Serve's own goroutine after
	// the connection is wired and before Serve blocks — i.e. the moment the
	// agent starts accepting input. Stdio callers do not need it (the client
	// owns the pipe and is already writing). An in-process host does: it lets
	// Start report bootstrap failures synchronously, and the happens-before
	// edge it publishes is what keeps a host-driven shutdown from racing the
	// SDK's unsynchronized Connection.logger field (see the note in
	// spettro/mobile).
	Ready func()
}

// Serve runs the ACP agent on opts.In/opts.Out until the client disconnects or
// ctx is cancelled. Out is reserved for JSON-RPC; all diagnostics go to Log.
// With the zero values that is stdin/stdout for the protocol and stderr for
// diagnostics.
func Serve(ctx context.Context, opts Options) error {
	in := io.Reader(os.Stdin)
	if opts.In != nil {
		in = opts.In
	}
	out := io.Writer(os.Stdout)
	if opts.Out != nil {
		out = opts.Out
	}
	logOut := io.Writer(os.Stderr)
	if opts.Log != nil {
		logOut = opts.Log
	}

	bridge := newBridge(opts)
	conn := acpsdk.NewAgentSideConnection(bridge, out, in)
	conn.SetLogger(slog.New(slog.NewTextHandler(logOut, nil)))
	bridge.conn = conn

	if opts.Ready != nil {
		opts.Ready()
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-conn.Done():
		return nil
	}
}
