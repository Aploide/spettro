package tui

import "unsafe"

// draftText is the growing text of a live stream draft. Appending to a
// string with += copied the whole draft on every token (quadratic in the
// answer's length, and that much garbage); draftText appends to a byte
// buffer and hands out strings that share it, as strings.Builder does.
//
// This is safe because the buffer is append-only: bytes a string already
// covers are never written again (a full buffer is replaced by a larger
// copy, the old one staying alive for the strings that use it).
//
// It is owned by one ChatMessage and used only from the Update goroutine. A
// copy of the message shares it, so append checks that the caller's text is
// still the buffer's whole content and starts a new buffer when it is not
// (see appendTo).
type draftText struct {
	buf []byte
}

// appendTo returns content+delta. When content is the draft's current text
// the delta is appended in place; otherwise (a new draft, or a copy of the
// message that fell behind) the draft restarts from content.
func (d *draftText) appendTo(content, delta string) string {
	if len(d.buf) != len(content) || (len(content) > 0 && unsafe.StringData(content) != &d.buf[0]) {
		d.buf = append(make([]byte, 0, 2*(len(content)+len(delta))), content...)
	}
	d.buf = append(d.buf, delta...)
	if len(d.buf) == 0 {
		return ""
	}
	return unsafe.String(&d.buf[0], len(d.buf))
}
