package lsp

import (
	"path/filepath"
	"testing"
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
