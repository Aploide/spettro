package chatcompletions

import (
	"bufio"
	"bytes"
	"io"
	"sync"
)

// maxLineSize bounds one SSE line (32 MB, the limit the OpenAI SDK uses), so
// a very large single-chunk tool call still decodes.
const maxLineSize = bufio.MaxScanTokenSize << 9

// readBufferSize is the initial read buffer. The scanner's default 4 KB
// means a read system call per 4 KB of reply, and a 50 KB tool call
// streamed as 12,000 events is 2.6 MB of reply.
const readBufferSize = 64 << 10

// readBuffers recycles the initial read buffers of finished streams, so a
// short reply does not cost a fresh 64 KB allocation. Owner: any goroutine;
// a buffer belongs to one EventReader between NewEventReader and Release.
var readBuffers = sync.Pool{New: func() any {
	b := make([]byte, readBufferSize)
	return &b
}}

// EventReader splits a text/event-stream body into events. It follows the
// OpenAI SDK's decoder line for line, so both clients see the same events:
// an event is dispatched on an empty line; "data:" lines are joined, each
// followed by a newline; comment lines (": keep-alive") are skipped; an
// event not terminated by an empty line before EOF is dropped.
//
// It is used by one goroutine.
type EventReader struct {
	scanner *bufio.Scanner
	buf     *[]byte
	data    bytes.Buffer
	event   []byte
	err     error
}

// NewEventReader reads events from r. Call Release when done.
func NewEventReader(r io.Reader) *EventReader {
	buf := readBuffers.Get().(*[]byte)
	s := bufio.NewScanner(r)
	s.Buffer(*buf, maxLineSize)
	return &EventReader{scanner: s, buf: buf}
}

// Release returns the reader's buffer for reuse. The reader, and any
// slice Data returned, must not be used afterwards.
func (e *EventReader) Release() {
	if e.buf != nil {
		readBuffers.Put(e.buf)
		e.buf = nil
		e.scanner = nil
	}
}

// Next advances to the next event and reports whether there is one. The
// data returned by Data is valid until the following call.
func (e *EventReader) Next() bool {
	if e.err != nil || e.scanner == nil {
		return false
	}
	e.data.Reset()
	for e.scanner.Scan() {
		line := e.scanner.Bytes()
		if len(line) == 0 {
			e.event = e.data.Bytes()
			return true
		}
		name, value, _ := bytes.Cut(line, []byte(":"))
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		if string(name) == "data" {
			e.data.Write(value)
			e.data.WriteByte('\n')
		}
	}
	e.err = e.scanner.Err()
	return false
}

// Data is the current event's data: its data lines joined, each followed
// by a newline.
func (e *EventReader) Data() []byte { return e.event }

// Err is the read error that ended the stream, or nil at a clean EOF.
func (e *EventReader) Err() error { return e.err }
