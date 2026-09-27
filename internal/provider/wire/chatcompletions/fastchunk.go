package chatcompletions

import (
	"bytes"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// decodeChunkFast decodes the common shape of a chunk without reflection.
// encoding/json decodes a typical 200-byte chunk at about 65 MB/s here
// (3 us per chunk; a 50 KB tool call streamed in 12,000 fragments spent
// 36 ms in it); this scanner is about four times faster, with one
// allocation per chunk instead of eleven (see BenchmarkDecodeChunk).
//
// It handles well-formed chunks whose members have the expected types and
// returns false for anything else (a syntax error, a mistyped or duplicate
// member, a member name differing only in case, an "error" member), which
// DecodeChunk then hands to encoding/json. When it returns true, c holds
// exactly what encoding/json would have decoded (FuzzDecodeChunkFast
// checks this against the encoding/json path).
func decodeChunkFast(data []byte, c *Chunk) bool {
	prevID := c.ID
	choices := c.Choices[:0]
	*c = Chunk{Choices: choices}
	p := chunkParser{data: data}
	if !p.object(func(key []byte) bool { return p.chunkMember(key, c, prevID) }) {
		return false
	}
	p.ws()
	return p.pos == len(p.data)
}

// Member bits of a chunk object, for duplicate detection.
const (
	memberID = 1 << iota
	memberChoices
	memberUsage
)

// chunkParser is a strict single-pass JSON reader over one chunk.
type chunkParser struct {
	data []byte
	pos  int
	// seen holds the member bits of the object being read (one object
	// level at a time is enough: each parse function saves and restores
	// it).
	seen int
}

func (p *chunkParser) ws() {
	data, i := p.data, p.pos
	for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n' || data[i] == '\r') {
		i++
	}
	p.pos = i
}

// peek returns the next non-space byte, or 0 at the end.
func (p *chunkParser) peek() byte {
	p.ws()
	if p.pos >= len(p.data) {
		return 0
	}
	return p.data[p.pos]
}

func (p *chunkParser) consume(b byte) bool {
	if p.peek() != b {
		return false
	}
	p.pos++
	return true
}

// literal consumes lit ("null", "true", "false").
func (p *chunkParser) literal(lit string) bool {
	p.ws()
	if len(p.data)-p.pos < len(lit) || string(p.data[p.pos:p.pos+len(lit)]) != lit {
		return false
	}
	p.pos += len(lit)
	return true
}

// null consumes a null if one is next.
func (p *chunkParser) null() bool {
	if p.peek() != 'n' {
		return false
	}
	return p.literal("null")
}

// object reads an object, calling member for each member name with the
// reader positioned at the value; member must consume the value.
func (p *chunkParser) object(member func(key []byte) bool) bool {
	saved := p.seen
	p.seen = 0
	defer func() { p.seen = saved }()
	if !p.consume('{') {
		return false
	}
	if p.consume('}') {
		return true
	}
	for {
		key, ok := p.rawString()
		if !ok || !p.consume(':') || !member(key) {
			return false
		}
		if p.consume(',') {
			continue
		}
		return p.consume('}')
	}
}

// array reads an array, calling elem for each element.
func (p *chunkParser) array(elem func() bool) bool {
	if !p.consume('[') {
		return false
	}
	if p.consume(']') {
		return true
	}
	for {
		if !elem() {
			return false
		}
		if p.consume(',') {
			continue
		}
		return p.consume(']')
	}
}

// field claims member bit for the current object; false on a duplicate.
func (p *chunkParser) field(bit int) bool {
	if p.seen&bit != 0 {
		return false
	}
	p.seen |= bit
	return true
}

// other skips the value of a member this decoder does not read. A name
// that matches a known one only case-insensitively is left to
// encoding/json, which matches names that way.
func (p *chunkParser) other(key []byte, known ...string) bool {
	for _, k := range known {
		if strings.EqualFold(string(key), k) {
			return false
		}
	}
	return p.skip()
}

