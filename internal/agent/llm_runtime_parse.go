package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// decodeJSONStrict decodes a tool's JSON arguments into target. It is strict
// about syntax — exactly one JSON value, nothing trailing — but deliberately
// lenient about shape: unknown fields are ignored. Models carry argument habits
// over from other harnesses (a `description` on a shell call, `-n` on grep, a
// `timeout` where none is declared), and rejecting the whole call for an extra
// key costs a round trip while telling the model nothing it can use.
func decodeJSONStrict(data []byte, target any) error {
	return decodeJSON(data, target, false)
}

// decodeJSONExact is decodeJSONStrict that also rejects unknown fields. The
// write tools (file-write, file-edit) use it: there a misspelled
// key is not harmless noise but a required value silently decoding as "" —
// `file_text` for content would truncate the file and still report success.
// Those tools map the common spellings from other harnesses explicitly and
// reject anything else.
func decodeJSONExact(data []byte, target any) error {
	return decodeJSON(data, target, true)
}

func decodeJSON(data []byte, target any, exact bool) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if exact {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(target); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("unexpected trailing JSON content")
	}
	return nil
}

// flexInt is an integer tool argument that also accepts a numeric string
// ("10") or a whole float (10.0), the shapes models most often send in place
// of a JSON integer. null and "" decode as zero.
type flexInt int

func (n *flexInt) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "null" {
		*n = 0
		return nil
	}
	if unq, err := strconv.Unquote(s); err == nil {
		s = strings.TrimSpace(unq)
		if s == "" {
			*n = 0
			return nil
		}
	}
	if v, err := strconv.Atoi(s); err == nil {
		*n = flexInt(v)
		return nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f == float64(int(f)) {
		*n = flexInt(int(f))
		return nil
	}
	return fmt.Errorf("expected an integer, got %s", string(data))
}

// flexBool is a boolean tool argument that also accepts "true"/"false" strings.
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if unq, err := strconv.Unquote(s); err == nil {
		s = strings.TrimSpace(unq)
	}
	switch strings.ToLower(s) {
	case "", "null", "false", "0":
		*b = false
	case "true", "1":
		*b = true
	default:
		return fmt.Errorf("expected a boolean, got %s", string(data))
	}
	return nil
}

// firstNonEmpty returns the first argument that is not blank, trimmed. It is how
// argument aliases collapse onto one value: the canonical name is passed first
// so it wins when the model sends both.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// firstPresent returns the first non-nil string among aliases of one argument,
// and whether any was sent. Unlike firstNonEmpty it tells "absent" from "empty":
// a write tool must be able to write "" on purpose, but never by omission.
func firstPresent(values ...*string) (string, bool) {
	for _, v := range values {
		if v != nil {
			return *v, true
		}
	}
	return "", false
}

// fileWriteArgs are file-write's arguments after alias resolution.
type fileWriteArgs struct {
	Path    string
	Content string
	Append  bool
}

// decodeFileWriteArgs decodes file-write's arguments exactly. file_path, and
// file_text/contents (the create spellings of other editors' tools), are
// accepted as aliases; any other key is rejected, and content must be sent —
// an absent content would otherwise truncate the file to "".
func decodeFileWriteArgs(raw []byte) (fileWriteArgs, error) {
	var in struct {
		Path     string   `json:"path"`
		FilePath string   `json:"file_path"`
		Content  *string  `json:"content"`
		FileText *string  `json:"file_text"`
		Contents *string  `json:"contents"`
		Append   flexBool `json:"append"`
	}
	if err := decodeJSONExact(raw, &in); err != nil {
		return fileWriteArgs{}, fmt.Errorf("file-write args: %w", err)
	}
	content, ok := firstPresent(in.Content, in.FileText, in.Contents)
	if !ok {
		return fileWriteArgs{}, fmt.Errorf(`file-write: content is required (send "content": "" to write an empty file)`)
	}
	return fileWriteArgs{
		Path:    firstNonEmpty(in.Path, in.FilePath),
		Content: content,
		Append:  bool(in.Append),
	}, nil
}

// fileEditPair is one find/replace of file-edit (old_string or an edits[] item).
type fileEditPair struct {
	OldString  string
	NewString  string
	ReplaceAll bool
	// Expected is an edits[] item's expected_replacements (0: not given).
	Expected int
	// hasNew records whether new_string was sent at all; see requireNew.
	hasNew bool
}

