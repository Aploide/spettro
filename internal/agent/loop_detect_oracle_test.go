package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

// volatileOutputOracle is the regular expression normalizeVolatile replaces
// (loop_normalize.go). It is the specification: normalizeVolatile must
// produce exactly volatileOutputOracle.ReplaceAllString(s, "#") for every s.
var volatileOutputOracle = regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ns|µs|us|ms|s|m|h)\b|\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?\b|\b\d{2}:\d{2}:\d{2}(?:\.\d+)?\b|0x[0-9a-fA-F]+|\bspool:\d+|spettro-spool-[^\s/\\]*[/\\]\d+\.txt|\bjob-\d+`)

// oracleCallSignature is callSignature as it was written with the regex.
func oracleCallSignature(name string, args json.RawMessage, status, output string) string {
	norm := bytes.TrimSpace(args)
	var buf bytes.Buffer
	if json.Compact(&buf, norm) == nil {
		norm = buf.Bytes()
	}
	ah := sha256.Sum256(norm)
	rh := sha256.Sum256([]byte(status + "\x00" + volatileOutputOracle.ReplaceAllString(output, "#")))
	return name + "\x00" + hex.EncodeToString(ah[:8]) + "\x00" + hex.EncodeToString(rh[:8])
}

func normalizedString(s string) string {
	var b strings.Builder
	normalizeVolatile(s, func(piece string) {
		if piece == "" {
			panic("normalizeVolatile emitted an empty piece")
		}
		b.WriteString(piece)
	})
	return b.String()
}

// normalizeFixtures are outputs with every kind of volatile fragment, and
// near misses of each.
var normalizeFixtures = []string{
	"",
	"FAIL pkg 0.012s\nok 12:30:01 0xdeadbeef spool:12 /tmp/spettro-spool-ab/3.txt job-7\n",
	"2024-01-02T03:04:05Z x 2024-01-02 03:04:05.123+02:00 1.5ms 2024-01-02T03:04:05.9-0700",
	"took 12µs, 3us, 4ns, 5m 6h 7s; 8msx 9sec 10.5.6s a11ms _12ms 13ms_",
	"0x 0xG 10x1f x0x2 0xABCdef9 00x7",
	"spool: spool:x xspool:3 spool:44abc job- job-x ajob-1 job-99_",
	"spettro-spool- spettro-spool-/1.txt spettro-spool-a b/1.txt spettro-spool-a\\22.txt spettro-spool-a/1.tx spettro-spool-é/5.txt",
	"12:30 12:30:1 123:45:67 12:34:56.789 12:34:56.x 01:02:03Z",
	"2024-1-02T03:04:05 2024-01-02X03:04:05 2024-01-02T03:04:05+0 2024-01-02T03:04:05+02:0 2024-01-02T03:04:05Zz",
	"日本 5ms 語 é5ms ü12:00:00",
	"\xff5ms\xfe 0x1\xff spettro-spool-\xff/1.txt",
}

func TestNormalizeMatchesRegexFixtures(t *testing.T) {
	for _, s := range append(normalizeFixtures, loopTestLog(400), loopTestGoListing(200)) {
		if got, want := normalizedString(s), volatileOutputOracle.ReplaceAllString(s, "#"); got != want {
			t.Fatalf("normalizeVolatile(%q)\n got %q\nwant %q", s, got, want)
		}
	}
}

// normalizeAlphabet is the building blocks of the random differential test:
// every token the alternatives are made of, plus word and non-word bytes.
var normalizeAlphabet = []string{
	"0", "1", "9", "5", ".", ":", "-", "+", "T", " ", "Z", "s", "m", "n", "u", "µ", "h", "x",
	"a", "f", "F", "\n", "\t", "/", "\\", "_", "spool:", "job-", "spettro-spool-", ".txt",
	"0x", "ms", "é", "\xff", "2024-01-02", "12:34:56",
}

// A deterministic slice of the fuzz space, run by every go test.
func TestNormalizeMatchesRegexRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for range 200_000 {
		var b strings.Builder
		for range rng.Intn(14) {
			b.WriteString(normalizeAlphabet[rng.Intn(len(normalizeAlphabet))])
		}
		s := b.String()
		if got, want := normalizedString(s), volatileOutputOracle.ReplaceAllString(s, "#"); got != want {
			t.Fatalf("normalizeVolatile(%q)\n got %q\nwant %q", s, got, want)
		}
	}
}

// FuzzNormalizeMatchesRegex is the differential fuzz test against the regex:
// go test ./internal/agent -run '^$' -fuzz FuzzNormalizeMatchesRegex
func FuzzNormalizeMatchesRegex(f *testing.F) {
	for _, s := range normalizeFixtures {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if got, want := normalizedString(s), volatileOutputOracle.ReplaceAllString(s, "#"); got != want {
			t.Fatalf("normalizeVolatile(%q)\n got %q\nwant %q", s, got, want)
		}
	})
}

// The streamed signature is byte-for-byte the one the regex version built.
func TestCallSignatureMatchesOracle(t *testing.T) {
	args := json.RawMessage(` {"command": "go test ./...",  "timeout": 30} `)
	for _, out := range append(normalizeFixtures, loopTestLog(2000), strings.Repeat("y", 5000)) {
		for _, status := range []string{"success", "error", ""} {
			if got, want := callSignature("bash", args, status, out), oracleCallSignature("bash", args, status, out); got != want {
				t.Fatalf("callSignature(%q) = %q, oracle %q", out, got, want)
			}
		}
	}
}

// callSignature on a 30 KB test log: the plan's ceiling is 4 allocations.
func TestCallSignatureAllocs(t *testing.T) {
	out := loopTestLog(400)[:30000]
	args := json.RawMessage(`{"command":"go test ./..."}`)
	if allocs := testing.AllocsPerRun(20, func() { callSignature("bash", args, "success", out) }); allocs > 4 {
		t.Fatalf("callSignature allocates %.0f times, want <= 4", allocs)
	}
}

// loopTestLog is a test-runner log of n lines, dense in durations and
// timestamps (the worst case for normalization).
func loopTestLog(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "%6d\t--- FAIL: TestHandler_%d (0.%03ds) 2026-09-27 10:00:%02d ptr=0x%x spool:%d\n", i, i, i%1000, i%60, 0xc000010000+i, i)
	}
	return b.String()
}

// loopTestGoListing is a numbered source listing of n functions (sparse in
// volatile fragments).
func loopTestGoListing(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "%6d\t// Func%d does thing %d.\n%6d\tfunc Func%d(a int) int { return a + %d }\n", 2*i+1, i, i, 2*i+2, i, i)
	}
	return b.String()
}