func (p *chunkParser) chunkMember(key []byte, c *Chunk, prevID string) bool {
	switch string(key) {
	case "id":
		if !p.field(memberID) {
			return false
		}
		if p.null() {
			return true
		}
		raw, ok := p.rawStringValue()
		if !ok {
			return false
		}
		if string(raw) == prevID && !containsByte(raw, '\\') {
			// The usual case: every chunk repeats the first one's id.
			c.ID = prevID
			return true
		}
		c.ID, ok = unquote(raw)
		return ok
	case "choices":
		if !p.field(memberChoices) {
			return false
		}
		if p.null() {
			c.Choices = nil
			return true
		}
		return p.array(func() bool {
			var toolCalls []ToolCallDelta
			if i := len(c.Choices); i < cap(c.Choices) {
				toolCalls = c.Choices[:i+1][i].Delta.ToolCalls[:0]
			}
			c.Choices = append(c.Choices, Choice{})
			choice := &c.Choices[len(c.Choices)-1]
			*choice = Choice{Delta: Delta{ToolCalls: toolCalls}}
			return p.choice(choice)
		})
	case "usage":
		if !p.field(memberUsage) {
			return false
		}
		if p.null() {
			return true
		}
		return p.usage(&c.Usage)
	}
	return p.other(key, "id", "choices", "usage", "error")
}

func (p *chunkParser) choice(ch *Choice) bool {
	const (
		memberIndex = 1 << iota
		memberDelta
		memberFinish
	)
	if p.null() {
		ch.Delta.ToolCalls = nil
		return true
	}
	ok := p.object(func(key []byte) bool {
		switch string(key) {
		case "index":
			return p.field(memberIndex) && p.intValue(&ch.Index)
		case "finish_reason":
			return p.field(memberFinish) && p.stringValue(&ch.FinishReason)
		case "delta":
			if !p.field(memberDelta) {
				return false
			}
			if p.null() {
				return true
			}
			return p.delta(&ch.Delta)
		}
		return p.other(key, "index", "finish_reason", "delta")
	})
	if len(ch.Delta.ToolCalls) == 0 {
		ch.Delta.ToolCalls = nil
	}
	return ok
}

func (p *chunkParser) delta(d *Delta) bool {
	const (
		memberContent = 1 << iota
		memberReasoning
		memberToolCalls
	)
	return p.object(func(key []byte) bool {
		switch string(key) {
		case "content":
			return p.field(memberContent) && p.stringValue(&d.Content)
		case "reasoning_content":
			return p.field(memberReasoning) && p.stringValue(&d.ReasoningContent)
		case "tool_calls":
			if !p.field(memberToolCalls) {
				return false
			}
			if p.null() {
				d.ToolCalls = nil
				return true
			}
			return p.array(func() bool {
				d.ToolCalls = append(d.ToolCalls, ToolCallDelta{})
				return p.toolCall(&d.ToolCalls[len(d.ToolCalls)-1])
			})
		}
		return p.other(key, "content", "reasoning_content", "tool_calls")
	})
}

func (p *chunkParser) toolCall(tc *ToolCallDelta) bool {
	const (
		memberIndex = 1 << iota
		memberID
		memberType
		memberFunction
		memberName
		memberArguments
	)
	if p.null() {
		return true
	}
	return p.object(func(key []byte) bool {
		switch string(key) {
		case "index":
			return p.field(memberIndex) && p.intValue(&tc.Index)
		case "id":
			return p.field(memberID) && p.stringValue(&tc.ID)
		case "type":
			return p.field(memberType) && p.stringValue(&tc.Type)
		case "function":
			if !p.field(memberFunction) {
				return false
			}
			if p.null() {
				return true
			}
			return p.object(func(key []byte) bool {
				switch string(key) {
				case "name":
					return p.field(memberName) && p.stringValue(&tc.Function.Name)
				case "arguments":
					return p.field(memberArguments) && p.stringValue(&tc.Function.Arguments)
				}
				return p.other(key, "name", "arguments")
			})
		}
		return p.other(key, "index", "id", "type", "function")
	})
}