// rawFileEditPair is the wire form of a fileEditPair: old_str/new_str (the
// str_replace spelling) are accepted beside old_string/new_string. An
// edits[] item may carry its own expected_replacements, as models trained on
// other multi-edit tools send it; at the top level the field is the whole
// call's.
type rawFileEditPair struct {
	OldString  *string  `json:"old_string"`
	OldStr     *string  `json:"old_str"`
	NewString  *string  `json:"new_string"`
	NewStr     *string  `json:"new_str"`
	ReplaceAll flexBool `json:"replace_all"`
	Expected   flexInt  `json:"expected_replacements"`
}

func (p rawFileEditPair) resolve() fileEditPair {
	oldText, _ := firstPresent(p.OldString, p.OldStr)
	newText, hasNew := firstPresent(p.NewString, p.NewStr)
	return fileEditPair{OldString: oldText, NewString: newText, ReplaceAll: bool(p.ReplaceAll), Expected: max(int(p.Expected), 0), hasNew: hasNew}
}

// flexEdits is file-edit's edits[] argument. Besides the JSON array the
// schema asks for, it accepts the two shapes models most often send instead
// (seen as "cannot unmarshal object into ... edits" errors, each costing a
// turn):
//
//   - a single edit object, which is treated as a one-item list;
//   - a JSON-encoded string holding either of those ("[{...}]" or "{...}").
//
// null and "" decode as no edits. Items are decoded exactly (unknown fields
// are rejected), the same as when they arrive inside a real array, so a typo
// in an item's field name is still reported rather than silently ignored.
type flexEdits []rawFileEditPair

func (e *flexEdits) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	// A string value: unwrap it once and decode what it contains. A string
	// inside that string is not unwrapped again; that shape is not seen in
	// practice and would only hide a real mistake.
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var inner string
		if err := json.Unmarshal(trimmed, &inner); err != nil {
			return err
		}
		trimmed = bytes.TrimSpace([]byte(inner))
	}
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*e = nil
		return nil
	}
	switch trimmed[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return err
		}
		out := make(flexEdits, 0, len(items))
		for i, item := range items {
			var pair rawFileEditPair
			if err := decodeJSONExact(item, &pair); err != nil {
				return fmt.Errorf("edits[%d]: %w", i, err)
			}
			out = append(out, pair)
		}
		*e = out
		return nil
	case '{':
		var pair rawFileEditPair
		if err := decodeJSONExact(trimmed, &pair); err != nil {
			return fmt.Errorf("edits: %w", err)
		}
		*e = flexEdits{pair}
		return nil
	}
	return fmt.Errorf("edits: expected an array of edit objects, got %s", truncate(string(trimmed), 40))
}

// requireNew rejects an edit whose new_string was never sent: decoding it as
// "" would delete the matched text and report a successful edit.
func (p fileEditPair) requireNew(label string) error {
	if p.hasNew {
		return nil
	}
	return fmt.Errorf(`%s: new_string is required (send "new_string": "" to delete the matched text)`, label)
}

// fileEditArgs are file-edit's arguments after alias resolution.
type fileEditArgs struct {
	Path      string
	Single    fileEditPair
	StartLine int
	EndLine   int
	Expected  int
	Edits     []fileEditPair
}

// decodeFileEditArgs decodes file-edit's arguments exactly (see
// decodeJSONExact), accepting file_path and old_str/new_str as aliases.
func decodeFileEditArgs(raw []byte) (fileEditArgs, error) {
	var in struct {
		Path     string `json:"path"`
		FilePath string `json:"file_path"`
		rawFileEditPair
		StartLine flexInt   `json:"start_line"`
		EndLine   flexInt   `json:"end_line"`
		Edits     flexEdits `json:"edits"`
	}
	if err := decodeJSONExact(raw, &in); err != nil {
		return fileEditArgs{}, fmt.Errorf("file-edit args: %w", err)
	}
	out := fileEditArgs{
		Path:      firstNonEmpty(in.Path, in.FilePath),
		Single:    in.rawFileEditPair.resolve(),
		StartLine: int(in.StartLine),
		EndLine:   int(in.EndLine),
	}
	// The top-level expected_replacements counts the whole call's
	// replacements, edits[] included.
	out.Expected, out.Single.Expected = out.Single.Expected, 0
	if out.Single.OldString != "" {
		if err := out.Single.requireNew("file-edit"); err != nil {
			return fileEditArgs{}, err
		}
	}
	for i, e := range in.Edits {
		pair := e.resolve()
		if pair.OldString != "" {
			if err := pair.requireNew(fmt.Sprintf("file-edit: edit %d", i+1)); err != nil {
				return fileEditArgs{}, err
			}
		}
		out.Edits = append(out.Edits, pair)
	}
	return out, nil
}
