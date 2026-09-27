package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// Add returns before the file is written; Read serves the content either
// way, and Path only returns once the file is complete on disk.
func TestSpoolAsyncWriteIsReadableAndPathWaits(t *testing.T) {
	s := NewSpoolStore()
	defer s.Cleanup()
	content := strings.Repeat("spooled line\n", 20000)
	id, err := s.Add(content)
	if err != nil {
		t.Fatal(err)
	}
	if got, _, size, err := s.Read(id, 0, 0); err != nil || got != content || size != len(content) {
		t.Fatalf("Read right after Add = (%d bytes, size %d, %v), want the whole content", len(got), size, err)
	}
	path := s.Path(id)
	if path == "" {
		t.Fatal("Path of a spooled entry is empty")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != content {
		t.Fatalf("file at Path has %d bytes (%v), want the whole content", len(data), err)
	}
}

// Many writers and readers at once (run with -race): every entry reads back
// exactly, and after Flush every file is on disk.
func TestSpoolConcurrentAddRead(t *testing.T) {
	s := NewSpoolStore()
	defer s.Cleanup()
	var wg sync.WaitGroup
	ids := make([]string, 64)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			content := fmt.Sprintf("entry %d\n%s", i, strings.Repeat("x", 3000))
			id, err := s.Add(content)
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = id
			if got, _, _, err := s.Read(id, 0, 0); err != nil || got != content {
				t.Errorf("Read(%s) = %q..., %v", id, got[:min(20, len(got))], err)
			}
		}()
	}
	wg.Wait()
	s.Flush()
	s.mu.Lock()
	pending := len(s.pending)
	s.mu.Unlock()
	if pending != 0 {
		t.Fatalf("%d writes still pending after Flush", pending)
	}
	for i, id := range ids {
		data, err := os.ReadFile(s.Path(id))
		if err != nil || !strings.HasPrefix(string(data), fmt.Sprintf("entry %d\n", i)) {
			t.Fatalf("file of %s: %v", id, err)
		}
	}
}

// Cleanup waits for queued writes, so none lands in (or recreates) the
// removed directory.
func TestSpoolCleanupWaitsForQueuedWrites(t *testing.T) {
	s := NewSpoolStore()
	for range 50 {
		if _, err := s.Add(strings.Repeat("y", 50000)); err != nil {
			t.Fatal(err)
		}
	}
	dir := s.Dir()
	s.Cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("spool dir after Cleanup: %v, want it removed", err)
	}
}

// Remove waits for the entry's write before deleting its file.
func TestSpoolRemovePendingEntry(t *testing.T) {
	s := NewSpoolStore()
	defer s.Cleanup()
	id, err := s.Add(strings.Repeat("z", 200000))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Dir(), "1.txt")
	s.Remove(id)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file after Remove: %v, want it removed", err)
	}
	if _, _, _, err := s.Read(id, 0, 0); err == nil {
		t.Fatal("Read after Remove must fail")
	}
}

// A write that fails keeps the content readable; Path names no file.
func TestSpoolFailedWriteServesFromMemory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not block file creation on Windows")
	}
	s := NewSpoolStore()
	defer s.Cleanup()
	if _, err := s.Add("first"); err != nil {
		t.Fatal(err)
	}
	s.Flush()
	if err := os.Chmod(s.Dir(), 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(s.Dir(), 0o700)
	if f, err := os.Create(filepath.Join(s.Dir(), "probe")); err == nil {
		f.Close()
		t.Skip("running with privileges that ignore directory permissions")
	}
	id, err := s.Add("second output")
	if err != nil {
		t.Fatal(err)
	}
	if path := s.Path(id); path != "" {
		t.Fatalf("Path of a failed write = %q, want empty", path)
	}
	if got, _, _, err := s.Read(id, 0, 0); err != nil || got != "second output" {
		t.Fatalf("Read of a failed write = %q, %v; want the content from memory", got, err)
	}
}
