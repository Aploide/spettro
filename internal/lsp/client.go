package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Diagnostic is the subset of the LSP diagnostic payload the agent needs.
type Diagnostic struct {
	Range struct {
		Start Position `json:"start"`
		End   Position `json:"end"`
	} `json:"range"`
	Severity int    `json:"severity"`
	Source   string `json:"source"`
	Message  string `json:"message"`
}

type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type Location struct {
	URI   string `json:"uri"`
	Range struct {
		Start Position `json:"start"`
		End   Position `json:"end"`
	} `json:"range"`
}

// TextEdit is one replacement inside a document, as produced by a rename's
// WorkspaceEdit.
type TextEdit struct {
	Range struct {
		Start Position `json:"start"`
		End   Position `json:"end"`
	} `json:"range"`
	NewText string `json:"newText"`
}

func severityLabel(s int) string {
	switch s {
	case 1:
		return "error"
	case 2:
		return "warning"
	case 3:
		return "info"
	case 4:
		return "hint"
	}
	return "diagnostic"
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Client is a minimal LSP client speaking JSON-RPC over the server's stdio.
// All methods are best-effort: a dead or wedged server surfaces as errors that
// callers are expected to swallow (the agent degrades to no-LSP behavior).
type Client struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex

	pendMu  sync.Mutex
	nextID  int64
	pending map[int64]chan rpcMessage

	// diagnostics store: uri → latest published set, with a generation counter
	// so callers can wait for a publish that happened after their change.
	diagMu   sync.Mutex
	diagCond *sync.Cond
	diags    map[string][]Diagnostic
	diagGen  map[string]int

	openMu   sync.Mutex
	openDocs map[string]int // uri → version

	closed   chan struct{}
	closeErr error
}

func fileURI(path string) string {
	return "file://" + filepath.ToSlash(path)
}

func uriToPath(uri string) string {
	p := strings.TrimPrefix(uri, "file://")
	return filepath.FromSlash(p)
}

// startClient spawns the server process, wires the reader loop, and completes
// the initialize handshake. The passed context bounds only the handshake.
func startClient(ctx context.Context, root, command string, args []string) (*Client, error) {
	cmd := exec.Command(command, args...)
	cmd.Dir = root
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &Client{
		cmd:      cmd,
		stdin:    stdin,
		pending:  map[int64]chan rpcMessage{},
		diags:    map[string][]Diagnostic{},
		diagGen:  map[string]int{},
		openDocs: map[string]int{},
		closed:   make(chan struct{}),
	}
	c.diagCond = sync.NewCond(&c.diagMu)
	go c.readLoop(stdout)
	go func() {
		err := cmd.Wait()
		c.diagMu.Lock()
		c.closeErr = fmt.Errorf("lsp server exited: %v", err)
		c.diagMu.Unlock()
		select {
		case <-c.closed:
		default:
			close(c.closed)
		}
		c.diagCond.Broadcast()
		c.failPending()
	}()

	initParams := map[string]any{
		"processId": nil,
		"rootUri":   fileURI(root),
		"workspaceFolders": []map[string]any{
			{"uri": fileURI(root), "name": filepath.Base(root)},
		},
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"publishDiagnostics": map[string]any{},
				"synchronization":    map[string]any{"didSave": true},
				"hover":              map[string]any{"contentFormat": []string{"plaintext", "markdown"}},
				"rename":             map[string]any{},
			},
			"workspace": map[string]any{},
		},
	}
	var initResult json.RawMessage
	if err := c.call(ctx, "initialize", initParams, &initResult); err != nil {
		c.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if err := c.notify("initialized", map[string]any{}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Close terminates the server process. Safe to call multiple times.
func (c *Client) Close() {
	select {
	case <-c.closed:
	default:
		_ = c.notify("exit", nil)
	}
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

func (c *Client) alive() bool {
	select {
	case <-c.closed:
		return false
	default:
		return true
	}
}

func (c *Client) failPending() {
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
}

func (c *Client) readLoop(stdout io.Reader) {
	r := bufio.NewReader(stdout)
	for {
		contentLen := 0
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
				n, err := strconv.Atoi(strings.TrimSpace(v))
				if err == nil {
					contentLen = n
				}
			}
		}
		if contentLen <= 0 {
			continue
		}
		body := make([]byte, contentLen)
		if _, err := io.ReadFull(r, body); err != nil {
			return
		}
		var msg rpcMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			continue
		}
		c.dispatch(msg)
	}
}

