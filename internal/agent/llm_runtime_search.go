package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"spettro/internal/ripgrep"
	"spettro/internal/sandbox"
)

const (
	// maxGlobResults caps the glob tool's file list.
	maxGlobResults = 1000
	// maxGrepLineChars clips each line grep prints, so one minified bundle
	// line cannot consume the whole output budget.
	maxGrepLineChars = 400
	// maxSearchFileBytes skips files larger than this (both backends).
	maxSearchFileBytes = 16 << 20
	// binarySniffBytes is how much of a file is inspected for a NUL byte.
	binarySniffBytes = 8000
)

// expandBraces expands shell-style brace alternatives: "*.{ts,tsx}" becomes
// ["*.ts", "*.tsx"]. Nested and repeated groups expand recursively; a pattern
// without a complete group is returned as-is.
func expandBraces(pattern string) []string {
	open := strings.IndexByte(pattern, '{')
	if open < 0 {
		return []string{pattern}
	}
	depth, closeIdx := 0, -1
	var commas []int
	for i := open; i < len(pattern) && closeIdx < 0; i++ {
		switch pattern[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				closeIdx = i
			}
		case ',':
			if depth == 1 {
				commas = append(commas, i)
			}
		}
	}
	if closeIdx < 0 || len(commas) == 0 {
		return []string{pattern}
	}
	prefix, suffix := pattern[:open], pattern[closeIdx+1:]
	var out []string
	start := open + 1
	for _, c := range append(commas, closeIdx) {
		out = append(out, expandBraces(prefix+pattern[start:c]+suffix)...)
		start = c + 1
	}
	return out
}

// grepTypeNames lists the grep type filters, for the unknown-type error.
const grepTypeNames = "go, ts, js, py, rs, md, toml, json, yaml, sh"

// typeExtensions maps type names (and the long names models often use for
// them) to file extensions. ok is false for an unknown name: silently
// searching every file would let the model believe the filter applied.
func typeExtensions(t string) (exts []string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "":
		return nil, true
	case "go", "golang":
		return []string{".go"}, true
	case "ts", "typescript", "tsx":
		return []string{".ts", ".tsx"}, true
	case "js", "javascript", "jsx":
		return []string{".js", ".jsx", ".mjs"}, true
	case "py", "python":
		return []string{".py"}, true
	case "rs", "rust":
		return []string{".rs"}, true
	case "md", "markdown":
		return []string{".md"}, true
	case "toml":
		return []string{".toml"}, true
	case "json":
		return []string{".json"}, true
	case "yaml", "yml":
		return []string{".yaml", ".yml"}, true
	case "sh", "bash", "shell":
		return []string{".sh", ".bash"}, true
	default:
		return nil, false
	}
}

// grepArgs are the grep tool's arguments. Besides the canonical names it
// accepts the spellings other harnesses use: include for glob, head_limit for
// max_results, and the rg/grep flags -i (case_insensitive), -C/-A/-B
// (context), -c (count mode) and -l (files_with_matches mode).
type grepArgs struct {
	Pattern string `json:"pattern"`
	// Symbol, when present (even empty), runs the symbol-index search instead
	// of a regex: ranked definitions of an identifier, then its usages. It is
	// what the retired repo-search tool did; an empty symbol lists all files.
	Symbol          *string  `json:"symbol"`
	Path            string   `json:"path"`
	Glob            string   `json:"glob"`
	Include         string   `json:"include"`
	Type            string   `json:"type"`
	CaseInsensitive flexBool `json:"case_insensitive"`
	Context         flexInt  `json:"context"`
	OutputMode      string   `json:"output_mode"`
	MaxResults      flexInt  `json:"max_results"`
	HeadLimit       flexInt  `json:"head_limit"`

	// The flag spellings are decoded by UnmarshalJSON with exact-case keys:
	// encoding/json folds case, which would read rg's -c (count) as -C.
	DashI     flexBool `json:"-"`
	DashC     flexInt  `json:"-"`
	DashA     flexInt  `json:"-"`
	DashB     flexInt  `json:"-"`
	DashCount flexBool `json:"-"`
	DashL     flexBool `json:"-"`
}

