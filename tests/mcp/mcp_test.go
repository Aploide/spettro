package mcp_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/mcp"
)

// writeServers writes a single file-type MCP server pointing at entryPoint.
func writeServers(t *testing.T, cwd, entryPoint string) {
	t.Helper()
	dir := filepath.Join(cwd, ".spettro")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"servers":[{"id":"fs","type":"file","entry_point":"` + entryPoint + `"}]}`
	if err := os.WriteFile(filepath.Join(dir, "mcp_servers.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSaveAuthUsesOwnerOnlyDir(t *testing.T) {
	cwd := t.TempDir()
	if err := mcp.SaveAuth(cwd, mcp.AuthState{ServerID: "fs", Token: "secret"}); err != nil {
		t.Fatalf("SaveAuth: %v", err)
	}
	dirInfo, err := os.Stat(filepath.Join(cwd, ".spettro"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf(".spettro dir perm = %o, want 700", perm)
	}
	fileInfo, err := os.Stat(filepath.Join(cwd, ".spettro", "mcp_auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("mcp_auth.json perm = %o, want 600", perm)
	}
}

func TestReadResourceReadsContainedFile(t *testing.T) {
	cwd := t.TempDir()
	root := filepath.Join(cwd, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeServers(t, cwd, root)

	got, err := mcp.ReadResource(cwd, "fs", "note.txt")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestReadResourceRejectsSymlinkEscape(t *testing.T) {
	cwd := t.TempDir()
	root := filepath.Join(cwd, "root")
	outside := filepath.Join(cwd, "outside")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	writeServers(t, cwd, root)

	_, err := mcp.ReadResource(cwd, "fs", "link.txt")
	if err == nil {
		t.Fatal("expected symlink-escape rejection, got nil error")
	}
	if !strings.Contains(err.Error(), "symlink") && !strings.Contains(err.Error(), "outside") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestReadResourceRejectsTraversal(t *testing.T) {
	cwd := t.TempDir()
	root := filepath.Join(cwd, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeServers(t, cwd, root)

	if _, err := mcp.ReadResource(cwd, "fs", "../../etc/hosts"); err == nil {
		t.Fatal("expected traversal rejection, got nil error")
	}
}

func TestReadResourceEnforcesSizeCap(t *testing.T) {
	cwd := t.TempDir()
	root := filepath.Join(cwd, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 3*1024*1024) // 3 MiB > 2 MiB cap
	if err := os.WriteFile(filepath.Join(root, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	writeServers(t, cwd, root)

	if _, err := mcp.ReadResource(cwd, "fs", "big.bin"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size-cap error, got %v", err)
	}
}
