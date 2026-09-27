package ripgrep

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRelease serves release archives from memory and counts requests: no
// test here ever reaches the network.
type fakeRelease struct {
	files    map[string][]byte // path below the server root -> body
	requests atomic.Int32
	delay    time.Duration
}

func (f *fakeRelease) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	time.Sleep(f.delay)
	body, ok := f.files[strings.TrimPrefix(r.URL.Path, "/")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(body)
}

func tarGz(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		typ := byte(tar.TypeReg)
		if strings.HasSuffix(name, "/") {
			typ = tar.TypeDir
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: typ}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipArchive(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// newTestInstaller points an installer at a fake release server for a
// platform with one archive, and hides any rg on PATH.
func newTestInstaller(t *testing.T, platform, assetName string, archive []byte, pinned string) (*Installer, *fakeRelease) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	rel := &fakeRelease{files: map[string][]byte{Version + "/" + assetName: archive}}
	srv := httptest.NewServer(rel)
	t.Cleanup(srv.Close)
	in := &Installer{
		BinDir:   filepath.Join(t.TempDir(), "bin"),
		BaseURL:  srv.URL,
		Assets:   map[string]Asset{platform: {Name: assetName, SHA256: pinned}},
		Platform: platform,
		Client:   srv.Client(),
		Enabled:  func() bool { return true },
	}
	return in, rel
}

func waitDownload(t *testing.T, in *Installer) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return in.Wait(ctx)
}

func TestDownloadInstallsVerifiedBinary(t *testing.T) {
	name := "ripgrep-" + Version + "-test.tar.gz"
	archive := tarGz(t, map[string]string{
		"ripgrep-" + Version + "-test/":          "",
		"ripgrep-" + Version + "-test/README.md": "readme",
		"ripgrep-" + Version + "-test/rg":        "#!/bin/sh\necho fake rg\n",
	})
	in, rel := newTestInstaller(t, "linux/amd64", name, archive, sum(archive))
	if _, ok := in.Path(); ok {
		t.Fatal("nothing is installed yet")
	}
	in.EnsureDownload()
	in.EnsureDownload() // a second call while running starts nothing
	if err := waitDownload(t, in); err != nil {
		t.Fatalf("download: %v", err)
	}
	if n := rel.requests.Load(); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
	got, ok := in.Path()
	if !ok || got != filepath.Join(in.BinDir, "rg") {
		t.Fatalf("Path() = %q, %v", got, ok)
	}
	data, err := os.ReadFile(got)
	if err != nil || string(data) != "#!/bin/sh\necho fake rg\n" {
		t.Fatalf("installed binary = %q, %v", data, err)
	}
	if info, _ := os.Stat(got); info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("binary not executable: %v", info.Mode())
	}
	// No temporary files are left behind.
	entries, _ := os.ReadDir(in.BinDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".rg") {
			t.Fatalf("leftover %s", e.Name())
		}
	}
	// A new process (installer) finds the install without downloading.
	again := &Installer{BinDir: in.BinDir, Assets: in.Assets, Platform: in.Platform}
	if p, ok := again.Path(); !ok || p != got {
		t.Fatalf("second installer Path() = %q, %v", p, ok)
	}
	again.EnsureDownload()
	if rel.requests.Load() != 1 {
		t.Fatal("an installed binary was downloaded again")
	}
}

func TestDownloadRejectsChecksumMismatch(t *testing.T) {
	name := "ripgrep-" + Version + "-test.tar.gz"
	archive := tarGz(t, map[string]string{"ripgrep-" + Version + "-test/rg": "tampered"})
	in, _ := newTestInstaller(t, "linux/amd64", name, archive, strings.Repeat("0", 64))
	in.EnsureDownload()
	if err := waitDownload(t, in); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v, want a checksum mismatch", err)
	}
	if _, ok := in.Path(); ok {
		t.Fatal("a binary from a mismatching archive was installed")
	}
	entries, _ := os.ReadDir(in.BinDir)
	if len(entries) != 0 {
		t.Fatalf("files left in bin dir: %v", entries)
	}
	// Not retried in the same process.
	in.EnsureDownload()
	if err := waitDownload(t, in); err == nil {
		t.Fatal("the failed download was replaced by a retry")
	}
}

func TestDownloadOfflineAndMissingAssetFallBack(t *testing.T) {
	name := "ripgrep-" + Version + "-test.tar.gz"
	archive := tarGz(t, map[string]string{"ripgrep-" + Version + "-test/rg": "x"})
	in, _ := newTestInstaller(t, "linux/amd64", name, archive, sum(archive))
	in.BaseURL = "http://127.0.0.1:1" // nothing listens: connection refused
	in.EnsureDownload()
	if err := waitDownload(t, in); err == nil {
		t.Fatal("expected an offline error")
	}
	if _, ok := in.Path(); ok {
		t.Fatal("offline download reported an install")
	}

	in2, rel := newTestInstaller(t, "linux/amd64", name, archive, sum(archive))
	delete(rel.files, Version+"/"+name) // 404
	in2.EnsureDownload()
	if err := waitDownload(t, in2); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want 404", err)
	}
}

