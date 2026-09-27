package chatcompletions

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/openai-go/packages/ssestream"
	xjson "github.com/charmbracelet/x/json"
)

// --- AppendString: encoding/json is the oracle --------------------------

func FuzzAppendStringMatchesEncodingJSON(f *testing.F) {
	for _, s := range []string{"", "plain", "quote \" backslash \\", "\x00\x01\x1f\x7f", "  ", "\xff\xfe invalid", "emoji \U0001F600", "<>&", "tab\tcr\rlf\n", "\xed\xa0\x80 surrogate"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := AppendString(nil, s)
		var decoded string
		if err := json.Unmarshal(got, &decoded); err != nil {
			t.Fatalf("AppendString(%q) = %s: not a JSON string: %v", s, got, err)
		}
		want, _ := json.Marshal(s)
		var wantDecoded string
		_ = json.Unmarshal(want, &wantDecoded)
		if decoded != wantDecoded {
			t.Fatalf("AppendString(%q) decodes to %q, encoding/json's to %q", s, decoded, wantDecoded)
		}
	})
}

func TestAppendStringDoesNotAllocateWithRoom(t *testing.T) {
	s := strings.Repeat("some text with \"quotes\" and ünïcode\n", 50)
	buf := make([]byte, 0, 4*len(s))
	if n := testing.AllocsPerRun(100, func() { buf = AppendString(buf[:0], s) }); n != 0 {
		t.Fatalf("AppendString allocated %v times", n)
	}
}

// --- ToolArgs: fantasy's re-validate-every-fragment rule is the oracle ----

// perFragmentOracle is the rule fantasy's OpenAI stream applies: append
// each fragment and re-validate the whole text; the first valid boundary
// finishes the call and later fragments are dropped. It returns the
// fragment index that finished it (-1 if none) and the text kept.
func perFragmentOracle(fragments []string) (int, string) {
	var text string
	for i, f := range fragments {
		text += f
		if xjson.IsValid(text) {
			return i, text
		}
	}
	return -1, text
}

func runToolArgs(fragments []string) (int, string) {
	var a ToolArgs
	finishedAt := -1
	for i, f := range fragments {
		if a.Add(f) && finishedAt < 0 {
			finishedAt = i
		}
	}
	return finishedAt, a.String()
}

// splitAt cuts s into fragments at the positions given by cuts (bytes of
// the fuzz input, taken as fragment lengths).
func splitAt(s string, cuts []byte) []string {
	var out []string
	for _, c := range cuts {
		if s == "" {
			break
		}
		n := min(int(c)%8, len(s))
		out = append(out, s[:n])
		s = s[n:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

func FuzzToolArgsMatchesPerFragmentValidation(f *testing.F) {
	seeds := []string{
		`{"path":"a.go","content":"x"}`, `{}`, `{}{}`, `{} `, `{"a":1]`, `["x",{"y":[1,2]}]`,
		`"a string"`, `"{\"a\":1}"`, `12`, `123 4`, `true`, `nul`, ` {"a" : "}\"{" } x`,
		`{"a":"\\"}`, `{"a":"\\\""}`, `}{`, `{"a":{"b":{"c":[]}}}`, "\n\t{\"k\":\"v\"}\n", `]`, `"`,
	}
	for _, s := range seeds {
		f.Add(s, []byte{1, 3, 2, 7, 0, 5})
		f.Add(s, []byte{})
	}
	f.Fuzz(func(t *testing.T, s string, cuts []byte) {
		fragments := splitAt(s, cuts)
		wantAt, wantText := perFragmentOracle(fragments)
		gotAt, gotText := runToolArgs(fragments)
		// Past scalarRecheckLimit bytes, arguments that are not an object
		// or array are no longer re-validated (documented on ToolArgs).
		if len(s) > scalarRecheckLimit {
			return
		}
		if gotAt != wantAt || gotText != wantText {
			t.Fatalf("fragments %q: ToolArgs finished at %d with %q, oracle at %d with %q", fragments, gotAt, gotText, wantAt, wantText)
		}
	})
}

// Work-count guard: a 50 KB argument text streamed in 12,000 fragments is
// validated once, not once per fragment.
func TestToolArgsValidatesLargeArgumentsOnce(t *testing.T) {
	content := strings.Repeat(`    fmt.Println(\"hello world from the generated file\") // filler\n`, 700)
	args := `{"path":"big.go","content":"` + content + `"}`
	var a ToolArgs
	step := max(1, len(args)/12000)
	finished := false
	for i := 0; i < len(args); i += step {
		if a.Add(args[i:min(i+step, len(args))]) {
			finished = true
		}
	}
	if !finished || a.String() != args {
		t.Fatalf("finished=%v, text intact=%v", finished, a.String() == args)
	}
	if a.Validations() != 1 {
		t.Fatalf("validated %d times, want 1", a.Validations())
	}
}

// --- EventReader: the OpenAI SDK's decoder is the oracle -----------------

func sdkEvents(t *testing.T, raw string) []string {
	t.Helper()
	res := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(raw))}
	dec := ssestream.NewDecoder(res)
	var out []string
	for dec.Next() {
		out = append(out, string(dec.Event().Data))
	}
	return out
}

func ownEvents(raw string) []string {
	r := NewEventReader(strings.NewReader(raw))
	var out []string
	for r.Next() {
		out = append(out, string(r.Data()))
	}
	return out
}

func FuzzEventReaderMatchesSDK(f *testing.F) {
	for _, s := range []string{
		"data: {\"a\":1}\n\ndata: [DONE]\n\n",
		": keep-alive\n\ndata:x\r\n\r\n",
		"event: e\ndata: one\ndata: two\n\n",
		"data: unterminated",
		"id: 1\nretry: 5\ndata\n\n",
		"\n\n\ndata: a\n\n",
		"data:  two spaces\n\n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if got, want := ownEvents(raw), sdkEvents(t, raw); !reflect.DeepEqual(got, want) {
			t.Fatalf("events of %q: got %q, SDK %q", raw, got, want)
		}
	})
}

