package agent

import (
	"strings"
)

// spettroCoAuthorTrailer is the mandatory Co-Authored-By trailer that Spettro
// guarantees on every commit it makes — directly via LLMCommitter, or
// indirectly when an LLM agent issues `git commit` through shell-exec/bash.
//
// Keep this string in sync with internal/agent/committer.go and
// internal/tui/model.go.
const spettroCoAuthorTrailer = "Co-Authored-By: Spettro <spettro@eyed.to>"

// commitTrailerFlag is the formatted `--trailer "..."` token we inject into
// rewritten git commit invocations. Single quotes keep the angle brackets safe
// from shell expansion inside `bash -lc`.
var commitTrailerFlag = " --trailer '" + spettroCoAuthorTrailer + "'"

// EnforceCommitCoAuthor rewrites a shell command so that every `git commit`
// invocation inside it carries the mandatory Spettro Co-Authored-By trailer.
//
// The transformation is:
//
//   - idempotent — segments that already mention `Co-Authored-By: Spettro` are
//     left alone, so chaining the rewriter twice (or a user-provided trailer
//     plus an auto-injected one) does not duplicate the line.
//
//   - segment-aware — multi-command strings like
//     `git status && git commit -m 'x' && git push`
//     get the trailer appended to the `git commit` segment only.
//
//   - quote- and subshell-safe — `;`, `|`, `&&`, `||`, and newlines inside
//     `'...'`, `"..."`, or `$(...)` are NOT treated as separators.
//
//   - tolerant of common git option prefixes (`-C dir`, `--git-dir=...`,
//     `-c key=val`, leading `env VAR=value`) and excludes plumbing variants
//     like `git commit-tree` and `git commit-graph`.
//
// The function is intentionally conservative: when the command shape is
// ambiguous (e.g. dynamic `$(...)` invocations, sub-shells, or piping git
// output through another tool), it falls back to leaving the segment as-is.
// The accompanying prompt rules in agents/git.md still require the trailer
// explicitly, so the LLM is the second line of defence.
func EnforceCommitCoAuthor(command string) string {
	if command == "" {
		return command
	}
	segments := splitShellSegmentsWithRanges(command)
	if len(segments) == 0 {
		return command
	}
	var out strings.Builder
	out.Grow(len(command) + len(commitTrailerFlag))
	cursor := 0
	for _, seg := range segments {
		out.WriteString(command[cursor:seg.start])
		body := command[seg.start:seg.end]
		if commitSegmentNeedsTrailer(body) {
			trailing := body[len(strings.TrimRight(body, " \t")):]
			leading := body[:len(body)-len(trailing)]
			out.WriteString(leading)
			out.WriteString(commitTrailerFlag)
			out.WriteString(trailing)
		} else {
			out.WriteString(body)
		}
		cursor = seg.end
	}
	out.WriteString(command[cursor:])
	return out.String()
}

// shellSegmentRange records the [start, end) bounds of one top-level shell
// segment inside the original command string. End points to the first index
// of the separator (or len(command) for the trailing segment).
type shellSegmentRange struct {
	start int
	end   int
}

// splitShellSegmentsWithRanges is a position-preserving variant of
// splitShellCommandSegments. It walks the command character by character,
// tracks quoting and `$(...)` depth, and emits the indices of each top-level
// segment. Unlike the existing splitter (which returns trimmed segment text)
// this keeps original whitespace and lets callers patch segments back into
// the source string without losing operators.
func splitShellSegmentsWithRanges(command string) []shellSegmentRange {
	var (
		ranges                  []shellSegmentRange
		inSingle, inDouble, esc bool
		subDepth                int
		start                   = 0
	)
	flush := func(endExclusive int) {
		ranges = append(ranges, shellSegmentRange{start: start, end: endExclusive})
	}
	for i := 0; i < len(command); i++ {
		ch := command[i]
		if esc {
			esc = false
			continue
		}
		switch ch {
		case '\\':
			esc = true
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '(':
			if !inSingle && !inDouble && i > 0 && command[i-1] == '$' {
				subDepth++
			}
		case ')':
			if !inSingle && !inDouble && subDepth > 0 {
				subDepth--
			}
		case ';', '\n':
			if !inSingle && !inDouble && subDepth == 0 {
				flush(i)
				start = i + 1
			}
		case '|':
			if !inSingle && !inDouble && subDepth == 0 {
				flush(i)
				if i+1 < len(command) && command[i+1] == '|' {
					i++
				}
				start = i + 1
			}
		case '&':
			if !inSingle && !inDouble && subDepth == 0 && i+1 < len(command) && command[i+1] == '&' {
				flush(i)
				i++
				start = i + 1
			}
		}
	}
	if start <= len(command) {
		ranges = append(ranges, shellSegmentRange{start: start, end: len(command)})
	}
	return ranges
}

