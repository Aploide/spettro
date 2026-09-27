package models

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"spettro/internal/safeio"
)

// cacheTTL is how long a catalog the server has confirmed stays fresh. Once
// it has passed, the background refresh asks the server again, with the
// validators the server sent last time, so an unchanged catalog costs one
// round trip and no download.
const cacheTTL = 6 * time.Hour

// refreshInterval is how often a long-running process runs a refresh round
// (see refresher.check). A round without a request costs a read and a hash
// of the 65 KB cache file.
const refreshInterval = time.Hour

// fetchTimeout bounds one catalog request.
const fetchTimeout = 10 * time.Second

// cacheMeta is the companion of the disk cache, ~/.spettro/catalog-meta.json.
//
// What it caches: the validators (ETag, Last-Modified) the server sent with
// the copy in catalog.json, and when a spettro process last confirmed that
// copy with the server. Key: the copy it describes, by content hash
// (CacheSHA256); a meta file whose hash does not match catalog.json (the cache
// was rewritten by an older spettro, or removed by spettro clean) is ignored,
// so a validator is never sent for content it does not describe.
// Invalidation: rewritten after every successful request. Ownership: shared
// by every spettro process; each write replaces the file by rename, and
// catalog.json is always written before its meta file, so a reader that sees
// a new catalog with the old meta ignores the meta and at worst downloads the
// catalog once more.
type cacheMeta struct {
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	CheckedAt    time.Time `json:"checked_at"`
	CacheSHA256  string    `json:"cache_sha256"`
}

// metaFile returns the path of the cache's companion file.
func metaFile() (string, error) {
	path, err := cacheFile()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "catalog-meta.json"), nil
}

// readMeta returns the meta file when it describes the cache whose hash is
// sum.
func readMeta(sum [sha256.Size]byte) (cacheMeta, bool) {
	path, err := metaFile()
	if err != nil {
		return cacheMeta{}, false
	}
	data, err := safeio.ReadFile(path)
	if err != nil {
		return cacheMeta{}, false
	}
	var meta cacheMeta
	if json.Unmarshal(data, &meta) != nil || meta.CacheSHA256 != fmt.Sprintf("%x", sum) {
		return cacheMeta{}, false
	}
	return meta, true
}

// LoadAndRefresh loads the catalog as Load does and hands it to apply on the
// calling goroutine, so the caller has a catalog before it returns. It then
// keeps the catalog current in the background for the life of the process:
// see refresher for what a refresh round does. apply runs again, on the
// background goroutine, each time a different catalog is found.
func LoadAndRefresh(apply func(Catalog)) {
	r := &refresher{apply: apply, now: time.Now}
	if cached, err := readCache(); err == nil {
		r.applied = cached.sum
		apply(cached.catalog)
	} else if cat, err := Snapshot(); err == nil {
		apply(cat)
	}
	go func() {
		r.check()
		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()
		for range ticker.C {
			r.check()
		}
	}()
}

// refresher keeps one process's catalog in step with the disk cache, which
// every spettro process shares, and with the server. It belongs to the
// goroutine LoadAndRefresh starts; nothing else reads or writes it.
type refresher struct {
	apply func(Catalog)
	now   func() time.Time
	// applied is the SHA-256 of the catalog.json content last handed to
	// apply, or zero while the process runs on the embedded snapshot.
	applied [sha256.Size]byte
}

// check runs one refresh round:
//
//  1. When catalog.json holds a different catalog than the one this process
//     applied (another spettro process downloaded a newer one), apply it.
//  2. When the server confirmed that copy less than cacheTTL ago, stop: no
//     request.
//  3. Otherwise ask the server, conditionally when the meta file has the
//     server's validators for that copy. A new catalog replaces the cache and
//     is applied; a 304 only records the confirmation.
//
// Failures (offline, a server error, an unwritable cache) leave everything as
// it was; the next round tries again.
func (r *refresher) check() {
	cached, cacheErr := readCache()
	var meta cacheMeta
	haveMeta := false
	if cacheErr == nil {
		r.applyIfNew(cached.catalog, cached.sum)
		meta, haveMeta = readMeta(cached.sum)
		checkedAt := cached.modTime // a cache written before meta files existed
		if haveMeta {
			checkedAt = meta.CheckedAt
		}
		if age := r.now().Sub(checkedAt); age >= 0 && age < cacheTTL {
			return
		}
	}

	var validators cacheMeta
	if haveMeta {
		validators = meta
	}
	res, err := fetch(validators)
	if err != nil {
		return
	}
	if res.notModified {
		meta.CheckedAt = r.now()
		meta.ETag = firstNonEmpty(res.etag, meta.ETag)
		meta.LastModified = firstNonEmpty(res.lastModified, meta.LastModified)
		_ = writeMeta(meta)
		return
	}
	sum := sha256.Sum256(res.body)
	if writeCacheFile(res.body) == nil {
		_ = writeMeta(cacheMeta{
			ETag:         res.etag,
			LastModified: res.lastModified,
			CheckedAt:    r.now(),
			CacheSHA256:  fmt.Sprintf("%x", sum),
		})
	}
	r.applyIfNew(res.catalog, sum)
}

