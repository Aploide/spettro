package chatcompletions

import (
	"io"
	"slices"
)

// Body is a request body assembled from byte chunks without copying them
// into one buffer: most chunks are per-message encodings shared with the
// provider's encoder cache, so a step that adds two messages to a
// 500-message history writes the unchanged 1 MB from where it already is.
//
// The chunks must never be modified once handed to a Body: the HTTP
// transport may still be reading them after the request returned.
type Body struct {
	chunks [][]byte
	size   int64
}

// Grow makes room for n more chunks.
func (b *Body) Grow(n int) {
	b.chunks = slices.Grow(b.chunks, n)
}

// Append adds a chunk. Empty chunks are ignored.
func (b *Body) Append(p []byte) {
	if len(p) == 0 {
		return
	}
	b.chunks = append(b.chunks, p)
	b.size += int64(len(p))
}

// AppendString adds a constant chunk such as ToolsEnd.
func (b *Body) AppendString(s string) {
	b.Append([]byte(s))
}

// Len is the total body size in bytes.
func (b *Body) Len() int64 { return b.size }

// Bytes returns the body as one contiguous slice (tests and debugging).
func (b *Body) Bytes() []byte {
	out := make([]byte, 0, b.size)
	for _, c := range b.chunks {
		out = append(out, c...)
	}
	return out
}

// Reader returns a fresh reader over the body. Every call starts from the
// beginning, so it also serves as http.Request.GetBody.
func (b *Body) Reader() io.ReadCloser {
	return &bodyReader{chunks: b.chunks}
}

// bodyReader reads a chunk list front to back.
type bodyReader struct {
	chunks [][]byte
	off    int
}

func (r *bodyReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) && len(r.chunks) > 0 {
		c := r.chunks[0][r.off:]
		k := copy(p[n:], c)
		n += k
		r.off += k
		if r.off == len(r.chunks[0]) {
			r.chunks = r.chunks[1:]
			r.off = 0
		}
	}
	if n == 0 && len(r.chunks) == 0 {
		return 0, io.EOF
	}
	return n, nil
}

func (r *bodyReader) Close() error { return nil }
