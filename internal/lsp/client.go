package lsp

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
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

	// diagnostics store: doc key → latest published set, with a generation
	// counter so callers can wait for a publish that happened after their
	// change. The key is docKey(path) rather than the URI as it travelled:
	// see dispatch.
	diagMu   sync.Mutex
	diagCond *sync.Cond
	diags    map[string]publishedDiags
	diagGen  map[string]int
	// pubGen counts every publish, whatever the document: waitBatch uses
	// it to tell when a burst of publishes (one diagnosis pass) is over.
	pubGen int

	openMu   sync.Mutex
	openDocs map[string]*openDoc // uri → what the server was last told

	// save is the server's textDocumentSync.save option: whether it wants
	// didSave at all, and whether with the full text. Servers such as
	// rust-analyzer and pylsp run their heavier checks only on save.
	save saveOptions

	closed   chan struct{}
	closeErr error
}

// publishedDiags is one document's latest diagnostics together with the path
// to display them under (the spelling the server used, so output keeps the
// filesystem's own casing) and the document version they were computed for
// (0 when the server did not say).
type publishedDiags struct {
	path    string
	list    []Diagnostic
	version int
}

// openDoc is a document the server holds open: once open, the server reads
// it from what it was sent rather than from disk, so the client has to keep
// it current. sum is the hash of the text last sent.
type openDoc struct {
	path       string
	languageID string
	version    int
	sum        [32]byte
}

// fileURI converts an absolute path into a file:// URI. Windows drive paths
// need a leading slash (file:///C:/x — file://C:/x would make the drive the
// URI authority, i.e. a UNC host, which servers then resolve as a network
// path) and a canonical uppercase drive letter so URIs built here compare
// equal to server-published URIs after canonicalization.
func fileURI(path string) string {
	p := filepath.ToSlash(path)
	if len(p) >= 2 && p[1] == ':' && isDriveLetter(p[0]) {
		p = "/" + strings.ToUpper(p[:1]) + p[1:]
	}
	return "file://" + p
}

// uriToPath is the inverse of fileURI, tolerant of the variants servers emit
// for the same file: percent-encoding (file:///c%3A/x) and lowercase drive
// letters (clangd lowercases them).
func uriToPath(uri string) string {
	p := strings.TrimPrefix(uri, "file://")
	if strings.Contains(p, "%") {
		if dec, err := url.PathUnescape(p); err == nil {
			p = dec
		}
	}
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' && isDriveLetter(p[1]) {
		p = strings.ToUpper(p[1:2]) + p[2:]
	}
	return filepath.FromSlash(p)
}

func isDriveLetter(c byte) bool {
	return ('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z')
}

// realPath resolves a path to its canonical on-disk spelling: symlinks
// expanded (macOS hands out /var/folders/... for a temp dir that really lives
// under /private/var) and, on Windows, 8.3 short components replaced by the
// long name the filesystem records — C:\Users\RUNNER~1\AppData\Local\Temp,
// which is what %TEMP% expands to on a CI runner, is really
// C:\Users\runneradmin\AppData\Local\Temp. Language servers resolve the paths
// they are handed and then answer in the resolved form, so speaking that form
// to them is what keeps their replies matchable. A path that cannot be
// resolved (it no longer exists) falls back to a plain Clean.
func realPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// docKey is the diagnostics-store key for a file: its real path, case-folded
// on Windows, whose filesystem is case-insensitive and whose servers are free
// to echo a casing other than the one we sent.
func docKey(path string) string { return foldPath(realPath(path)) }

