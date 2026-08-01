// Package update implements the CLI's self-update: checking GitHub for the
// latest release and, on request, downloading and installing it in place of
// the running binary.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Repo is the GitHub "owner/name" this CLI publishes releases to.
const Repo = "aploide/spettro"

const apiLatestURL = "https://api.github.com/repos/" + Repo + "/releases/latest"

// Asset is a single downloadable file attached to a GitHub release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// Release is the subset of the GitHub release API this package needs.
type Release struct {
	Version string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

func httpClient() *http.Client { return &http.Client{Timeout: 15 * time.Second} }

// LatestRelease fetches the newest published GitHub release for Repo.
func LatestRelease(ctx context.Context) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiLatestURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "spettro-cli")

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github release check failed (HTTP %d)", resp.StatusCode)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

// asset returns the release asset matching name, if any.
func (r *Release) asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// IsNewer reports whether latest denotes a strictly greater version than
// current. Malformed versions (including the "dev" build tag) are treated as
// never newer, so a from-source build is never flagged and a bad tag never
// triggers an update.
func IsNewer(current, latest string) bool {
	c, ok1 := parseVersion(current)
	l, ok2 := parseVersion(latest)
	if !ok1 || !ok2 {
		return false
	}
	for i := range 3 {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

// parseVersion parses "vMAJOR[.MINOR[.PATCH]]" (with an optional
// "-prerelease" suffix, which is ignored) into a 3-component tuple.
func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" {
		return out, false
	}
	v = strings.SplitN(v, "-", 2)[0]
	fields := strings.Split(v, ".")
	if len(fields) == 0 || len(fields) > 3 {
		return out, false
	}
	for i, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