func (c *Client) dispatch(msg rpcMessage) {
	switch {
	case msg.Method != "" && len(msg.ID) > 0:
		// Server→client request: answer with a null result so servers that
		// require a response (workspace/configuration, registerCapability,
		// workDoneProgress/create, ...) don't stall.
		var result any
		if msg.Method == "workspace/configuration" {
			var params struct {
				Items []json.RawMessage `json:"items"`
			}
			_ = json.Unmarshal(msg.Params, &params)
			result = make([]any, len(params.Items))
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result}
		raw, _ := json.Marshal(resp)
		_ = c.write(raw)
	case msg.Method == "textDocument/publishDiagnostics":
		var params struct {
			URI         string       `json:"uri"`
			Diagnostics []Diagnostic `json:"diagnostics"`
		}
		if json.Unmarshal(msg.Params, &params) != nil {
			return
		}
		c.diagMu.Lock()
		c.diags[params.URI] = params.Diagnostics
		c.diagGen[params.URI]++
		c.diagMu.Unlock()
		c.diagCond.Broadcast()
	case msg.Method != "":
		// other notification: ignore
	default:
		// response to one of our requests
		var id int64
		if json.Unmarshal(msg.ID, &id) != nil {
			return
		}
		c.pendMu.Lock()
		ch, ok := c.pending[id]
		if ok {
			delete(c.pending, id)
		}
		c.pendMu.Unlock()
		if ok {
			ch <- msg
		}
	}
}

func (c *Client) write(payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n", len(payload)); err != nil {
		return err
	}
	_, err := c.stdin.Write(payload)
	return err
}

func (c *Client) notify(method string, params any) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return c.write(raw)
}

func (c *Client) call(ctx context.Context, method string, params, result any) error {
	if !c.alive() {
		return fmt.Errorf("lsp server not running")
	}
	c.pendMu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan rpcMessage, 1)
	c.pending[id] = ch
	c.pendMu.Unlock()

	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if err := c.write(raw); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
		return ctx.Err()
	case <-c.closed:
		return fmt.Errorf("lsp server exited")
	case resp, ok := <-ch:
		if !ok {
			return fmt.Errorf("lsp server exited")
		}
		if resp.Error != nil {
			return fmt.Errorf("lsp %s: %s", method, resp.Error.Message)
		}
		if result != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	}
}

// syncFile opens (or re-syncs with full text) a document and returns the
// diagnostics generation for its URI at the moment of the sync, so callers can
// wait for a publish that happened afterwards.
func (c *Client) syncFile(path, languageID, content string) (uri string, sinceGen int, err error) {
	uri = fileURI(path)
	c.diagMu.Lock()
	sinceGen = c.diagGen[uri]
	c.diagMu.Unlock()

	c.openMu.Lock()
	version, open := c.openDocs[uri]
	version++
	c.openDocs[uri] = version
	c.openMu.Unlock()

	if !open {
		err = c.notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri": uri, "languageId": languageID, "version": version, "text": content,
			},
		})
	} else {
		err = c.notify("textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": uri, "version": version},
			"contentChanges": []map[string]any{{"text": content}},
		})
	}
	return uri, sinceGen, err
}

// waitDiagnostics blocks until a publishDiagnostics newer than sinceGen lands
// for uri, or the context expires; either way it returns the current set.
func (c *Client) waitDiagnostics(ctx context.Context, uri string, sinceGen int) []Diagnostic {
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			c.diagCond.Broadcast()
		case <-stop:
		}
	}()
	c.diagMu.Lock()
	defer c.diagMu.Unlock()
	for c.diagGen[uri] <= sinceGen && ctx.Err() == nil && c.closeErr == nil {
		c.diagCond.Wait()
	}
	out := make([]Diagnostic, len(c.diags[uri]))
	copy(out, c.diags[uri])
	return out
}