func (a *grepArgs) UnmarshalJSON(data []byte) error {
	type plain grepArgs
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	flags := []struct {
		key string
		dst json.Unmarshaler
	}{
		{"-i", &p.DashI}, {"-C", &p.DashC}, {"-A", &p.DashA}, {"-B", &p.DashB},
		{"-c", &p.DashCount}, {"-l", &p.DashL},
	}
	for _, f := range flags {
		if v, ok := raw[f.key]; ok {
			if err := f.dst.UnmarshalJSON(v); err != nil {
				return fmt.Errorf("%s: %w", f.key, err)
			}
		}
	}
	*a = grepArgs(p)
	return nil
}

// grepQuery is a validated grep request.
type grepQuery struct {
	pattern    string
	re         *regexp.Regexp
	ignoreCase bool
	root       string // absolute file or directory to search
	rootRel    string // root relative to the workspace ("." for the workspace)
	rootIsFile bool
	globs      []string      // brace-expanded glob filters (OR)
	globPats   []globPattern // globs, compiled
	exts       []string      // type filter
	context    int
	mode       string
	max        int
}

// grepLine is one printed line of a grep result: a match or a context line.
type grepLine struct {
	num   int
	text  string
	match bool
}

// grepFileResult holds one file's matches (and, in content mode, context).
type grepFileResult struct {
	path  string
	count int
	lines []grepLine
}

func (r *toolRuntime) newGrepQuery(args grepArgs) (grepQuery, error) {
	q := grepQuery{pattern: args.Pattern}
	if strings.TrimSpace(args.Pattern) == "" {
		return q, fmt.Errorf("grep: pattern is required")
	}
	q.ignoreCase = bool(args.CaseInsensitive) || bool(args.DashI)
	regexPattern := args.Pattern
	if q.ignoreCase {
		regexPattern = "(?i)" + regexPattern
	}
	re, err := regexp.Compile(regexPattern)
	if err != nil {
		return q, fmt.Errorf("grep: invalid pattern (Go RE2 syntax): %w", err)
	}
	q.re = re
	q.root, q.rootRel = r.cwd, "."
	if p := strings.TrimSpace(args.Path); p != "" && p != "." {
		abs, rel, err := r.resolvePath(p)
		if err != nil {
			return q, fmt.Errorf("grep path: %w", err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return q, fmt.Errorf("grep path: %w", err)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return q, fmt.Errorf("grep path: %s is not a regular file", rel)
		}
		q.root, q.rootRel, q.rootIsFile = abs, rel, !info.IsDir()
		if q.rootRel == "" {
			q.rootRel = "."
		}
	}
	if g := firstNonEmpty(args.Glob, args.Include); g != "" {
		q.globs = expandBraces(strings.TrimPrefix(g, "./"))
		q.globPats = compileGlobs(q.globs)
	}
	exts, ok := typeExtensions(args.Type)
	if !ok {
		return q, fmt.Errorf("grep: unknown type %q (supported: %s); use glob for other extensions, e.g. *.java", args.Type, grepTypeNames)
	}
	q.exts = exts
	q.context = max(int(args.Context), int(args.DashC), int(args.DashA), int(args.DashB), 0)
	q.mode = strings.TrimSpace(args.OutputMode)
	if q.mode == "" {
		switch {
		case bool(args.DashCount):
			q.mode = "count"
		case bool(args.DashL):
			q.mode = "files_with_matches"
		default:
			q.mode = "content"
		}
	}
	switch q.mode {
	case "content", "files_with_matches", "count":
	default:
		return q, fmt.Errorf("grep: unknown output_mode %q (content, files_with_matches or count)", q.mode)
	}
	q.max = int(args.MaxResults)
	if q.max <= 0 {
		q.max = int(args.HeadLimit)
	}
	if q.max <= 0 {
		q.max = 200
	}
	return q, nil
}

// wantsFile applies the type and glob filters to a file (path relative to the
// workspace). Globs containing a slash match the path — relative to the
// workspace or to the search root — and slash-less globs match the file name.
func (q grepQuery) wantsFile(rel string) bool {
	if len(q.exts) > 0 && !slices.Contains(q.exts, strings.ToLower(filepath.Ext(rel))) {
		return false
	}
	if len(q.globs) == 0 || q.rootIsFile {
		return true
	}
	relRoot := rel
	if q.rootRel != "." {
		relRoot = strings.TrimPrefix(rel, q.rootRel+"/")
	}
	base := rel[strings.LastIndexByte(rel, '/')+1:]
	for i, g := range q.globs {
		if strings.Contains(g, "/") {
			if q.globPats[i].match(rel) || q.globPats[i].match(relRoot) {
				return true
			}
			continue
		}
		if ok, err := filepath.Match(g, base); err == nil && ok {
			return true
		}
	}
	return false
}

// runGrep implements the grep tool: ripgrep when it is installed (fast, and
// exact about .gitignore), otherwise a Go walk honouring the root .gitignore.
// Both skip binary files, clip long lines and stop at max_results.
// runSymbolSearch answers grep's symbol form: a case-insensitive literal
// search backed by the symbol index, listing ranked definitions of an
// identifier before its usages.
func (r *toolRuntime) runSymbolSearch(ctx context.Context, args grepArgs) (string, error) {
	if strings.TrimSpace(args.Pattern) != "" {
		return "", fmt.Errorf("grep: pass either pattern (a regex search) or symbol (a definitions-first lookup), not both")
	}
	out, err := r.searcher.Search(ctx, r.cwd, strings.TrimSpace(*args.Symbol))
	if err != nil {
		return "", err
	}
	r.markReadFromSearch(out)
	return r.spoolResult("grep", out), nil
}

// runListDir answers glob without a pattern: the immediate entries of one
// directory (default: the working directory), directories marked with a
// trailing slash. Unlike a pattern walk it is not recursive and shows every
// entry, ignored or not, exactly as the directory holds them.
func (r *toolRuntime) runListDir(dirPath string) (string, error) {
	dir := r.cwd
	if strings.TrimSpace(dirPath) != "" {
		abs, _, err := r.resolvePath(dirPath)
		if err != nil {
			return "", fmt.Errorf("glob: %w", err)
		}
		dir = abs
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			lines = append(lines, e.Name()+"/")
		} else {
			lines = append(lines, e.Name())
		}
	}
	return strings.Join(lines, "\n"), nil
}

