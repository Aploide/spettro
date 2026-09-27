package provider

import (
	"container/list"
	"encoding/base64"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Images and tool schemas ride on every request of a conversation, but they
// only change when the user attaches a new image or the tool surface
// changes. Re-reading and base64-encoding five 400 KB screenshots cost
// about 2 ms and 5 MB per request at 100 messages, and re-parsing ~17 KB
// of tool schemas about 60 us and 1,200 allocations; both caches below turn
// that into a stat call and a map lookup (see BenchmarkSendWithImages and
// BenchmarkBuildFantasyCall).

// mediaCacheLimit bounds the bytes the media cache holds (raw file bytes
// plus their data-URL encodings).
const mediaCacheLimit = 64 << 20

// mediaFile is one cached image file.
type mediaFile struct {
	path  string
	size  int64
	mtime time.Time
	// data is the raw file content. It is shared: callers must not modify
	// it.
	data []byte
	// dataURL is "data:<media type>;base64,<data>", built on first use;
	// its base64 part is dataURL[b64Start:].
	dataURL  string
	b64Start int
}

func (f *mediaFile) cost() int { return len(f.data) + len(f.dataURL) }

// mediaCache caches image files attached to requests.
//
// What: each file's bytes and, once needed, their base64 data URL. Key: the
// file path, valid while the file's size and modification time match what
// was cached (checked with a stat on every use). Invalidation: an entry
// whose file changed is replaced; least recently used entries are evicted
// once the total passes mediaCacheLimit. Owner: shared by every goroutine
// that builds requests; mu guards it.
type mediaCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element // path -> element holding *mediaFile
	lru     list.List                // front: most recently used
	total   int
	limit   int
}

// requestMedia is the process-wide media cache.
var requestMedia = &mediaCache{limit: mediaCacheLimit}

// file returns the cached entry for path, reading the file when it is not
// cached or changed since. ok is false when the file cannot be read.
func (c *mediaCache) file(path string) (*mediaFile, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	c.mu.Lock()
	if el, hit := c.entries[path]; hit {
		f := el.Value.(*mediaFile)
		if f.size == info.Size() && f.mtime.Equal(info.ModTime()) {
			c.lru.MoveToFront(el)
			c.mu.Unlock()
			return f, true
		}
		c.removeLocked(el)
	}
	c.mu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	f := &mediaFile{path: path, size: info.Size(), mtime: info.ModTime(), data: data}
	if int64(len(data)) != info.Size() {
		// Written to between the stat and the read: serve it uncached.
		return f, true
	}
	c.mu.Lock()
	c.insertLocked(f)
	c.mu.Unlock()
	return f, true
}

// bytes returns the content of the image at path.
func (c *mediaCache) bytes(path string) ([]byte, bool) {
	f, ok := c.file(path)
	if !ok {
		return nil, false
	}
	return f.data, true
}

// dataURL returns the image at path as a base64 data URL and the offset of
// its base64 part.
func (c *mediaCache) dataURL(path string) (url string, b64Start int, ok bool) {
	f, ok := c.file(path)
	if !ok {
		return "", 0, false
	}
	c.mu.Lock()
	url, b64Start = f.dataURL, f.b64Start
	c.mu.Unlock()
	if url != "" {
		return url, b64Start, true
	}
	prefix := "data:" + mediaTypeFromPath(path) + ";base64,"
	buf := make([]byte, len(prefix)+base64.StdEncoding.EncodedLen(len(f.data)))
	copy(buf, prefix)
	base64.StdEncoding.Encode(buf[len(prefix):], f.data)
	url, b64Start = string(buf), len(prefix)
	c.mu.Lock()
	if f.dataURL == "" {
		f.dataURL, f.b64Start = url, b64Start
		if el, cached := c.entries[f.path]; cached && el.Value == f {
			c.total += len(url)
			c.evictLocked()
		}
	}
	c.mu.Unlock()
	return url, b64Start, true
}

// base64 returns the image at path base64-encoded.
func (c *mediaCache) base64(path string) (string, bool) {
	url, start, ok := c.dataURL(path)
	if !ok {
		return "", false
	}
	return url[start:], true
}

func (c *mediaCache) insertLocked(f *mediaFile) {
	if c.entries == nil {
		c.entries = map[string]*list.Element{}
	}
	if el, dup := c.entries[f.path]; dup {
		c.removeLocked(el)
	}
	c.entries[f.path] = c.lru.PushFront(f)
	c.total += f.cost()
	c.evictLocked()
}

func (c *mediaCache) removeLocked(el *list.Element) {
	f := el.Value.(*mediaFile)
	c.lru.Remove(el)
	delete(c.entries, f.path)
	c.total -= f.cost()
}

// evictLocked drops least recently used entries until the cache fits its
// limit. The newest entry is kept even when it alone is over the limit.
func (c *mediaCache) evictLocked() {
	for c.total > c.limit && c.lru.Len() > 1 {
		c.removeLocked(c.lru.Back())
	}
}

// maxParsedSchemas bounds the schema cache; Spettro's tool surfaces use a
// few dozen distinct schemas.
const maxParsedSchemas = 1024

// schemaCache caches tool input schemas parsed into maps for the fantasy
// SDK's FunctionTool.
//
// What: the parsed map, or nil when the schema is not a JSON object. Key:
// the schema bytes themselves (as a string; a lookup with a []byte key does
// not allocate). Invalidation: none is needed, a key is its own content;
// the whole cache is dropped when it passes maxParsedSchemas entries.
// Owner: shared by every goroutine that builds requests; mu guards it.
//
// The maps are shared between requests, so nothing may modify them; the
// fantasy providers only read InputSchema.
type schemaCache struct {
	mu     sync.Mutex
	parsed map[string]map[string]any
}

var toolSchemas = &schemaCache{}

// parse returns schema parsed as a JSON object, or nil when it is not one.
func (c *schemaCache) parse(schema json.RawMessage) map[string]any {
	c.mu.Lock()
	parsed, ok := c.parsed[string(schema)]
	c.mu.Unlock()
	if ok {
		return parsed
	}
	return c.parseAndStore(schema)
}

// parseAndStore is parse's miss path, kept apart so the hit path does not
// allocate (the unmarshal target escapes).
func (c *schemaCache) parseAndStore(schema json.RawMessage) map[string]any {
	var parsed map[string]any
	if err := json.Unmarshal(schema, &parsed); err != nil {
		parsed = nil
	}
	c.mu.Lock()
	if c.parsed == nil || len(c.parsed) >= maxParsedSchemas {
		c.parsed = map[string]map[string]any{}
	}
	c.parsed[string(schema)] = parsed
	c.mu.Unlock()
	return parsed
}
