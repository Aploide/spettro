package agent

import (
	"fmt"
	"slices"
	"strings"
)

// Match tiers for file-edit/multi-edit old_string lookup. Exact matching is
// tried first; the fuzzy tiers only exist to absorb whitespace drift in the
// model's quoting, so they compare line-by-line under a normalizer and then
// re-apply the file's real indentation to new_string.
const (
	editTierExact = iota + 1
	editTierWhitespace
	editTierLineTrim
)

func editTierNote(tier int) string {
	switch tier {
	case editTierWhitespace:
		return "matched after whitespace normalization"
	case editTierLineTrim:
		return "matched after per-line whitespace trim"
	default:
		return ""
	}
}

// collapseWS collapses runs of spaces/tabs within a line to a single space.
// A leading run collapses to a single space (not nothing), so indented vs
// unindented lines stay distinct at this tier; only tier 3 ignores that.
func collapseWS(line string) string {
	var b strings.Builder
	inRun := false
	for _, r := range line {
		if r == ' ' || r == '\t' {
			inRun = true
			continue
		}
		if inRun {
			b.WriteByte(' ')
		}
		inRun = false
		b.WriteRune(r)
	}
	return b.String()
}

func trimLineWS(line string) string {
	return strings.Trim(line, " \t")
}

// trimCollapseWS is the tier-3 normalizer: strip leading/trailing whitespace
// and collapse internal runs, so indentation drift and internal spacing drift
// can be absorbed together (either alone already matches at tier 2).
func trimCollapseWS(line string) string {
	return collapseWS(trimLineWS(line))
}

func leadingWS(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

// fuzzyLineSpans returns the start indices of non-overlapping spans of
// contentLines whose normalized form equals the normalized oldLines.
func fuzzyLineSpans(contentLines, oldLines []string, norm func(string) string) []int {
	k := len(oldLines)
	normOld := make([]string, k)
	allEmpty := true
	for i, l := range oldLines {
		normOld[i] = norm(l)
		if normOld[i] != "" {
			allEmpty = false
		}
	}
	// A pattern that normalizes to nothing would match everywhere; refuse it.
	if allEmpty {
		return nil
	}
	var spans []int
	for i := 0; i+k <= len(contentLines); i++ {
		ok := true
		for j := range k {
			if norm(contentLines[i+j]) != normOld[j] {
				ok = false
				break
			}
		}
		if ok {
			spans = append(spans, i)
			i += k - 1 // skip past this span so matches never overlap
		}
	}
	return spans
}

// reindentNewString shifts new_string's lines from old_string's indentation
// base to the file's, so a fuzzy match doesn't flatten the file's real
// indentation. Lines that don't share old_string's base indent (or blank
// lines) are left as written.
func reindentNewString(newStr, oldIndent, fileIndent string) string {
	if oldIndent == fileIndent {
		return newStr
	}
	lines := strings.Split(newStr, "\n")
	for i, l := range lines {
		if trimLineWS(l) == "" {
			continue
		}
		if strings.HasPrefix(l, oldIndent) {
			lines[i] = fileIndent + l[len(oldIndent):]
		}
	}
	return strings.Join(lines, "\n")
}

// replaceWithFallback applies one old->new replacement to content using a
// fallback ladder: exact match, whitespace-normalized match, then per-line
// trimmed match. It returns the updated content, the number of replacements,
// and the tier that matched. exactUnique controls whether an ambiguous exact
// match (without replaceAll) is an error (multi-edit) or replaces the first
// occurrence (single file-edit's historical behavior); fuzzy tiers always
// require uniqueness.
func replaceWithFallback(content, oldStr, newStr string, replaceAll, exactUnique bool) (string, int, int, error) {
	if n := strings.Count(content, oldStr); n > 0 {
		if replaceAll {
			return strings.ReplaceAll(content, oldStr, newStr), n, editTierExact, nil
		}
		if n > 1 && exactUnique {
			return "", 0, 0, fmt.Errorf("old_string matches %d times; add surrounding context to make it unique or set replace_all", n)
		}
		return strings.Replace(content, oldStr, newStr, 1), 1, editTierExact, nil
	}

	contentLines := strings.Split(content, "\n")
	oldLines := strings.Split(oldStr, "\n")
	for _, tier := range []struct {
		id   int
		norm func(string) string
	}{
		{editTierWhitespace, collapseWS},
		{editTierLineTrim, trimCollapseWS},
	} {
		spans := fuzzyLineSpans(contentLines, oldLines, tier.norm)
		if len(spans) == 0 {
			continue
		}
		if !replaceAll && len(spans) > 1 {
			return "", 0, 0, fmt.Errorf("old_string is not an exact match and matches %d times after whitespace normalization; add surrounding context to make it unique or set replace_all", len(spans))
		}
		if !replaceAll {
			spans = spans[:1]
		}
		k := len(oldLines)
		// Apply back-to-front so earlier span indices stay valid.
		for _, s := range slices.Backward(spans) {

			adjusted := reindentNewString(newStr, leadingWS(oldLines[0]), leadingWS(contentLines[s]))
			repl := strings.Split(adjusted, "\n")
			contentLines = append(contentLines[:s], append(repl, contentLines[s+k:]...)...)
		}
		return strings.Join(contentLines, "\n"), len(spans), tier.id, nil
	}
	return "", 0, 0, fmt.Errorf("old_string not found: %q", truncate(oldStr, 80))
}
