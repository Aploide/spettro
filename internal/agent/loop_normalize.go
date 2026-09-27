package agent

import "strings"

// Volatile-output normalization for the loop detector.
//
// A tool result's signature must not change just because the output carries
// a fresh duration, timestamp, pointer address, spool id or job id: a
// failing test that prints "FAIL pkg 0.012s" and then "FAIL pkg 0.015s" is
// the same result. normalizeVolatile replaces each such fragment with "#".
//
// It is a hand-written equivalent of
//
//	regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ns|µs|us|ms|s|m|h)\b` +
//		`|\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?\b` +
//		`|\b\d{2}:\d{2}:\d{2}(?:\.\d+)?\b` +
//		`|0x[0-9a-fA-F]+` +
//		`|\bspool:\d+` +
//		`|spettro-spool-[^\s/\\]*[/\\]\d+\.txt` +
//		`|\bjob-\d+`).ReplaceAllString(output, "#")
//
// with the same leftmost-first semantics: at each position the alternatives
// are tried in that order, the first that matches wins, and the scan resumes
// after it. The regex version cost about 5 ms on a 30 KB test log and ran on
// every tool result; this one is 25-40x faster and allocates nothing (see
// BenchmarkCallSignature in loop_detect_bench_test.go). The regex is kept in
// loop_detect_oracle_test.go as the oracle of a differential fuzz test
// (FuzzNormalizeMatchesRegex); change both together.

// normalizeVolatile calls emit with the consecutive pieces of s after every
// volatile fragment is replaced by "#". Concatenating the pieces gives the
// normalized text; emit never sees an empty piece.
func normalizeVolatile(s string, emit func(string)) {
	last := 0
	for i := 0; i < len(s); {
		c := s[i]
		if !canStartVolatile(c) {
			i++
			continue
		}
		if n := volatileMatchAt(s, i); n > 0 {
			if i > last {
				emit(s[last:i])
			}
			emit("#")
			i += n
			last = i
			continue
		}
		if isASCIIDigit(c) {
			i = nextVolatileCandidateInDigits(s, i)
			continue
		}
		i++
	}
	if last < len(s) {
		emit(s[last:])
	}
}

// canStartVolatile reports whether a fragment can start with byte c: every
// alternative starts with a digit, "0x", "spool:", "spettro-spool-" or
// "job-".
func canStartVolatile(c byte) bool {
	return isASCIIDigit(c) || c == 's' || c == 'j'
}

// nextVolatileCandidateInDigits returns where to resume the scan after no
// fragment started at the digit s[i]. Inside a run of digits there is no
// word boundary, so only the boundary-free hex alternative can start there,
// and "0x" can only start at the run's last digit.
func nextVolatileCandidateInDigits(s string, i int) int {
	end := digitRunEnd(s, i)
	if end-1 > i && s[end-1] == '0' {
		return end - 1
	}
	return end
}

// volatileMatchAt returns the length of the volatile fragment starting at
// s[i], or 0 when none does, trying the alternatives in the regex's order.
func volatileMatchAt(s string, i int) int {
	boundary := isWordBoundary(s, i)
	if boundary && isASCIIDigit(s[i]) {
		if n := durationAt(s, i); n > 0 {
			return n
		}
		if n := timestampAt(s, i); n > 0 {
			return n
		}
		if n := clockAt(s, i); n > 0 {
			return n
		}
	}
	if n := hexAt(s, i); n > 0 {
		return n
	}
	if boundary {
		if n := prefixedNumberAt(s, i, "spool:"); n > 0 {
			return n
		}
	}
	if n := spoolPathAt(s, i); n > 0 {
		return n
	}
	if boundary {
		if n := prefixedNumberAt(s, i, "job-"); n > 0 {
			return n
		}
	}
	return 0
}

// durationUnits are the duration suffixes in the regex's alternation order.
var durationUnits = [...]string{"ns", "µs", "us", "ms", "s", "m", "h"}

// durationAt matches \d+(?:\.\d+)?(?:ns|µs|us|ms|s|m|h)\b at s[i].
func durationAt(s string, i int) int {
	digitsEnd := digitRunEnd(s, i)
	for _, p := range optionalFraction(s, digitsEnd) {
		if p < 0 {
			continue
		}
		for _, unit := range durationUnits {
			if strings.HasPrefix(s[p:], unit) && isWordBoundary(s, p+len(unit)) {
				return p + len(unit) - i
			}
		}
	}
	return 0
}

// timestampAt matches
// \d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?\b
// at s[i].
func timestampAt(s string, i int) int {
	if i+19 > len(s) ||
		!digitsAt(s, i, 4) || s[i+4] != '-' || !digitsAt(s, i+5, 2) || s[i+7] != '-' || !digitsAt(s, i+8, 2) ||
		(s[i+10] != 'T' && s[i+10] != ' ') ||
		!digitsAt(s, i+11, 2) || s[i+13] != ':' || !digitsAt(s, i+14, 2) || s[i+16] != ':' || !digitsAt(s, i+17, 2) {
		return 0
	}
	for _, p := range optionalFraction(s, i+19) {
		if p < 0 {
			continue
		}
		for _, end := range timezoneEnds(s, p) {
			if end >= 0 && isWordBoundary(s, end) {
				return end - i
			}
		}
	}
	return 0
}

