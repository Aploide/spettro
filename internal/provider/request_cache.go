package provider

import (
	"container/list"
	"encoding/base64"
	"encoding/json"
	"os"
	"sync"
	"time"
	"unsafe"
)

// Images and tool schemas ride on every request of a conversation, but they
// only change when the user attaches a new image or the tool surface
// changes. Re-reading and base64-encoding five 400 KB screenshots cost
// about 2 ms and 5 MB per request at 100 messages, and re-parsing ~17 KB
// of tool schemas about 60 us and 1,200 allocations; both caches below turn
// that into a stat call and a map lookup (see BenchmarkSendWithImages and
// BenchmarkBuildFantasyCall).

const (
	// mediaCacheLimit bounds the bytes the media cache holds. 6 MB keeps
	// eleven 400 KB screenshots as data URLs; a session whose images pass
	// it re-reads and re-encodes the least recently used ones (about 1 ms
	// each), as every request did before the cache. With
	// encoderCacheLimit it stays under the performance plan's 15 MB
	// memory-regression allowance.
	mediaCacheLimit = 6 << 20
	// maxMediaEntries bounds the number of entries, some of which may hold
	// nothing yet (see mediaCache.entry).
	maxMediaEntries = 256
	// mediaIdle is how long an unused image stays cached, so the images of
	// a finished or cleared conversation do not stay in memory for the rest
	// of the process.
	mediaIdle = 5 * time.Minute
)

// mediaFile is one cached image file. Each representation is kept only
// once a caller asked for it: the native client needs only the data URL,
// the fantasy SDK's file parts only the raw bytes (holding both for every
// image cost 2.3 times the file size).
type mediaFile struct {
	path    string
	size    int64
	mtime   time.Time
	lastUse time.Time
	// raw is the file content, kept once bytes asked for it. It is shared:
	// callers must not modify it.
	raw []byte
	// literal is the image's data URL as a JSON string literal,
	// "\"data:<media type>;base64,<data>\"", kept once urlLiteral or base64
	// asked for it; the base64 part starts at b64Start. Never modified once
	// stored: request bodies and base64's strings point into it.
	literal  []byte
	b64Start int
}

func (f *mediaFile) cost() int { return len(f.raw) + len(f.literal) }

// mediaCache caches image files attached to requests.
//
// What: each file's bytes and its data URL, each built on first use. Key:
// the file path, valid while the file's size and modification time match
// what was cached (checked with a stat on every use). Invalidation: an
// entry whose file changed is replaced; least recently used entries are
// evicted once the total passes mediaCacheLimit; entries unused for
// mediaIdle are dropped by a timer. Owner: shared by every goroutine that
// builds requests and by the idle timer's goroutine; mu guards the entry
// list and every entry's fields. Stored contents are never modified, so
// callers use them after mu is released.
type mediaCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element // path -> element holding *mediaFile
	lru     list.List                // front: most recently used
	total   int
	limit   int
	idle    *time.Timer // pending idle sweep, nil when none
}

// requestMedia is the process-wide media cache.
var requestMedia = &mediaCache{limit: mediaCacheLimit}

// entry returns the cache entry for path, creating an empty one when the
// file is not cached or changed since it was. ok is false when path is not
// a regular file.
func (c *mediaCache) entry(path string) (*mediaFile, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if el, hit := c.entries[path]; hit {
		f := el.Value.(*mediaFile)
		if f.size == info.Size() && f.mtime.Equal(info.ModTime()) {
			f.lastUse = now
			c.lru.MoveToFront(el)
			return f, true
		}
		c.removeLocked(el)
	}
	f := &mediaFile{path: path, size: info.Size(), mtime: info.ModTime(), lastUse: now}
	if c.entries == nil {
		c.entries = map[string]*list.Element{}
	}
	c.entries[path] = c.lru.PushFront(f)
	c.evictLocked()
	c.armIdleLocked()
	return f, true
}

// read reads f's file. cacheable is false when the file no longer has the
// size f was created for (it was written to after the stat): the content
// is then served but not stored.
func (f *mediaFile) read() (data []byte, cacheable bool, ok bool) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		return nil, false, false
	}
	return data, int64(len(data)) == f.size, true
}

// store runs set, which fills an unset field of f and returns the bytes it
// added, and charges them to the cache while f is still cached.
func (c *mediaCache) store(f *mediaFile, set func() int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	added := set()
	if el, cached := c.entries[f.path]; cached && el.Value == f {
		c.total += added
		c.evictLocked()
	}
}