// allDiagnostics returns a snapshot of every URI's latest published set.
func (c *Client) allDiagnostics() map[string][]Diagnostic {
	c.diagMu.Lock()
	defer c.diagMu.Unlock()
	out := make(map[string][]Diagnostic, len(c.diags))
	for uri, ds := range c.diags {
		cp := make([]Diagnostic, len(ds))
		copy(cp, ds)
		out[uri] = cp
	}
	return out
}

// references runs textDocument/references at the given position.
func (c *Client) references(ctx context.Context, uri string, pos Position) ([]Location, error) {
	var locs []Location
	err := c.call(ctx, "textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     pos,
		"context":      map[string]any{"includeDeclaration": true},
	}, &locs)
	return locs, err
}

// hover runs textDocument/hover and flattens the contents to plain text.
// The contents field varies by server: MarkupContent, a MarkedString, or an
// array of MarkedStrings; all shapes collapse to their value strings.
func (c *Client) hover(ctx context.Context, uri string, pos Position) (string, error) {
	var raw struct {
		Contents json.RawMessage `json:"contents"`
	}
	if err := c.call(ctx, "textDocument/hover", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     pos,
	}, &raw); err != nil {
		return "", err
	}
	return flattenHoverContents(raw.Contents), nil
}

func flattenHoverContents(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	type marked struct {
		Language string `json:"language"`
		Value    string `json:"value"`
	}
	var m marked // MarkupContent has the same value field
	if json.Unmarshal(raw, &m) == nil && m.Value != "" {
		return m.Value
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) == nil {
		var out []string
		for _, p := range parts {
			if v := flattenHoverContents(p); strings.TrimSpace(v) != "" {
				out = append(out, v)
			}
		}
		return strings.Join(out, "\n\n")
	}
	return ""
}

// rename runs textDocument/rename and returns the workspace edit collapsed to
// a uri → edits map (both the `changes` and `documentChanges` shapes are
// handled). Servers that answer with file create/rename/delete operations are
// rejected: applying those safely is out of scope for the tool.
func (c *Client) rename(ctx context.Context, uri string, pos Position, newName string) (map[string][]TextEdit, error) {
	var raw struct {
		Changes         map[string][]TextEdit `json:"changes"`
		DocumentChanges []json.RawMessage     `json:"documentChanges"`
	}
	if err := c.call(ctx, "textDocument/rename", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     pos,
		"newName":      newName,
	}, &raw); err != nil {
		return nil, err
	}
	edits := map[string][]TextEdit{}
	for u, es := range raw.Changes {
		edits[u] = append(edits[u], es...)
	}
	for _, dc := range raw.DocumentChanges {
		var op struct {
			Kind         string `json:"kind"`
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			Edits []TextEdit `json:"edits"`
		}
		if json.Unmarshal(dc, &op) != nil {
			continue
		}
		if op.Kind != "" {
			return nil, fmt.Errorf("rename requires a file %s operation, which is not supported", op.Kind)
		}
		if op.TextDocument.URI != "" {
			edits[op.TextDocument.URI] = append(edits[op.TextDocument.URI], op.Edits...)
		}
	}
	return edits, nil
}

// definition runs textDocument/definition at the given position. The result
// may be a Location, []Location, or LocationLink[] depending on the server;
// only the first two are handled (links are rare with our capabilities).
func (c *Client) definition(ctx context.Context, uri string, pos Position) ([]Location, error) {
	var raw json.RawMessage
	if err := c.call(ctx, "textDocument/definition", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     pos,
	}, &raw); err != nil {
		return nil, err
	}
	var locs []Location
	if json.Unmarshal(raw, &locs) == nil {
		return locs, nil
	}
	var single Location
	if json.Unmarshal(raw, &single) == nil && single.URI != "" {
		return []Location{single}, nil
	}
	return nil, nil
}
