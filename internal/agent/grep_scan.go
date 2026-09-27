package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"regexp"
	"regexp/syntax"
	"sync"
	"sync/atomic"
)

// The pure-Go grep backend, used when ripgrep is not available.
//
// Measured on a 61k-file / 1.2 GB tree (perf harness TestPerfGrep), the
// previous backend read every file line by line through a string-allocating
// bufio reader on one goroutine: 4.6 s and 4.5 GB allocated for a full scan,
// 14.8 s case-insensitive. This one
//
//   - reads each file whole into a per-worker buffer (after an 8 KB head
//     that decides whether it is binary) and tests the whole buffer first:
//     only a file that can match is split into lines;
//   - answers plain literals with bytes.Index (SIMD) instead of the regexp,
//     and case-insensitive ASCII literals by lower-casing the buffer once;
//   - searches fileWorkers() files at a time.
//
// Results are still reported in walk order, and a content search still
// stops once max_results matches are certain: see grepWithWalk.

// grepMatcher decides whether text matches a grep query. It is built once
// per call and shared read-only by the workers.
type grepMatcher struct {
	re *regexp.Regexp // the query regexp, applied to one line at a time
	// whole is re in multi-line mode, applied to a whole file first: any line
	// match is also a match of the file in that mode (^ and $ match at line
	// boundaries), so a file it rejects has no matching line. nil when the
	// pattern uses \A or \z, which no longer mean the line's ends there.
	whole *regexp.Regexp
	// literal is set when the pattern is a plain string: containment is
	// then the whole test.
	literal []byte
	// foldLiteral is set for a case-insensitive pattern that is a plain
	// ASCII string: lower-cased, it is looked for in lower-cased text.
	// foldRisky marks one holding k or s, whose (?i) matches also include
	// the Kelvin sign and the long s; a file holding those is searched with
	// the regexp instead.
	foldLiteral []byte
	foldRisky   bool
}

// foldPartners are the non-ASCII runes (?i) treats as k or s.
var foldPartners = [][]byte{[]byte("K"), []byte("ſ")}

func newGrepMatcher(q grepQuery) *grepMatcher {
	m := &grepMatcher{re: q.re}
	parsed, err := syntax.Parse(q.re.String(), syntax.Perl)
	if err != nil {
		return m
	}
	parsed = parsed.Simplify()
	if parsed.Op == syntax.OpLiteral {
		switch lit := string(parsed.Rune); {
		case parsed.Flags&syntax.FoldCase == 0:
			m.literal = []byte(lit)
		case isASCII(lit):
			m.foldLiteral = lowerASCII(nil, []byte(lit))
			m.foldRisky = bytes.ContainsAny(m.foldLiteral, "ks")
		}
	}
	// In multi-line mode ^ and $ parse as line anchors, so what is left as
	// a text anchor was written as \A or \z.
	wholeSrc := "(?m)" + q.re.String()
	if multi, err := syntax.Parse(wholeSrc, syntax.Perl); err == nil && !usesTextAnchors(multi) {
		m.whole, _ = regexp.Compile(wholeSrc)
	}
	return m
}

