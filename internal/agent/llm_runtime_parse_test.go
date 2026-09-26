package agent

import "testing"

func TestDecodeJSONStrictIgnoresUnknownFields(t *testing.T) {
	var args struct {
		Command string `json:"command"`
	}
	if err := decodeJSONStrict([]byte(`{"command":"ls","description":"list files","timeout":5}`), &args); err != nil {
		t.Fatalf("unknown fields rejected: %v", err)
	}
	if args.Command != "ls" {
		t.Fatalf("command = %q", args.Command)
	}
}

func TestDecodeJSONStrictRejectsTrailingContent(t *testing.T) {
	var args struct {
		Command string `json:"command"`
	}
	if err := decodeJSONStrict([]byte(`{"command":"ls"}{"command":"rm"}`), &args); err == nil {
		t.Fatal("trailing JSON value accepted")
	}
	if err := decodeJSONStrict([]byte(`{"command":`), &args); err == nil {
		t.Fatal("truncated JSON accepted")
	}
}

func TestFlexIntAcceptsCommonShapes(t *testing.T) {
	cases := map[string]int{`10`: 10, `"10"`: 10, `" 7 "`: 7, `10.0`: 10, `null`: 0, `""`: 0, `-3`: -3}
	for in, want := range cases {
		var v struct {
			N flexInt `json:"n"`
		}
		if err := decodeJSONStrict([]byte(`{"n":`+in+`}`), &v); err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if int(v.N) != want {
			t.Errorf("%s = %d, want %d", in, v.N, want)
		}
	}
	for _, bad := range []string{`"ten"`, `1.5`, `true`} {
		var v struct {
			N flexInt `json:"n"`
		}
		if err := decodeJSONStrict([]byte(`{"n":`+bad+`}`), &v); err == nil {
			t.Errorf("%s accepted as integer", bad)
		}
	}
}

func TestFlexBoolAcceptsStrings(t *testing.T) {
	cases := map[string]bool{`true`: true, `"true"`: true, `false`: false, `"False"`: false, `null`: false, `1`: true}
	for in, want := range cases {
		var v struct {
			B flexBool `json:"b"`
		}
		if err := decodeJSONStrict([]byte(`{"b":`+in+`}`), &v); err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if bool(v.B) != want {
			t.Errorf("%s = %v, want %v", in, v.B, want)
		}
	}
}
