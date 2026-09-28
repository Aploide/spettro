package chatcompletions

import (
	"encoding/json"
	"strings"
)

// scalarRecheckLimit bounds the per-fragment re-validation of arguments
// that do not start like an object, array or string (see ToolArgs).
const scalarRecheckLimit = 4096

// ToolArgs accumulates one tool call's streamed argument fragments and
// reports when they first form a complete JSON value.
//
// The rule is the one the fantasy SDK applies (a call is finished as soon
// as its accumulated arguments are valid JSON at a fragment boundary; later
// fragments are ignored), but in O(n) overall instead of re-validating the
// whole text on every fragment. That quadratic re-validation is what made a
// 50 KB file write streamed in 12,000 fragments cost 1.4 s.
//
// How: a valid JSON object, array or string ends exactly where a scanner
// that tracks string literals and bracket depth first returns to depth zero.
// So the full text is validated once, at the first fragment boundary after
// that point. If it is valid the call is finished; if not, no longer text
// can be valid either (a second value or garbage follows the first one, or
// the first one is itself malformed), so it is never validated again.
// Arguments that start any other way (a bare number or literal, or
// garbage) are not an object and never run; they are re-validated per
// fragment like fantasy does, up to scalarRecheckLimit bytes.
//
// FuzzToolArgsMatchesPerFragmentValidation checks it against the
// re-validate-everything rule.
type ToolArgs struct {
	text strings.Builder
	// kind is the first non-space byte, 0 until one arrived.
	kind     byte
	depth    int
	inString bool
	escaped  bool
	// ended is set once the scanner saw the first value end.
	ended bool
	// decided is set once validity can no longer change.
	decided  bool
	finished bool
	// validations counts full-text validations (a work-count guard).
	validations int
}

// Add appends one fragment and reports whether the arguments became a
// complete JSON value with it. After that (or once they can never become
// one) further fragments are ignored and Add returns false.
func (a *ToolArgs) Add(fragment string) bool {
	if a.finished {
		return false
	}
	a.text.WriteString(fragment)
	if a.decided {
		return false
	}
	crossedEnd := a.scan(fragment)
	switch a.kind {
	case 0:
		return false
	case '{', '[', '"':
		if !crossedEnd {
			return false
		}
		a.decided = true
		a.finished = a.validate()
		return a.finished
	}
	if a.text.Len() > scalarRecheckLimit {
		a.decided = true
		return false
	}
	a.finished = a.validate()
	a.decided = a.finished
	return a.finished
}

// scan advances the string/depth state over fragment and reports whether
// the first top-level value ended inside it.
func (a *ToolArgs) scan(fragment string) bool {
	if a.ended {
		return false
	}
	for i := 0; i < len(fragment); i++ {
		c := fragment[i]
		if a.kind == 0 {
			if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
				continue
			}
			a.kind = c
			if c != '{' && c != '[' && c != '"' {
				return false
			}
		}
		if a.inString {
			switch {
			case a.escaped:
				a.escaped = false
			case c == '\\':
				a.escaped = true
			case c == '"':
				a.inString = false
				if a.depth == 0 {
					a.ended = true
					return true
				}
			}
			continue
		}
		switch c {
		case '"':
			a.inString = true
		case '{', '[':
			a.depth++
		case '}', ']':
			a.depth--
			if a.depth == 0 {
				a.ended = true
				return true
			}
		}
	}
	return false
}

func (a *ToolArgs) validate() bool {
	a.validations++
	return json.Valid([]byte(a.text.String()))
}

// Finished reports whether the arguments completed (see Add).
func (a *ToolArgs) Finished() bool { return a.finished }

// Valid reports whether the accumulated text is valid JSON, checking it in
// full: the end-of-stream decision for a call that never finished.
func (a *ToolArgs) Valid() bool {
	if a.finished {
		return true
	}
	return a.validate()
}

// String returns the accumulated argument text.
func (a *ToolArgs) String() string { return a.text.String() }

// Len is the accumulated argument length in bytes.
func (a *ToolArgs) Len() int { return a.text.Len() }

// Validations counts the full-text validations done so far.
func (a *ToolArgs) Validations() int { return a.validations }