// foldPath is docKey's second half, for callers that already resolved.
func foldPath(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

// stderrSink, when non-nil, receives the language servers' stderr. Servers are
// silent by default — their logs are noise the agent has no use for — but a
// test that can only fail on someone else's machine needs to be able to say
// what the server was doing.
var stderrSink io.Writer

// startClient spawns the server process, wires the reader loop, and completes
// the initialize handshake. The passed context bounds only the handshake.
func startClient(ctx context.Context, root, command string, args []string, settings map[string]any) (*Client, error) {
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
	cmd.Stderr = stderrSink
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &Client{
		cmd:      cmd,
		stdin:    stdin,
		pending:  map[int64]chan rpcMessage{},
		diags:    map[string]publishedDiags{},
		diagGen:  map[string]int{},
		openDocs: map[string]*openDoc{},
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
	if settings != nil {
		initParams["initializationOptions"] = settings
	}
	var initResult json.RawMessage
	if err := c.call(ctx, "initialize", initParams, &initResult); err != nil {
		c.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	c.save = parseSaveOptions(initResult)
	if err := c.notify(ctx, "initialized", map[string]any{}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Close terminates the server process. Safe to call multiple times. It waits
// (briefly) for the process to actually exit: on Windows a live server keeps
// handles on workspace files, so anything that deletes or replaces them right
// after a restart fails while the old process lingers.
func (c *Client) Close() {
	select {
	case <-c.closed:
	default:
		// A courtesy only, and bounded: a server that stopped reading its
		// input would block this write, which then gives up and abandons
		// the server, as below.
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_ = c.notify(ctx, "exit", nil)
		cancel()
	}
	c.abandon()
	select {
	case <-c.closed:
	case <-time.After(5 * time.Second):
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
		// No deadline of its own: if the server stopped reading, this
		// blocks the read loop until a request's deadline abandons the
		// server, which unblocks it (see write).
		_ = c.write(context.Background(), raw)
	case msg.Method == "textDocument/publishDiagnostics":
		var params struct {
			URI         string       `json:"uri"`
			Version     *int         `json:"version"`
			Diagnostics []Diagnostic `json:"diagnostics"`
		}
		if json.Unmarshal(msg.Params, &params) != nil {
			return
		}
		// Key by resolved path, never by the URI as it arrived: servers echo
		// their own spelling of a file (percent-encoded, lowercase drive
		// letter, or the long form of the short path we sent them), and every
		// spelling has to land on the key waitDiagnostics is watching or the
		// publish is never seen and the caller waits out its whole deadline.
		path := realPath(uriToPath(params.URI))
		key := foldPath(path)
		version := 0
		if params.Version != nil {
			version = *params.Version
		}
		c.diagMu.Lock()
		c.diags[key] = publishedDiags{path: path, list: params.Diagnostics, version: version}
		c.diagGen[key]++
		c.pubGen++
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

// write sends one framed message, giving up when ctx ends first.
//
// A pipe write blocks once the server stops draining its stdin (a wedged or
// busy-looping server), and nothing but closing the pipe unblocks it. So the
// frame is written on its own goroutine, and if ctx ends before it
// completes the server is abandoned: its stdin closed and the process
// killed. That is also the only safe outcome, because a partly written frame
// leaves the stream unparseable. The manager starts a fresh server on the
// next request (ensureStarted checks alive), and the writer goroutine
// returns as soon as the pipe is closed, so nothing leaks.
func (c *Client) write(ctx context.Context, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- c.writeFrame(payload) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	// The write may have finished just as ctx ended: prefer its result
	// over killing a server that is fine.
	select {
	case err := <-done:
		return err
	default:
	}
	c.abandon()
	return fmt.Errorf("lsp server is not reading its input: %w", ctx.Err())
}

// writeFrame writes the header and body of one message. writeMu keeps
// concurrent frames from interleaving.
func (c *Client) writeFrame(payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n", len(payload)); err != nil {
		return err
	}
	_, err := c.stdin.Write(payload)
	return err
}

// abandon stops the server without waiting for it: closing stdin fails
// every blocked write, and the kill ends the process, after which the
// cmd.Wait goroutine marks the client closed. Safe to call repeatedly.
func (c *Client) abandon() {
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return c.write(ctx, raw)
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
	if err := c.write(ctx, raw); err != nil {
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
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

// doc is a synced document: the URI to address it with in requests, plus the
// diagnostics key, the generation seen at sync time and the version sent, so
// the caller can wait for a publish about this very text.
type doc struct {
	uri      string
	key      string
	sinceGen int
	version  int
}

// syncFile opens (or re-syncs with full text) a document. ctx bounds the
// write (see write).
func (c *Client) syncFile(ctx context.Context, path, languageID, content string) (doc, error) {
	d := doc{uri: fileURI(path), key: docKey(path)}
	c.diagMu.Lock()
	d.sinceGen = c.diagGen[d.key]
	c.diagMu.Unlock()

	c.openMu.Lock()
	od, open := c.openDocs[d.uri]
	if !open {
		od = &openDoc{path: path, languageID: languageID}
		c.openDocs[d.uri] = od
	}
	od.version++
	od.sum = sha256.Sum256([]byte(content))
	version := od.version
	c.openMu.Unlock()
	d.version = version

	var err error
	if !open {
		err = c.notify(ctx, "textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri": d.uri, "languageId": languageID, "version": version, "text": content,
			},
		})
	} else {
		err = c.notify(ctx, "textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": d.uri, "version": version},
			"contentChanges": []map[string]any{{"text": content}},
		})
	}
	return d, err
}

// resyncOpen brings every open document except skip (a URI) back in line with
// the disk. The server reads an open document from what it was last sent, so
// a file changed behind its back — by a shell command, a rename that wrote
// several files, a git checkout — would otherwise be analysed, and reported
// on, as it was: errors the disk no longer has, and none of the ones it does.
// Changed files are re-sent (and saved, for on-save checkers); deleted ones
// are closed.
func (c *Client) resyncOpen(ctx context.Context, skip string) {
	type entry struct {
		uri string
		doc openDoc
	}
	c.openMu.Lock()
	docs := make([]entry, 0, len(c.openDocs))
	for uri, od := range c.openDocs {
		if uri != skip {
			docs = append(docs, entry{uri, *od})
		}
	}
	c.openMu.Unlock()
	for _, e := range docs {
		if ctx.Err() != nil {
			return
		}
		raw, err := os.ReadFile(e.doc.path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				c.closeDoc(ctx, e.uri)
			}
			continue
		}
		if sha256.Sum256(raw) == e.doc.sum {
			continue
		}
		content := string(raw)
		if d, err := c.syncFile(ctx, e.doc.path, e.doc.languageID, content); err == nil {
			_ = c.didSave(ctx, d, content)
		}
	}
}

// closeDoc tells the server a document is gone, so it drops what it holds.
func (c *Client) closeDoc(ctx context.Context, uri string) {
	c.openMu.Lock()
	_, open := c.openDocs[uri]
	delete(c.openDocs, uri)
	c.openMu.Unlock()
	if open {
		_ = c.notify(ctx, "textDocument/didClose", map[string]any{
			"textDocument": map[string]any{"uri": uri},
		})
	}
}

// saveOptions is the part of the server's textDocumentSync capability that
// governs didSave.
type saveOptions struct {
	enabled     bool
	includeText bool
}

// parseSaveOptions reads textDocumentSync.save out of an initialize result.
// The capability is either a bare TextDocumentSyncKind number (no save
// notifications) or an object whose save field is a bool or {includeText}.
func parseSaveOptions(initResult json.RawMessage) saveOptions {
	var res struct {
		Capabilities struct {
			TextDocumentSync json.RawMessage `json:"textDocumentSync"`
		} `json:"capabilities"`
	}
	if json.Unmarshal(initResult, &res) != nil {
		return saveOptions{}
	}
	var tds struct {
		Save json.RawMessage `json:"save"`
	}
	if json.Unmarshal(res.Capabilities.TextDocumentSync, &tds) != nil || len(tds.Save) == 0 {
		return saveOptions{}
	}
	var b bool
	if json.Unmarshal(tds.Save, &b) == nil {
		return saveOptions{enabled: b}
	}
	var opts struct {
		IncludeText bool `json:"includeText"`
	}
	if json.Unmarshal(tds.Save, &opts) == nil {
		return saveOptions{enabled: true, includeText: opts.IncludeText}
	}
	return saveOptions{}
}

// didSave tells the server the synced document now matches the disk, for
// servers that asked for save notifications. The file really was just
// written, so this is the truth, and it is what triggers on-save checkers.
func (c *Client) didSave(ctx context.Context, d doc, content string) error {
	if !c.save.enabled {
		return nil
	}
	params := map[string]any{"textDocument": map[string]any{"uri": d.uri}}
	if c.save.includeText {
		params["text"] = content
	}
	return c.notify(ctx, "textDocument/didSave", params)
}

// waitDiagnostics blocks until a publishDiagnostics about the synced text
// lands for the document, or the context expires; either way it returns the
// current set.
func (c *Client) waitDiagnostics(ctx context.Context, d doc) []Diagnostic {
	c.waitGen(ctx, d, d.sinceGen)
	return c.diagnosticsFor(d.key)
}

// versionedBatchQuiet is how long waitSettled keeps listening after a
// publish marked with the edit's version: that set is final for the edited
// file, but the same diagnosis pass publishes the files that depend on it
// (gopls sends a package's reverse dependencies within a millisecond of the
// file itself, in either order), and the other-files summary needs those.
const versionedBatchQuiet = 10 * time.Millisecond

// waitSettled waits for the first publish about the synced text and then
// for as long as policy says later publishes may still complete it (see
// settlePolicy): several servers publish in stages, and most of the rest
// mark the publish with the document version, which proves it final once
// the rest of its batch is in. fresh reports whether such a publish landed
// before ctx expired; without one the returned set predates the edit.
func (c *Client) waitSettled(ctx context.Context, d doc, policy settlePolicy) (ds []Diagnostic, fresh bool) {
	gen, fresh := c.waitGen(ctx, d, d.sinceGen)
	if !fresh {
		return c.diagnosticsFor(d.key), false
	}
	switch {
	case policy.stages > 1:
		c.waitStages(ctx, d, gen, policy)
	case c.publishedVersion(d.key) >= d.version:
		c.waitBatch(ctx, versionedBatchQuiet)
	default:
		for ctx.Err() == nil {
			qctx, cancel := context.WithTimeout(ctx, policy.quiet)
			next, _ := c.waitGen(qctx, d, gen)
			cancel()
			if next <= gen {
				break
			}
			gen = next
		}
	}
	return c.diagnosticsFor(d.key), true
}

// waitBatch returns once the server has published nothing, about any
// document, for quiet (or ctx ends).
func (c *Client) waitBatch(ctx context.Context, quiet time.Duration) {
	c.diagMu.Lock()
	seen := c.pubGen
	c.diagMu.Unlock()
	for ctx.Err() == nil {
		qctx, cancel := context.WithTimeout(ctx, quiet)
		next := c.waitAnyPublish(qctx, seen)
		cancel()
		if next <= seen {
			return
		}
		seen = next
	}
}

// waitAnyPublish blocks until the server publishes anything after
// generation since, ctx ends or the server exits, and returns pubGen then.
func (c *Client) waitAnyPublish(ctx context.Context, since int) int {
	stop := context.AfterFunc(ctx, func() {
		c.diagMu.Lock()
		defer c.diagMu.Unlock()
		c.diagCond.Broadcast()
	})
	defer stop()
	c.diagMu.Lock()
	defer c.diagMu.Unlock()
	for c.pubGen <= since && ctx.Err() == nil && c.closeErr == nil {
		c.diagCond.Wait()
	}
	return c.pubGen
}

// waitStages waits, after the first publish at generation gen, for the rest
// of a staged server's publishes: until it has published policy.stages sets
// or policy.ceiling has passed since the first.
func (c *Client) waitStages(ctx context.Context, d doc, gen int, policy settlePolicy) {
	sctx, cancel := context.WithTimeout(ctx, policy.ceiling)
	defer cancel()
	for seen := 1; seen < policy.stages && sctx.Err() == nil; {
		next, _ := c.waitGen(sctx, d, gen)
		if next <= gen {
			return
		}
		seen += next - gen
		gen = next
	}
}

// publishedVersion returns the document version of the latest publish for
// key, 0 when the server did not say.
func (c *Client) publishedVersion(key string) int {
	c.diagMu.Lock()
	defer c.diagMu.Unlock()
	return c.diags[key].version
}

// waitGen blocks until the document has a publish newer than generation
// sinceGen about the synced text, the context expires or the server exits,
// and returns the generation then and whether that publish arrived. A publish
// the server marks with an older document version answers an earlier state
// of the file (one still in flight when the new text was sent), so it does
// not count.
func (c *Client) waitGen(ctx context.Context, d doc, sinceGen int) (int, bool) {
	// Taking the lock before broadcasting closes the gap between the loop's
	// ctx check and Wait: without it the wake-up could land in that gap and
	// be lost, leaving the waiter asleep until some unrelated publish.
	stop := context.AfterFunc(ctx, func() {
		c.diagMu.Lock()
		defer c.diagMu.Unlock()
		c.diagCond.Broadcast()
	})
	defer stop()
	c.diagMu.Lock()
	defer c.diagMu.Unlock()
	arrived := func() bool {
		if c.diagGen[d.key] <= sinceGen {
			return false
		}
		v := c.diags[d.key].version
		return v == 0 || v >= d.version
	}
	for !arrived() && ctx.Err() == nil && c.closeErr == nil {
		c.diagCond.Wait()
	}
	return c.diagGen[d.key], arrived()
}

// diagnosticsFor returns a copy of the document's latest published set.
func (c *Client) diagnosticsFor(key string) []Diagnostic {
	c.diagMu.Lock()
	defer c.diagMu.Unlock()
	list := c.diags[key].list
	out := make([]Diagnostic, len(list))
	copy(out, list)
	return out
}

// allDiagnostics returns a snapshot of every document's latest published set,
// keyed by the path to display it under.
func (c *Client) allDiagnostics() map[string][]Diagnostic {
	c.diagMu.Lock()
	defer c.diagMu.Unlock()
	out := make(map[string][]Diagnostic, len(c.diags))
	for _, pd := range c.diags {
		cp := make([]Diagnostic, len(pd.list))
		copy(cp, pd.list)
		out[pd.path] = cp
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
