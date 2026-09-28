package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func startReleaseServer(t *testing.T, tag string) *atomic.Int64 {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"tag_name":"` + tag + `","assets":[]}`))
	}))
	t.Cleanup(srv.Close)
	orig := apiLatestURL
	apiLatestURL = srv.URL
	t.Cleanup(func() { apiLatestURL = orig })
	return &hits
}

// Launches within releaseCheckTTL reuse the first check.
func TestLatestReleaseIsCachedForADay(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	hits := startReleaseServer(t, "v9.0.0")

	for range 3 {
		rel, err := LatestRelease(context.Background())
		if err != nil || rel.Version != "v9.0.0" {
			t.Fatalf("LatestRelease = %+v, %v", rel, err)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("three checks made %d requests, want 1", n)
	}
}

func TestReleaseCacheExpires(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Now()
	storeRelease(&Release{Version: "v1.0.0"}, now.Add(-releaseCheckTTL-time.Minute))
	if _, ok := cachedRelease(now); ok {
		t.Fatal("an expired check was served from the cache")
	}
	storeRelease(&Release{Version: "v1.0.0"}, now.Add(time.Hour))
	if _, ok := cachedRelease(now); ok {
		t.Fatal("a check dated in the future was served from the cache")
	}
	storeRelease(&Release{Version: "v1.0.0"}, now.Add(-time.Hour))
	if rel, ok := cachedRelease(now); !ok || rel.Version != "v1.0.0" {
		t.Fatalf("fresh check not served: %+v %v", rel, ok)
	}
}

// An explicit check ignores a fresh cache and refreshes it.
func TestRefreshLatestReleaseBypassesCache(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	storeRelease(&Release{Version: "v1.0.0"}, time.Now())
	hits := startReleaseServer(t, "v2.0.0")

	rel, err := RefreshLatestRelease(context.Background())
	if err != nil || rel.Version != "v2.0.0" {
		t.Fatalf("RefreshLatestRelease = %+v, %v", rel, err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
	if cached, ok := cachedRelease(time.Now()); !ok || cached.Version != "v2.0.0" {
		t.Fatalf("cache not refreshed: %+v %v", cached, ok)
	}
}
