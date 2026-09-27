package indexer

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"spettro/internal/safeio"
)

// The symbol cache on disk (<project>/.spettro/cache/symbols.idx) is a
// stream of unsigned varints and length-prefixed strings, written and read
// through buffered I/O one file entry at a time:
//
//	magic "spettro-symbols\n", version
//	root
//	kind count, kind names
//	file count, then per file:
//	    path, mtime (UnixNano, zig-zag), size
//	    text, symbol count, then per symbol:
//	        line, sigStart, sigEnd-sigStart, nameStart, nameEnd-nameStart, kind
//	"end"
//
// It replaces a single gob message of the whole index, which encoded into
// one in-memory buffer and decoded through a whole-file read: saving the
// perf corpus's index allocated 446 MB and loading it 550 MB (432 MB peak
// RSS) for a 70 MB file. Streamed, both allocate little more than the
// index itself (BenchmarkCacheRoundTrip).
const (
	cacheMagic   = "spettro-symbols\n"
	cacheVersion = 3 // 1 was symbols.json, 2 symbols.gob
	cacheEnd     = "end"
	// maxCacheString bounds a string read from the cache: an entry's text
	// holds at most the lines of one file under maxFileSize.
	maxCacheString = 64 << 20
)

// legacyCacheNames are the files earlier versions cached the index in,
// removed once the current cache is written next to them.
var legacyCacheNames = []string{"symbols.json", "symbols.gob"}

// cachedFile is one entry of a cache snapshot.
type cachedFile struct {
	path    string
	entries *fileSymbols
}

// writeCache stores the snapshot at path atomically (a temporary file,
// then a rename). Best-effort persistence: a failed write just means a
// cold rebuild next session, never a failed search.
func writeCache(path, root string, files []cachedFile) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	w := cacheWriter{w: bufio.NewWriterSize(tmp, 256<<10)}
	w.encode(root, files)
	if w.err != nil {
		return w.err
	}
	if err := w.w.Flush(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := safeio.Replace(tmp.Name(), path); err != nil {
		return err
	}
	for _, name := range legacyCacheNames {
		if legacy := filepath.Join(filepath.Dir(path), name); legacy != path {
			_ = os.Remove(legacy)
		}
	}
	return nil
}

// cacheWriter writes the cache format; the first error sticks and later
// writes do nothing.
type cacheWriter struct {
	w   *bufio.Writer
	buf [binary.MaxVarintLen64]byte
	err error
}

func (c *cacheWriter) encode(root string, files []cachedFile) {
	c.raw(cacheMagic)
	c.uint(cacheVersion)
	c.string(root)
	// Every kind id in the snapshot was assigned before this table was
	// loaded, so it covers them all.
	var kinds []string
	if t := kindTable.Load(); t != nil {
		kinds = t.names
	}
	c.uint(uint64(len(kinds)))
	for _, k := range kinds {
		c.string(k)
	}
	c.uint(uint64(len(files)))
	for _, f := range files {
		e := f.entries
		c.string(f.path)
		c.uint(zigzag(e.modTime))
		c.uint(uint64(e.size))
		c.string(e.text)
		c.uint(uint64(len(e.syms)))
		for _, p := range e.syms {
			c.uint(uint64(p.line))
			c.uint(uint64(p.sigStart))
			c.uint(uint64(p.sigEnd - p.sigStart))
			c.uint(uint64(p.nameStart))
			c.uint(uint64(p.nameEnd - p.nameStart))
			c.uint(uint64(p.kind))
		}
	}
	c.string(cacheEnd)
}

func (c *cacheWriter) uint(v uint64) {
	if c.err == nil {
		n := binary.PutUvarint(c.buf[:], v)
		_, c.err = c.w.Write(c.buf[:n])
	}
}

func (c *cacheWriter) raw(s string) {
	if c.err == nil {
		_, c.err = c.w.WriteString(s)
	}
}

func (c *cacheWriter) string(s string) {
	c.uint(uint64(len(s)))
	c.raw(s)
}

// zigzag maps a signed value to an unsigned one with small magnitudes
// staying small (mtimes before 1970 are possible on some filesystems).
func zigzag(v int64) uint64   { return uint64(v<<1) ^ uint64(v>>63) }
func unzigzag(v uint64) int64 { return int64(v>>1) ^ -int64(v&1) }

// errBadCache reports a cache that is not in the current format, belongs
// to another root, or is damaged.
var errBadCache = errors.New("symbol cache unusable")

// readCache loads the cache at path, written for root. Any error means the
// cache is unusable (the index then builds from scratch).
func readCache(path, root string) (map[string]*fileSymbols, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := cacheReader{r: bufio.NewReaderSize(f, 256<<10)}
	files := r.decode(root)
	if r.err != nil {
		return nil, r.err
	}
	return files, nil
}

// cacheReader reads the cache format; the first error sticks and later
// reads return zero values.
type cacheReader struct {
	r   *bufio.Reader
	buf []byte
	err error
}

func (c *cacheReader) decode(root string) map[string]*fileSymbols {
	if c.raw(len(cacheMagic)) != cacheMagic || c.uint() != cacheVersion || c.string() != root {
		c.fail(errBadCache)
		return nil
	}
	// The file's kind ids are mapped to this process's ids.
	nKinds := c.count(0xffff + 1)
	kindIDs := make([]uint16, nKinds)
	for i := range kindIDs {
		kindIDs[i] = kindID(c.string())
	}
	n := c.count(1 << 24)
	files := make(map[string]*fileSymbols, n)
	for range n {
		path := c.string()
		e := &fileSymbols{modTime: unzigzag(c.uint()), size: int64(c.uint()), text: c.string()}
		e.syms = make([]packedSymbol, c.count(maxCacheString))
		for i := range e.syms {
			p := packedSymbol{line: int32(c.uint())}
			p.sigStart = uint32(c.uint())
			p.sigEnd = p.sigStart + uint32(c.uint())
			p.nameStart = uint32(c.uint())
			p.nameEnd = p.nameStart + uint32(c.uint())
			if k := c.uint(); k < uint64(len(kindIDs)) {
				p.kind = kindIDs[k]
			} else {
				c.fail(errBadCache)
			}
			e.syms[i] = p
		}
		if c.err != nil {
			return nil
		}
		if !e.valid(kindCount()) {
			c.fail(errBadCache)
			return nil
		}
		e.index()
		files[path] = e
	}
	if c.string() != cacheEnd {
		c.fail(errBadCache)
	}
	return files
}

func (c *cacheReader) fail(err error) {
	if c.err == nil {
		c.err = err
	}
}

func (c *cacheReader) uint() uint64 {
	if c.err != nil {
		return 0
	}
	v, err := binary.ReadUvarint(c.r)
	if err != nil {
		c.fail(fmt.Errorf("%w: %v", errBadCache, err))
	}
	return v
}

// count reads a length and checks it against limit.
func (c *cacheReader) count(limit int) int {
	v := c.uint()
	if v > uint64(limit) {
		c.fail(errBadCache)
		return 0
	}
	return int(v)
}

// raw reads exactly n bytes as a string (the one allocation per string the
// load makes; the read buffer is reused).
func (c *cacheReader) raw(n int) string {
	if c.err != nil {
		return ""
	}
	if cap(c.buf) < n {
		c.buf = make([]byte, n)
	}
	c.buf = c.buf[:n]
	if _, err := io.ReadFull(c.r, c.buf); err != nil {
		c.fail(fmt.Errorf("%w: %v", errBadCache, err))
		return ""
	}
	return string(c.buf)
}

func (c *cacheReader) string() string {
	return c.raw(c.count(maxCacheString))
}
