package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"spettro/internal/ignore"
)

// skipDirs are directories to skip when walking the workspace. They are only
// skipped below the search root: a search pointed explicitly at one of them
// still runs.
var skipDirs = map[string]bool{
	".git":         true,
	".spettro":     true,
	"vendor":       true,
	"node_modules": true,
	"dist":         true,
	"build":        true,
}

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

// workspaceWalker walks a directory tree below the workspace the way the
// search tools see it: skipDirs and the root .gitignore prune entries, and
// symlinked directories are not followed.
type workspaceWalker struct {
	cwd     string
	matcher *ignore.Matcher
	// symlinkedFiles makes the walk visit symlinks to regular files (glob:
	// a CLAUDE.md -> AGENTS.md link is a file the model should see listed).
	// grep leaves it off, matching ripgrep, which skips every symlink it
	// meets while traversing; both grep backends then agree.
	symlinkedFiles bool
}

func (r *toolRuntime) newWorkspaceWalker() workspaceWalker {
	return workspaceWalker{cwd: r.cwd, matcher: ignore.NewMatcher(r.cwd)}
}

// ignoredBelow reports whether rel (a file, relative to the workspace) is
// excluded by the root .gitignore, checking the file and each directory
// strictly below rootRel — the same entries the walk would have pruned.
func (w workspaceWalker) ignoredBelow(rootRel, rel string) bool {
	if w.matcher.Ignored(rel, false) {
		return true
	}
	for dir := path.Dir(rel); dir != "." && dir != "/" && dir != rootRel; dir = path.Dir(dir) {
		if rootRel != "." && !strings.HasPrefix(dir, rootRel+"/") {
			break
		}
		if w.matcher.Ignored(dir, true) {
			return true
		}
	}
	return false
}

// walk calls visit for every regular file below root (and, with
// symlinkedFiles, every symlink to one) with its path relative
// to the workspace (slash-separated). It stops early when ctx is done (the
// ctx error is returned) or when visit returns errStopWalk.
func (w workspaceWalker) walk(ctx context.Context, root string, visit func(abs, rel string, d fs.DirEntry) error) error {
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return nil // skip inaccessible entries
		}
		rel, relErr := filepath.Rel(w.cwd, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if path == root {
				return nil
			}
			if skipDirs[d.Name()] || w.matcher.Ignored(rel, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			if !w.symlinkedFiles {
				return nil
			}
			if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
				return nil // dangling, or a link to a directory (not followed)
			}
		} else if !d.Type().IsRegular() {
			return nil
		}
		if w.matcher.Ignored(rel, false) {
			return nil
		}
		return visit(path, rel, d)
	})
	if errors.Is(err, errStopWalk) {
		return nil
	}
	return err
}

// errStopWalk ends a workspace walk early without reporting an error.
var errStopWalk = errors.New("stop walk")