// usesTextAnchors reports whether re contains a beginning- or end-of-text
// anchor.
func usesTextAnchors(re *syntax.Regexp) bool {
	if re.Op == syntax.OpBeginText || re.Op == syntax.OpEndText {
		return true
	}
	for _, sub := range re.Sub {
		if usesTextAnchors(sub) {
			return true
		}
	}
	return false
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// lowerASCII appends src with ASCII letters lower-cased to dst[:0]; other
// bytes, UTF-8 sequences included, are copied as they are, so offsets in
// the result are offsets in src.
func lowerASCII(dst, src []byte) []byte {
	if cap(dst) < len(src) {
		dst = make([]byte, len(src))
	}
	dst = dst[:len(src)]
	for i, c := range src {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		dst[i] = c
	}
	return dst
}

// fileScan is one file prepared for matching: its text and, for a folded
// literal, the lower-cased copy (same offsets).
type fileScan struct {
	m        *grepMatcher
	lower    []byte
	useRegex bool // folded literal, but the file holds a fold partner
}

// prepare decides whether data can match at all, and readies per-line
// matching when it can. lowerBuf is scratch space for the lower-cased copy.
func (m *grepMatcher) prepare(data, lowerBuf []byte) (fileScan, bool) {
	fs := fileScan{m: m}
	switch {
	case m.literal != nil:
		return fs, bytes.Contains(data, m.literal)
	case m.foldLiteral != nil:
		if m.foldRisky && (bytes.Contains(data, foldPartners[0]) || bytes.Contains(data, foldPartners[1])) {
			fs.useRegex = true
			return fs, m.whole == nil || m.whole.Match(data)
		}
		fs.lower = lowerASCII(lowerBuf, data)
		return fs, bytes.Contains(fs.lower, m.foldLiteral)
	case m.whole != nil:
		return fs, m.whole.Match(data)
	}
	return fs, true
}

// matchLine tests the line data[start:end].
func (fs fileScan) matchLine(data []byte, start, end int) bool {
	switch {
	case fs.m.literal != nil:
		return bytes.Contains(data[start:end], fs.m.literal)
	case fs.m.foldLiteral != nil && !fs.useRegex:
		return bytes.Contains(fs.lower[start:end], fs.m.foldLiteral)
	}
	return fs.m.re.Match(data[start:end])
}

// grepCollector gathers one file's result as its lines are tested, keeping
// context lines around matches in content mode.
type grepCollector struct {
	q      *grepQuery
	budget int // content mode: stop after budget+1 matches (and their context)
	fr     grepFileResult
	before []grepLine // context lines not yet printed, at most q.context
	after  int        // context lines still owed after the last match
}

// add records line num; it reports whether the file needs no more lines.
func (c *grepCollector) add(num int, text []byte, match bool) (done bool) {
	content := c.q.mode == "content"
	switch {
	case match:
		c.fr.count++
		if content {
			c.fr.lines = append(c.fr.lines, c.before...)
			c.before = c.before[:0]
			c.fr.lines = append(c.fr.lines, grepLine{num: num, text: string(text), match: true})
			c.after = c.q.context
		}
	case !content:
	case c.after > 0:
		c.fr.lines = append(c.fr.lines, grepLine{num: num, text: string(text)})
		c.after--
	case c.q.context > 0:
		if len(c.before) == c.q.context {
			c.before = append(c.before[:0], c.before[1:]...)
		}
		c.before = append(c.before, grepLine{num: num, text: string(text)})
	}
	return content && c.fr.count > c.budget && c.after == 0
}

// grepWorker is one search goroutine's reusable state.
type grepWorker struct {
	m     *grepMatcher
	q     *grepQuery
	buf   []byte
	lower []byte
}

// maxRetainedGrepBuffer bounds the per-worker buffers kept between files;
// a bigger file gets a buffer of its own, dropped afterwards.
const maxRetainedGrepBuffer = 2 << 20

// searchFile searches one file of a walk (never an explicit path: those are
// streamed by grepStream); ok is false when it has no match or is not a
// searchable text file. In content mode it stops after budget+1 matches
// (and their context).
func (w *grepWorker) searchFile(ctx context.Context, abs, rel string, budget int) (grepFileResult, bool, error) {
	f, err := os.Open(abs)
	if err != nil {
		return grepFileResult{}, false, nil
	}
	data, ok := w.readText(f)
	f.Close()
	if !ok {
		return grepFileResult{}, false, nil
	}
	fs, can := w.m.prepare(data, w.lower)
	if fs.lower != nil {
		w.lower = retainBuffer(fs.lower)
	}
	if !can {
		return grepFileResult{}, false, nil
	}
	c := grepCollector{q: w.q, budget: budget, fr: grepFileResult{path: rel}}
	start := 0
	for num := 1; start < len(data); num++ {
		if num%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return grepFileResult{}, false, err
			}
		}
		end := len(data)
		next := end
		if i := bytes.IndexByte(data[start:], '\n'); i >= 0 {
			end = start + i
			next = end + 1
		}
		if c.add(num, data[start:end], fs.matchLine(data, start, end)) {
			break
		}
		start = next
	}
	return c.fr, c.fr.count > 0, nil
}