func (r *toolRuntime) runGrep(ctx context.Context, args grepArgs) (string, error) {
	q, err := r.newGrepQuery(args)
	if err != nil {
		return "", err
	}
	var results []grepFileResult
	usedRipgrep := false
	if rg, ok := lookRipgrep(); ok {
		results, err = r.grepWithRipgrep(ctx, rg, q)
		usedRipgrep = err == nil
	} else {
		r.fetchRipgrep()
	}
	if !usedRipgrep {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("grep: %w", ctxErr)
		}
		results, err = r.grepWithWalk(ctx, q)
		if err != nil {
			return "", fmt.Errorf("grep walk: %w", err)
		}
	}
	var truncated bool
	if q.mode == "content" {
		results, truncated = capGrepResults(results, q.max, q.context)
	} else if len(results) > q.max {
		// count and files_with_matches answer "how many" and "which files":
		// max_results caps the files listed, never a file's count.
		results, truncated = results[:q.max], true
	}
	if len(results) == 0 {
		return fmt.Sprintf("no matches for %q", args.Pattern), nil
	}
	// Mark as read from search
	r.mu.Lock()
	for _, fr := range results {
		r.readSet[fr.path] = struct{}{}
	}
	r.mu.Unlock()
	return formatGrepResults(results, q, truncated), nil
}

// looksBinary reports whether data looks like a binary file: a NUL byte in
// the first binarySniffBytes, the heuristic git and ripgrep use.
func looksBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), binarySniffBytes)], 0) >= 0
}

// lookRipgrep finds the rg binary: on PATH, or the copy downloaded into
// ~/.spettro/bin. It never starts a download (fetchRipgrep does). It is a
// variable so tests can force the Go backend or a specific binary.
var lookRipgrep = func() (string, bool) { return ripgrep.Default.Path() }

