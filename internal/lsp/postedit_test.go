package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"spettro/internal/lsp/lsptest"
)

// TestMain lets the test binary double as a scripted language server: the
// fake-server tests below point the manager at os.Args[0].
func TestMain(m *testing.M) {
	lsptest.MaybeServe()
	os.Exit(m.Run())
}

// fakeManager returns a manager for a fresh workspace whose only server is the
// scripted one, claiming ".fk" files.
func fakeManager(t *testing.T, opts lsptest.Options) (*Manager, string) {
	t.Helper()
	raw, _ := json.Marshal(opts)
	t.Setenv(lsptest.EnvVar, string(raw))
	root := realPath(t.TempDir())
	cfg := Config{Servers: map[string]ServerConfig{
		"fake": {Command: os.Args[0], Args: lsptest.Args, Filetypes: []string{".fk"}},
	}}
	// also on disk, so a Restart's config reload keeps the fake server
	writeLspJSON(t, root, cfg)
	m := newManager(root, cfg)
	t.Cleanup(m.Shutdown)
	return m, root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// postEdit runs one post-edit pass with the given budget and reports how long
// it took.
func postEdit(t *testing.T, m *Manager, path string, budget time.Duration) (string, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	start := time.Now()
	out := m.PostEditDiagnostics(ctx, path)
	return out, time.Since(start)
}

func TestPostEditListsErrorsOnly(t *testing.T) {
	m, root := fakeManager(t, lsptest.Options{})
	a := filepath.Join(root, "a.fk")
	writeFile(t, a, "fine\nERR one\nWARN two\nERR three\n")
	out, _ := postEdit(t, m, a, 10*time.Second)
	want := "Diagnostics (errors) in a.fk:\n" +
		"a.fk:2:1: bad thing: ERR one (fake)\n" +
		"a.fk:4:1: bad thing: ERR three (fake)"
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}

	// fixing the file clears the block entirely: a clean edit adds nothing
	writeFile(t, a, "fine\nWARN two\n")
	out, took := postEdit(t, m, a, 10*time.Second)
	if out != "" {
		t.Fatalf("clean file should add nothing, got:\n%s", out)
	}
	// a running server answers well inside the budget: publish + settle
	if took > 2*time.Second {
		t.Fatalf("clean post-edit pass took %s", took)
	}
}

func TestPostEditCapsErrors(t *testing.T) {
	m, root := fakeManager(t, lsptest.Options{})
	a := filepath.Join(root, "a.fk")
	var sb strings.Builder
	for i := range MaxPostEditErrors + 5 {
		fmt.Fprintf(&sb, "ERR %d\n", i)
	}
	writeFile(t, a, sb.String())
	out, _ := postEdit(t, m, a, 10*time.Second)
	lines := strings.Split(out, "\n")
	if len(lines) != 1+MaxPostEditErrors+1 {
		t.Fatalf("want header + %d errors + overflow line, got %d lines:\n%s", MaxPostEditErrors, len(lines), out)
	}
	if last := lines[len(lines)-1]; last != "... and 5 more errors in a.fk" {
		t.Fatalf("overflow line = %q", last)
	}
}

func TestPostEditCountsOtherFiles(t *testing.T) {
	m, root := fakeManager(t, lsptest.Options{})
	a := filepath.Join(root, "a.fk")
	writeFile(t, filepath.Join(root, "b.fk"), "caller\n")
	// gone.fk does not exist: a deleted file's stale errors are not counted
	writeFile(t, a, "BREAK b.fk\nBREAK gone.fk\n")
	out, _ := postEdit(t, m, a, 10*time.Second)
	want := "No errors in a.fk; 2 errors in 1 other file: b.fk (2) — use the lsp tool (op: diagnostics) to list them."
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}

	writeFile(t, a, "ERR here\nBREAK b.fk\n")
	out, _ = postEdit(t, m, a, 10*time.Second)
	if !strings.HasPrefix(out, "Diagnostics (errors) in a.fk:\na.fk:1:1: bad thing: ERR here (fake)\n") ||
		!strings.HasSuffix(out, "Also 2 errors in 1 other file: b.fk (2) — use the lsp tool (op: diagnostics) to list them.") {
		t.Fatalf("unexpected report:\n%s", out)
	}
}