func TestDownloadRespectsOptOutAndPlatform(t *testing.T) {
	name := "ripgrep-" + Version + "-test.tar.gz"
	archive := tarGz(t, map[string]string{"ripgrep-" + Version + "-test/rg": "x"})
	in, rel := newTestInstaller(t, "linux/amd64", name, archive, sum(archive))
	in.Enabled = func() bool { return false }
	in.EnsureDownload()
	if err := waitDownload(t, in); err != nil || rel.requests.Load() != 0 {
		t.Fatalf("opted out, yet requests=%d err=%v", rel.requests.Load(), err)
	}

	in2, rel2 := newTestInstaller(t, "plan9/amd64", name, archive, sum(archive))
	in2.Platform = "haiku/amd64" // no pinned asset
	in2.EnsureDownload()
	if rel2.requests.Load() != 0 {
		t.Fatal("an unsupported platform made a request")
	}
}

// EnsureDownload never waits for the download itself.
func TestEnsureDownloadDoesNotBlock(t *testing.T) {
	name := "ripgrep-" + Version + "-test.tar.gz"
	archive := tarGz(t, map[string]string{"ripgrep-" + Version + "-test/rg": "x"})
	in, rel := newTestInstaller(t, "linux/amd64", name, archive, sum(archive))
	rel.delay = 500 * time.Millisecond
	start := time.Now()
	in.EnsureDownload()
	if took := time.Since(start); took > 200*time.Millisecond {
		t.Fatalf("EnsureDownload blocked for %s", took)
	}
	if _, ok := in.Path(); ok {
		t.Fatal("installed before the download finished")
	}
	if err := waitDownload(t, in); err != nil {
		t.Fatal(err)
	}
	if _, ok := in.Path(); !ok {
		t.Fatal("not installed after the download")
	}
}

func TestWindowsZipLayout(t *testing.T) {
	name := "ripgrep-" + Version + "-test.zip"
	archive := zipArchive(t, map[string]string{
		"ripgrep-" + Version + "-test/complete/rg.exe": "wrong: not the release dir",
		"ripgrep-" + Version + "-test/rg.exe":          "MZ fake",
	})
	in, _ := newTestInstaller(t, "windows/amd64", name, archive, sum(archive))
	in.EnsureDownload()
	if err := waitDownload(t, in); err != nil {
		t.Fatal(err)
	}
	p, ok := in.Path()
	if !ok || filepath.Base(p) != "rg.exe" {
		t.Fatalf("Path() = %q, %v", p, ok)
	}
	if data, _ := os.ReadFile(p); string(data) != "MZ fake" {
		t.Fatalf("extracted %q", data)
	}
}

// A stale install (an older pinned version) is not used, and is replaced.
func TestStaleVersionIsReplaced(t *testing.T) {
	name := "ripgrep-" + Version + "-test.tar.gz"
	archive := tarGz(t, map[string]string{"ripgrep-" + Version + "-test/rg": "new"})
	in, _ := newTestInstaller(t, "linux/amd64", name, archive, sum(archive))
	if err := os.MkdirAll(in.BinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(in.BinDir, "rg"), []byte("old"), 0o755)
	_ = os.WriteFile(in.markerPath(), []byte("14.1.1 old.tar.gz abc\n"), 0o644)
	if _, ok := in.Path(); ok {
		t.Fatal("a stale install was used")
	}
	in.EnsureDownload()
	if err := waitDownload(t, in); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(in.BinDir, "rg")); string(data) != "new" {
		t.Fatalf("binary = %q, want the new one", data)
	}
}

// The default installer never downloads inside a test binary, whatever the
// config says: agent tests that fall back to the Go grep must not fetch.
func TestDefaultNeverDownloadsUnderTest(t *testing.T) {
	if downloadEnabled() {
		t.Fatal("downloads enabled under go test")
	}
}

// Every pinned asset is a known archive shape with a well-formed digest.
func TestPinnedAssets(t *testing.T) {
	for platform, a := range assets {
		if !strings.HasPrefix(a.Name, "ripgrep-"+Version+"-") || !(strings.HasSuffix(a.Name, ".tar.gz") || strings.HasSuffix(a.Name, ".zip")) {
			t.Errorf("%s: bad asset name %q", platform, a.Name)
		}
		if b, err := hex.DecodeString(a.SHA256); err != nil || len(b) != sha256.Size {
			t.Errorf("%s: bad digest %q", platform, a.SHA256)
		}
		if strings.HasPrefix(platform, "windows/") != strings.HasSuffix(a.Name, ".zip") {
			t.Errorf("%s: archive type does not fit the platform", platform)
		}
	}
}
