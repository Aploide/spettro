package update

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"spettro/internal/homedir"
	"spettro/internal/safeio"
)

// releaseCheckTTL is how long a successful release check is reused. The
// startup check runs on every launch; GitHub allows 60 unauthenticated API
// requests per hour per address, and a release a day late is harmless.
const releaseCheckTTL = 24 * time.Hour

// releaseCheck is the on-disk form of the release-check cache.
type releaseCheck struct {
	CheckedAt time.Time `json:"checked_at"`
	Release   Release   `json:"release"`
}

// releaseCheckPath is the release-check cache file,
// ~/.spettro/update-check.json.
//
// The cache holds the newest release GitHub reported. Key: none, there is
// one repository. Invalidation: it expires releaseCheckTTL after CheckedAt,
// and a checked_at in the future (a clock moved back) counts as expired.
// Ownership: any process may read or rewrite it; a write replaces the file
// by rename, so a reader sees the old or the new document, never a mix.
func releaseCheckPath() (string, error) {
	home, err := homedir.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".spettro", "update-check.json"), nil
}

// LatestRelease returns the newest published release, from the release-check
// cache when it is younger than releaseCheckTTL and from GitHub otherwise
// (see FetchLatestRelease). A successful fetch refreshes the cache; a failed
// one is not cached, so the next launch tries again.
func LatestRelease(ctx context.Context) (*Release, error) {
	if rel, ok := cachedRelease(time.Now()); ok {
		return rel, nil
	}
	rel, err := FetchLatestRelease(ctx)
	if err != nil {
		return nil, err
	}
	storeRelease(rel, time.Now())
	return rel, nil
}

// cachedRelease returns the cached release when the cache is fresh at now.
func cachedRelease(now time.Time) (*Release, bool) {
	path, err := releaseCheckPath()
	if err != nil {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var c releaseCheck
	if json.Unmarshal(data, &c) != nil || c.Release.Version == "" {
		return nil, false
	}
	age := now.Sub(c.CheckedAt)
	if age < 0 || age >= releaseCheckTTL {
		return nil, false
	}
	return &c.Release, true
}

// storeRelease writes rel to the release-check cache, best-effort.
func storeRelease(rel *Release, now time.Time) {
	path, err := releaseCheckPath()
	if err != nil {
		return
	}
	data, err := json.Marshal(releaseCheck{CheckedAt: now, Release: *rel})
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "update-check.json.tmp-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || safeio.Replace(tmp.Name(), path) != nil {
		_ = os.Remove(tmp.Name())
	}
}
