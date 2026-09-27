package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// fileReadDefaultLines is how many lines a file-read without an explicit
	// range returns; the footer tells the model where to continue.
	fileReadDefaultLines = 2000
	// fileReadDefaultChars is the default budget of file content one
	// file-read returns (config limits.file_read_chars overrides it). Only
	// the file's own text counts: the line-number prefixes come on top, so a
	// 2000-line file costs no more of the budget than its bytes.
	fileReadDefaultChars = 60000
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
// naming the offset to continue from. The history budget counts the file's
// content only, not the line-number prefixes. Binary files are refused.
//
// The file is streamed: only the page being returned is held in memory, so a
// ranged read of a multi-GB log costs a pass over it, not several copies, and
// ctx (the tool deadline) can stop the pass. Non-regular files (FIFOs,
// devices) are refused rather than blocking the step.
func (r *toolRuntime) runFileRead(ctx context.Context, rawArgs []byte) (string, error) {
	var args fileReadArgs
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("file-read args: %w", err)
	}
	path := firstNonEmpty(args.Path, args.FilePath)
	abs, rel, err := r.resolvePath(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("file-read: %s is a directory; use ls or glob to list it", rel)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("file-read: %s is not a regular file (%s)", rel, info.Mode().Type())
	}
	start := 1
	switch {
	case args.StartLine > 0:
		start = int(args.StartLine)
	case args.Offset > 0:
		start = int(args.Offset)
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
	// The budget is of file content: the numbering and the footer come on
	// top of it.
	budget := r.historyLimit("file-read")

	// The lock keeps a concurrent edit in the same batch from landing
	// between the read and the stamp, which would stamp stale content.
	unlock := r.lockFile(abs)
	f, err := os.Open(abs)
	if err != nil {
		unlock()
		return "", err
	}
	// Stat the open file before reading it: the identity stamped with the
	// content must never be newer than the content (file_stamps.go).
	opened, _ := f.Stat()
	pg, err := readFilePage(ctx, f, start, end, budget)
	f.Close()
	if err != nil {
		unlock()
		return "", fmt.Errorf("file-read: %w", err)
	}
	if looksBinaryText(pg.head) {
		unlock()
		return "", fmt.Errorf("file-read: %s looks like a binary file (%d bytes); file-read returns text only — use view-image for images, or a shell command (file, xxd, strings) to inspect it", rel, pg.size)
	}
	r.mu.Lock()
	r.readSet[rel] = struct{}{}
	delete(r.requiredReads, rel)
	r.mu.Unlock()
	r.recordReadStampSum(rel, pg.sum, opened)
	unlock()
	r.warmLSP(abs)

	total := pg.total
	if total == 0 {
		return "(empty file)", nil
	}
	if start > total {
		return "", fmt.Errorf("file-read: %s has %d lines; start line %d is past the end", rel, total, start)
	}
	end = min(end, total)
	body, last, clipped := renderNumberedLines(pg.lines, start, end, budget)
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

// pageLine is one line kept for a file-read page: its text, cut a little
// past fileReadMaxLineChars, and its full length in bytes.
type pageLine struct {
	text string
	size int
}

// filePage is what one streaming pass over a file yields.
type filePage struct {
	lines []pageLine // lines start..; stops once past end or the budget
	total int        // number of lines in the file
	size  int64      // bytes in the file
	sum   [32]byte   // SHA-256 of the whole content (the read stamp)
	head  []byte     // the first binarySniffBytes, for the binary check
}

// readFilePage streams rd once, keeping only lines start..end (and no more
// than about budget bytes of them) while counting every line and hashing the
// whole content. Lines follow splitFileLines: a trailing newline ends the
// last line rather than starting an empty one.
func readFilePage(ctx context.Context, rd io.Reader, start, end, budget int) (filePage, error) {
	var pg filePage
	h := sha256.New()
	br := bufio.NewReaderSize(io.TeeReader(rd, h), 64<<10)
	if head, _ := br.Peek(binarySniffBytes); len(head) > 0 {
		pg.head = append([]byte(nil), head...)
	}
	keepCap := fileReadMaxLineChars + utf8.UTFMax
	kept := 0
	var cur []byte
	curSize, lineNum := 0, 1
	collecting := func() bool { return lineNum >= start && lineNum <= end && kept <= budget }
	for reads := 1; ; reads++ {
		if reads%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return pg, err
			}
		}
		chunk, err := br.ReadSlice('\n')
		pg.size += int64(len(chunk))
		if len(chunk) > 0 {
			piece := chunk
			if piece[len(piece)-1] == '\n' {
				piece = piece[:len(piece)-1]
			}
			curSize += len(piece)
			if collecting() && len(cur) < keepCap {
				cur = append(cur, piece[:min(len(piece), keepCap-len(cur))]...)
			}
			if chunk[len(chunk)-1] == '\n' {
				if collecting() {
					pg.lines = append(pg.lines, pageLine{text: string(cur), size: curSize})
					kept += len(cur)
				}
				pg.total++
				lineNum++
				cur, curSize = cur[:0], 0
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if err != io.EOF {
				return pg, err
			}
			if curSize > 0 { // a last line without a trailing newline
				if collecting() {
					pg.lines = append(pg.lines, pageLine{text: string(cur), size: curSize})
				}
				pg.total++
			}
			break
		}
	}
	copy(pg.sum[:], h.Sum(nil))
	return pg, nil
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

// renderNumberedLines renders lines start..end numbered — page holds the
// lines from start on — stopping before their content would exceed budget
// (at least one line is always rendered; the numbering does not count). It
// returns the text, the number of the last line rendered, and whether any
// line was clipped to fileReadMaxLineChars.
func renderNumberedLines(page []pageLine, start, end, budget int) (text string, last int, clipped bool) {
	var b strings.Builder
	content := 0
	last = start - 1
	for i := start; i <= end && i-start < len(page); i++ {
		pl := page[i-start]
		line := pl.text
		if pl.size > fileReadMaxLineChars {
			line = clipUTF8(line, fileReadMaxLineChars) + fmt.Sprintf(" …(line truncated, %d chars)", pl.size)
			clipped = true
		}
		if budget > 0 && content+len(line)+1 > budget && i > start {
			break
		}
		content += len(line) + 1
		b.WriteString(formatNumberedLine(i, line))
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
