package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// binaryName is the single executable packed inside each release archive
// (see .github/workflows/release.yml).
func binaryName() string {
	if runtime.GOOS == "windows" {
		return "spettro.exe"
	}
	return "spettro"
}

// archiveExt is the container each platform's release is published in. Windows
// gets .zip because it has no bundled tar-aware unpacker for users who fetch a
// release by hand, and because Explorer opens zips natively.
func archiveExt() string {
	if runtime.GOOS == "windows" {
		return "zip"
	}
	return "tar.gz"
}

func assetName(version string) string {
	return fmt.Sprintf("spettro_%s_%s_%s.%s", version, runtime.GOOS, runtime.GOARCH, archiveExt())
}

// Apply downloads the release archive matching the running OS/arch, verifies
// its checksum against the release's checksums.txt, and replaces the
// currently running executable with the extracted binary. It returns the
// (unchanged) path to the executable on success.
func Apply(ctx context.Context, rel *Release) (string, error) {
	wantName := assetName(rel.Version)
	tarAsset, ok := rel.asset(wantName)
	if !ok {
		return "", fmt.Errorf("no release build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	sumsAsset, ok := rel.asset("checksums.txt")
	if !ok {
		return "", errors.New("release is missing checksums.txt")
	}

	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(exe)

	sums, err := fetchChecksums(ctx, sumsAsset.URL)
	if err != nil {
		return "", fmt.Errorf("fetch checksums: %w", err)
	}
	wantSum, ok := sums[wantName]
	if !ok {
		return "", fmt.Errorf("checksums.txt has no entry for %s", wantName)
	}

	archivePath, gotSum, err := downloadToTemp(ctx, tarAsset.URL, dir)
	if err != nil {
		return "", fmt.Errorf("download update: %w", err)
	}
	defer os.Remove(archivePath)

	if !strings.EqualFold(gotSum, wantSum) {
		return "", fmt.Errorf("checksum mismatch for %s", wantName)
	}

	binPath, err := extractBinary(archivePath, dir)
	if err != nil {
		return "", fmt.Errorf("extract update: %w", err)
	}

	if err := replaceExecutable(binPath, exe); err != nil {
		os.Remove(binPath)
		return "", fmt.Errorf("install update: %w", err)
	}
	return exe, nil
}

func fetchChecksums(ctx context.Context, url string) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for line := range strings.SplitSeq(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		out[fields[1]] = fields[0]
	}
	return out, nil
}

// downloadToTemp streams url into a new temp file inside dir (so the later
// install onto exe can be a same-filesystem rename) and returns its path
// plus the sha256 of its contents.
func downloadToTemp(ctx context.Context, url, dir string) (path string, sum string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	f, err := os.CreateTemp(dir, ".spettro-update-*."+archiveExt())
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		os.Remove(f.Name())
		return "", "", err
	}
	return f.Name(), hex.EncodeToString(h.Sum(nil)), nil
}

// extractBinary pulls the binaryName entry out of the release archive at
// archivePath into a new executable temp file inside dir, picking the
// unpacker that matches this platform's release format.
func extractBinary(archivePath, dir string) (string, error) {
	if archiveExt() == "zip" {
		return extractBinaryZip(archivePath, dir)
	}
	return extractBinaryTarGz(archivePath, dir)
}

func extractBinaryTarGz(archivePath, dir string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("archive has no %s entry", binaryName())
		}
		if err != nil {
			return "", err
		}
		if filepath.Base(hdr.Name) != binaryName() || hdr.Typeflag != tar.TypeReg {
			continue
		}
		return writeExtracted(tr, dir)
	}
}

func extractBinaryZip(archivePath, dir string) (string, error) {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", err
	}
	defer zr.Close()

	for _, entry := range zr.File {
		if filepath.Base(entry.Name) != binaryName() || entry.FileInfo().IsDir() {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			return "", err
		}
		defer rc.Close()
		return writeExtracted(rc, dir)
	}
	return "", fmt.Errorf("archive has no %s entry", binaryName())
}

// writeExtracted copies the release binary into a fresh temp file next to the
// installed one, so the later install can be a same-filesystem rename.
func writeExtracted(src io.Reader, dir string) (string, error) {
	out, err := os.CreateTemp(dir, ".spettro-new-*")
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		out.Close()
		os.Remove(out.Name())
		return "", err
	}
	if _, err := io.Copy(out, src); err != nil {
		return fail(err)
	}
	// A no-op on Windows, where executability comes from the file extension.
	if err := out.Chmod(0o755); err != nil {
		return fail(err)
	}
	out.Close()
	return out.Name(), nil
}

