package models

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

const testCatalogDoc = `{"version":1,"updated":"2026-07-01","providers":{
	"anthropic":{"name":"Anthropic","api":"anthropic","base_url":"https://api.anthropic.com","env":"ANTHROPIC_API_KEY",
		"models":{"claude-fable-5":{"name":"Fable 5","context":200000}}}}}`

// catalogServer serves testCatalogDoc, answering 304 to a conditional
// request when notModified is set, and counts requests.
type catalogServer struct {
	requests        atomic.Int64
	ifModifiedSince atomic.Value // string
	notModified     bool
}

func startCatalogServer(t *testing.T, s *catalogServer) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		ims := r.Header.Get("If-Modified-Since")
		s.ifModifiedSince.Store(ims)
		if s.notModified && ims != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(testCatalogDoc))
	}))
	t.Cleanup(srv.Close)
	orig := catalogURL
	catalogURL = srv.URL
	t.Cleanup(func() { catalogURL = orig })
}

func writeCache(t *testing.T, home string, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(home, ".spettro", "catalog.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(testCatalogDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

// Without a cache, Load serves the embedded snapshot and makes no request.
func TestLoadWithoutCacheUsesSnapshotOffline(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := &catalogServer{}
	startCatalogServer(t, srv)

	cat, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Providers) == 0 || cat.Updated == "" {
		t.Fatalf("snapshot catalog is empty: %+v", cat)
	}
	if n := srv.requests.Load(); n != 0 {
		t.Fatalf("Load made %d network requests, want 0", n)
	}
}

func TestSnapshotDecodes(t *testing.T) {
	cat, err := Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.DateOnly, cat.Updated); err != nil {
		t.Fatalf("snapshot updated date %q: %v", cat.Updated, err)
	}
	for id, p := range cat.Providers {
		if p.API != APIOpenAI && p.API != APIAnthropic {
			t.Errorf("provider %s has unknown api %q", id, p.API)
		}
	}
}

func TestRefreshSkipsFreshCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCache(t, home, time.Now().Add(-time.Minute))
	srv := &catalogServer{}
	startCatalogServer(t, srv)

	refreshIfStale(func(Catalog) { t.Error("onRefresh called for a fresh cache") })
	if n := srv.requests.Load(); n != 0 {
		t.Fatalf("fresh cache: %d requests, want 0", n)
	}
}

func TestRefreshStaleCacheIsConditional(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stale := time.Now().Add(-2 * cacheTTL).Truncate(time.Second)
	path := writeCache(t, home, stale)
	srv := &catalogServer{notModified: true}
	startCatalogServer(t, srv)

	refreshIfStale(func(Catalog) { t.Error("onRefresh called for an unchanged catalog") })
	if n := srv.requests.Load(); n != 1 {
		t.Fatalf("stale cache: %d requests, want 1", n)
	}
	ims, _ := srv.ifModifiedSince.Load().(string)
	if got, err := http.ParseTime(ims); err != nil || !got.Equal(stale) {
		t.Fatalf("If-Modified-Since = %q, want %v", ims, stale)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(info.ModTime()) > time.Minute {
		t.Fatalf("304 did not restart the cache TTL (mtime %v)", info.ModTime())
	}
}

func TestRefreshWithoutCacheDownloadsAndReports(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srv := &catalogServer{notModified: true}
	startCatalogServer(t, srv)

	var got Catalog
	refreshIfStale(func(c Catalog) { got = c })
	if ims, _ := srv.ifModifiedSince.Load().(string); ims != "" {
		t.Fatalf("request without a cache sent If-Modified-Since %q", ims)
	}
	if _, ok := got.Providers["anthropic"]; !ok {
		t.Fatalf("onRefresh got %+v", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".spettro", "catalog.json")); err != nil {
		t.Fatalf("cache not written: %v", err)
	}
}

// TestEmbeddedCatalogFreshness fails when the embedded snapshot is more
// than 30 days older than the live catalog. It needs the network, so it runs
// only when SPETTRO_CATALOG_FRESHNESS_CHECK=1 (the CI workflow sets it).
func TestEmbeddedCatalogFreshness(t *testing.T) {
	if os.Getenv("SPETTRO_CATALOG_FRESHNESS_CHECK") != "1" {
		t.Skip("set SPETTRO_CATALOG_FRESHNESS_CHECK=1 to compare the snapshot with the live catalog")
	}
	snap, err := Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(catalogURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var live Catalog
	if err := json.NewDecoder(resp.Body).Decode(&live); err != nil {
		t.Fatal(err)
	}
	snapDate, err1 := time.Parse(time.DateOnly, snap.Updated)
	liveDate, err2 := time.Parse(time.DateOnly, live.Updated)
	if err1 != nil || err2 != nil {
		t.Fatalf("dates: snapshot %q (%v), live %q (%v)", snap.Updated, err1, live.Updated, err2)
	}
	if age := liveDate.Sub(snapDate); age > 30*24*time.Hour {
		t.Fatalf("embedded catalog snapshot (%s) is %d days older than the live catalog (%s); run go generate ./internal/models",
			snap.Updated, int(age.Hours()/24), live.Updated)
	}
}
