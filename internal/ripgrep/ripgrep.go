// Package ripgrep finds the rg binary the grep tool prefers over its pure-Go
// backend: rg on PATH, or a pinned release Spettro downloads into
// ~/.spettro/bin the first time grep would have used it.
//
// The download (product decision D9) never delays a tool call: it runs in
// the background, and the calls made meanwhile use the Go backend. The
// archive is fetched over HTTPS from the ripgrep GitHub release, checked
// against the SHA-256 pinned here for the OS/architecture, and only then
// unpacked; the binary is installed by an atomic rename. Failure (offline,
// a proxy, a checksum mismatch, an unsupported platform) just leaves the Go
// backend in use and is not retried in the same process. Setting
// ripgrep_download_disabled in config.json turns the download off; under
// `go test` the default installer never downloads.
package ripgrep

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"spettro/internal/config"
	"spettro/internal/homedir"
	"spettro/internal/safeio"
)

// Version is the ripgrep release Spettro downloads.
const Version = "15.2.0"

// releaseURL is where the release assets are fetched from.
const releaseURL = "https://github.com/BurntSushi/ripgrep/releases/download"

// Asset is one release archive: its file name and SHA-256 as published with
// the release (the .sha256 files and GitHub's asset digests agree).
type Asset struct {
	Name   string
	SHA256 string
}

// assets maps GOOS/GOARCH to the archive for it. Linux uses the static musl
// builds, so the binary runs whatever the libc.
var assets = map[string]Asset{
	"darwin/arm64":  {"ripgrep-15.2.0-aarch64-apple-darwin.tar.gz", "3750b2e93f37e0c692657da574d7019a101c0084da05a790c83fd335bad973e4"},
	"darwin/amd64":  {"ripgrep-15.2.0-x86_64-apple-darwin.tar.gz", "af7825fcc69a2afc7a7aea55fc9af90e26421d8f20fe59df32e233c0b8a231c1"},
	"linux/amd64":   {"ripgrep-15.2.0-x86_64-unknown-linux-musl.tar.gz", "33e15bcf1624b25cdd2a55813a47a2f95dbe126268203e76aa6a585d1e7b149c"},
	"linux/arm64":   {"ripgrep-15.2.0-aarch64-unknown-linux-musl.tar.gz", "800b1e7206afe799dfb5a6901f23147cfaabe0e52210538100f61e86e1740915"},
	"linux/arm":     {"ripgrep-15.2.0-armv7-unknown-linux-musleabihf.tar.gz", "0332b481aa007969a54d5c19e793208e73405c48d38f226bdee56b9ed085cdde"},
	"windows/amd64": {"ripgrep-15.2.0-x86_64-pc-windows-msvc.zip", "71b2fef860abe467217a538ff31de02f5258807c0129f771846f87bd029aafc5"},
	"windows/arm64": {"ripgrep-15.2.0-aarch64-pc-windows-msvc.zip", "e4abca10c3a64ebea742667dd7009449d49403db5460dd6873e389fa2945360f"},
	"windows/386":   {"ripgrep-15.2.0-i686-pc-windows-msvc.zip", "9bf73bdb3fda9ad4b0235e1295b02c717031c986afa4d7c05dd0af8b74010a95"},
}

const (
	// maxArchiveBytes bounds the download (the archives are ~2 MB).
	maxArchiveBytes = 32 << 20
	// maxBinaryBytes bounds the unpacked binary (rg is ~5 MB).
	maxBinaryBytes = 64 << 20
	// downloadTimeout bounds the whole background download.
	downloadTimeout = 2 * time.Minute
)