// --- DecodeChunk ----------------------------------------------------------

func TestDecodeChunkIsLenientAndResets(t *testing.T) {
	var c Chunk
	first := `{"id":"a","choices":[{"index":0,"delta":{"content":"x","tool_calls":[{"index":0,"id":"t","function":{"name":"n","arguments":"{"}}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
	if err := DecodeChunk([]byte(first), &c); err != nil {
		t.Fatal(err)
	}
	if c.Choices[0].Delta.ToolCalls[0].Function.Arguments != "{" || c.Usage.TotalTokens != 5 {
		t.Fatalf("decoded %+v", c)
	}
	// A later chunk must not inherit anything from the previous one, and a
	// mistyped field is skipped rather than failing the stream.
	if err := DecodeChunk([]byte(`{"id":"a","choices":[{"index":"zero","delta":{"content":"y"}}]}`), &c); err != nil {
		t.Fatal(err)
	}
	want := Chunk{ID: "a", Choices: []Choice{{Delta: Delta{Content: "y"}}}}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("decoded %+v, want %+v", c, want)
	}
	if err := DecodeChunk([]byte(`{"id":`), &c); err == nil {
		t.Fatal("malformed JSON accepted")
	}
	for raw, msg := range map[string]string{
		`{"error":null}`:                  "",
		`{"error":"plain"}`:               "plain",
		`{"error":{"message":"m","c":1}}`: `{"message":"m","c":1}`,
		`{"error":42}`:                    "42",
	} {
		if err := DecodeChunk([]byte(raw), &c); err != nil {
			t.Fatal(err)
		}
		if c.Error == nil {
			t.Fatalf("%s: error member not detected", raw)
		}
		if got := ErrorMessage(c.Error); got != msg {
			t.Fatalf("%s: message %q, want %q", raw, got, msg)
		}
	}
	if err := DecodeChunk([]byte(`{"id":"a","choices":[]}`), &c); err != nil || c.Error != nil {
		t.Fatalf("error member reported where there is none: %v %q", err, c.Error)
	}
}

// --- decodeChunkFast: decodeChunkStd (encoding/json) is the oracle ---------

// normalizeChunk maps empty slices to nil: encoding/json keeps "[]" as an
// empty slice where the fast path may leave nil, and callers only look at
// lengths.
func normalizeChunk(c Chunk) Chunk {
	if len(c.Choices) == 0 {
		c.Choices = nil
	}
	for i := range c.Choices {
		if len(c.Choices[i].Delta.ToolCalls) == 0 {
			c.Choices[i].Delta.ToolCalls = nil
		}
	}
	return c
}

func FuzzDecodeChunkFast(f *testing.F) {
	seeds := []string{
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel\nlo \u00e9 \ud83d\ude00"},"finish_reason":null,"logprobs":null}]}`,
		`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"file-write","arguments":"{\"path\":\"a\\\\b\"}"}}]}}]}`,
		`{"id":"c","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"prompt_tokens_details":{"cached_tokens":4,"audio_tokens":0},"completion_tokens_details":{"reasoning_tokens":1}}}`,
		`{"id":"c","choices":[{"index":0,"delta":{"reasoning_content":"think"},"finish_reason":"stop"}],"usage":null}`,
		`{"id":null,"choices":null}`,
		`{"choices":[null,{"delta":null,"index":-1}]}`,
		`{"Choices":[{"index":0}]}`,
		`{"id":"a","id":"b"}`,
		`{"id":"c","choices":[{"index":1.5}]}`,
		`{"id":"c","choices":[{"index":"0"}]}`,
		`{"id":"\ud800 lone","choices":[{"delta":{"content":"\ud83d\u0041 bad pair \xff raw"}}]}`,
		`{"error":{"message":"x"}}`,
		`{"id":"c", "extra": [1, {"a": [true, false, null, -0.5e+3]}], "choices" : [ ] }`,
		`{"id":"c"} trailing`,
		`{"id":"c",}`,
		` {"id":"x"} `,
		`[]`,
		`null`,
		`{"id":"c","choices":[{"delta":{"content":"a\u0000b"}}]}`,
		`{"id":"c","choices":[{"index":01}]}`,
		`{"choices":[{"index":0,"delta":{"cont\u0065nt":"hi"}}]}`,
		`{"\u0069d":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"n\u0061me":"x"}}]}}]}`,
		`{"id":"a","choices":[],"\u0065rror":{"message":"boom"}}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var fast Chunk
		fast.ID = "c" // a previous chunk's id, as in a stream
		if !decodeChunkFast(data, &fast) {
			return
		}
		var std Chunk
		if err := decodeChunkStd(data, &std); err != nil {
			t.Fatalf("fast path accepted %q, encoding/json rejects it: %v", data, err)
		}
		if a, b := normalizeChunk(fast), normalizeChunk(std); !reflect.DeepEqual(a, b) {
			t.Fatalf("decoding %q:\nfast %+v\nstd  %+v", data, a, b)
		}
	})
}

// TestDecodeChunkEscapedMemberName pins that a member name written with an
// escape sequence decodes like the plain name, as encoding/json decodes it:
// the fast path compares raw names, so it must hand such a chunk over.
func TestDecodeChunkEscapedMemberName(t *testing.T) {
	var c Chunk
	if err := DecodeChunk([]byte(`{"choices":[{"index":0,"delta":{"cont\u0065nt":"hi"}}]}`), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Choices) != 1 || c.Choices[0].Delta.Content != "hi" {
		t.Fatalf("decoded %+v, want one choice with content \"hi\"", c)
	}
	// A mid-stream error must not be skipped as an unknown member.
	c = Chunk{}
	if err := DecodeChunk([]byte(`{"id":"a","choices":[],"\u0065rror":{"message":"boom"}}`), &c); err != nil {
		t.Fatal(err)
	}
	if want := `{"message":"boom"}`; string(c.Error) != want {
		t.Fatalf("decoded error %s, want %s", c.Error, want)
	}
}

func BenchmarkDecodeChunk(b *testing.B) {
	data := []byte(`{"id":"chatcmpl-8a7f","object":"chat.completion.chunk","created":1727430000,"model":"some-model","choices":[{"index":0,"delta":{"content":"xxxxxxxxxxxxxxx \n"},"finish_reason":null}]}` + "\n")
	for _, impl := range []struct {
		name   string
		decode func([]byte, *Chunk) error
	}{{"fast", DecodeChunk}, {"encoding-json", decodeChunkStd}} {
		b.Run(impl.name, func(b *testing.B) {
			var c Chunk
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			for b.Loop() {
				if err := impl.decode(data, &c); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// --- Body ------------------------------------------------------------------

func TestBodyReaderReplays(t *testing.T) {
	var b Body
	b.AppendString(`{"a":`)
	b.Append(nil)
	b.Append([]byte(`[1,2,3]`))
	b.AppendString(`}`)
	for range 2 {
		got, err := io.ReadAll(b.Reader())
		if err != nil || string(got) != `{"a":[1,2,3]}` || int64(len(got)) != b.Len() {
			t.Fatalf("read %q (len %d), err %v", got, b.Len(), err)
		}
	}
	// Tiny reads cross chunk boundaries correctly.
	r := b.Reader()
	var out bytes.Buffer
	p := make([]byte, 3)
	for {
		n, err := r.Read(p)
		out.Write(p[:n])
		if err == io.EOF {
			break
		}
	}
	if out.String() != string(b.Bytes()) {
		t.Fatalf("small reads gave %q", out.String())
	}
}

// --- Request envelope ---------------------------------------------------------

func TestRequestEnvelopeIsValidJSON(t *testing.T) {
	var b Body
	b.Append(AppendRequestHead(nil, Options{Model: "m", MaxTokens: 10, MaxTokensField: "max_tokens", ReasoningEffort: "high"}))
	b.Append(AppendSystemMessage(nil, "s"))
	b.AppendString(",")
	b.Append(AppendAssistant(nil, Assistant{ReasoningContent: "r", ToolCalls: []ToolCall{{ID: "1", Name: "n", Arguments: "{}"}}}))
	b.AppendString(ToolsStart)
	b.Append(AppendFunctionTool(nil, "t", "", []byte(`{"type":"object"}`)))
	b.AppendString(ToolsEnd)
	var v map[string]any
	if err := json.Unmarshal(b.Bytes(), &v); err != nil {
		t.Fatalf("%s: %v", b.Bytes(), err)
	}
	if v["stream"] != true || v["max_tokens"] != float64(10) || v["reasoning_effort"] != "high" || v["tool_choice"] != "auto" {
		t.Fatalf("envelope %v", v)
	}
}
