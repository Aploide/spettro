// Package lsptest is a scripted language server for tests. A test binary
// serves it from TestMain (see MaybeServe), and the test points Spettro's LSP
// config at its own executable, so post-edit diagnostics can be exercised
// deterministically — slow starts, staged publishes, a server that never
// answers — on machines without any real language server installed.
//
// Diagnostics follow the document text: every line containing "ERR" gets an
// error, every line containing "WARN" a warning, and a line
// "BREAK <relpath>" publishes two errors for that other workspace file.
package lsptest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// EnvVar carries the JSON-encoded Options to the server process. Its presence
// is what switches a test binary into server mode.
const EnvVar = "SPETTRO_FAKE_LSP"

// Options scripts the fake server's behaviour.
type Options struct {
	// InitDelay delays the initialize response, like a cold server.
	InitDelay time.Duration `json:"init_delay,omitempty"`
	// Silent never publishes diagnostics.
	Silent bool `json:"silent,omitempty"`
	// SaveOnly publishes only on didSave (like on-save checkers); the server
	// then advertises save support.
	SaveOnly bool `json:"save_only,omitempty"`
	// Staged publishes an empty set first and the real one Staged later,
	// like servers that send syntactic before semantic results.
	Staged time.Duration `json:"staged,omitempty"`
	// StopReading stops consuming stdin once initialized, like a wedged
	// server whose input pipe then fills up.
	StopReading bool `json:"stop_reading,omitempty"`
	// StartLog, when set, gets one line appended per server start.
	StartLog string `json:"start_log,omitempty"`
}

// Env returns the environment entry that makes a test binary serve opts.
func Env(opts Options) string {
	raw, _ := json.Marshal(opts)
	return EnvVar + "=" + string(raw)
}

// Args are the arguments to launch the test binary with as a server: a -run
// pattern matching nothing, as a guard should the environment be lost.
var Args = []string{"-test.run=^$"}

// MaybeServe turns the process into the fake server when EnvVar is set and
// never returns in that case. Call it first thing in TestMain.
func MaybeServe() {
	raw, ok := os.LookupEnv(EnvVar)
	if !ok {
		return
	}
	var opts Options
	if err := json.Unmarshal([]byte(raw), &opts); err != nil {
		fmt.Fprintf(os.Stderr, "lsptest: bad %s: %v\n", EnvVar, err)
		os.Exit(2)
	}
	Serve(os.Stdin, os.Stdout, opts)
	os.Exit(0)
}

type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

type server struct {
	opts Options
	out  io.Writer
	mu   sync.Mutex
	root string
	docs map[string]string // uri → text
}

// Serve runs the fake server until its input closes or it is told to exit.
func Serve(in io.Reader, out io.Writer, opts Options) {
	if opts.StartLog != "" {
		if f, err := os.OpenFile(opts.StartLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintf(f, "start %d\n", os.Getpid())
			f.Close()
		}
	}
	s := &server{opts: opts, out: out, docs: map[string]string{}}
	r := bufio.NewReader(in)
	for {
		msg, err := readMessage(r)
		if err != nil {
			return
		}
		switch msg.Method {
		case "initialize":
			var p struct {
				RootURI string `json:"rootUri"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			s.root = uriToPath(p.RootURI)
			time.Sleep(opts.InitDelay)
			tds := map[string]any{"openClose": true, "change": 1}
			if opts.SaveOnly {
				tds["save"] = map[string]any{"includeText": false}
			}
			s.reply(msg.ID, map[string]any{"capabilities": map[string]any{"textDocumentSync": tds}})
		case "initialized":
			if opts.StopReading {
				// Never read again, so the client's writes back up. (A bare
				// select{} would trip the runtime's deadlock detector.)
				time.Sleep(time.Hour)
			}
		case "textDocument/didOpen":
			var p struct {
				TextDocument struct {
					URI  string `json:"uri"`
					Text string `json:"text"`
				} `json:"textDocument"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			s.changed(p.TextDocument.URI, p.TextDocument.Text, false)
		case "textDocument/didChange":
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
				ContentChanges []struct {
					Text string `json:"text"`
				} `json:"contentChanges"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			if n := len(p.ContentChanges); n > 0 {
				s.changed(p.TextDocument.URI, p.ContentChanges[n-1].Text, false)
			}
		case "textDocument/didClose":
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			s.mu.Lock()
			delete(s.docs, p.TextDocument.URI)
			s.mu.Unlock()
			// like real servers: a closed document has nothing to report
			s.publish(p.TextDocument.URI, []map[string]any{})
		case "textDocument/didSave":
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			s.mu.Lock()
			text := s.docs[p.TextDocument.URI]
			s.mu.Unlock()
			s.changed(p.TextDocument.URI, text, true)
		case "shutdown":
			s.reply(msg.ID, nil)
		case "exit":
			return
		default:
			if len(msg.ID) > 0 {
				s.reply(msg.ID, nil)
			}
		}
	}
}

func (s *server) changed(uri, text string, saved bool) {
	s.mu.Lock()
	s.docs[uri] = text
	s.mu.Unlock()
	if s.opts.Silent || s.opts.SaveOnly != saved {
		return
	}
	if s.opts.Staged > 0 {
		s.publish(uri, []map[string]any{})
		go func() {
			time.Sleep(s.opts.Staged)
			s.publishFor(uri, text)
		}()
		return
	}
	s.publishFor(uri, text)
}

// publishFor publishes the diagnostics text implies, for its own document and
// for any file it names on a BREAK line.
func (s *server) publishFor(uri, text string) {
	var diags []map[string]any
	for i, line := range strings.Split(text, "\n") {
		switch {
		case strings.Contains(line, "ERR"):
			diags = append(diags, diag(i, 1, "bad thing: "+strings.TrimSpace(line)))
		case strings.Contains(line, "WARN"):
			diags = append(diags, diag(i, 2, "questionable: "+strings.TrimSpace(line)))
		}
		if rel, ok := strings.CutPrefix(strings.TrimSpace(line), "BREAK "); ok {
			other := filepath.Join(s.root, filepath.FromSlash(rel))
			s.publish(pathToURI(other), []map[string]any{diag(0, 1, "broken by edit"), diag(1, 1, "also broken")})
		}
	}
	if diags == nil {
		diags = []map[string]any{}
	}
	s.publish(uri, diags)
}

func diag(line, severity int, msg string) map[string]any {
	pos := map[string]any{"line": line, "character": 0}
	return map[string]any{
		"range":    map[string]any{"start": pos, "end": pos},
		"severity": severity,
		"source":   "fake",
		"message":  msg,
	}
}

func (s *server) publish(uri string, diags []map[string]any) {
	s.send(map[string]any{"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics",
		"params": map[string]any{"uri": uri, "diagnostics": diags}})
}

func (s *server) reply(id json.RawMessage, result any) {
	s.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *server) send(msg any) {
	raw, _ := json.Marshal(msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.out, "Content-Length: %d\r\n\r\n%s", len(raw), raw)
}

func readMessage(r *bufio.Reader) (message, error) {
	n := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return message{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			n, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return message{}, err
	}
	var msg message
	err := json.Unmarshal(body, &msg)
	return msg, err
}

// uriToPath and pathToURI mirror the client's conversions closely enough for
// the absolute paths tests use (Windows drive paths get the leading slash).
func uriToPath(uri string) string {
	p := strings.TrimPrefix(uri, "file://")
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return filepath.FromSlash(p)
}

func pathToURI(path string) string {
	p := filepath.ToSlash(path)
	if len(p) >= 2 && p[1] == ':' {
		p = "/" + p
	}
	return "file://" + p
}
