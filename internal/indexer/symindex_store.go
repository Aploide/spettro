package indexer

import (
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// fileSymbols is the index entry for one source file: its symbols as of the
// file's modification time and size.
//
// The symbols are packed: their signatures (and any name not inside its
// signature) are concatenated into one string, and each symbol keeps
// offsets into it. A Symbol per definition, with five string headers and
// the path repeated in each, held 100 MB for the 42k Go files (549k
// symbols) of the 60k-file perf corpus; packed, the same index holds about
// half that (TestPackedEntriesStayCompact guards the per-symbol size).
//
// Entries are immutable once built: a change replaces the entry, so the
// map can be snapshotted (and entries compared by pointer) while lookups
// go on.
type fileSymbols struct {
	modTime int64 // UnixNano
	size    int64
	text    string
	syms    []packedSymbol
	// names is "\n" + each symbol's lower-cased name + "\n"..., so one
	// strings.Contains tells whether the file can hold a match. Derived,
	// not persisted.
	names string
}

// packedSymbol is one definition: its line, kind and the offsets of its
// signature and name in the entry's text.
type packedSymbol struct {
	line               int32
	sigStart, sigEnd   uint32
	nameStart, nameEnd uint32
	kind               uint16 // see kindID
}

// newFileSymbols packs the extracted symbols of a file.
func newFileSymbols(modTime, size int64, syms []Symbol) *fileSymbols {
	var text strings.Builder
	packed := make([]packedSymbol, len(syms))
	for i, s := range syms {
		p := packedSymbol{line: int32(s.Line), kind: kindID(s.Kind)}
		p.sigStart = uint32(text.Len())
		text.WriteString(s.Signature)
		p.sigEnd = uint32(text.Len())
		// The name is normally inside the signature (any occurrence will
		// do: the substring is the same). A custom extractor may return
		// one that is not; it is then stored after the signature.
		if at := strings.Index(s.Signature, s.Name); at >= 0 {
			p.nameStart = p.sigStart + uint32(at)
		} else {
			p.nameStart = uint32(text.Len())
			text.WriteString(s.Name)
		}
		p.nameEnd = p.nameStart + uint32(len(s.Name))
		packed[i] = p
	}
	fs := &fileSymbols{modTime: modTime, size: size, text: text.String(), syms: packed}
	fs.index()
	return fs
}

// index fills names.
func (fs *fileSymbols) index() {
	var b strings.Builder
	for i := range fs.syms {
		b.WriteByte('\n')
		b.WriteString(strings.ToLower(fs.name(i)))
	}
	b.WriteByte('\n')
	fs.names = b.String()
}

func (fs *fileSymbols) name(i int) string {
	return fs.text[fs.syms[i].nameStart:fs.syms[i].nameEnd]
}

// symbol unpacks symbol i of the file at path. The strings share the
// entry's memory; nothing is copied.
func (fs *fileSymbols) symbol(path string, i int) Symbol {
	p := fs.syms[i]
	return Symbol{
		Path:      path,
		Line:      int(p.line),
		Kind:      kindName(p.kind),
		Name:      fs.text[p.nameStart:p.nameEnd],
		Signature: fs.text[p.sigStart:p.sigEnd],
	}
}

// sameAs reports whether two entries hold the same file state and symbols.
func (fs *fileSymbols) sameAs(o *fileSymbols) bool {
	return fs.modTime == o.modTime && fs.size == o.size && fs.text == o.text && slices.Equal(fs.syms, o.syms)
}

// valid reports whether every offset lies inside text and every kind id
// is below kinds (a cache read from disk is checked before use).
func (fs *fileSymbols) valid(kinds int) bool {
	n := uint32(len(fs.text))
	for _, p := range fs.syms {
		if p.sigStart > p.sigEnd || p.sigEnd > n || p.nameStart > p.nameEnd || p.nameEnd > n || int(p.kind) >= kinds {
			return false
		}
	}
	return true
}

// kindTable interns symbol kinds ("func", "type", ...) so each packed
// symbol stores a small id instead of a string header.
//
// Cache: maps a kind to its id and back. Key: the kind string. Never
// invalidated: ids are only meaningful within a process (the disk cache
// stores kind names). Any goroutine may use it: readers load the current
// immutable table without locking (the parse workers look up a kind per
// symbol, 549k times for the perf corpus); a new kind, which is rare, is
// added under kindMu by publishing a copy.
var (
	kindTable atomic.Pointer[kindSet]
	kindMu    sync.Mutex
)

type kindSet struct {
	names []string
	ids   map[string]uint16
}

// kindID returns the id of kind, assigning one on first use. Past 65535
// distinct kinds (the built-in extractors use six) further kinds share the
// last id.
func kindID(kind string) uint16 {
	if t := kindTable.Load(); t != nil {
		if id, ok := t.ids[kind]; ok {
			return id
		}
	}
	kindMu.Lock()
	defer kindMu.Unlock()
	old := kindTable.Load()
	if old == nil {
		old = &kindSet{ids: map[string]uint16{}}
	}
	if id, ok := old.ids[kind]; ok {
		return id
	}
	if len(old.names) > 0xffff {
		return 0xffff
	}
	id := uint16(len(old.names))
	next := &kindSet{names: append(slices.Clip(old.names), kind), ids: maps.Clone(old.ids)}
	next.ids[kind] = id
	kindTable.Store(next)
	return id
}

func kindName(id uint16) string { return kindTable.Load().names[id] }

// kindCount is how many kinds have ids.
func kindCount() int {
	if t := kindTable.Load(); t != nil {
		return len(t.names)
	}
	return 0
}