// Installer finds rg and downloads it when missing. The zero value is not
// usable; Default is the process-wide one.
//
// Cache: the PATH lookup is done once per Installer (a binary installed on
// PATH mid-session is picked up by the next process), and the managed
// binary, once found or installed, is remembered. Invalidation: none within
// a process; a new pinned Version makes the next process ignore the old
// install (its marker names the old version) and download again.
// Concurrency: every method may be called from any goroutine; mu guards the
// state, and the download runs on a goroutine of its own.
type Installer struct {
	// BinDir is where rg is installed. Empty means ~/.spettro/bin, resolved
	// on first use (not at package initialisation, so a test binary that
	// moves HOME in TestMain never sees the real home).
	BinDir string
	// BaseURL is the release download URL without the version.
	BaseURL string
	// Assets maps GOOS/GOARCH to the release archive.
	Assets map[string]Asset
	// Platform is GOOS/GOARCH, for tests.
	Platform string
	// Client makes the download request.
	Client *http.Client
	// Enabled reports whether a download may start (the config opt-out).
	// It is asked at most once per process: the answer, either way, stands
	// until the next process (a user who opted out does not pay a config
	// read on every grep).
	Enabled func() bool

	pathOnce sync.Once
	onPath   string

	mu        sync.Mutex
	binDir    string        // BinDir, or the resolved default
	managed   string        // installed binary, once verified present
	started   bool          // a download was started, or declined, in this process
	done      chan struct{} // closed when that download ends
	lastError error         // why it failed, for diagnostics
}

// Default is the installer the grep tool uses: ~/.spettro/bin, the pinned
// release, and the config.json opt-out. Under `go test` it never downloads.
var Default = &Installer{
	BaseURL:  releaseURL,
	Assets:   assets,
	Platform: runtime.GOOS + "/" + runtime.GOARCH,
	Client:   &http.Client{Timeout: downloadTimeout},
	Enabled:  downloadEnabled,
}

// downloadEnabled is the default opt-out check: config.json's
// ripgrep_download_disabled, and never inside a test binary.
func downloadEnabled() bool {
	if inTestBinary() {
		return false
	}
	cfg, err := config.Load()
	return err == nil && !cfg.RipgrepDownloadDisabled
}

// inTestBinary reports whether this process is a `go test` binary, which
// registers the test.v flag before any test runs. It avoids importing the
// testing package, which testing.Testing() would link into the release
// binary (+154 KB).
func inTestBinary() bool { return flag.Lookup("test.v") != nil }

// dirLocked returns BinDir, resolving the default ~/.spettro/bin once; ""
// when there is no home directory. Callers hold mu.
func (in *Installer) dirLocked() string {
	if in.binDir == "" {
		in.binDir = in.BinDir
		if in.binDir == "" {
			if home, err := homedir.Dir(); err == nil {
				in.binDir = filepath.Join(home, ".spettro", "bin")
			}
		}
	}
	return in.binDir
}

// binaryName is rg's file name on the platform.
func (in *Installer) binaryName() string {
	if strings.HasPrefix(in.Platform, "windows/") {
		return "rg.exe"
	}
	return "rg"
}

// markerPath records which release the installed binary came from.
// Callers hold mu.
func (in *Installer) markerPathLocked() string { return filepath.Join(in.dirLocked(), "rg.version") }

