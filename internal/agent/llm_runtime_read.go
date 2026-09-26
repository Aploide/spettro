package agent

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// fileReadDefaultLines is how many lines a file-read without an explicit
	// range returns; the footer tells the model where to continue.
	fileReadDefaultLines = 2000
	// fileReadMaxLineChars clips a single line so one minified line cannot
	// consume the read budget.
	fileReadMaxLineChars = 2000
	// fileReadFooterReserve is the budget share held back for the footer.
	fileReadFooterReserve = 300
)

// fileReadArgs are the file-read arguments. offset/limit (1-based first line,
// line count — the convention other harnesses train models on) are accepted
// alongside start_line/end_line, and file_path alongside path.
type fileReadArgs struct {
	Path      string  `json:"path"`
	FilePath  string  `json:"file_path"`
	StartLine flexInt `json:"start_line"`
	EndLine   flexInt `json:"end_line"`
	Offset    flexInt `json:"offset"`
	Limit     flexInt `json:"limit"`
}

// runFileRead implements file-read. Every read — whole file or range — comes
// back in one format: each line prefixed by its right-aligned 1-based number
// and a tab (cat -n style, "     7\tcode"), the numbers file-edit's
// start_line/end_line refer to. Output is capped at fileReadDefaultLines lines
// (unless the call asked for a range) and at the history budget, with a footer
// naming the offset to continue from. Binary files are refused.
func (r *toolRuntime) runFileRead(rawArgs []byte) (string, error) {
	var args fileReadArgs
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("file-read args: %w", err)
	}
	path := firstNonEmpty(args.Path, args.FilePath)
	abs, rel, err := r.resolvePath(path)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(abs); err == nil && info.IsDir() {
		return "", fmt.Errorf("file-read: %s is a directory; use ls or glob to list it", rel)
	}
	// The lock keeps a concurrent edit in the same batch from landing
	// between the read and the stamp, which would stamp stale content.
	unlock := r.lockFile(abs)
	data, err := os.ReadFile(abs)
	if err != nil {
		unlock()
		return "", err
	}
	if looksBinaryText(data) {
		unlock()
		return "", fmt.Errorf("file-read: %s looks like a binary file (%d bytes); file-read returns text only — use view-image for images, or a shell command (file, xxd, strings) to inspect it", rel, len(data))
	}
	r.mu.Lock()
	r.readSet[rel] = struct{}{}
	delete(r.requiredReads, rel)
	r.mu.Unlock()
	r.recordReadStamp(rel, data)
	unlock()

	lines := splitFileLines(string(data))
	total := len(lines)
	if total == 0 {
		return "(empty file)", nil
	}
	start := 1
	switch {
	case args.StartLine > 0:
		start = int(args.StartLine)
	case args.Offset > 0:
		start = int(args.Offset)
	}
	if start > total {
		return "", fmt.Errorf("file-read: %s has %d lines; start line %d is past the end", rel, total, start)
	}
	end := start + fileReadDefaultLines - 1
	switch {
	case args.EndLine > 0:
		end = int(args.EndLine)
	case args.Limit > 0:
		end = start + int(args.Limit) - 1
	}
	if end < start {
		return "", fmt.Errorf("file-read: end line %d is before start line %d", end, start)
	}
	end = min(end, total)

	budget := r.historyLimit("file-read")
	budget = max(budget-fileReadFooterReserve, budget/2)
	body, last, clipped := renderNumberedLines(lines, start, end, budget)
	var notes []string
	if clipped {
		notes = append(notes, fmt.Sprintf("lines longer than %d chars were clipped", fileReadMaxLineChars))
	}
	if last < total {
		notes = append(notes, fmt.Sprintf("showing lines %d-%d of %d; continue with file-read {\"path\":%q,\"offset\":%d,\"limit\":%d}",
			start, last, total, rel, last+1, fileReadDefaultLines))
	}
	if len(notes) == 0 {
		return body, nil
	}
	return body + "[" + strings.Join(notes, "; ") + "]", nil
}

// splitFileLines splits file content into lines; a trailing newline ends the
// last line rather than starting an empty one.
func splitFileLines(content string) []string {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// formatNumberedLine renders one line in file-read's numbered format.
func formatNumberedLine(num int, text string) string {
	return fmt.Sprintf("%6d\t%s\n", num, text)
}

// renderNumberedLines renders lines[start-1:end] numbered, stopping before the
// output would exceed budget (at least one line is always rendered). It
// returns the text, the number of the last line rendered, and whether any
// line was clipped to fileReadMaxLineChars.
func renderNumberedLines(lines []string, start, end, budget int) (text string, last int, clipped bool) {
	var b strings.Builder
	last = start - 1
	for i := start; i <= end; i++ {
		line := lines[i-1]
		if len(line) > fileReadMaxLineChars {
			line = clipUTF8(line, fileReadMaxLineChars) + fmt.Sprintf(" …(line truncated, %d chars)", len(lines[i-1]))
			clipped = true
		}
		row := formatNumberedLine(i, line)
		if budget > 0 && b.Len()+len(row) > budget && i > start {
			break
		}
		b.WriteString(row)
		last = i
	}
	return b.String(), last, clipped
}

// clipUTF8 shortens s to at most n bytes without splitting a rune.
func clipUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// looksBinaryText reports whether data should be refused as binary: a NUL
// byte in the first binarySniffBytes (the git/ripgrep heuristic), or — for
// content that is not valid UTF-8 — mostly control characters.
func looksBinaryText(data []byte) bool {
	if looksBinary(data) {
		return true
	}
	sniff := data[:min(len(data), binarySniffBytes)]
	if len(sniff) == 0 || utf8.Valid(sniff) {
		return false
	}
	control := 0
	for _, c := range sniff {
		if c < 0x20 && c != '\t' && c != '\n' && c != '\r' && c != '\f' && c != 0x1b {
			control++
		}
	}
	return control*10 > len(sniff)*3
}

// fileReadTruncate caps already-numbered file-read output at budget on a line
// boundary, with a footer pointing at file-read (the file is on disk; paging a
// spool copy through job-output would be the wrong tool). It is spoolResult's
// file-read branch.
func fileReadTruncate(out string, budget int) string {
	if budget <= 0 || len(out) <= budget {
		return out
	}
	head := out[:max(budget-fileReadFooterReserve, 0)]
	if i := strings.LastIndexByte(head, '\n'); i >= 0 {
		head = head[:i+1]
	}
	next := ""
	if lastLine := lastNumberedLine(head); lastLine > 0 {
		next = fmt.Sprintf(" with offset %d", lastLine+1)
	}
	return head + fmt.Sprintf("[truncated; continue with file-read%s]", next)
}

// lastNumberedLine extracts the line number of the last "     N\t..." row in
// numbered file-read output, or 0.
func lastNumberedLine(out string) int {
	trimmed := strings.TrimRight(out, "\n")
	row := trimmed[strings.LastIndexByte(trimmed, '\n')+1:]
	numText, _, ok := strings.Cut(row, "\t")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(numText))
	if err != nil {
		return 0
	}
	return n
}
