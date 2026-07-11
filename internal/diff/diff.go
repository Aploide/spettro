// Package diff produces unified diffs of file contents and renders them for
// the terminal: colored, line-numbered, side-by-side when the terminal is
// wide enough, and collapsible so huge diffs stay cheap to draw.
package diff

import (
	"fmt"
	"strings"
)

// Guards that keep diff computation cheap on pathological inputs. A diff of
// two multi-megabyte blobs is never useful in an approval prompt, and the LCS
// table is O(n*m) on the trimmed middle.
const (
	maxInputBytes = 1 << 20 // 1 MiB per side
	maxLCSCells   = 4_000_000
	contextLines  = 3
)

type lineKind int

const (
	kindContext lineKind = iota
	kindDel
	kindAdd
)

type diffLine struct {
	kind  lineKind
	oldNo int // 1-based old line number; 0 for additions
	newNo int // 1-based new line number; 0 for deletions
	text  string
}

// Unified returns a plain-text unified diff (git style, 3 context lines) of
// old vs new, labelled with path. It returns "" when the contents are equal.
// Oversized inputs yield a one-line summary instead of a full diff.
func Unified(path, old, new string) string {
	if old == new {
		return ""
	}
	if len(old) > maxInputBytes || len(new) > maxInputBytes {
		return fmt.Sprintf("--- a/%s\n+++ b/%s\n(diff too large to display: %d -> %d bytes)\n", path, path, len(old), len(new))
	}
	lines := computeLines(splitLines(old), splitLines(new))

	var sb strings.Builder
	oldLabel := "a/" + path
	if old == "" {
		oldLabel = "/dev/null"
	}
	fmt.Fprintf(&sb, "--- %s\n+++ b/%s\n", oldLabel, path)
	for _, h := range buildHunks(lines) {
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", h.oldStart, h.oldCount, h.newStart, h.newCount)
		for _, l := range h.lines {
			switch l.kind {
			case kindAdd:
				sb.WriteString("+")
			case kindDel:
				sb.WriteString("-")
			default:
				sb.WriteString(" ")
			}
			sb.WriteString(l.text)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	// A trailing newline produces a phantom empty last element; drop it so
	// "a\n" diffs as one line, not two.
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// computeLines produces the full aligned line sequence (context + edits) for
// old vs new using an LCS over the middle region after trimming the common
// prefix and suffix. When the middle is too large for the LCS table it falls
// back to a whole-block replacement, which is always correct, just coarser.
func computeLines(oldLines, newLines []string) []diffLine {
	// Trim common prefix.
	pre := 0
	for pre < len(oldLines) && pre < len(newLines) && oldLines[pre] == newLines[pre] {
		pre++
	}
	// Trim common suffix (of the remainder).
	suf := 0
	for suf < len(oldLines)-pre && suf < len(newLines)-pre &&
		oldLines[len(oldLines)-1-suf] == newLines[len(newLines)-1-suf] {
		suf++
	}
	midOld := oldLines[pre : len(oldLines)-suf]
	midNew := newLines[pre : len(newLines)-suf]

	var out []diffLine
	for i := 0; i < pre; i++ {
		out = append(out, diffLine{kind: kindContext, oldNo: i + 1, newNo: i + 1, text: oldLines[i]})
	}

	oldNo, newNo := pre+1, pre+1
	emitDel := func(text string) {
		out = append(out, diffLine{kind: kindDel, oldNo: oldNo, text: text})
		oldNo++
	}
	emitAdd := func(text string) {
		out = append(out, diffLine{kind: kindAdd, newNo: newNo, text: text})
		newNo++
	}
	emitCtx := func(text string) {
		out = append(out, diffLine{kind: kindContext, oldNo: oldNo, newNo: newNo, text: text})
		oldNo++
		newNo++
	}

	if len(midOld)*len(midNew) > maxLCSCells {
		for _, l := range midOld {
			emitDel(l)
		}
		for _, l := range midNew {
			emitAdd(l)
		}
	} else {
		for _, op := range lcsOps(midOld, midNew) {
			switch op.kind {
			case kindDel:
				emitDel(op.text)
			case kindAdd:
				emitAdd(op.text)
			default:
				emitCtx(op.text)
			}
		}
	}

	for i := 0; i < suf; i++ {
		out = append(out, diffLine{
			kind:  kindContext,
			oldNo: len(oldLines) - suf + i + 1,
			newNo: len(newLines) - suf + i + 1,
			text:  oldLines[len(oldLines)-suf+i],
		})
	}
	return out
}

type editOp struct {
	kind lineKind
	text string
}

// lcsOps returns the edit script (context/del/add) between a and b via a
// classic LCS dynamic program.
func lcsOps(a, b []string) []editOp {
	n, m := len(a), len(b)
	table := make([]int, (n+1)*(m+1))
	idx := func(i, j int) int { return i*(m+1) + j }
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				table[idx(i, j)] = table[idx(i+1, j+1)] + 1
			} else if table[idx(i+1, j)] >= table[idx(i, j+1)] {
				table[idx(i, j)] = table[idx(i+1, j)]
			} else {
				table[idx(i, j)] = table[idx(i, j+1)]
			}
		}
	}
	var ops []editOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, editOp{kindContext, a[i]})
			i++
			j++
		case table[idx(i+1, j)] >= table[idx(i, j+1)]:
			ops = append(ops, editOp{kindDel, a[i]})
			i++
		default:
			ops = append(ops, editOp{kindAdd, b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, editOp{kindDel, a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, editOp{kindAdd, b[j]})
	}
	return ops
}

type hunk struct {
	oldStart, oldCount int
	newStart, newCount int
	lines              []diffLine
}

// buildHunks groups the aligned line sequence into hunks with contextLines of
// surrounding context, merging hunks whose context would overlap.
func buildHunks(lines []diffLine) []hunk {
	// Mark lines to keep: every edit plus context around it.
	keep := make([]bool, len(lines))
	for i, l := range lines {
		if l.kind == kindContext {
			continue
		}
		lo := i - contextLines
		if lo < 0 {
			lo = 0
		}
		hi := i + contextLines
		if hi > len(lines)-1 {
			hi = len(lines) - 1
		}
		for k := lo; k <= hi; k++ {
			keep[k] = true
		}
	}

	var hunks []hunk
	i := 0
	for i < len(lines) {
		if !keep[i] {
			i++
			continue
		}
		j := i
		for j < len(lines) && keep[j] {
			j++
		}
		h := hunk{lines: lines[i:j]}
		for _, l := range h.lines {
			if l.kind != kindAdd {
				if h.oldStart == 0 {
					h.oldStart = l.oldNo
				}
				h.oldCount++
			}
			if l.kind != kindDel {
				if h.newStart == 0 {
					h.newStart = l.newNo
				}
				h.newCount++
			}
		}
		// A pure-insertion (or pure-deletion) hunk still needs a start anchor.
		if h.oldStart == 0 {
			h.oldStart = anchorBefore(h.lines, false)
		}
		if h.newStart == 0 {
			h.newStart = anchorBefore(h.lines, true)
		}
		hunks = append(hunks, h)
		i = j
	}
	return hunks
}

// anchorBefore returns the line number just before a hunk that has no lines on
// one side (e.g. an insertion into an empty region), matching git's convention.
func anchorBefore(lines []diffLine, newSide bool) int {
	for _, l := range lines {
		if newSide && l.newNo > 0 {
			return l.newNo - 1
		}
		if !newSide && l.oldNo > 0 {
			return l.oldNo - 1
		}
	}
	return 0
}
