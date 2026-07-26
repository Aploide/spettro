// Package spettromobile is the gomobile bridge that lets a mobile app drive
// Spettro in-process instead of spawning `spettro --acp` as a subprocess —
// which iOS does not permit.
//
// It runs the exact same bootstrap and ACP serve loop as the desktop CLI (see
// internal/acpserve) but over in-memory pipes, and exposes it as a
// line-oriented engine: the host sends one complete JSON-RPC message per
// Send, and receives one complete message per EngineHandler.OnLine. Newline
// framing is owned entirely by this package, mirroring the Swift ACPTransport
// contract on the app side.
//
// The exported surface is deliberately restricted to what gomobile can bind:
// string, int64, []byte, exported struct pointers, and interfaces made of
// those. No channels, no maps, no error values other than the trailing
// `error` return gomobile understands.
//
// Bind with `make ios-framework` in the repository root.
package spettromobile

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"spettro/internal/acpserve"
)

// maxLineBytes caps a single JSON-RPC message. It matches the limit the ACP
// SDK applies to its own reader, so neither direction can be the bottleneck.
const maxLineBytes = 10 * 1024 * 1024

// EngineHandler receives everything the engine produces. It is implemented on
// the host side (in Swift, by an NSObject subclass conforming to the generated
// protocol).
//
// Every method is called from a background goroutine, never from the caller of
// Start/Send/Stop, and OnLine calls are serialized in arrival order. A handler
// that blocks applies backpressure to the engine, so hop to your own queue
// rather than doing slow work inline.
type EngineHandler interface {
	// OnLine delivers one complete JSON-RPC message with the trailing
	// newline stripped. The slice is owned by the callee.
	OnLine(line []byte)
	// OnLog delivers one diagnostic line — the content that goes to stderr
	// in stdio mode.
	OnLog(line string)
	// OnExit reports that the engine has stopped and will produce no further
	// callbacks. 0 means a clean Stop or client disconnect; 1 means the
	// engine failed (the reason was already delivered through OnLog).
	OnExit(code int64)
}

// Exit codes reported through EngineHandler.OnExit.
const (
	exitClean  = 0
	exitFailed = 1
)

// Engine is one running ACP agent. Create it with Start; it cannot be reused
// after Stop, but a new Start in the same process is fine and is what the app
// does when it restarts the agent after a teardown.
type Engine struct {
	// inW feeds the ACP server's reader. Closing it is the graceful shutdown
	// signal: the server sees EOF, winds down its own goroutines and returns.
	inW *io.PipeWriter
	// outW is the ACP server's writer; the reader goroutine turns it into
	// OnLine callbacks.
	outW *io.PipeWriter

	log *lineWriter

	cancel context.CancelFunc

	stopOnce sync.Once
	// started closes once the ACP server is accepting input; failed closes
	// instead if the bootstrap died before that. Exactly one of the two
	// closes, which is what makes Start's return value meaningful.
	started chan struct{}
	failed  chan struct{}
	bootErr error
	exited  chan struct{}

	sendMu  sync.Mutex
	stopped bool
}

// SetHome points $HOME at dir for everything the engine does afterwards:
// global config, keys.enc, master.key and the agent's own storage all hang off
// it. Call it before Start; it is idempotent and safe to repeat.
//
// The host has to tell us, because the process default is wrong on iOS and
// wrong in a way that only shows on real hardware. $HOME there is the app's
// data container, whose root is not writable — only its standard children
// (Documents, Library, tmp) are — so creating $HOME/.spettro fails with EPERM
// on a device while succeeding on the Simulator, where the container is an
// ordinary directory on the Mac.
//
// This cannot be done from the host's own setenv: Go snapshots the environment
// when the runtime initializes, which for a statically linked framework is
// before any application code runs. Only os.Setenv updates the copy that
// os.UserHomeDir reads.
func SetHome(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("spettro: home directory is required")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("spettro: home %q is not usable: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("spettro: home %q is not a directory", dir)
	}
	return os.Setenv("HOME", dir)
}

