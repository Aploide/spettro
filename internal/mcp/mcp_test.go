package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeServers(t *testing.T, cwd, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cwd, ".spettro"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(serversPath(cwd), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadServers(t *testing.T) {
	cwd := t.TempDir()
	if srvs, err := LoadServers(cwd); err != nil || srvs != nil {
		t.Errorf("missing config should be (nil, nil), got (%v, %v)", srvs, err)
	}

	writeServers(t, cwd, `{"servers":[
		{"id":" docs ","name":"Docs","type":" FILE ","entry_point":"docs"},
		{"id":"api","name":"API","type":"http","entry_point":"http://x"},
		{"id":"","entry_point":"nope"},
		{"id":"no-entry"}
	]}`)
	srvs, err := LoadServers(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(srvs) != 2 {
		t.Fatalf("got %d servers, want 2: %+v", len(srvs), srvs)
	}
	if srvs[0].ID != "docs" || srvs[0].Type != "file" {
		t.Errorf("normalization wrong: %+v", srvs[0])
	}
	if srvs[1].Type != "http" {
		t.Errorf("http server wrong: %+v", srvs[1])
	}
}

func TestSaveLoadAuth(t *testing.T) {
	cwd := t.TempDir()

	if err := SaveAuth(cwd, AuthState{Token: "t"}); err == nil {
		t.Error("missing server_id must error")
	}
	if err := SaveAuth(cwd, AuthState{ServerID: "s"}); err == nil {
		t.Error("missing token must error")
	}

	if err := SaveAuth(cwd, AuthState{ServerID: "api", Token: " tok1 "}); err != nil {
		t.Fatal(err)
	}
	st, ok, err := LoadAuth(cwd, "api")
	if err != nil || !ok {
		t.Fatalf("LoadAuth: %v, %v", ok, err)
	}
	if st.Token != "tok1" || st.UpdatedAt.IsZero() {
		t.Errorf("auth state = %+v", st)
	}

	// Replacing the same server updates in place.
	if err := SaveAuth(cwd, AuthState{ServerID: "api", Token: "tok2"}); err != nil {
		t.Fatal(err)
	}
	st, _, _ = LoadAuth(cwd, "api")
	if st.Token != "tok2" {
		t.Errorf("token after replace = %q", st.Token)
	}

	if _, ok, err := LoadAuth(cwd, "other"); err != nil || ok {
		t.Errorf("unknown server should be (false, nil), got (%v, %v)", ok, err)
	}
}

func setupFileServer(t *testing.T) string {
	t.Helper()
	cwd := t.TempDir()
	root := filepath.Join(cwd, "docs")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "b.md"), []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "outside.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeServers(t, cwd, `{"servers":[{"id":"docs","name":"Docs","type":"file","entry_point":"docs"}]}`)
	return cwd
}

func TestListAndReadFileResources(t *testing.T) {
	cwd := setupFileServer(t)
	res, err := ListResources(cwd, "docs")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("got %d resources: %+v", len(res), res)
	}
	ids := []string{res[0].ID, res[1].ID}
	if !(strings.Contains(strings.Join(ids, ","), "a.md") && strings.Contains(strings.Join(ids, ","), "sub/b.md")) {
		t.Errorf("ids = %v", ids)
	}

	body, err := ReadResource(cwd, "docs", "sub/b.md")
	if err != nil {
		t.Fatal(err)
	}
	if body != "beta" {
		t.Errorf("body = %q", body)
	}

	if _, err := ReadResource(cwd, "nope", "a.md"); err == nil {
		t.Error("unknown server must error")
	}
}

func TestReadFileResourceTraversalBlocked(t *testing.T) {
	cwd := setupFileServer(t)
	if _, err := ReadResource(cwd, "docs", "../outside.txt"); err == nil {
		t.Fatal("path traversal must be blocked")
	}
}

func TestReadFileResourceSymlinkEscapeBlocked(t *testing.T) {
	cwd := setupFileServer(t)
	link := filepath.Join(cwd, "docs", "leak.txt")
	if err := os.Symlink(filepath.Join(cwd, "outside.txt"), link); err != nil {
		t.Skip("symlinks not supported")
	}
	if _, err := ReadResource(cwd, "docs", "leak.txt"); err == nil {
		t.Fatal("symlink escaping the root must be blocked")
	}
}

func TestWithinRoot(t *testing.T) {
	cases := []struct {
		root, target string
		want         bool
	}{
		{"/a/b", "/a/b/c", true},
		{"/a/b", "/a/b", true},
		{"/a/b", "/a", false},
		{"/a/b", "/a/bc", false},
		{"/a/b", "/a/b/../x", false},
	}
	for _, c := range cases {
		if got := withinRoot(c.root, filepath.Clean(c.target)); got != c.want {
			t.Errorf("withinRoot(%q, %q) = %v, want %v", c.root, c.target, got, c.want)
		}
	}
}