// timezoneEnds returns the candidate ends of (?:Z|[+-]\d{2}:?\d{2})? at s[p]
// in the regex's preference order ("Z" or "+hh:mm", then "+hhmm", then no
// zone); -1 marks a candidate that does not apply.
func timezoneEnds(s string, p int) [3]int {
	ends := [3]int{-1, -1, p}
	switch {
	case p < len(s) && s[p] == 'Z':
		ends[0] = p + 1
	case p < len(s) && (s[p] == '+' || s[p] == '-') && digitsAt(s, p+1, 2):
		if p+3 < len(s) && s[p+3] == ':' && digitsAt(s, p+4, 2) {
			ends[0] = p + 6
		}
		if digitsAt(s, p+3, 2) {
			ends[1] = p + 5
		}
	}
	return ends
}

// clockAt matches \d{2}:\d{2}:\d{2}(?:\.\d+)?\b at s[i].
func clockAt(s string, i int) int {
	if i+8 > len(s) || !digitsAt(s, i, 2) || s[i+2] != ':' || !digitsAt(s, i+3, 2) || s[i+5] != ':' || !digitsAt(s, i+6, 2) {
		return 0
	}
	for _, p := range optionalFraction(s, i+8) {
		if p >= 0 && isWordBoundary(s, p) {
			return p - i
		}
	}
	return 0
}

// hexAt matches 0x[0-9a-fA-F]+ at s[i].
func hexAt(s string, i int) int {
	if !strings.HasPrefix(s[i:], "0x") {
		return 0
	}
	p := i + 2
	for p < len(s) && isHexDigit(s[p]) {
		p++
	}
	if p == i+2 {
		return 0
	}
	return p - i
}

// prefixedNumberAt matches prefix\d+ at s[i] (the caller checks the leading
// word boundary).
func prefixedNumberAt(s string, i int, prefix string) int {
	p := i + len(prefix)
	if !strings.HasPrefix(s[i:], prefix) || p >= len(s) || !isASCIIDigit(s[p]) {
		return 0
	}
	return digitRunEnd(s, p) - i
}

// spoolPathAt matches spettro-spool-[^\s/\\]*[/\\]\d+\.txt at s[i].
func spoolPathAt(s string, i int) int {
	const prefix = "spettro-spool-"
	if !strings.HasPrefix(s[i:], prefix) {
		return 0
	}
	p := i + len(prefix)
	for p < len(s) && !isRegexSpace(s[p]) && s[p] != '/' && s[p] != '\\' {
		p++
	}
	if p+1 >= len(s) || (s[p] != '/' && s[p] != '\\') || !isASCIIDigit(s[p+1]) {
		return 0
	}
	end := digitRunEnd(s, p+1)
	if !strings.HasPrefix(s[end:], ".txt") {
		return 0
	}
	return end + len(".txt") - i
}

// optionalFraction returns the candidate ends of (?:\.\d+)? at s[p] in the
// regex's (greedy) preference order: after the fraction, then without it.
// -1 marks an absent fraction.
func optionalFraction(s string, p int) [2]int {
	if p+1 < len(s) && s[p] == '.' && isASCIIDigit(s[p+1]) {
		return [2]int{digitRunEnd(s, p+1), p}
	}
	return [2]int{-1, p}
}

// digitRunEnd returns the index just past the run of ASCII digits at s[p].
func digitRunEnd(s string, p int) int {
	for p < len(s) && isASCIIDigit(s[p]) {
		p++
	}
	return p
}

// digitsAt reports whether s[p:p+n] is n ASCII digits.
func digitsAt(s string, p, n int) bool {
	if p < 0 || p+n > len(s) {
		return false
	}
	for k := range n {
		if !isASCIIDigit(s[p+k]) {
			return false
		}
	}
	return true
}

// isWordBoundary reports whether \b holds at s[p]: exactly one of the bytes
// around p is an ASCII word character (RE2's \b is ASCII-only).
func isWordBoundary(s string, p int) bool {
	before := p > 0 && isASCIIWordChar(s[p-1])
	after := p < len(s) && isASCIIWordChar(s[p])
	return before != after
}

func isASCIIDigit(c byte) bool { return '0' <= c && c <= '9' }

func isHexDigit(c byte) bool {
	return isASCIIDigit(c) || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
}

func isASCIIWordChar(c byte) bool {
	return isASCIIDigit(c) || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || c == '_'
}

// isRegexSpace reports whether c is in RE2's \s: [\t\n\f\r ].
func isRegexSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r'
}