func (p *chunkParser) usage(u *Usage) bool {
	const (
		prompt = 1 << iota
		completion
		total
		promptDetails
		completionDetails
		cached
		reasoning
	)
	return p.object(func(key []byte) bool {
		switch string(key) {
		case "prompt_tokens":
			return p.field(prompt) && p.intValue(&u.PromptTokens)
		case "completion_tokens":
			return p.field(completion) && p.intValue(&u.CompletionTokens)
		case "total_tokens":
			return p.field(total) && p.intValue(&u.TotalTokens)
		case "prompt_tokens_details":
			if !p.field(promptDetails) {
				return false
			}
			if p.null() {
				return true
			}
			return p.object(func(key []byte) bool {
				if string(key) == "cached_tokens" {
					return p.field(cached) && p.intValue(&u.PromptTokensDetails.CachedTokens)
				}
				return p.other(key, "cached_tokens")
			})
		case "completion_tokens_details":
			if !p.field(completionDetails) {
				return false
			}
			if p.null() {
				return true
			}
			return p.object(func(key []byte) bool {
				if string(key) == "reasoning_tokens" {
					return p.field(reasoning) && p.intValue(&u.CompletionTokensDetails.ReasoningTokens)
				}
				return p.other(key, "reasoning_tokens")
			})
		}
		return p.other(key, "prompt_tokens", "completion_tokens", "total_tokens", "prompt_tokens_details", "completion_tokens_details")
	})
}

// stringValue reads a string or null into *dst (null leaves it unchanged,
// as encoding/json does).
func (p *chunkParser) stringValue(dst *string) bool {
	if p.null() {
		return true
	}
	raw, ok := p.rawStringValue()
	if !ok {
		return false
	}
	*dst, ok = unquote(raw)
	return ok
}

// intValue reads an integer or null into *dst. Fractions, exponents and
// values past 18 digits are left to encoding/json.
func (p *chunkParser) intValue(dst *int64) bool {
	if p.null() {
		return true
	}
	neg := p.pos < len(p.data) && p.data[p.pos] == '-'
	if neg {
		p.pos++
	}
	digits := p.pos
	var n int64
	for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
		n = n*10 + int64(p.data[p.pos]-'0')
		p.pos++
	}
	count := p.pos - digits
	if count == 0 || count > 18 || (count > 1 && p.data[digits] == '0') {
		return false
	}
	if p.pos < len(p.data) {
		switch p.data[p.pos] {
		case '.', 'e', 'E':
			return false
		}
	}
	if neg {
		n = -n
	}
	*dst = n
	return true
}

// rawStringValue reads a string value and returns its raw content (between
// the quotes, escapes untouched).
func (p *chunkParser) rawStringValue() ([]byte, bool) {
	if p.peek() != '"' {
		return nil, false
	}
	return p.rawString()
}

// stringStop marks the bytes that end a run of plain string content: the
// closing quote, a backslash and the control characters JSON forbids raw.
var stringStop = func() (t [256]bool) {
	for c := range 0x20 {
		t[c] = true
	}
	t['"'], t['\\'] = true, true
	return t
}()

// rawString reads a string (a member name or value), validating it.
func (p *chunkParser) rawString() ([]byte, bool) {
	if !p.consume('"') {
		return nil, false
	}
	start := p.pos
	for p.pos < len(p.data) {
		// Plain content is scanned with a lookup table and local
		// variables (the compiler keeps them in registers, not p.pos):
		// half the decoder's time before (BenchmarkDecodeChunk).
		data, i := p.data, p.pos
		for i < len(data) && !stringStop[data[i]] {
			i++
		}
		p.pos = i
		if p.pos >= len(p.data) {
			break
		}
		c := p.data[p.pos]
		switch {
		case c == '"':
			raw := p.data[start:p.pos]
			p.pos++
			return raw, true
		case c == '\\':
			p.pos++
			if p.pos >= len(p.data) {
				return nil, false
			}
			switch p.data[p.pos] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				p.pos++
			case 'u':
				if p.pos+5 > len(p.data) || !isHex4(p.data[p.pos+1:p.pos+5]) {
					return nil, false
				}
				p.pos += 5
			default:
				return nil, false
			}
		case c < 0x20:
			return nil, false
		default:
			p.pos++
		}
	}
	return nil, false
}

