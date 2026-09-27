package models

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testCatalogDoc = `{"version":1,"updated":"2026-07-01","providers":{
	"anthropic":{"name":"Anthropic","api":"anthropic","base_url":"https://api.anthropic.com","env":"ANTHROPIC_API_KEY",
		"models":{"claude-fable-5":{"name":"Fable 5","context":200000}}}}}`

// Validators the test server sends with every catalog response.
const (
	testETag         = `W/"catalog-v1"`
	testLastModified = "Wed, 01 Jul 2026 10:00:00 GMT"
)

// catalogServer serves doc (testCatalogDoc when empty) with testETag and
// testLastModified, answers 304 to a conditional request when notModified is
// set, and records the requests.
type catalogServer struct {
	doc             string
	notModified     bool
	requests        atomic.Int64
	ifNoneMatch     atomic.Value // string
	ifModifiedSince atomic.Value // string
}

func startCatalogServer(t *testing.T, s *catalogServer) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		inm, ims := r.Header.Get("If-None-Match"), r.Header.Get("If-Modified-Since")
		s.ifNoneMatch.Store(inm)
		s.ifModifiedSince.Store(ims)
		w.Header().Set("ETag", testETag)
		w.Header().Set("Last-Modified", testLastModified)
		if s.notModified && (inm != "" || ims != "") {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		doc := s.doc
		if doc == "" {
			doc = testCatalogDoc
		}
		_, _ = w.Write([]byte(doc))
	}))
	t.Cleanup(srv.Close)
	orig := catalogURL
	catalogURL = srv.URL
	t.Cleanup(func() { catalogURL = orig })
}

