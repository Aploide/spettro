package agent

import (
	"context"
	"strings"
	"testing"
)

// file-edit's edits accepts, besides an array, a single edit object and a
// JSON-encoded string holding an array or an object: the shapes some models
// send instead of the array the schema asks for.
func TestFileEditEditsAcceptsLenientShapes(t *testing.T) {
	cases := []struct {
		name string
		args string
	}{
		{"array", `{"path":"e.txt","edits":[{"old_string":"a","new_string":"A"}]}`},
		{"single object", `{"path":"e.txt","edits":{"old_string":"a","new_string":"A"}}`},
		{"string holding an array", `{"path":"e.txt","edits":"[{\"old_string\":\"a\",\"new_string\":\"A\"}]"}`},
		{"string holding an object", `{"path":"e.txt","edits":" {\"old_str\":\"a\",\"new_str\":\"A\"} "}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, dir := newEditTestRuntime(t)
			p := writeTestFile(t, dir, "e.txt", "a b\n")
			if _, err := r.runFileEdit(context.Background(), "file-edit", []byte(tc.args)); err != nil {
				t.Fatal(err)
			}
			if got := readTestFile(t, p); got != "A b\n" {
				t.Fatalf("file = %q", got)
			}
		})
	}
}

// The lenient shapes keep the strictness of the array form: an item with an
// unknown field is rejected, and a string that is not JSON is an error that
// says what was expected.
func TestFlexEditsRejectsMalformedItems(t *testing.T) {
	cases := []struct {
		name, args, want string
	}{
		{"unknown field in object", `{"path":"e.txt","edits":{"old_string":"a","new_string":"A","bogus":1}}`, "bogus"},
		{"unknown field in string array", `{"path":"e.txt","edits":"[{\"old_string\":\"a\",\"new_string\":\"A\",\"bogus\":1}]"}`, "bogus"},
		{"plain string", `{"path":"e.txt","edits":"replace a with A"}`, "expected an array of edit objects"},
		{"number", `{"path":"e.txt","edits":7}`, "expected an array of edit objects"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeFileEditArgs([]byte(tc.args))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// null and an empty string mean no edits, like an omitted field.
func TestFlexEditsEmptyValues(t *testing.T) {
	for _, v := range []string{`null`, `""`, `"  "`, `[]`} {
		got, err := decodeFileEditArgs([]byte(`{"path":"e.txt","old_string":"a","new_string":"A","edits":` + v + `}`))
		if err != nil {
			t.Fatalf("edits=%s: %v", v, err)
		}
		if len(got.Edits) != 0 {
			t.Fatalf("edits=%s: got %d edits, want none", v, len(got.Edits))
		}
	}
}