// readText reads a walked file into the worker's buffer. ok is false for a
// file over maxSearchFileBytes or one that looks binary (decided on its
// first binarySniffBytes, before the rest is read).
func (w *grepWorker) readText(f *os.File) ([]byte, bool) {
	info, err := f.Stat()
	if err != nil || info.Size() > maxSearchFileBytes {
		return nil, false
	}
	size := int(info.Size())
	buf := w.buf[:0]
	if cap(buf) < size+1 {
		buf = make([]byte, 0, size+1)
	}
	head, _ := io.ReadFull(f, buf[:min(size, binarySniffBytes)])
	buf = buf[:head]
	if looksBinary(buf) {
		w.buf = retainBuffer(buf)
		return nil, false
	}
	// Read to EOF: the file may have grown since the stat.
	for {
		if len(buf) == cap(buf) {
			if len(buf) > maxSearchFileBytes {
				w.buf = retainBuffer(buf)
				return nil, false
			}
			buf = append(buf, 0)[:len(buf)]
		}
		n, err := f.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err != nil {
			break
		}
	}
	w.buf = retainBuffer(buf)
	return buf, true
}

// retainBuffer returns buf for reuse, or nil when it is too big to keep.
func retainBuffer(buf []byte) []byte {
	if cap(buf) > maxRetainedGrepBuffer {
		return nil
	}
	return buf[:0]
}

// grepWindowPerWorker sets how far the search may run ahead of the file
// the results wait on: fileWorkers() times this many files. Unbounded, the
// walk and the workers kept going while the ordered commit waited on one
// slow file, so an early max_results cut had already searched (and held
// the matches of) thousands of files: a 15 MB first file followed by 20k
// matching files cost 104 ms of CPU and +25 MB of heap against 32 ms and
// +2 MB for the sequential search (perf review, TestReviewGrepLookahead).
const grepWindowPerWorker = 16

// grepWalkStats counts a search's work, for tests.
type grepWalkStats struct {
	searched atomic.Int64 // files opened and searched
}

// grepWithWalk is the pure-Go grep backend.
//
// Ordering guarantee: the walk numbers files in walk order and hands them to
// fileWorkers() goroutines; their results are committed strictly in that
// order, so the reported files, and where max_results cuts them, are the
// same as a one-file-at-a-time search. The search stops (cancelling the
// walk and the workers) once the committed files hold more than max_results
// matches (content mode) or files (the other modes); the files after that
// point that were already searched are discarded.
//
// Bounded look-ahead: the walk hands out file number n only once file
// n - window has been committed (window = fileWorkers() *
// grepWindowPerWorker), so at most window files are searched or held past
// the one the commit waits on. In content mode each file also stops after
// max_results minus the matches committed when it was handed out (plus
// one, to detect the cut): enough, since the matches committed before it
// can only have grown by the time it is committed.
func (r *toolRuntime) grepWithWalk(ctx context.Context, q grepQuery) ([]grepFileResult, error) {
	return r.grepWithWalkStats(ctx, q, &grepWalkStats{})
}

