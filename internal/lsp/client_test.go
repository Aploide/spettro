package lsp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFileURI(t *testing.T) {
	cases := []struct{ path, want string }{
		{"/home/u/a.go", "file:///home/u/a.go"},
		{"C:/Users/u/a.c", "file:///C:/Users/u/a.c"},
		// drive letter is canonicalized to uppercase
		{"c:/Users/u/a.c", "file:///C:/Users/u/a.c"},
	}
	for _, tc := range cases {
		if got := fileURI(tc.path); got != tc.want {
			t.Errorf("fileURI(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestURIToPath(t *testing.T) {
	cases := []struct{ uri, want string }{
		{"file:///home/u/a.go", "/home/u/a.go"},
		{"file:///C:/Users/u/a.c", "C:/Users/u/a.c"},
		// clangd lowercases drive letters; VS Code-style percent-encodes the colon
		{"file:///c:/Users/u/a.c", "C:/Users/u/a.c"},
		{"file:///c%3A/Users/u/a.c", "C:/Users/u/a.c"},
		{"file:///home/u/with%20space/a.go", "/home/u/with space/a.go"},
	}
	for _, tc := range cases {
		if got := uriToPath(tc.uri); got != filepath.FromSlash(tc.want) {
			t.Errorf("uriToPath(%q) = %q, want %q", tc.uri, got, filepath.FromSlash(tc.want))
		}
	}
}

// Server-published URIs must canonicalize to the exact URI syncFile builds,
// or diagnostic generation tracking never matches.
func TestURICanonicalRoundTrip(t *testing.T) {
	for _, uri := range []string{"file:///C:/w/a.c", "file:///c:/w/a.c", "file:///c%3A/w/a.c"} {
		if got := fileURI(uriToPath(uri)); got != "file:///C:/w/a.c" {
			t.Errorf("canonical(%q) = %q, want file:///C:/w/a.c", uri, got)
		}
	}
}

// Every spelling of one file has to collapse to a single diagnostics key:
// clangd resolves the path it is handed (the short C:\Users\RUNNER~1\... form
// a Windows temp dir expands to, a symlinked /var on macOS) before answering.
func TestDocKeyCollapsesSpellings(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.c")
	if err := os.WriteFile(file, []byte("int main(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := docKey(file)
	spellings := []string{
		dir + string(filepath.Separator) + "." + string(filepath.Separator) + "a.c",
		filepath.Join(dir, "sub", "..", "a.c"),
		uriToPath(fileURI(file)),
	}
	if runtime.GOOS == "windows" {
		spellings = append(spellings, strings.ToUpper(file))
	}
	// symlinks need elevation on Windows; skip the case where they are denied
	link := filepath.Join(dir, "link.c")
	if err := os.Symlink(file, link); err == nil {
		spellings = append(spellings, link)
	}
	for _, s := range spellings {
		if got := docKey(s); got != want {
			t.Errorf("docKey(%q) = %q, want %q", s, got, want)
		}
	}
}

// discardWriteCloser stands in for the server's stdin when a test drives the
// client's bookkeeping without a process behind it.
type discardWriteCloser struct{}

func (discardWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (discardWriteCloser) Close() error                { return nil }

// The regression behind the Windows-only e2e failure: a publish that names the
// file differently than syncFile did must still wake waitDiagnostics, instead
// of leaving it to burn its whole deadline.
func TestWaitDiagnosticsMatchesServerSpelling(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.c")
	if err := os.WriteFile(file, []byte("int main(){ return x; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Client{
		stdin:    discardWriteCloser{},
		diags:    map[string]publishedDiags{},
		diagGen:  map[string]int{},
		openDocs: map[string]*openDoc{},
		closed:   make(chan struct{}),
	}
	c.diagCond = sync.NewCond(&c.diagMu)

	d, err := c.syncFile(file, "c", "int main(){ return x; }\n")
	if err != nil {
		t.Fatal(err)
	}

	// the server answers about the same file spelled its own way
	otherSpelling := dir + string(filepath.Separator) + "." + string(filepath.Separator) + "a.c"
	params, _ := json.Marshal(map[string]any{
		"uri":         fileURI(otherSpelling),
		"diagnostics": []map[string]any{{"severity": 1, "message": "use of undeclared identifier 'x'"}},
	})
	c.dispatch(rpcMessage{Method: "textDocument/publishDiagnostics", Params: params})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ds := c.waitDiagnostics(ctx, d)
	if len(ds) != 1 || !strings.Contains(ds[0].Message, "undeclared identifier") {
		t.Fatalf("publish under a different spelling was not matched: %+v", ds)
	}
}