// commitSegmentNeedsTrailer returns true when `seg` invokes `git commit`
// without already mentioning the Spettro co-author trailer.
func commitSegmentNeedsTrailer(seg string) bool {
	if !isGitCommitInvocation(seg) {
		return false
	}
	// Idempotent: don't double-add if the user (or a previous pass) already
	// included the trailer somewhere in the segment text. We match a generous
	// "Co-Authored-By: Spettro" prefix so any variation of email/case still
	// counts.
	if strings.Contains(seg, "Co-Authored-By: Spettro") {
		return false
	}
	return true
}

// isGitCommitInvocation answers "does this segment run `git commit`?". We
// lex the segment shell-style (quotes are recognised) and skip leading env
// assignments + git's own multi-token global options before checking the
// subcommand. Plumbing variants like `commit-tree` / `commit-graph` do NOT
// match — only the porcelain `commit` does.
func isGitCommitInvocation(seg string) bool {
	tokens := lexShellTokens(seg)
	idx := 0
	for idx < len(tokens) {
		t := tokens[idx]
		if t == "" {
			idx++
			continue
		}
		// Strip a leading `env` wrapper. Anything that's not `git`/`/.../git`
		// after env assignments means this segment does not invoke git at all.
		if t == "env" {
			idx++
			continue
		}
		if !looksLikeEnvAssignment(t) {
			break
		}
		idx++
	}
	if idx >= len(tokens) {
		return false
	}
	cmd := tokens[idx]
	if cmd != "git" && !strings.HasSuffix(cmd, "/git") {
		return false
	}
	idx++
	for idx < len(tokens) {
		f := tokens[idx]
		if !strings.HasPrefix(f, "-") {
			break
		}
		// Skip git global options that take a separate value.
		switch f {
		case "-C", "--git-dir", "--work-tree", "-c", "--namespace", "--super-prefix", "--exec-path":
			idx += 2
			continue
		}
		// Inline `--key=val` style — only the flag token to skip.
		idx++
	}
	if idx >= len(tokens) {
		return false
	}
	return tokens[idx] == "commit"
}

// looksLikeEnvAssignment matches the `VAR=value` shape — i.e. an unquoted
// identifier followed by `=`. Anything containing slashes, quotes, or shell
// metacharacters is rejected so we don't mistake paths or strings for env
// assignments.
func looksLikeEnvAssignment(t string) bool {
	eq := strings.IndexByte(t, '=')
	if eq <= 0 {
		return false
	}
	for i := range eq {
		ch := t[i]
		switch {
		case ch >= 'A' && ch <= 'Z':
		case ch >= 'a' && ch <= 'z':
		case ch >= '0' && ch <= '9':
		case ch == '_':
		default:
			return false
		}
	}
	return true
}

// lexShellTokens splits seg into shell-style tokens respecting `'...'`,
// `"..."`, and backslash escapes. Quotes are preserved in the returned tokens
// only when they were already part of the inner text — we strip the outer
// quote characters. Subshell starts (`$(`) are treated as opaque single tokens
// so we never mistake their contents for the actual command.
func lexShellTokens(seg string) []string {
	var tokens []string
	var (
		cur                     strings.Builder
		inSingle, inDouble, esc bool
		subDepth                int
		started                 bool
	)
	flush := func() {
		if started {
			tokens = append(tokens, cur.String())
			cur.Reset()
			started = false
		}
	}
	for i := 0; i < len(seg); i++ {
		ch := seg[i]
		if esc {
			cur.WriteByte(ch)
			esc = false
			started = true
			continue
		}
		switch ch {
		case '\\':
			esc = true
		case '\'':
			if !inDouble {
				inSingle = !inSingle
				started = true
				continue
			}
			cur.WriteByte(ch)
			started = true
		case '"':
			if !inSingle {
				inDouble = !inDouble
				started = true
				continue
			}
			cur.WriteByte(ch)
			started = true
		case '$':
			if !inSingle && i+1 < len(seg) && seg[i+1] == '(' {
				subDepth++
				cur.WriteByte(ch)
				started = true
				continue
			}
			cur.WriteByte(ch)
			started = true
		case '(':
			cur.WriteByte(ch)
			started = true
		case ')':
			if subDepth > 0 {
				subDepth--
			}
			cur.WriteByte(ch)
			started = true
		case ' ', '\t':
			if inSingle || inDouble || subDepth > 0 {
				cur.WriteByte(ch)
				started = true
				continue
			}
			flush()
		default:
			cur.WriteByte(ch)
			started = true
		}
	}
	flush()
	return tokens
}