func isHex4(b []byte) bool {
	for _, c := range b {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

// skip reads and discards any valid JSON value.
func (p *chunkParser) skip() bool {
	switch p.peek() {
	case '{':
		return p.object(func([]byte) bool { return p.skip() })
	case '[':
		return p.array(p.skip)
	case '"':
		_, ok := p.rawString()
		return ok
	case 't':
		return p.literal("true")
	case 'f':
		return p.literal("false")
	case 'n':
		return p.literal("null")
	}
	return p.number()
}

// number reads a JSON number.
func (p *chunkParser) number() bool {
	d := p.data
	i := p.pos
	if i < len(d) && d[i] == '-' {
		i++
	}
	switch {
	case i < len(d) && d[i] == '0':
		i++
	case i < len(d) && d[i] >= '1' && d[i] <= '9':
		for i < len(d) && d[i] >= '0' && d[i] <= '9' {
			i++
		}
	default:
		return false
	}
	if i < len(d) && d[i] == '.' {
		i++
		start := i
		for i < len(d) && d[i] >= '0' && d[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	if i < len(d) && (d[i] == 'e' || d[i] == 'E') {
		i++
		if i < len(d) && (d[i] == '+' || d[i] == '-') {
			i++
		}
		start := i
		for i < len(d) && d[i] >= '0' && d[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	p.pos = i
	return true
}

// Values that recur in every chunk, returned without allocating.
var internedStrings = map[string]string{
	"stop": "stop", "length": "length", "tool_calls": "tool_calls", "function": "function",
	"content_filter": "content_filter", "": "",
}

// unquote decodes the raw content of a validated JSON string the way
// encoding/json does, invalid UTF-8 becoming U+FFFD.
func unquote(raw []byte) (string, bool) {
	if !containsByte(raw, '\\') && utf8.Valid(raw) {
		if len(raw) <= len("content_filter") {
			if s, ok := internedStrings[string(raw)]; ok {
				return s, true
			}
		}
		return string(raw), true
	}
	var b strings.Builder
	b.Grow(len(raw))
	for i := 0; i < len(raw); {
		c := raw[i]
		if c == '\\' {
			i++
			switch raw[i] {
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'u':
				r := hex4(raw[i+1 : i+5])
				i += 5
				if utf16.IsSurrogate(r) {
					r2 := rune(-1)
					if i+6 <= len(raw) && raw[i] == '\\' && raw[i+1] == 'u' {
						r2 = hex4(raw[i+2 : i+6])
					}
					if dec := utf16.DecodeRune(r, r2); dec != utf8.RuneError {
						i += 6
						r = dec
					} else {
						r = utf8.RuneError
					}
				}
				b.WriteRune(r)
				continue
			default: // '"', '\\', '/'
				b.WriteByte(raw[i])
			}
			i++
			continue
		}
		if c < utf8.RuneSelf {
			b.WriteByte(c)
			i++
			continue
		}
		r, size := utf8.DecodeRune(raw[i:])
		b.WriteRune(r) // invalid bytes decode to U+FFFD
		i += size
	}
	return b.String(), true
}

func hex4(b []byte) rune {
	var r rune
	for _, c := range b {
		switch {
		case '0' <= c && c <= '9':
			c -= '0'
		case 'a' <= c && c <= 'f':
			c = c - 'a' + 10
		default:
			c = c - 'A' + 10
		}
		r = r<<4 | rune(c)
	}
	return r
}

func containsByte(b []byte, c byte) bool {
	return bytes.IndexByte(b, c) >= 0
}
