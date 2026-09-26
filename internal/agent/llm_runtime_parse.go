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
	dec := json.NewDecoder(bytes.NewReader(data))
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
