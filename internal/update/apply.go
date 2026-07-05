package update

import (
	"archive/tar"
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

// binaryName is the single executable packed inside each release tarball
// (see .github/workflows/release.yml).
const binaryName = "spettro"

func assetName(version string) string {
	return fmt.Sprintf("spettro_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
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
	for _, line := range strings.Split(string(body), "\n") {
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

	f, err := os.CreateTemp(dir, ".spettro-update-*.tar.gz")
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

// extractBinary pulls the binaryName entry out of the tar.gz archive at
// archivePath into a new executable temp file inside dir.
func extractBinary(archivePath, dir string) (string, error) {
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
			return "", fmt.Errorf("archive has no %s entry", binaryName)
		}
		if err != nil {
			return "", err
		}
		if filepath.Base(hdr.Name) != binaryName || hdr.Typeflag != tar.TypeReg {
			continue
		}

		out, err := os.CreateTemp(dir, ".spettro-new-*")
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			os.Remove(out.Name())
			return "", err
		}
		if err := out.Chmod(0o755); err != nil {
			out.Close()
			os.Remove(out.Name())
			return "", err
		}
		out.Close()
		return out.Name(), nil
	}
}

// replaceExecutable installs newPath as target. Renaming is atomic and,
// crucially, safe to do onto a currently-running executable on Unix (the
// running process keeps its old inode open until it exits); a same-directory
// temp file (see downloadToTemp/extractBinary) keeps this a same-filesystem
// rename in the common case. If rename isn't possible we fall back to a
// copy, which will cleanly fail with "text file busy" on Linux rather than
// corrupt the running image.
func replaceExecutable(newPath, target string) error {
	if err := os.Rename(newPath, target); err == nil {
		return nil
	}
	src, err := os.Open(newPath)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(target, os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	os.Remove(newPath)
	return nil
}