// runGlob implements the glob tool using filepath.WalkDir with ** support.
// The pattern is matched against paths relative to the workspace and, when a
// path argument narrows the search, relative to that directory as well, so
// both {"pattern":"internal/**/*.go"} and {"path":"internal","pattern":"**/*.go"}
// work. Brace alternatives ("*.{ts,tsx}") are expanded.
func (r *toolRuntime) runGlob(ctx context.Context, pattern, subPath string) (string, error) {
	if strings.TrimSpace(pattern) == "" {
		return "", fmt.Errorf("glob: pattern is required")
	}
	root := r.cwd
	if strings.TrimSpace(subPath) != "" {
		abs, _, err := r.resolvePath(subPath)
		if err != nil {
			return "", fmt.Errorf("glob path: %w", err)
		}
		root = abs
	}
	patterns := expandBraces(strings.TrimPrefix(strings.TrimSpace(pattern), "./"))

	var matches []string
	total := 0
	walker := r.newWorkspaceWalker()
	walker.symlinkedFiles = true
	err := walker.walk(ctx, root, func(abs, rel string, _ fs.DirEntry) error {
		relRoot := rel
		if root != r.cwd {
			if rr, err := filepath.Rel(root, abs); err == nil {
				relRoot = filepath.ToSlash(rr)
			}
		}
		for _, p := range patterns {
			if matchGlobPattern(p, rel) || matchGlobPattern(p, relRoot) {
				total++
				if len(matches) < maxGlobResults {
					matches = append(matches, rel)
				}
				break
			}
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("glob walk: %w", err)
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		return fmt.Sprintf("no files match %q", pattern), nil
	}
	out := fmt.Sprintf("%d files:\n%s", total, strings.Join(matches, "\n"))
	if total > len(matches) {
		out += fmt.Sprintf("\n(showing the first %d of %d files; narrow the pattern or path to see the rest)", len(matches), total)
	}
	return out, nil
}

// matchGlobPattern matches a slash-separated path against a glob pattern with ** support.
func matchGlobPattern(pattern, rel string) bool {
	patParts := strings.Split(pattern, "/")
	pathParts := strings.Split(rel, "/")
	return globMatch(patParts, pathParts)
}

func globMatch(patParts, pathParts []string) bool {
	if len(patParts) == 0 && len(pathParts) == 0 {
		return true
	}
	if len(patParts) == 0 {
		return false
	}
	if patParts[0] == "**" {
		// ** can match zero or more path components
		// Try matching rest of pattern against every suffix of path
		restPat := patParts[1:]
		// Zero-component match: skip ** entirely
		if globMatch(restPat, pathParts) {
			return true
		}
		// One or more components match
		for i := 1; i <= len(pathParts); i++ {
			if globMatch(restPat, pathParts[i:]) {
				return true
			}
		}
		return false
	}
	if len(pathParts) == 0 {
		return false
	}
	matched, err := filepath.Match(patParts[0], pathParts[0])
	if err != nil || !matched {
		return false
	}
	return globMatch(patParts[1:], pathParts[1:])
}

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

// typeExtensions maps type names to file extensions.
func typeExtensions(t string) []string {
	switch strings.ToLower(t) {
	case "go":
		return []string{".go"}
	case "ts":
		return []string{".ts", ".tsx"}
	case "js":
		return []string{".js", ".jsx", ".mjs"}
	case "py":
		return []string{".py"}
	case "rs":
		return []string{".rs"}
	case "md":
		return []string{".md"}
	case "toml":
		return []string{".toml"}
	case "json":
		return []string{".json"}
	case "yaml", "yml":
		return []string{".yaml", ".yml"}
	case "sh":
		return []string{".sh", ".bash"}
	default:
		return nil
	}
}

// grepArgs are the grep tool's arguments. Besides the canonical names it
// accepts the spellings other harnesses use: include for glob, -i for
// case_insensitive, -C for context and head_limit for max_results.
type grepArgs struct {
	Pattern         string   `json:"pattern"`
	Path            string   `json:"path"`
	Glob            string   `json:"glob"`
	Include         string   `json:"include"`
	Type            string   `json:"type"`
	CaseInsensitive flexBool `json:"case_insensitive"`
	DashI           flexBool `json:"-i"`
	Context         flexInt  `json:"context"`
	DashC           flexInt  `json:"-C"`
	OutputMode      string   `json:"output_mode"`
	MaxResults      flexInt  `json:"max_results"`
	HeadLimit       flexInt  `json:"head_limit"`
}

// grepQuery is a validated grep request.
type grepQuery struct {
	pattern    string
	re         *regexp.Regexp
	ignoreCase bool
	root       string // absolute file or directory to search
	rootRel    string // root relative to the workspace ("." for the workspace)
	rootIsFile bool
	globs      []string // brace-expanded glob filters (OR)
	exts       []string // type filter
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
		q.root, q.rootRel, q.rootIsFile = abs, rel, !info.IsDir()
		if q.rootRel == "" {
			q.rootRel = "."
		}
	}
	if g := firstNonEmpty(args.Glob, args.Include); g != "" {
		q.globs = expandBraces(strings.TrimPrefix(g, "./"))
	}
	q.exts = typeExtensions(args.Type)
	q.context = max(int(args.Context), int(args.DashC), 0)
	q.mode = strings.TrimSpace(args.OutputMode)
	if q.mode == "" {
		q.mode = "content"
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
	base := filepath.Base(filepath.FromSlash(rel))
	for _, g := range q.globs {
		if strings.Contains(g, "/") {
			if matchGlobPattern(g, rel) || matchGlobPattern(g, relRoot) {
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
	results, truncated := capGrepResults(results, q.max, q.context)
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

// grepWithWalk is the pure-Go grep backend.
func (r *toolRuntime) grepWithWalk(ctx context.Context, q grepQuery) ([]grepFileResult, error) {
	var results []grepFileResult
	total := 0
	search := func(abs, rel string) error {
		if !q.wantsFile(rel) {
			return nil
		}
		fr, ok := grepFile(abs, rel, q)
		if !ok {
			return nil
		}
		results = append(results, fr)
		total += fr.count
		if total > q.max {
			return errStopWalk
		}
		return nil
	}
	if q.rootIsFile {
		err := search(q.root, q.rootRel)
		if errors.Is(err, errStopWalk) {
			err = nil
		}
		return results, err
	}
	err := r.newWorkspaceWalker().walk(ctx, q.root, func(abs, rel string, _ fs.DirEntry) error {
		return search(abs, rel)
	})
	return results, err
}

// grepFile searches one file; ok is false when it has no match or is not a
// searchable text file.
func grepFile(abs, rel string, q grepQuery) (grepFileResult, bool) {
	info, err := os.Stat(abs)
	if err != nil || info.Size() > maxSearchFileBytes {
		return grepFileResult{}, false
	}
	data, err := os.ReadFile(abs)
	if err != nil || looksBinary(data) {
		return grepFileResult{}, false
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var matchLines []int
	for i, line := range lines {
		if q.re.MatchString(line) {
			matchLines = append(matchLines, i)
		}
	}
	if len(matchLines) == 0 {
		return grepFileResult{}, false
	}
	fr := grepFileResult{path: rel, count: len(matchLines)}
	if q.mode != "content" {
		return fr, true
	}
	included := make([]bool, len(lines))
	isMatch := make([]bool, len(lines))
	for _, mi := range matchLines {
		isMatch[mi] = true
		for j := max(mi-q.context, 0); j <= min(mi+q.context, len(lines)-1); j++ {
			included[j] = true
		}
	}
	for i, line := range lines {
		if included[i] {
			fr.lines = append(fr.lines, grepLine{num: i + 1, text: line, match: isMatch[i]})
		}
	}
	return fr, true
}

// looksBinary reports whether data looks like a binary file: a NUL byte in
// the first binarySniffBytes, the heuristic git and ripgrep use.
func looksBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), binarySniffBytes)], 0) >= 0
}

// lookRipgrep finds the rg binary. It is a variable so tests can force the Go
// backend or a specific binary.
var lookRipgrep = func() (string, bool) {
	ripgrepOnce.Do(func() {
		ripgrepPath, _ = exec.LookPath("rg")
	})
	return ripgrepPath, ripgrepPath != ""
}

var (
	ripgrepOnce sync.Once
	ripgrepPath string
)

// grepWithRipgrep runs the search through ripgrep's JSON output. Any failure
// other than "no matches" (an rg too old for a flag, a pattern rg's regex
// engine rejects) is returned so the caller falls back to the Go walk.
func (r *toolRuntime) grepWithRipgrep(ctx context.Context, rg string, q grepQuery) ([]grepFileResult, error) {
	// --sort path keeps results (and so max_results truncation) in the same
	// order as the Go walk and stable across runs.
	args := []string{
		"--json", "--no-config", "--hidden", "--no-require-git", "--no-messages",
		"--sort", "path", "--max-filesize", strconv.Itoa(maxSearchFileBytes),
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
	results, total, parseErr := parseRipgrepJSON(stdout, q, keep)
	stoppedEarly := total > q.max
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
	return results, nil
}

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
// more than q.max matches have been seen. total is the number of matches read.
// keep, when set, is an extra filter on each file's workspace-relative path.
func parseRipgrepJSON(stdout io.Reader, q grepQuery, keep func(rel string) bool) (results []grepFileResult, total int, err error) {
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
			if total > q.max {
				return results, total, nil
			}
		}
	}
	flush()
	return results, total, sc.Err()
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
		fmt.Fprintf(&sb, "(results truncated at %d matches; narrow the pattern, path or glob, or raise max_results)\n", q.max)
	}
	return strings.TrimRight(sb.String(), "\n")
}