// fetchRipgrep starts the background download of rg (product decision D9)
// after a grep had to use the Go backend, unless the sandbox confines the
// network: a user who cut the agent's commands off the network does not
// expect Spettro to reach out on their behalf. The download never delays
// this or any other call; later greps use rg once it is installed.
func (r *toolRuntime) fetchRipgrep() {
	if p := r.sandboxPolicy(); p.Enabled() && p.Net != "" && p.Net != sandbox.NetAll {
		return
	}
	ripgrep.Default.EnsureDownload()
}

// grepWithRipgrep runs the search through ripgrep's JSON output. Any failure
// other than "no matches" (an rg too old for a flag, a pattern rg's regex
// engine rejects) is returned so the caller falls back to the Go walk.
func (r *toolRuntime) grepWithRipgrep(ctx context.Context, rg string, q grepQuery) ([]grepFileResult, error) {
	// No --sort path: it makes rg search one file at a time (a full scan of
	// a 61k-file tree took 2.06 s sorted against 1.70 s, and 288 ms against
	// 187 ms on an 11k-file subtree; rgbench.sh / rgsort.sh in the perf
	// notes). Results are sorted into walk order here instead, which keeps
	// the listing stable; when max_results truncates, which files made the
	// cut can vary between runs (product decision D6).
	args := []string{
		"--json", "--no-config", "--hidden", "--no-require-git", "--no-messages",
		"--max-filesize", strconv.Itoa(maxSearchFileBytes),
	}
	if q.ignoreCase {
		args = append(args, "--ignore-case")
	}
	if q.mode == "content" && q.context > 0 {
		args = append(args, "--context", strconv.Itoa(q.context))
	}
	// keep re-applies the root .gitignore to rg's results when include globs
	// were delegated: rg lets a --glob override win over every ignore file,
	// so `--glob '*.log'` would search the *.log files .gitignore excludes.
	var keep func(rel string) bool
	if !q.rootIsFile {
		// The trailing slash limits the exclusion to directories, as in the
		// Go walk: a file named build or dist is still searched.
		for name := range skipDirs {
			args = append(args, "--glob", "!"+name+"/")
		}
		// rg ORs include globs, so only one kind of include can be delegated
		// to it; wantsFile applies every filter again on the results anyway.
		delegated := false
		switch {
		case len(q.globs) > 0 && !slices.ContainsFunc(q.globs, func(g string) bool { return strings.Contains(g, "/") }):
			for _, g := range q.globs {
				args = append(args, "--glob", g)
			}
			delegated = true
		case len(q.globs) == 0 && len(q.exts) > 0:
			for _, ext := range q.exts {
				args = append(args, "--glob", "*"+ext)
			}
			delegated = true
		}
		if delegated {
			walker := r.newWorkspaceWalker()
			keep = func(rel string) bool { return !walker.ignoredBelow(q.rootRel, rel) }
		}
	}
	args = append(args, "--regexp", q.pattern, "--", q.rootRel)

	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(rctx, rg, args...)
	cmd.Dir = r.cwd
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	results, stoppedEarly, parseErr := parseRipgrepJSON(stdout, q, keep)
	if stoppedEarly {
		cancel() // enough matches: stop rg instead of draining it
	}
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if parseErr != nil {
		return nil, parseErr
	}
	if waitErr != nil && !stoppedEarly {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 1 {
			return nil, fmt.Errorf("rg: %v: %s", waitErr, strings.TrimSpace(stderr.String()))
		}
	}
	slices.SortFunc(results, func(a, b grepFileResult) int { return compareWalkOrder(a.path, b.path) })
	return results, nil
}

// rgOverfetch is how many times max_results worth of matches (content
// mode) or files (the other modes) grep reads from an unsorted rg before
// stopping it. rg reports files in the order its threads finish them, so
// the margin lets the sorted, truncated answer lean towards the files a
// sorted search would have listed first.
const rgOverfetch = 4

// rgMessage is the subset of ripgrep's --json output grep reads.
type rgMessage struct {
	Type string `json:"type"`
	Data struct {
		Path       rgText `json:"path"`
		Lines      rgText `json:"lines"`
		LineNumber int    `json:"line_number"`
	} `json:"data"`
}

type rgText struct {
	Text  *string `json:"text"`
	Bytes string  `json:"bytes"` // base64, for non-UTF-8 content; skipped
}