// Path returns the rg to run: the one on PATH, else the downloaded one when
// it is installed. It never starts a download (see EnsureDownload).
func (in *Installer) Path() (string, bool) {
	in.pathOnce.Do(func() { in.onPath, _ = exec.LookPath("rg") })
	if in.onPath != "" {
		return in.onPath, true
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.managed == "" && in.dirLocked() != "" && in.installedLocked() {
		in.managed = filepath.Join(in.dirLocked(), in.binaryName())
	}
	return in.managed, in.managed != ""
}

// installedLocked reports whether BinDir holds the pinned release's binary:
// the file exists and the marker names this version and archive.
func (in *Installer) installedLocked() bool {
	asset, ok := in.Assets[in.Platform]
	if !ok {
		return false
	}
	marker, err := os.ReadFile(in.markerPathLocked())
	if err != nil || strings.TrimSpace(string(marker)) != markerText(asset) {
		return false
	}
	info, err := os.Stat(filepath.Join(in.dirLocked(), in.binaryName()))
	return err == nil && info.Mode().IsRegular()
}

func markerText(a Asset) string { return Version + " " + a.Name + " " + a.SHA256 }

// EnsureDownload starts the background download when rg is neither on PATH
// nor installed, the platform has a pinned asset and the download is
// enabled. It returns at once; at most one download runs per Installer and
// process, a failed one is not retried, and a declined one (opted out) is
// not reconsidered.
func (in *Installer) EnsureDownload() {
	if _, ok := in.Path(); ok {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.started || in.dirLocked() == "" {
		return
	}
	if _, ok := in.Assets[in.Platform]; !ok {
		return
	}
	in.started = true
	if in.Enabled != nil && !in.Enabled() {
		return
	}
	in.done = make(chan struct{})
	go in.download(in.dirLocked())
}

// Wait blocks until a started download ends or ctx is done, and returns the
// download's error (nil also when none was started). For tests and callers
// that can afford to wait.
func (in *Installer) Wait(ctx context.Context) error {
	in.mu.Lock()
	done := in.done
	in.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.lastError
}

// download fetches, verifies and installs the pinned archive into dir.
func (in *Installer) download(dir string) {
	err := in.install(dir)
	in.mu.Lock()
	in.lastError = err
	if err == nil {
		in.managed = filepath.Join(dir, in.binaryName())
	}
	close(in.done)
	in.mu.Unlock()
}

func (in *Installer) install(dir string) error {
	asset := in.Assets[in.Platform]
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	archive, err := in.fetch(dir, asset)
	if err != nil {
		return err
	}
	defer os.Remove(archive)
	tmpBin, err := in.extract(dir, archive, asset.Name)
	if err != nil {
		return err
	}
	defer os.Remove(tmpBin) // gone after the rename; cleans up a failure
	if err := safeio.Replace(tmpBin, filepath.Join(dir, in.binaryName())); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "rg.version"), []byte(markerText(asset)+"\n"), 0o644)
}

// fetch downloads the archive into dir and verifies its checksum,
// returning the temporary file's path.
func (in *Installer) fetch(dir string, asset Asset) (string, error) {
	url := strings.TrimSuffix(in.BaseURL, "/") + "/" + Version + "/" + asset.Name
	ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := in.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", asset.Name, resp.Status)
	}
	f, err := os.CreateTemp(dir, ".rg-download-*")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxArchiveBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
	case n > maxArchiveBytes:
		err = fmt.Errorf("download %s: larger than %d bytes", asset.Name, maxArchiveBytes)
	case hex.EncodeToString(h.Sum(nil)) != asset.SHA256:
		err = fmt.Errorf("download %s: checksum mismatch", asset.Name)
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// extract copies the rg binary out of the archive into an executable
// temporary file in dir.
func (in *Installer) extract(dir, archive, name string) (string, error) {
	out, err := os.CreateTemp(dir, ".rg-*")
	if err != nil {
		return "", err
	}
	if strings.HasSuffix(name, ".zip") {
		err = extractZip(archive, in.binaryName(), out)
	} else {
		err = extractTarGz(archive, in.binaryName(), out)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(out.Name(), 0o755)
	}
	if err != nil {
		os.Remove(out.Name())
		return "", err
	}
	return out.Name(), nil
}

// errNoBinary reports an archive without the expected binary.
var errNoBinary = errors.New("archive holds no rg binary")

// isBinaryEntry matches the release layout: <release dir>/rg(.exe).
func isBinaryEntry(entry, bin string) bool {
	dir, base := path.Split(entry)
	return base == bin && strings.Count(dir, "/") == 1
}

func extractTarGz(archive, bin string, out io.Writer) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return errNoBinary
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeReg && isBinaryEntry(hdr.Name, bin) {
			return copyBounded(out, tr)
		}
	}
}

func extractZip(archive, bin string, out io.Writer) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		if !zf.Mode().IsRegular() || !isBinaryEntry(zf.Name, bin) {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		return copyBounded(out, rc)
	}
	return errNoBinary
}

// copyBounded copies at most maxBinaryBytes, failing beyond that.
func copyBounded(dst io.Writer, src io.Reader) error {
	n, err := io.Copy(dst, io.LimitReader(src, maxBinaryBytes+1))
	if err == nil && n > maxBinaryBytes {
		err = fmt.Errorf("rg binary larger than %d bytes", maxBinaryBytes)
	}
	return err
}
