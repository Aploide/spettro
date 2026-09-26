package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// maxArgsSnippet bounds how much of the model's raw arguments is quoted back
// in a parse-error message: enough to locate the problem, not a second copy
// of a large file body.
const maxArgsSnippet = 160

// TruncatedArgsError is the tool result fed back when a tool call's
// arguments were cut off by the output token limit. The call is never
// executed: a "repaired" truncated file-write would silently write half a
// file.
func TruncatedArgsError(maxOutput int) string {
	limit := "the output token limit"
	if maxOutput > 0 {
		limit = fmt.Sprintf("the output token limit (%d tokens)", maxOutput)
	}
	return "error: your tool call arguments were truncated at " + limit +
		" and the call was NOT executed. Split the content into smaller pieces: write a large file in several" +
		" steps (create it with the first part, then add each further part with file-write append=true), or" +
		" use file-edit for targeted changes instead of rewriting whole files."
}

// normalizeToolArgs turns the raw argument text a model produced for one tool
// call into the JSON object the tool runtime decodes. It returns the args to
// use and, when they could not be recovered, an error message for the model
// (the args are then "{}").
//
// When truncated is set the reply was cut at the output limit; invalid JSON is
// then reported as truncated and never repaired, since completing a cut-off
// payload would fabricate content. Otherwise a conservative repair pass fixes
// the common near-misses weaker models emit (markdown code fences, trailing
// commas, raw newlines/tabs inside strings, a JSON object double-encoded as a
// string) before giving up with a precise parse error.
func normalizeToolArgs(raw string, truncated bool, maxOutput int) (json.RawMessage, string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return json.RawMessage(`{}`), ""
	}
	if obj, ok := asJSONObject(trimmed); ok {
		return obj, ""
	}
	if truncated {
		return json.RawMessage(`{}`), TruncatedArgsError(maxOutput)
	}
	if repaired, ok := repairToolArgs(trimmed); ok {
		return repaired, ""
	}
	return json.RawMessage(`{}`), argsParseError(trimmed)
}

// asJSONObject accepts s when it is a JSON object, or a JSON string whose
// content is itself a JSON object (a double-encoded payload).
func asJSONObject(s string) (json.RawMessage, bool) {
	if s == "" || !json.Valid([]byte(s)) {
		return nil, false
	}
	switch s[0] {
	case '{':
		return json.RawMessage(s), true
	case '"':
		var inner string
		if json.Unmarshal([]byte(s), &inner) != nil {
			return nil, false
		}
		inner = strings.TrimSpace(inner)
		if inner != "" && inner[0] == '{' && json.Valid([]byte(inner)) {
			return json.RawMessage(inner), true
		}
	}
	return nil, false
}

// repairToolArgs applies the conservative fixes described on
// normalizeToolArgs, returning the result only if it is a valid JSON object.
// It never adds missing closing brackets or quotes: a structurally incomplete
// payload is reported, not guessed at.
func repairToolArgs(s string) (json.RawMessage, bool) {
	s = stripCodeFence(s)
	s = escapeControlCharsInStrings(s)
	s = removeTrailingCommas(s)
	return asJSONObject(strings.TrimSpace(s))
}

// endsMidJSON reports whether s, after the conservative repairs, is a JSON
// prefix that stops in the middle of a value — the signature of arguments cut
// off mid-stream rather than malformed.
func endsMidJSON(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return false
	}
	t = removeTrailingCommas(escapeControlCharsInStrings(stripCodeFence(t)))
	var v any
	err := json.Unmarshal([]byte(t), &v)
	var syn *json.SyntaxError
	return errors.As(err, &syn) && syn.Error() == "unexpected end of JSON input"
}

// stripCodeFence removes a surrounding ```json ... ``` (or bare ```) fence.
func stripCodeFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return s
	}
	t = strings.TrimPrefix(t, "```")
	if nl := strings.IndexByte(t, '\n'); nl >= 0 {
		// Drop the info string ("json") on the opening fence line.
		if lang := strings.TrimSpace(t[:nl]); !strings.ContainsAny(lang, "{[\"") {
			t = t[nl+1:]
		}
	}
	t = strings.TrimSpace(t)
	return strings.TrimSuffix(t, "```")
}

// escapeControlCharsInStrings escapes raw control characters (newline, tab,
// carriage return, …) that appear inside JSON string literals — invalid JSON
// that models often emit when writing multi-line code into an argument.
func escapeControlCharsInStrings(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 16)
	inString, escaped := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			b.WriteByte(c)
			continue
		}
		if escaped {
			escaped = false
			b.WriteByte(c)
			continue
		}
		switch {
		case c == '\\':
			escaped = true
			b.WriteByte(c)
		case c == '"':
			inString = false
			b.WriteByte(c)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20:
			fmt.Fprintf(&b, `\u%04x`, c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// removeTrailingCommas drops commas that directly precede a closing brace or
// bracket (ignoring whitespace), outside string literals.
func removeTrailingCommas(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inString, escaped := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			b.WriteByte(c)
			continue
		}
		if c == '"' {
			inString = true
			b.WriteByte(c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(s) && strings.IndexByte(" \t\r\n", s[j]) >= 0 {
				j++
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

// argsParseError builds a precise message for arguments that could not be
// parsed or repaired: the decoder's complaint, the byte offset, and a short
// excerpt around it so the model can see exactly what to fix.
func argsParseError(s string) string {
	var v any
	err := json.Unmarshal([]byte(s), &v)
	detail := "not a JSON object"
	offset := -1
	if err != nil {
		detail = err.Error()
		var syn *json.SyntaxError
		if errors.As(err, &syn) {
			offset = int(syn.Offset)
		}
	} else if _, isObj := v.(map[string]any); !isObj {
		detail = "arguments must be a JSON object, got " + jsonKind(v)
	}
	var sb strings.Builder
	sb.WriteString("error: your tool call arguments are not valid JSON and the call was NOT executed (")
	sb.WriteString(detail)
	if offset >= 0 {
		fmt.Fprintf(&sb, " at byte %d", offset)
	}
	sb.WriteString(").")
	if snippet := excerptAround(s, offset); snippet != "" {
		fmt.Fprintf(&sb, " Near: %q.", snippet)
	}
	if err != nil && strings.Contains(err.Error(), "unexpected end of JSON input") {
		sb.WriteString(" The arguments end abruptly: if they were long they were probably cut off by the output token limit, so split the content into smaller writes (file-write append=true adds to a file) or use file-edit.")
	}
	sb.WriteString(` Resend the call with a single valid JSON object: escape newlines as \n and quotes as \" inside strings, no trailing commas, no comments or code fences.`)
	return sb.String()
}

func excerptAround(s string, offset int) string {
	if offset < 0 || offset > len(s) {
		offset = len(s)
	}
	start := max(offset-maxArgsSnippet/2, 0)
	end := min(start+maxArgsSnippet, len(s))
	return strings.ToValidUTF8(s[start:end], "")
}

func jsonKind(v any) string {
	switch v.(type) {
	case []any:
		return "an array"
	case string:
		return "a string"
	case float64:
		return "a number"
	case bool:
		return "a boolean"
	case nil:
		return "null"
	}
	return "a non-object value"
}