// parseRipgrepJSON reads rg --json output into per-file results, stopping once
// more than rgOverfetch times q.max matches (content mode) or files (the other
// modes) have been seen; stopped reports that. keep, when set, is an extra
// filter on each file's workspace-relative path.
func parseRipgrepJSON(stdout io.Reader, q grepQuery, keep func(rel string) bool) (results []grepFileResult, stopped bool, err error) {
	total := 0
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	var cur *grepFileResult
	flush := func() {
		if cur != nil && cur.count > 0 {
			results = append(results, *cur)
		}
		cur = nil
	}
	for sc.Scan() {
		var msg rgMessage
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		switch msg.Type {
		case "begin":
			flush()
			if msg.Data.Path.Text == nil {
				continue // non-UTF-8 path
			}
			rel := filepath.ToSlash(filepath.Clean(*msg.Data.Path.Text))
			if !q.wantsFile(rel) || (keep != nil && !keep(rel)) {
				continue
			}
			cur = &grepFileResult{path: rel}
		case "match", "context":
			if cur == nil || msg.Data.Lines.Text == nil {
				continue
			}
			text := strings.TrimRight(*msg.Data.Lines.Text, "\r\n")
			isMatch := msg.Type == "match"
			if isMatch {
				cur.count++
				total++
			}
			if q.mode == "content" {
				cur.lines = append(cur.lines, grepLine{num: msg.Data.LineNumber, text: text, match: isMatch})
			}
		case "end":
			flush()
			limit := rgOverfetch * q.max
			if q.mode == "content" && total > limit || q.mode != "content" && len(results) > limit {
				return results, true, nil
			}
		}
	}
	flush()
	return results, false, sc.Err()
}

// capGrepResults trims results to at most max matches, keeping the context
// lines that belong to the kept matches. truncated reports whether anything
// was dropped.
func capGrepResults(results []grepFileResult, maxMatches, context int) ([]grepFileResult, bool) {
	total := 0
	for i, fr := range results {
		if total+fr.count <= maxMatches {
			total += fr.count
			continue
		}
		keep := maxMatches - total
		if keep <= 0 {
			return results[:i], true
		}
		fr.count = keep
		if len(fr.lines) > 0 {
			seen, lastMatch := 0, 0
			for _, l := range fr.lines {
				if l.match {
					seen++
					if seen == keep {
						lastMatch = l.num
						break
					}
				}
			}
			cut := len(fr.lines)
			for j, l := range fr.lines {
				if l.num > lastMatch+context || (l.match && l.num > lastMatch) {
					cut = j
					break
				}
			}
			fr.lines = fr.lines[:cut]
		}
		out := append(results[:i:i], fr)
		return out, true
	}
	return results, false
}

// clipLine shortens a line to maxGrepLineChars, on a rune boundary.
func clipLine(s string) string {
	if len(s) <= maxGrepLineChars {
		return s
	}
	cut := maxGrepLineChars
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf(" …(line truncated, %d chars)", len(s))
}

// formatGrepResults renders results in the grep tool's output format.
func formatGrepResults(results []grepFileResult, q grepQuery, truncated bool) string {
	totalMatches := 0
	for _, fr := range results {
		totalMatches += fr.count
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d matches in %d files:\n", totalMatches, len(results))
	switch q.mode {
	case "files_with_matches":
		for _, fr := range results {
			sb.WriteString(fr.path)
			sb.WriteString("\n")
		}
	case "count":
		for _, fr := range results {
			fmt.Fprintf(&sb, "%s: %d\n", fr.path, fr.count)
		}
	default: // "content"
		for _, fr := range results {
			prev := 0
			for _, l := range fr.lines {
				if prev > 0 && l.num != prev+1 {
					sb.WriteString("--\n")
				}
				fmt.Fprintf(&sb, "%s:%d: %s\n", fr.path, l.num, clipLine(l.text))
				prev = l.num
			}
		}
	}
	if truncated {
		unit := "matches"
		if q.mode != "content" {
			unit = "files"
		}
		fmt.Fprintf(&sb, "(results truncated at %d %s; narrow the pattern, path or glob, or raise max_results)\n", q.max, unit)
	}
	return strings.TrimRight(sb.String(), "\n")
}