func (r *toolRuntime) grepWithWalkStats(ctx context.Context, q grepQuery, stats *grepWalkStats) ([]grepFileResult, error) {
	if q.rootIsFile {
		fr, ok, err := grepStream(ctx, q.root, q.rootRel, q, q.max, true)
		if err != nil || !ok {
			return nil, err
		}
		return []grepFileResult{fr}, nil
	}
	m := newGrepMatcher(q)
	sctx, stop := context.WithCancel(ctx)
	defer stop()

	type job struct {
		seq      int
		abs, rel string
		budget   int // content mode: matches this file may still need
	}
	type done struct {
		seq int
		fr  grepFileResult
		ok  bool
		err error
	}
	workers := fileWorkers()
	window := workers * grepWindowPerWorker
	jobs := make(chan job, window)
	dones := make(chan done, window)
	// slots holds one token per file handed out but not yet committed.
	slots := make(chan struct{}, window)
	var committedMatches atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := grepWorker{m: m, q: &q}
			for j := range jobs {
				var d done
				if sctx.Err() == nil {
					stats.searched.Add(1)
					fr, ok, err := w.searchFile(sctx, j.abs, j.rel, j.budget)
					d = done{seq: j.seq, fr: fr, ok: ok, err: err}
				} else {
					d = done{seq: j.seq, err: sctx.Err()}
				}
				dones <- d
			}
		}()
	}
	var walkErr error
	go func() {
		defer close(jobs)
		seq := 0
		walkErr = r.newWorkspaceWalker().walk(sctx, q.root, func(abs, rel string, _ fs.DirEntry) error {
			if !q.wantsFile(rel) {
				return nil
			}
			select {
			case slots <- struct{}{}:
			case <-sctx.Done():
				return sctx.Err()
			}
			j := job{seq: seq, abs: abs, rel: rel, budget: q.max - int(committedMatches.Load())}
			select {
			case jobs <- j:
				seq++
				return nil
			case <-sctx.Done():
				return sctx.Err()
			}
		})
	}()
	go func() {
		wg.Wait()
		close(dones)
	}()

	var results []grepFileResult
	pending := map[int]done{}
	next, total, stopped := 0, 0, false
	for d := range dones {
		if stopped {
			continue // draining: the answer is complete
		}
		pending[d.seq] = d
		for !stopped {
			p, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++
			<-slots
			if p.err != nil || !p.ok {
				continue
			}
			results = append(results, p.fr)
			total += p.fr.count
			committedMatches.Store(int64(total))
			if q.mode == "content" && total > q.max || q.mode != "content" && len(results) > q.max {
				stopped = true
				stop()
			}
		}
	}
	// dones is closed only after every worker returned, and a worker only
	// after the walk closed jobs, so walkErr is final here.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if walkErr != nil && !stopped && !errors.Is(walkErr, context.Canceled) {
		return nil, walkErr
	}
	return results, nil
}

// grepReaders pools the 64 KB readers grepStream uses.
var grepReaders = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 64*1024) }}

// grepStream searches one file line by line without holding it in memory:
// the path the model named explicitly, which, like ripgrep, is searched
// whatever its size and even if it looks binary (explicit), so "no matches"
// never hides a skipped file. ok is false when it has no match. In content
// mode it stops reading after budget+1 matches (the caller trims to the
// budget and reports the truncation).
func grepStream(ctx context.Context, abs, rel string, q grepQuery, budget int, explicit bool) (grepFileResult, bool, error) {
	f, err := os.Open(abs)
	if err != nil {
		return grepFileResult{}, false, nil
	}
	defer f.Close()
	if !explicit {
		if info, err := f.Stat(); err != nil || info.Size() > maxSearchFileBytes {
			return grepFileResult{}, false, nil
		}
	}
	br := grepReaders.Get().(*bufio.Reader)
	br.Reset(f)
	defer func() {
		br.Reset(nil)
		grepReaders.Put(br)
	}()
	if !explicit {
		if head, _ := br.Peek(binarySniffBytes); looksBinary(head) {
			return grepFileResult{}, false, nil
		}
	}
	c := grepCollector{q: &q, budget: budget, fr: grepFileResult{path: rel}}
	var long []byte // a line longer than the reader's buffer, pieced together
	for num := 1; ; num++ {
		if num%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return grepFileResult{}, false, err
			}
		}
		chunk, readErr := br.ReadSlice('\n')
		if readErr == bufio.ErrBufferFull {
			long = append(long, chunk...)
			num--
			continue
		}
		line := chunk
		if long != nil {
			line = append(long, chunk...)
			long = nil
		}
		if len(line) == 0 && readErr != nil {
			break
		}
		line = bytes.TrimSuffix(line, []byte{'\n'})
		if c.add(num, line, q.re.Match(line)) || readErr != nil {
			break
		}
	}
	return c.fr, c.fr.count > 0, nil
}
