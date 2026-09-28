package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"spettro/internal/ripgrep"
	"spettro/internal/sandbox"
)

// A grep that falls back to the Go backend starts the rg download in the
// background, unless the sandbox confines the network; the grep itself
// never waits for it. The release server is a local fake.
func TestGrepFallbackStartsRipgrepDownload(t *testing.T) {
	origLook, origDefault := lookRipgrep, ripgrep.Default
	t.Cleanup(func() { lookRipgrep, ripgrep.Default = origLook, origDefault })
	lookRipgrep = func() (string, bool) { return "", false }

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		time.Sleep(300 * time.Millisecond) // a slow network must not slow grep
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	newInstaller := func() *ripgrep.Installer {
		t.Setenv("PATH", t.TempDir())
		return &ripgrep.Installer{
			BinDir:   filepath.Join(t.TempDir(), "bin"),
			BaseURL:  srv.URL,
			Assets:   map[string]ripgrep.Asset{"test/os": {Name: "rg.tar.gz", SHA256: "00"}},
			Platform: "test/os",
			Client:   srv.Client(),
			Enabled:  func() bool { return true },
		}
	}

	// Network confined: no download.
	ripgrep.Default = newInstaller()
	r := newShellTestRuntime(t)
	writeTree(t, r.cwd, map[string]string{"a.txt": "needle\n"})
	r.sandboxState = NewSandboxState(sandbox.Policy{Net: sandbox.NetNone})
	if _, err := r.runGrep(context.Background(), grepArgs{Pattern: "needle"}); err != nil {
		t.Fatal(err)
	}
	if err := ripgrep.Default.Wait(context.Background()); err != nil || requests.Load() != 0 {
		t.Fatalf("download under a network sandbox: requests=%d err=%v", requests.Load(), err)
	}

	// Unconfined: the download starts, and grep returns before it ends.
	ripgrep.Default = newInstaller()
	r.sandboxState = nil
	start := time.Now()
	out, err := r.runGrep(context.Background(), grepArgs{Pattern: "needle"})
	if err != nil || out == "" {
		t.Fatalf("grep: %q %v", out, err)
	}
	if took := time.Since(start); took > 250*time.Millisecond {
		t.Fatalf("grep waited for the download: %s", took)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = ripgrep.Default.Wait(ctx)
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", requests.Load())
	}
}