// applyIfNew hands cat to apply unless it is the catalog applied last.
func (r *refresher) applyIfNew(cat Catalog, sum [sha256.Size]byte) {
	if sum == r.applied {
		return
	}
	r.applied = sum
	if r.apply != nil {
		r.apply(cat)
	}
}

// fetchResult is the outcome of one catalog request.
type fetchResult struct {
	// notModified reports a 304: the cached copy is current.
	notModified bool
	// body and catalog are the downloaded document (a 200 only).
	body    []byte
	catalog Catalog
	// etag and lastModified are the validators the server sent, if any.
	etag, lastModified string
}

// fetch requests the catalog. Validators from a previous response make the
// request conditional (If-None-Match, If-Modified-Since, with the server's
// own values rather than a local clock reading); a zero cacheMeta makes it
// unconditional.
func fetch(validators cacheMeta) (fetchResult, error) {
	req, err := http.NewRequest(http.MethodGet, catalogURL, nil)
	if err != nil {
		return fetchResult{}, fmt.Errorf("catalog fetch: %w", err)
	}
	conditional := validators.ETag != "" || validators.LastModified != ""
	if validators.ETag != "" {
		req.Header.Set("If-None-Match", validators.ETag)
	}
	if validators.LastModified != "" {
		req.Header.Set("If-Modified-Since", validators.LastModified)
	}
	resp, err := (&http.Client{Timeout: fetchTimeout}).Do(req)
	if err != nil {
		return fetchResult{}, fmt.Errorf("catalog fetch: %w", err)
	}
	defer resp.Body.Close()

	res := fetchResult{etag: resp.Header.Get("ETag"), lastModified: resp.Header.Get("Last-Modified")}
	switch {
	case resp.StatusCode == http.StatusNotModified && conditional:
		res.notModified = true
		return res, nil
	case resp.StatusCode != http.StatusOK:
		return fetchResult{}, fmt.Errorf("catalog fetch: status %d", resp.StatusCode)
	}
	if res.body, err = io.ReadAll(resp.Body); err != nil {
		return fetchResult{}, fmt.Errorf("catalog read: %w", err)
	}
	if res.catalog, err = parseCatalog(res.body); err != nil {
		return fetchResult{}, err
	}
	return res, nil
}

// Fetch downloads the catalog unconditionally and updates the disk cache.
func Fetch() (Catalog, error) {
	res, err := fetch(cacheMeta{})
	if err != nil {
		return Catalog{}, err
	}
	if writeCacheFile(res.body) == nil {
		_ = writeMeta(cacheMeta{
			ETag:         res.etag,
			LastModified: res.lastModified,
			CheckedAt:    time.Now(),
			CacheSHA256:  fmt.Sprintf("%x", sha256.Sum256(res.body)),
		})
	}
	return res.catalog, nil
}

// writeCacheFile replaces catalog.json with body.
func writeCacheFile(body []byte) error {
	path, err := cacheFile()
	if err != nil {
		return err
	}
	return replaceFile(path, body)
}

// writeMeta replaces the meta file with meta.
func writeMeta(meta cacheMeta) error {
	path, err := metaFile()
	if err != nil {
		return err
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return replaceFile(path, data)
}

// replaceFile writes data to a temporary file next to path and renames it
// over path, so a concurrent reader (another spettro process) sees the old
// or the new content, never a partial file.
func replaceFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := safeio.Replace(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// firstNonEmpty returns a when it is set and b otherwise.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
