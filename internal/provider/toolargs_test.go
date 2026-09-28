package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeToolArgs(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		truncated bool
		wantArgs  string // compacted JSON; "" = don't check
		wantErr   string // substring of the error; "" = no error
	}{
		{"valid object", `{"path":"a.go"}`, false, `{"path":"a.go"}`, ""},
		{"empty means no args", "  ", false, `{}`, ""},
		{"double-encoded object", `"{\"path\":\"a.go\"}"`, false, `{"path":"a.go"}`, ""},
		{"code fence", "```json\n{\"path\":\"a.go\"}\n```", false, `{"path":"a.go"}`, ""},
		{"trailing commas", `{"a":[1,2,],"b":{"c":1,},}`, false, `{"a":[1,2],"b":{"c":1}}`, ""},
		{"raw newline and tab in string", "{\"content\":\"a\n\tb\"}", false, `{"content":"a\n\tb"}`, ""},
		{"comma inside string is kept", `{"s":"x,}"}`, false, `{"s":"x,}"}`, ""},
		{"truncated is never repaired", `{"path":"a.go","content":"pack`, true, `{}`, "truncated at the output token limit (8000 tokens)"},
		{"valid args survive a truncated reply", `{"path":"a.go"}`, true, `{"path":"a.go"}`, ""},
		{"unparseable", `{"pattern": foo}`, false, `{}`, "at byte"},
		{"array is not an object", `[1,2]`, false, `{}`, "got an array"},
		{"abrupt end hints at truncation", `{"path":"a.go","content":"x`, false, `{}`, "cut off by the output token limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, errMsg := normalizeToolArgs(tc.raw, tc.truncated, 8000)
			if tc.wantErr == "" && errMsg != "" {
				t.Fatalf("unexpected error: %s", errMsg)
			}
			if tc.wantErr != "" && !strings.Contains(errMsg, tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", errMsg, tc.wantErr)
			}
			if tc.wantArgs != "" {
				var got, want any
				if err := json.Unmarshal(args, &got); err != nil {
					t.Fatalf("args %q are not valid JSON: %v", args, err)
				}
				_ = json.Unmarshal([]byte(tc.wantArgs), &want)
				gb, _ := json.Marshal(got)
				wb, _ := json.Marshal(want)
				if string(gb) != string(wb) {
					t.Fatalf("args = %s, want %s", gb, wb)
				}
			}
		})
	}
}

func TestEndsMidJSON(t *testing.T) {
	for s, want := range map[string]bool{
		`{"a":"b`:        true,
		`{"a":[1,2`:      true,
		`{"a":1}`:        false,
		`{"a": foo}`:     false,
		"":               false,
		"{\"a\":\"x\ny":  true,
		`{"a":1,"b":2,`:  true,
		`{"a":"b"} junk`: false,
	} {
		if got := endsMidJSON(s); got != want {
			t.Errorf("endsMidJSON(%q) = %v, want %v", s, got, want)
		}
	}
}
