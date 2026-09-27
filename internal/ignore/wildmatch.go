package ignore

import "strings"

// Wildmatch reports whether text matches pattern the way git's wildmatch
// does with WM_PATHNAME (the mode .gitignore rules with a slash use):
//
//   - "*" matches any run of characters except "/", "?" any one but "/";
//   - "**" that fills a whole path segment ("a/**/b", "**/b", "a/**")
//     matches any number of segments, zero included; any other "**" is "*";
//   - "[...]" is a bracket expression: "!" or "^" negates it, "a-z" ranges,
//     "[:alpha:]"-style classes, and it never matches "/";
//   - a backslash makes the next character literal.
//
// It is a port of dowild() in git's wildmatch.c and keeps its three-way
// result, which prunes the search: a failed "*" tells an enclosing "**"
// whether trying further positions can still help. It does not allocate.
func Wildmatch(pattern, text string) bool {
	return dowild(pattern, text) == wmMatch
}

// dowild results, as in git.
const (
	wmMatch            = 0
	wmNoMatch          = 1
	wmAbortAll         = -1 // no later text position can match either
	wmAbortToStarStar  = -2 // only an enclosing "**" may still match
	wmNegateClass      = '!'
	wmNegateClassAlias = '^'
)

// at returns s[i], or 0 past the end (git works on NUL-terminated strings).
func at(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return 0
}

func dowild(pat, text string) int {
	p, t := 0, 0
	for ; p < len(pat); p, t = p+1, t+1 {
		pch := pat[p]
		tch := at(text, t)
		if t >= len(text) && pch != '*' {
			return wmAbortAll
		}
		switch pch {
		case '\\':
			// Literal match with the following character.
			p++
			pch = at(pat, p)
			if tch != pch {
				return wmNoMatch
			}
			continue
		case '?':
			if tch == '/' {
				return wmNoMatch
			}
			continue
		case '*':
			matchSlash := false
			p++
			if at(pat, p) == '*' {
				prevP := p - 2
				for at(pat, p+1) == '*' {
					p++
				}
				p++
				// "**" counts only when it fills a whole segment.
				if (prevP < 0 || pat[prevP] == '/') &&
					(p >= len(pat) || pat[p] == '/' || (pat[p] == '\\' && at(pat, p+1) == '/')) {
					// Try "**/" as matching no directory at all first.
					if p < len(pat) && pat[p] == '/' && dowild(pat[p+1:], text[t:]) == wmMatch {
						return wmMatch
					}
					matchSlash = true
				}
			}
			if p >= len(pat) {
				// A trailing "**" matches everything; a trailing "*" only
				// what has no slash left.
				if !matchSlash && strings.IndexByte(text[t:], '/') >= 0 {
					return wmNoMatch
				}
				return wmMatch
			}
			if !matchSlash && pat[p] == '/' {
				// One "*" followed by a slash matches up to the next slash,
				// which the loop then consumes.
				slash := strings.IndexByte(text[t:], '/')
				if slash < 0 {
					return wmNoMatch
				}
				t += slash
				break
			}
			for {
				if t >= len(text) {
					break
				}
				// A literal after the star: skip ahead to its next
				// occurrence (never past a slash unless "**").
				if !isGlobSpecial(pat[p]) {
					want := pat[p]
					for t < len(text) && (matchSlash || text[t] != '/') && text[t] != want {
						t++
					}
					if t >= len(text) || text[t] != want {
						if matchSlash {
							return wmAbortAll
						}
						return wmAbortToStarStar
					}
				}
				tch = text[t]
				if m := dowild(pat[p:], text[t:]); m != wmNoMatch {
					if !matchSlash || m != wmAbortToStarStar {
						return m
					}
				} else if !matchSlash && tch == '/' {
					return wmAbortToStarStar
				}
				t++
			}
			return wmAbortAll
		case '[':
			np, ok := matchBracket(pat, p, tch)
			if np < 0 {
				return wmAbortAll
			}
			if !ok || tch == '/' {
				return wmNoMatch
			}
			p = np
			continue
		default:
			if tch != pch {
				return wmNoMatch
			}
			continue
		}
		// Only the "*/" case breaks out of the switch to here: t is on the
		// slash that p is on, and the loop's increment steps over both.
	}
	if t < len(text) {
		return wmNoMatch
	}
	return wmMatch
}

// matchBracket evaluates the bracket expression starting at pat[p] ('[')
// against tch. It returns the index of the closing ']' and whether tch is
// in the set; the index is -1 for a malformed expression (git aborts then).
func matchBracket(pat string, p int, tch byte) (int, bool) {
	p++
	pch := at(pat, p)
	if pch == wmNegateClassAlias {
		pch = wmNegateClass
	}
	negated := pch == wmNegateClass
	if negated {
		p++
		pch = at(pat, p)
	}
	var prev byte
	matched := false
	for {
		if pch == 0 {
			return -1, false
		}
		switch {
		case pch == '\\':
			p++
			pch = at(pat, p)
			if pch == 0 {
				return -1, false
			}
			if tch == pch {
				matched = true
			}
		case pch == '-' && prev != 0 && at(pat, p+1) != 0 && at(pat, p+1) != ']':
			p++
			pch = at(pat, p)
			if pch == '\\' {
				p++
				pch = at(pat, p)
				if pch == 0 {
					return -1, false
				}
			}
			if tch <= pch && tch >= prev {
				matched = true
			}
			pch = 0 // so prev resets: a range cannot start a range
		case pch == '[' && at(pat, p+1) == ':':
			s := p + 2
			q := s
			for q < len(pat) && pat[q] != ']' {
				q++
			}
			if q >= len(pat) {
				return -1, false
			}
			if q-s-1 < 0 || pat[q-1] != ':' {
				// No ":]": an ordinary '[' in the set.
				p = s - 2
				pch = '['
				if tch == pch {
					matched = true
				}
				break
			}
			in, ok := inClass(pat[s:q-1], tch)
			if !ok {
				return -1, false
			}
			if in {
				matched = true
			}
			p = q
			pch = 0
		default:
			if tch == pch {
				matched = true
			}
		}
		prev = pch
		p++
		pch = at(pat, p)
		if pch == ']' {
			break
		}
	}
	return p, matched != negated
}

// inClass tests tch against a POSIX character class name; ok is false for
// an unknown name.
func inClass(name string, c byte) (in, ok bool) {
	isUpper := c >= 'A' && c <= 'Z'
	isLower := c >= 'a' && c <= 'z'
	isDigit := c >= '0' && c <= '9'
	isAlpha := isUpper || isLower
	switch name {
	case "alnum":
		return isAlpha || isDigit, true
	case "alpha":
		return isAlpha, true
	case "blank":
		return c == ' ' || c == '\t', true
	case "cntrl":
		return c < 0x20 || c == 0x7f, true
	case "digit":
		return isDigit, true
	case "graph":
		return c > 0x20 && c < 0x7f, true
	case "lower":
		return isLower, true
	case "print":
		return c >= 0x20 && c < 0x7f, true
	case "punct":
		return c > 0x20 && c < 0x7f && !isAlpha && !isDigit, true
	case "space":
		return c == ' ' || (c >= '\t' && c <= '\r'), true
	case "upper":
		return isUpper, true
	case "xdigit":
		return isDigit || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F'), true
	}
	return false, false
}

// isGlobSpecial reports whether c starts wildmatch syntax.
func isGlobSpecial(c byte) bool { return c == '*' || c == '?' || c == '[' || c == '\\' }