// Start runs the full ACP bootstrap and serve loop on background goroutines
// and returns once the engine is accepting input. A bootstrap failure (bad
// config, unreadable storage) is returned as an error and no handler callback
// ever fires — an engine that never started never exits. Once Start has
// returned successfully, exactly one OnExit is guaranteed.
//
// Start does block for the length of the bootstrap, which reads config and may
// probe local model endpoints, so call it off the UI thread.
//
// cwd is the project directory. Per-project state goes to <cwd>/.spettro;
// global state (config, keys.enc, master.key) goes to $HOME/.spettro — call
// SetHome first, because on iOS the default is a directory nothing may write to.
func Start(cwd string, handler EngineHandler) (*Engine, error) {
	if handler == nil {
		return nil, errors.New("spettro: an EngineHandler is required")
	}
	if strings.TrimSpace(cwd) == "" {
		return nil, errors.New("spettro: cwd is required")
	}
	// Fail fast and synchronously on the one mistake the host is likely to
	// make; everything else is reported through the handler.
	if info, err := os.Stat(cwd); err != nil {
		return nil, fmt.Errorf("spettro: cwd %q is not usable: %w", cwd, err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("spettro: cwd %q is not a directory", cwd)
	}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())

	e := &Engine{
		inW:     inW,
		outW:    outW,
		log:     newLineWriter(handler.OnLog),
		cancel:  cancel,
		started: make(chan struct{}),
		failed:  make(chan struct{}),
		exited:  make(chan struct{}),
	}

	var readerDone sync.WaitGroup
	readerDone.Add(1)
	go func() {
		defer readerDone.Done()
		scanner := bufio.NewScanner(outR)
		scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
		for scanner.Scan() {
			raw := scanner.Bytes()
			if len(bytes.TrimSpace(raw)) == 0 {
				continue
			}
			// scanner.Bytes() aliases the scanner's buffer, and the handler
			// may be Swift holding on to the Data; hand it a copy.
			line := make([]byte, len(raw))
			copy(line, raw)
			handler.OnLine(line)
		}
	}()

	go func() {
		err := acpserve.Run(ctx, acpserve.Options{
			CWD:   cwd,
			In:    inR,
			Out:   outW,
			Log:   e.log,
			Ready: func() { close(e.started) },
		})

		// Did the bootstrap get far enough to serve? If not, the failure
		// belongs to Start's return value, not to the handler.
		serving := false
		select {
		case <-e.started:
			serving = true
		default:
		}

		code := int64(exitClean)
		if err != nil && !errors.Is(err, context.Canceled) {
			if serving {
				e.log.emitLine("spettro: " + err.Error())
			}
			code = exitFailed
		}

		// Closing outW both ends the reader goroutine and unblocks any late
		// writer inside the ACP server (a deferred command announcement, a
		// response from a request that was in flight when the client went
		// away) with ErrClosedPipe instead of leaving it parked forever.
		_ = outW.Close()
		_ = inR.Close()
		readerDone.Wait()

		// Release anything still holding the serve context, then stop
		// emitting: no callback may follow OnExit.
		cancel()
		e.log.close()

		if !serving {
			e.bootErr = err
			if e.bootErr == nil {
				// Unreachable: acpserve.Run only returns nil once it has
				// served, which implies Ready fired. Guard anyway so Start
				// can never hand back (nil, nil).
				e.bootErr = errors.New("spettro: engine stopped before it started")
			}
			close(e.failed)
			return
		}
		close(e.exited)
		handler.OnExit(code)
	}()

	select {
	case <-e.started:
		return e, nil
	case <-e.failed:
		return nil, e.bootErr
	}
}

// Send writes one complete JSON-RPC message to the engine. line must not
// contain a newline; the framing newline is added here. Send is safe to call
// from any thread and never interleaves two messages.
func (e *Engine) Send(line []byte) error {
	if len(bytes.TrimSpace(line)) == 0 {
		return errors.New("spettro: refusing to send an empty message")
	}
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		return errors.New("spettro: message contains a newline; send one message per call")
	}

	e.sendMu.Lock()
	defer e.sendMu.Unlock()
	if e.stopped {
		return errors.New("spettro: engine is stopped")
	}
	// One Write per message: io.Pipe serializes whole Write calls, so two
	// concurrent senders can never split each other's frame.
	framed := make([]byte, 0, len(line)+1)
	framed = append(framed, line...)
	framed = append(framed, '\n')
	if _, err := e.inW.Write(framed); err != nil {
		return fmt.Errorf("spettro: send failed: %w", err)
	}
	return nil
}

// Stop shuts the engine down. It is idempotent, safe to call from any thread,
// and returns immediately — shutdown completes on the engine's own goroutines
// and is announced by EngineHandler.OnExit. It must not block, because the
// host may call it from the same thread a handler callback is waiting on.
func (e *Engine) Stop() {
	e.stopOnce.Do(func() {
		e.sendMu.Lock()
		e.stopped = true
		e.sendMu.Unlock()
		// EOF on the server's reader is the protocol-level "client hung up":
		// the SDK cancels in-flight requests, drains notifications and closes
		// the connection, which is what lets acpserve.Run return.
		_ = e.inW.Close()
	})
}

// waitForExit blocks until OnExit has been dispatched. It is unexported so it
// stays out of the gomobile surface (the host observes exit through the
// handler); tests use it to make shutdown deterministic.
func (e *Engine) waitForExit(ctx context.Context) error {
	select {
	case <-e.exited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// lineWriter turns an arbitrary byte stream into whole-line callbacks. The
// ACP SDK's slog handler writes one record per Write, but nothing in the
// contract guarantees that, so partial lines are buffered until their newline
// arrives.
type lineWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	emit   func(string)
	closed bool
}

func newLineWriter(emit func(string)) *lineWriter {
	return &lineWriter{emit: emit}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		// Swallow late diagnostics rather than erroring: a write failure on
		// the log stream would look like a protocol fault to the SDK.
		return len(p), nil
	}
	w.buf.Write(p)
	for {
		b := w.buf.Bytes()
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			break
		}
		line := string(bytes.TrimRight(b[:i], "\r"))
		w.buf.Next(i + 1)
		w.emit(line)
	}
	return len(p), nil
}

// emitLine delivers one already-complete diagnostic line.
func (w *lineWriter) emitLine(line string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.emit(line)
}

// close flushes a trailing partial line and stops all further emission.
func (w *lineWriter) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	if rest := strings.TrimRight(w.buf.String(), "\r\n"); rest != "" {
		w.emit(rest)
	}
	w.buf.Reset()
	w.closed = true
}