// A cold server must not hold the edit hostage: the pass returns at its
// budget with a note, the start carries on in the background, and every
// caller shares that one start.
func TestPostEditSlowStartIsSharedAndBounded(t *testing.T) {
	startLog := filepath.Join(t.TempDir(), "starts")
	m, root := fakeManager(t, lsptest.Options{InitDelay: 1500 * time.Millisecond, StartLog: startLog})
	a := filepath.Join(root, "a.fk")
	writeFile(t, a, "ERR one\n")

	m.Warm(a)
	m.Warm(a)
	out, took := postEdit(t, m, a, 300*time.Millisecond)
	if took > time.Second {
		t.Fatalf("post-edit pass waited %s for a starting server", took)
	}
	if !strings.Contains(out, "still starting") || !strings.Contains(out, "a.fk") {
		t.Fatalf("expected a still-starting note, got: %q", out)
	}

	out, _ = postEdit(t, m, a, 10*time.Second)
	if !strings.Contains(out, "a.fk:1:1: bad thing: ERR one") {
		t.Fatalf("expected errors once the server is up, got: %q", out)
	}
	if n := countStarts(t, startLog); n != 1 {
		t.Fatalf("server started %d times, want once per session", n)
	}
}

func TestPostEditSilentServerIsBounded(t *testing.T) {
	m, root := fakeManager(t, lsptest.Options{Silent: true})
	a := filepath.Join(root, "a.fk")
	writeFile(t, a, "ERR one\n")
	// start the server first so the budget below is spent waiting on
	// diagnostics, not on the start
	if _, _, err := m.clientFor(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	out, took := postEdit(t, m, a, 400*time.Millisecond)
	if took > 400*time.Millisecond+time.Second {
		t.Fatalf("pass took %s against a silent server", took)
	}
	if !strings.Contains(out, "not checked") {
		t.Fatalf("expected a not-checked note, got: %q", out)
	}
}

// A server that stopped reading its input makes the client's writes block
// once the pipe is full; the pass must still return at its deadline.
func TestPostEditWedgedServerIsBounded(t *testing.T) {
	m, root := fakeManager(t, lsptest.Options{StopReading: true})
	a := filepath.Join(root, "a.fk")
	writeFile(t, a, strings.Repeat("x", 4<<20)) // far past any pipe buffer
	if _, _, err := m.clientFor(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	_, took := postEdit(t, m, a, 300*time.Millisecond)
	if took > 300*time.Millisecond+time.Second {
		t.Fatalf("pass took %s against a wedged server", took)
	}
}

// On-save checkers only report after didSave, which the pass sends to servers
// that asked for it.
func TestPostEditSendsDidSave(t *testing.T) {
	m, root := fakeManager(t, lsptest.Options{SaveOnly: true})
	a := filepath.Join(root, "a.fk")
	writeFile(t, a, "ERR on save\n")
	out, _ := postEdit(t, m, a, 10*time.Second)
	if !strings.Contains(out, "bad thing: ERR on save") {
		t.Fatalf("expected the on-save diagnostics, got: %q", out)
	}
}

// Servers that publish an empty syntactic pass before the real one must not
// have their errors cut off by the first publish.
func TestPostEditWaitsForStagedPublish(t *testing.T) {
	m, root := fakeManager(t, lsptest.Options{Staged: 150 * time.Millisecond})
	a := filepath.Join(root, "a.fk")
	writeFile(t, a, "ERR late\n")
	out, _ := postEdit(t, m, a, 10*time.Second)
	if !strings.Contains(out, "bad thing: ERR late") {
		t.Fatalf("expected the second-stage errors, got: %q", out)
	}
}

func TestPostEditMissingServerIsSilent(t *testing.T) {
	root := realPath(t.TempDir())
	m := newManager(root, Config{Servers: map[string]ServerConfig{
		"fake": {Command: filepath.Join(root, "no-such-server"), Filetypes: []string{".fk"}},
	}})
	t.Cleanup(m.Shutdown)
	a := filepath.Join(root, "a.fk")
	writeFile(t, a, "ERR one\n")
	for range 2 {
		if out, took := postEdit(t, m, a, 3*time.Second); out != "" || took > time.Second {
			t.Fatalf("missing server: got %q after %s, want nothing, fast", out, took)
		}
	}
	// files no server claims add nothing either
	txt := filepath.Join(root, "notes.txt")
	writeFile(t, txt, "ERR\n")
	if out, _ := postEdit(t, m, txt, time.Second); out != "" {
		t.Fatalf("unclaimed file type: got %q", out)
	}
}

// A restart during a start must not leak the half-started server: the start
// is cancelled rather than waited out, and the next use starts a fresh one.
func TestRestartDuringStart(t *testing.T) {
	startLog := filepath.Join(t.TempDir(), "starts")
	m, root := fakeManager(t, lsptest.Options{InitDelay: 2 * time.Second, StartLog: startLog})
	a := filepath.Join(root, "a.fk")
	writeFile(t, a, "ERR one\n")
	m.Warm(a)
	waitForStarts(t, startLog, 1)
	began := time.Now()
	msg := m.Restart("")
	if took := time.Since(began); took > time.Second {
		t.Fatalf("restart waited %s for a start it should have cancelled", took)
	}
	if !strings.Contains(msg, "restarted lsp server(s): fake") {
		t.Fatalf("restart should report the start it cancelled, got: %s", msg)
	}
	m.mu.Lock()
	running := len(m.clients)
	m.mu.Unlock()
	if running != 0 {
		t.Fatalf("server started before the restart was registered after it")
	}
	out, _ := postEdit(t, m, a, 10*time.Second)
	if !strings.Contains(out, "ERR one") {
		t.Fatalf("expected diagnostics from the fresh server, got: %q", out)
	}
	if n := countStarts(t, startLog); n != 2 {
		t.Fatalf("got %d starts, want the discarded one plus a fresh one", n)
	}
}

// Restarting one server leaves the other servers' starts alone.
func TestRestartOneServerKeepsOtherStarts(t *testing.T) {
	raw, _ := json.Marshal(lsptest.Options{InitDelay: 700 * time.Millisecond})
	t.Setenv(lsptest.EnvVar, string(raw))
	root := realPath(t.TempDir())
	cfg := Config{Servers: map[string]ServerConfig{
		"fake":  {Command: os.Args[0], Args: lsptest.Args, Filetypes: []string{".fk"}},
		"other": {Command: os.Args[0], Args: lsptest.Args, Filetypes: []string{".ot"}},
	}}
	writeLspJSON(t, root, cfg)
	m := newManager(root, cfg)
	t.Cleanup(m.Shutdown)
	a, b := filepath.Join(root, "a.fk"), filepath.Join(root, "b.ot")
	writeFile(t, a, "x\n")
	writeFile(t, b, "ERR\n")
	m.Warm(a)
	m.Warm(b)
	m.Restart("fake")
	m.mu.Lock()
	_, otherStarting := m.starting["other"]
	m.mu.Unlock()
	if !otherStarting {
		t.Fatal("restarting fake abandoned other's start")
	}
	if out, _ := postEdit(t, m, b, 10*time.Second); !strings.Contains(out, "bad thing: ERR") {
		t.Fatalf("other server should have come up, got: %q", out)
	}
}

// Shutdown cancels a start in flight instead of waiting it out, and a caller
// that was waiting on that start does not bring a server back up on the
// discarded manager, where nothing would ever close it.
func TestShutdownDuringStartIsFastAndFinal(t *testing.T) {
	startLog := filepath.Join(t.TempDir(), "starts")
	m, root := fakeManager(t, lsptest.Options{InitDelay: 5 * time.Second, StartLog: startLog})
	a := filepath.Join(root, "a.fk")
	writeFile(t, a, "ERR one\n")
	res := make(chan string, 1)
	go func() {
		out, _ := postEdit(t, m, a, 10*time.Second)
		res <- out
	}()
	waitForStarts(t, startLog, 1)
	began := time.Now()
	m.Shutdown()
	if took := time.Since(began); took > 1500*time.Millisecond {
		t.Fatalf("shutdown waited %s for a start it should have cancelled", took)
	}
	select {
	case out := <-res:
		if out != "" {
			t.Fatalf("post-edit after shutdown should add nothing, got: %q", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting post-edit call did not return after shutdown")
	}
	m.Warm(a)
	time.Sleep(300 * time.Millisecond)
	m.mu.Lock()
	clients, starting := len(m.clients), len(m.starting)
	m.mu.Unlock()
	if clients != 0 || starting != 0 {
		t.Fatalf("shut-down manager has %d clients and %d starts", clients, starting)
	}
	if n := countStarts(t, startLog); n != 1 {
		t.Fatalf("got %d server starts, want only the cancelled one", n)
	}
}

func TestShutdownUnder(t *testing.T) {
	parent := realPath(t.TempDir())
	wt := filepath.Join(parent, "wt")
	sub := filepath.Join(wt, "pkg")
	sibling := filepath.Join(parent, "wt2")
	for _, d := range []string{sub, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mgrs := map[string]*Manager{}
	regMu.Lock()
	for _, root := range []string{wt, sub, sibling} {
		mgrs[root] = newManager(root, Config{})
		registry[root] = mgrs[root]
	}
	regMu.Unlock()
	t.Cleanup(func() {
		regMu.Lock()
		delete(registry, sibling)
		regMu.Unlock()
	})

	ShutdownUnder(wt)

	regMu.Lock()
	_, wtKept := registry[wt]
	_, subKept := registry[sub]
	_, sibKept := registry[sibling]
	regMu.Unlock()
	if wtKept || subKept || !sibKept {
		t.Fatalf("registry after ShutdownUnder: wt=%v sub=%v sibling=%v", wtKept, subKept, sibKept)
	}
	if !mgrs[wt].shutDown || !mgrs[sub].shutDown || mgrs[sibling].shutDown {
		t.Fatal("ShutdownUnder must stop the worktree's managers and only those")
	}
}

// A file changed on disk behind the server's back — by a shell command, or a
// rename that wrote it — is re-sent before the next check, so its errors are
// the disk's and not the ones the server saw last.
func TestPostEditResyncsOpenFilesChangedOnDisk(t *testing.T) {
	m, root := fakeManager(t, lsptest.Options{})
	a, b, c := filepath.Join(root, "a.fk"), filepath.Join(root, "b.fk"), filepath.Join(root, "c.fk")
	writeFile(t, a, "fine\n")
	writeFile(t, b, "ERR in b\n")
	writeFile(t, c, "ERR in c\n")
	postEdit(t, m, b, 10*time.Second) // opens b with its error
	postEdit(t, m, c, 10*time.Second) // and c
	if out, _ := postEdit(t, m, a, 10*time.Second); !strings.Contains(out, "2 errors in 2 other files: b.fk (1), c.fk (1)") {
		t.Fatalf("expected b and c counted, got: %q", out)
	}

	// b is fixed and c deleted, neither through the server
	writeFile(t, b, "fine now\n")
	if err := os.Remove(c); err != nil {
		t.Fatal(err)
	}
	if out, _ := postEdit(t, m, a, 10*time.Second); out != "" {
		t.Fatalf("b was fixed on disk and c deleted, got: %q", out)
	}
	cl, _, err := m.clientFor(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	cl.openMu.Lock()
	_, cOpen := cl.openDocs[fileURI(c)]
	cl.openMu.Unlock()
	if cOpen {
		t.Fatal("a deleted file should be closed on the server")
	}

	// and broken again behind the server's back
	writeFile(t, b, "ERR again\n")
	if out, _ := postEdit(t, m, a, 10*time.Second); !strings.Contains(out, "1 error in 1 other file: b.fk (1)") {
		t.Fatalf("expected b's new error, got: %q", out)
	}
}

// The other files a tool call wrote (the rest of a rename) reach the server
// even when it never had them open, so their errors are counted.
func TestPostEditSyncsAlsoChangedFiles(t *testing.T) {
	m, root := fakeManager(t, lsptest.Options{})
	a, b := filepath.Join(root, "a.fk"), filepath.Join(root, "b.fk")
	writeFile(t, a, "fine\n")
	writeFile(t, b, "ERR in b\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := m.PostEditDiagnostics(ctx, a, a, b)
	if out != "No errors in a.fk; 1 error in 1 other file: b.fk (1) — use the lsp tool (op: diagnostics) to list them." {
		t.Fatalf("got: %q", out)
	}
}

func waitForStarts(t *testing.T, path string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil && strings.Count(string(raw), "start ") >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server did not start %d time(s)", n)
}

func TestClientForHonoursContextWhileStarting(t *testing.T) {
	m, root := fakeManager(t, lsptest.Options{InitDelay: time.Second})
	a := filepath.Join(root, "a.fk")
	writeFile(t, a, "x\n")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, _, err := m.clientFor(ctx, a); !errors.Is(err, ErrServerStarting) {
		t.Fatalf("got %v, want ErrServerStarting", err)
	}
}

func countStarts(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(raw), "start ")
}

func TestPostEditReportFormat(t *testing.T) {
	var long Diagnostic
	long.Range.Start = Position{Line: 9, Character: 4}
	long.Severity = 1
	long.Message = "Type 'X' is not assignable\n  to type 'Y'.\n" + strings.Repeat("é", 400)
	got := formatError("src/a.ts", long)
	if !strings.HasPrefix(got, "src/a.ts:10:5: Type 'X' is not assignable to type 'Y'. éé") || !strings.HasSuffix(got, "…") {
		t.Fatalf("formatError = %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncation split a rune: %q", got)
	}

	cases := []struct {
		rep  postEditReport
		want string
	}{
		{postEditReport{rel: "a.go", skip: true}, ""},
		{postEditReport{rel: "a.go", server: "gopls"}, ""},
		{postEditReport{rel: "a.go", server: "gopls", starting: true}, "(gopls is still starting, so a.go was not checked for errors yet; later edits will be.)"},
		{postEditReport{rel: "a.go", server: "gopls", noAnswer: true}, "(gopls reported no diagnostics for a.go in time; it was not checked for errors.)"},
		{postEditReport{rel: "a.go", others: []fileErrors{{"b.go", 1}, {"c.go", 1}, {"d.go", 1}, {"e.go", 1}, {"f.go", 1}, {"g.go", 3}}},
			"No errors in a.go; 8 errors in 6 other files: b.go (1), c.go (1), d.go (1), e.go (1), f.go (1), 1 more — use the lsp tool (op: diagnostics) to list them."},
	}
	for _, tc := range cases {
		if got := tc.rep.format(); got != tc.want {
			t.Errorf("format(%+v) =\n%q\nwant\n%q", tc.rep, got, tc.want)
		}
	}
}

func TestParseSaveOptions(t *testing.T) {
	cases := map[string]saveOptions{
		`{"capabilities":{"textDocumentSync":1}}`:                             {},
		`{"capabilities":{"textDocumentSync":{"change":1}}}`:                  {},
		`{"capabilities":{"textDocumentSync":{"save":true}}}`:                 {enabled: true},
		`{"capabilities":{"textDocumentSync":{"save":false}}}`:                {},
		`{"capabilities":{"textDocumentSync":{"save":{}}}}`:                   {enabled: true},
		`{"capabilities":{"textDocumentSync":{"save":{"includeText":true}}}}`: {enabled: true, includeText: true},
		`{"capabilities":{}}`: {},
		`not json`:            {},
	}
	for in, want := range cases {
		if got := parseSaveOptions(json.RawMessage(in)); got != want {
			t.Errorf("parseSaveOptions(%s) = %+v, want %+v", in, got, want)
		}
	}
}

func TestWaitSettledCollectsLaterPublishes(t *testing.T) {
	c := &Client{
		stdin:    discardWriteCloser{},
		diags:    map[string]publishedDiags{},
		diagGen:  map[string]int{},
		openDocs: map[string]*openDoc{},
		closed:   make(chan struct{}),
	}
	c.diagCond = sync.NewCond(&c.diagMu)
	file := filepath.Join(t.TempDir(), "a.ts")
	d, err := c.syncFile(context.Background(), file, "typescript", "")
	if err != nil {
		t.Fatal(err)
	}
	publish := func(msgs ...string) {
		ds := []map[string]any{}
		for _, m := range msgs {
			ds = append(ds, map[string]any{"severity": 1, "message": m})
		}
		params, _ := json.Marshal(map[string]any{"uri": fileURI(file), "diagnostics": ds})
		c.dispatch(rpcMessage{Method: "textDocument/publishDiagnostics", Params: params})
	}

	// nothing published: not fresh, returned at the deadline
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if _, fresh := c.waitSettled(ctx, d, settlePolicy{quiet: 50 * time.Millisecond}); fresh {
		t.Fatal("no publish should not count as fresh")
	}
	cancel()

	// an empty syntactic publish, then the semantic one inside the window
	go func() {
		publish()
		time.Sleep(40 * time.Millisecond)
		publish("semantic error")
	}()
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ds, fresh := c.waitSettled(ctx, d, settlePolicy{quiet: 200 * time.Millisecond})
	if !fresh || len(ds) != 1 || ds[0].Message != "semantic error" {
		t.Fatalf("got %+v fresh=%v, want the later publish", ds, fresh)
	}
}

func TestFindServerBinaryFallsBackToInstallDirs(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "gobin")
	origDirs := extraBinDirs
	extraBinDirs = func(string) []string { return []string{filepath.Join(bin, "missing"), bin} }
	t.Cleanup(func() { extraBinDirs = origDirs })
	installed := filepath.Join(bin, "gopls")
	stubLookPath(t, installed)

	if got, ok := FindServerBinary("gopls", "/w"); !ok || got != "/usr/bin/"+installed {
		t.Fatalf("got %q ok=%v, want the install-dir hit", got, ok)
	}
	if _, ok := FindServerBinary("pylsp", "/w"); ok {
		t.Fatal("pylsp is nowhere")
	}
	// a PATH hit keeps the bare name
	stubLookPath(t, "gopls")
	if got, ok := FindServerBinary("gopls", "/w"); !ok || got != "gopls" {
		t.Fatalf("got %q ok=%v, want bare PATH name", got, ok)
	}

	// zero-config detection picks the install-dir binary up too
	stubLookPath(t, installed)
	cfg, ok := loadConfig(t.TempDir())
	if !ok || cfg.Servers["go"].Command != "/usr/bin/"+installed {
		t.Fatalf("detection missed the install-dir gopls: %+v ok=%v", cfg.Servers["go"], ok)
	}
}

// A publish marked with an older document version answers an earlier text,
// still in flight when the new one was sent, so it is not the edit's answer.
func TestWaitSettledIgnoresOlderVersions(t *testing.T) {
	c := &Client{
		stdin:    discardWriteCloser{},
		diags:    map[string]publishedDiags{},
		diagGen:  map[string]int{},
		openDocs: map[string]*openDoc{},
		closed:   make(chan struct{}),
	}
	c.diagCond = sync.NewCond(&c.diagMu)
	file := filepath.Join(t.TempDir(), "a.go")
	if _, err := c.syncFile(context.Background(), file, "go", "v1"); err != nil {
		t.Fatal(err)
	}
	d, err := c.syncFile(context.Background(), file, "go", "v2")
	if err != nil {
		t.Fatal(err)
	}
	publish := func(version int, msg string) {
		params, _ := json.Marshal(map[string]any{"uri": fileURI(file), "version": version,
			"diagnostics": []map[string]any{{"severity": 1, "message": msg}}})
		c.dispatch(rpcMessage{Method: "textDocument/publishDiagnostics", Params: params})
	}
	publish(1, "about v1")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if _, fresh := c.waitSettled(ctx, d, settlePolicy{quiet: 20 * time.Millisecond}); fresh {
		t.Fatal("a publish for version 1 answered the sync of version 2")
	}
	cancel()
	publish(2, "about v2")
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ds, fresh := c.waitSettled(ctx, d, settlePolicy{quiet: 20 * time.Millisecond})
	if !fresh || len(ds) != 1 || ds[0].Message != "about v2" {
		t.Fatalf("got %+v fresh=%v, want the version 2 publish", ds, fresh)
	}
}

func TestServerSettings(t *testing.T) {
	for cmd, want := range map[string]bool{
		"gopls": true, "/home/u/go/bin/gopls": true, `C:\go\bin\gopls.exe`: runtime.GOOS == "windows",
		"typescript-language-server": false, "rust-analyzer": false,
	} {
		got := serverSettings(cmd)
		if (got != nil) != want || (want && got["diagnosticsDelay"] != "0s") {
			t.Errorf("serverSettings(%q) = %v", cmd, got)
		}
	}
}

// The lsp tool's own requests (diagnostics, hover, references, rename) must
// honour their deadline against a server that stopped reading its input,
// and the next request must get a working server again.
func TestToolRequestsAgainstWedgedServerAreBounded(t *testing.T) {
	m, root := fakeManager(t, lsptest.Options{StopReading: true})
	a := filepath.Join(root, "a.fk")
	writeFile(t, a, strings.Repeat("x", 4<<20)) // far past any pipe buffer
	if _, _, err := m.clientFor(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	for name, run := range map[string]func(ctx context.Context) error{
		"diagnostics": func(ctx context.Context) error { _, err := m.DiagnosticsForFile(ctx, a); return err },
		"hover":       func(ctx context.Context) error { _, err := m.Hover(ctx, a, "", 1, 1); return err },
		"references":  func(ctx context.Context) error { _, err := m.Lookup(ctx, a, "", "references", 1, 1); return err },
		"rename":      func(ctx context.Context) error { _, err := m.RenameEdits(ctx, a, "", 1, 1, "y"); return err },
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		done := make(chan struct{})
		go func() {
			_ = run(ctx)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: still blocked 3s after a 300ms deadline", name)
		}
		cancel()
	}
}
