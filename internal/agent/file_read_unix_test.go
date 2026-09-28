//go:build unix

package agent

import (
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// file-read streams the file: it honours cancellation (a FIFO never blocks the
// step past its deadline) and refuses non-regular files outright.
func TestFileReadRefusesFIFOAndHonoursContext(t *testing.T) {
	r := newReadRuntime(t, nil)
	if err := syscall.Mkfifo(filepath.Join(r.cwd, "pipe"), 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.runFileRead(context.Background(), []byte(`{"path":"pipe","offset":1,"limit":5}`))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("FIFO read: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("file-read blocked on a FIFO")
	}
	writeTree(t, r.cwd, map[string]string{"big.txt": strings.Repeat("line\n", 100000)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.runFileRead(ctx, []byte(`{"path":"big.txt","offset":99990,"limit":5}`)); err == nil {
		t.Fatal("cancelled read succeeded")
	}
	out, err := r.runFileRead(context.Background(), []byte(`{"path":"big.txt","offset":99999,"limit":5}`))
	if err != nil {
		t.Fatal(err)
	}
	if out != " 99999\tline\n100000\tline\n" {
		t.Fatalf("paged read = %q", out)
	}
}