// bytes returns the content of the image at path.
func (c *mediaCache) bytes(path string) ([]byte, bool) {
	f, ok := c.entry(path)
	if !ok {
		return nil, false
	}
	c.mu.Lock()
	raw := f.raw
	c.mu.Unlock()
	if raw != nil {
		return raw, true
	}
	raw, cacheable, ok := f.read()
	if !ok {
		return nil, false
	}
	if cacheable {
		c.store(f, func() int {
			if f.raw != nil {
				return 0
			}
			f.raw = raw
			return len(raw)
		})
	}
	return raw, true
}

// urlLiteral returns the image at path as the JSON string literal of its
// base64 data URL, and the offset of the base64 part in it.
func (c *mediaCache) urlLiteral(path string) (literal []byte, b64Start int, ok bool) {
	f, ok := c.entry(path)
	if !ok {
		return nil, 0, false
	}
	c.mu.Lock()
	literal, b64Start, raw := f.literal, f.b64Start, f.raw
	c.mu.Unlock()
	if literal != nil {
		return literal, b64Start, true
	}
	cacheable := true
	if raw == nil {
		if raw, cacheable, ok = f.read(); !ok {
			return nil, 0, false
		}
	}
	literal, b64Start = dataURLLiteral(mediaTypeFromPath(path), raw)
	if cacheable {
		c.store(f, func() int {
			if f.literal != nil {
				return 0
			}
			f.literal, f.b64Start = literal, b64Start
			return len(literal)
		})
	}
	return literal, b64Start, true
}

// base64 returns the image at path base64-encoded.
func (c *mediaCache) base64(path string) (string, bool) {
	literal, start, ok := c.urlLiteral(path)
	if !ok {
		return "", false
	}
	n := len(literal) - 1 - start // the base64 part, without the closing quote
	if n == 0 {
		return "", true
	}
	// A string view of the cached literal, which is never modified: a copy
	// would cost the image's size again on every request of the fantasy
	// path (533 KB per 400 KB screenshot).
	return unsafe.String(&literal[start], n), true
}

// dataURLLiteral encodes data as the JSON string literal of a data URL,
// "\"data:<mediaType>;base64,<base64>\"", and returns the offset of the
// base64 part. The media types mediaTypeFromPath returns and base64 need
// no escaping, so the literal is the URL between two quotes
// (TestDataURLLiteralIsJSON checks it against encoding/json).
func dataURLLiteral(mediaType string, data []byte) ([]byte, int) {
	prefix := `"data:` + mediaType + ";base64,"
	literal := make([]byte, len(prefix)+base64.StdEncoding.EncodedLen(len(data))+1)
	copy(literal, prefix)
	base64.StdEncoding.Encode(literal[len(prefix):], data)
	literal[len(literal)-1] = '"'
	return literal, len(prefix)
}

func (c *mediaCache) removeLocked(el *list.Element) {
	f := el.Value.(*mediaFile)
	c.lru.Remove(el)
	delete(c.entries, f.path)
	c.total -= f.cost()
}

// evictLocked drops least recently used entries until the cache fits its
// limits. The newest entry is kept even when it alone is over the limit.
func (c *mediaCache) evictLocked() {
	for (c.total > c.limit || c.lru.Len() > maxMediaEntries) && c.lru.Len() > 1 {
		c.removeLocked(c.lru.Back())
	}
}

// armIdleLocked schedules an idle sweep while entries remain and none is
// pending.
func (c *mediaCache) armIdleLocked() {
	if c.idle == nil && c.lru.Len() > 0 {
		c.idle = time.AfterFunc(mediaIdle, c.onIdleTimer)
	}
}

// onIdleTimer runs on the timer's goroutine. An entry is dropped between
// mediaIdle and twice that after its last use.
func (c *mediaCache) onIdleTimer() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.idle = nil
	c.dropIdleLocked(time.Now())
	c.armIdleLocked()
}

// dropIdleLocked drops the entries last used more than mediaIdle before
// now. The list is in last-use order, so they are all at its back.
func (c *mediaCache) dropIdleLocked(now time.Time) {
	cutoff := now.Add(-mediaIdle)
	for c.lru.Len() > 0 {
		back := c.lru.Back()
		if !back.Value.(*mediaFile).lastUse.Before(cutoff) {
			return
		}
		c.removeLocked(back)
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