// writeCache writes doc as the disk cache with the given modification time
// and returns its path.
func writeCache(t *testing.T, home, doc string, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(home, ".spettro", "catalog.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeTestMeta writes a meta file describing doc, confirmed at checkedAt,
// with the test server's validators.
func writeTestMeta(t *testing.T, doc string, checkedAt time.Time) {
	t.Helper()
	meta := cacheMeta{
		ETag:         testETag,
		LastModified: testLastModified,
		CheckedAt:    checkedAt,
		CacheSHA256:  fmt.Sprintf("%x", sha256.Sum256([]byte(doc))),
	}
	if err := writeMeta(meta); err != nil {
		t.Fatal(err)
	}
}

// readTestMeta returns the meta file as written.
func readTestMeta(t *testing.T, home string) cacheMeta {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".spettro", "catalog-meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta cacheMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	return meta
}

// newTestRefresher returns a refresher that has applied doc (none when doc
// is empty) and records every later apply.
func newTestRefresher(doc string, applied *[]Catalog) *refresher {
	r := &refresher{now: time.Now, apply: func(c Catalog) { *applied = append(*applied, c) }}
	if doc != "" {
		r.applied = sha256.Sum256([]byte(doc))
	}
	return r
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

// LoadAndRefresh hands the cached catalog over before it returns.
func TestLoadAndRefreshAppliesCacheSynchronously(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCache(t, home, testCatalogDoc, time.Now())
	writeTestMeta(t, testCatalogDoc, time.Now())
	startCatalogServer(t, &catalogServer{})

	var got []Catalog
	LoadAndRefresh(func(c Catalog) { got = append(got, c) })
	if len(got) != 1 || got[0].Providers["anthropic"].Name != "Anthropic" {
		t.Fatalf("applied before return: %+v", got)
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

// A cache the server confirmed within cacheTTL costs no request, whether the
// confirmation is in the meta file or, for a cache written before meta files
// existed, in the file's modification time.
func TestRefreshSkipsFreshCache(t *testing.T) {
	for _, withMeta := range []bool{true, false} {
		t.Run(fmt.Sprintf("meta=%v", withMeta), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			writeCache(t, home, testCatalogDoc, time.Now().Add(-time.Minute))
			if withMeta {
				writeTestMeta(t, testCatalogDoc, time.Now().Add(-time.Minute))
			}
			srv := &catalogServer{}
			startCatalogServer(t, srv)

			var applied []Catalog
			newTestRefresher(testCatalogDoc, &applied).check()
			if n := srv.requests.Load(); n != 0 {
				t.Fatalf("fresh cache: %d requests, want 0", n)
			}
			if len(applied) != 0 {
				t.Fatalf("the catalog already applied was applied again")
			}
		})
	}
}

// A stale cache is revalidated with the server's own ETag and Last-Modified,
// not a local clock reading, and a 304 restarts the TTL without a download.
func TestRefreshStaleCacheSendsServerValidators(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCache(t, home, testCatalogDoc, time.Now().Add(-2*cacheTTL))
	writeTestMeta(t, testCatalogDoc, time.Now().Add(-2*cacheTTL))
	srv := &catalogServer{notModified: true}
	startCatalogServer(t, srv)

	var applied []Catalog
	newTestRefresher(testCatalogDoc, &applied).check()
	if n := srv.requests.Load(); n != 1 {
		t.Fatalf("stale cache: %d requests, want 1", n)
	}
	if inm, _ := srv.ifNoneMatch.Load().(string); inm != testETag {
		t.Errorf("If-None-Match = %q, want %q", inm, testETag)
	}
	if ims, _ := srv.ifModifiedSince.Load().(string); ims != testLastModified {
		t.Errorf("If-Modified-Since = %q, want the server's %q", ims, testLastModified)
	}
	if len(applied) != 0 {
		t.Errorf("a 304 applied a catalog")
	}
	if meta := readTestMeta(t, home); time.Since(meta.CheckedAt) > time.Minute {
		t.Errorf("304 did not restart the TTL (checked_at %v)", meta.CheckedAt)
	}
}

// A meta file that describes other content (catalog.json was rewritten by a
// spettro without meta support) is ignored: its validators would vouch for
// content the cache no longer holds.
func TestRefreshIgnoresMetaForOtherContent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCache(t, home, testCatalogDoc, time.Now().Add(-2*cacheTTL))
	writeTestMeta(t, "some older catalog", time.Now())
	srv := &catalogServer{notModified: true}
	startCatalogServer(t, srv)

	var applied []Catalog
	newTestRefresher(testCatalogDoc, &applied).check()
	if n := srv.requests.Load(); n != 1 {
		t.Fatalf("%d requests, want 1 (the mismatched meta must not make the cache fresh)", n)
	}
	inm, _ := srv.ifNoneMatch.Load().(string)
	ims, _ := srv.ifModifiedSince.Load().(string)
	if inm != "" || ims != "" {
		t.Fatalf("sent validators %q / %q from a meta file for other content", inm, ims)
	}
	if meta := readTestMeta(t, home); meta.CacheSHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(testCatalogDoc))) {
		t.Fatalf("meta not rewritten for the downloaded content: %+v", meta)
	}
}

// A long-running process applies a catalog another spettro process
// downloaded into the shared cache, without a request of its own.
func TestRefreshAppliesCatalogFromAnotherProcess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	newer := strings.Replace(testCatalogDoc, "Fable 5", "Fable 6", 1)
	writeCache(t, home, newer, time.Now())
	writeTestMeta(t, newer, time.Now())
	srv := &catalogServer{}
	startCatalogServer(t, srv)

	var applied []Catalog
	newTestRefresher(testCatalogDoc, &applied).check()
	if n := srv.requests.Load(); n != 0 {
		t.Fatalf("%d requests, want 0", n)
	}
	if len(applied) != 1 || applied[0].Providers["anthropic"].Models["claude-fable-5"].Name != "Fable 6" {
		t.Fatalf("applied = %+v, want the catalog on disk", applied)
	}
}

func TestRefreshWithoutCacheDownloadsAndReports(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srv := &catalogServer{notModified: true}
	startCatalogServer(t, srv)

	var applied []Catalog
	newTestRefresher("", &applied).check()
	if ims, _ := srv.ifModifiedSince.Load().(string); ims != "" {
		t.Fatalf("request without a cache sent If-Modified-Since %q", ims)
	}
	if len(applied) != 1 {
		t.Fatalf("applied %d catalogs, want 1", len(applied))
	}
	if _, ok := applied[0].Providers["anthropic"]; !ok {
		t.Fatalf("onRefresh got %+v", applied[0])
	}
	if _, err := os.Stat(filepath.Join(home, ".spettro", "catalog.json")); err != nil {
		t.Fatalf("cache not written: %v", err)
	}
	if meta := readTestMeta(t, home); meta.ETag != testETag || meta.LastModified != testLastModified {
		t.Fatalf("meta did not keep the server's validators: %+v", meta)
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
