package skills

import (
	"fmt"
	"maps"
	"regexp"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// nameRE matches the strict spec-defined name: lowercase a-z, 0-9, hyphens,
// no leading/trailing/consecutive hyphens, 1-64 chars.
var nameRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// splitFrontmatter returns (frontmatter, body). If the file does not start
// with `---`, the entire content is returned as body and frontmatter is empty.
func splitFrontmatter(content string) (string, string) {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	trimmed := strings.TrimLeft(content, " \t\n")
	if !strings.HasPrefix(trimmed, "---") {
		return "", content
	}
	rest := trimmed[3:]
	rest = strings.TrimLeft(rest, " \t")
	if !strings.HasPrefix(rest, "\n") {
		return "", content
	}
	rest = rest[1:]
	before, after, ok := strings.Cut(rest, "\n---")
	if !ok {
		return "", content
	}
	front := before
	body := strings.TrimLeft(after, " \t\n")
	return front, body
}

// parse extracts metadata from SKILL.md content. dirName is the name of the
// skill's directory, used when the frontmatter has no name.
//
// Parsing follows Claude Code's leniency so skills written for it load
// unchanged: every frontmatter field is optional, and a file without
// frontmatter is all body. A missing name becomes dirName; a missing
// description becomes the first non-empty line of the body (a leading
// Markdown heading marker stripped). The only error is a skill with neither
// a description nor a body, which would give the model nothing to go on.
// Spec violations that do not stop the skill from working (a name that is
// not lowercase-hyphenated, an over-long description) are reported in
// Skill.Issues.
//
// Every metadata value is cleaned of terminal escape sequences and control
// characters (see CleanText) before anything else sees it: skill folders
// come from cloned repositories, and the name, description and argument
// hint are drawn straight onto the terminal by the TUI's menus.
func parse(content, dirName string) (Skill, error) {
	front, body := splitFrontmatter(content)
	dirName = strings.TrimSpace(CleanText(dirName))
	skill := parseFrontmatter(CleanText(front))
	skill.Name = strings.TrimSpace(skill.Name)
	skill.Description = strings.TrimSpace(skill.Description)
	if skill.Name == "" {
		skill.Name = dirName
	}
	if skill.Name == "" {
		return Skill{}, fmt.Errorf("skill has no name and no directory name")
	}
	if strings.ContainsFunc(skill.Name, unicode.IsSpace) {
		original := skill.Name
		skill.Name = commandSafeName(original, dirName)
		skill.Issues = append(skill.Issues,
			fmt.Sprintf("name %q contains whitespace, so it could not be run as /name; using %q", original, skill.Name))
	}
	if skill.Description == "" {
		skill.Description = CleanText(firstBodyLine(body))
		if skill.Description == "" {
			return Skill{}, fmt.Errorf("skill %q has no description and an empty body", skill.Name)
		}
		skill.Issues = append(skill.Issues, "frontmatter has no description; using the first line of the body")
	}
	if !nameRE.MatchString(skill.Name) {
		skill.Issues = append(skill.Issues,
			fmt.Sprintf("name %q does not match the spec (lowercase a-z, 0-9, hyphens; no leading/trailing/consecutive hyphens)", skill.Name))
	}
	if len(skill.Name) > 64 {
		skill.Issues = append(skill.Issues, fmt.Sprintf("name exceeds 64 characters (%d)", len(skill.Name)))
	}
	if len(skill.Description) > 1024 {
		skill.Issues = append(skill.Issues, fmt.Sprintf("description exceeds 1024 characters (%d)", len(skill.Description)))
	}
	return skill, nil
}

// commandSafeName returns the name a skill whose name has whitespace is
// known by instead. A skill is run as "/<name> args" and hosts split the
// command at the first space, so such a name could never be run. The
// directory name is used when it has no whitespace itself (it is what the
// name should have matched anyway); otherwise the name's whitespace runs
// become single hyphens.
func commandSafeName(name, dirName string) string {
	if dirName != "" && !strings.ContainsFunc(dirName, unicode.IsSpace) {
		return dirName
	}
	return strings.Join(strings.Fields(name), "-")
}

// CleanText removes terminal escape sequences (CSI, OSC and the like) and
// every other control character from s, keeping newlines and tabs. An
// invalid UTF-8 byte becomes U+FFFD. Skill files are untrusted: without
// this a description could retitle the terminal window or write the
// clipboard (OSC 52) the moment the TUI lists it. parse applies it to the
// frontmatter; a host showing a skill's body as text applies it itself.
func CleanText(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			return -1
		}
		return r
	}, ansi.Strip(s))
}

// firstBodyLine returns the first non-empty line of a Markdown body with any
// heading marker ("# ") removed, or "" for an empty body.
func firstBodyLine(body string) string {
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if line != "" {
			return line
		}
	}
	return ""
}

// parseFrontmatter is a deliberately small YAML subset parser tailored to
// the SKILL.md frontmatter shape. It supports:
//
//   - Top-level `key: value` pairs (string scalars, optionally quoted),
//     including values that continue on more-indented lines or start on
//     the line after the key (see readContinuation).
//   - Multi-line block scalars with `|` and `>` indicators.
//   - Lists, either inline (`[a, b]`) or as indented `- item` lines; they
//     are joined with single spaces (see assignField).
//   - One level of nesting under `metadata:` with `key: value` pairs.
//
// Anything else is skipped rather than rejected: a skill whose frontmatter
// this parser cannot fully read still loads with the fields it could, which
// matches the Agent Skills recommendation for cross-client compatibility.
func parseFrontmatter(front string) Skill {
	lines := strings.Split(front, "\n")
	var skill Skill
	skill.Metadata = map[string]string{}

	i := 0
	for i < len(lines) {
		line := strings.TrimRight(lines[i], " \t")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || indentOf(line) > 0 {
			// Blank, comment, or a stray indented line at top level.
			i++
			continue
		}
		key, rest, ok := splitKey(trimmed)
		if !ok {
			i++
			continue
		}
		key = strings.ToLower(key)
		rest = strings.TrimSpace(rest)

		switch {
		case strings.HasPrefix(rest, "|") || strings.HasPrefix(rest, ">"):
			// Block scalar: consumes the following indented lines.
			value, consumed := readBlockScalar(lines[i+1:])
			i += 1 + consumed
			assignField(&skill, key, value)
		case key == "metadata" && rest == "":
			meta, consumed := readNestedMap(lines[i+1:])
			maps.Copy(skill.Metadata, meta)
			i += 1 + consumed
		case rest == "":
			// "key:" followed by "- item" lines is a list, and followed by
			// more-indented text it is a scalar that starts on the next
			// line; anything else leaves the value empty.
			items, consumed := readList(lines[i+1:])
			if len(items) == 0 {
				var value string
				value, consumed = readContinuation("", lines[i+1:])
				items = []string{unquote(value)}
			}
			i += 1 + consumed
			assignField(&skill, key, strings.Join(items, " "))
		case strings.HasPrefix(rest, "[") && strings.HasSuffix(rest, "]"):
			assignField(&skill, key, strings.Join(splitInlineList(rest), " "))
			i++
		default:
			// A plain or quoted scalar may go on over more-indented lines.
			value, consumed := readContinuation(rest, lines[i+1:])
			assignField(&skill, key, unquote(value))
			i += 1 + consumed
		}
	}
	return skill
}

func indentOf(line string) int {
	n := 0
	for n < len(line) {
		c := line[n]
		if c == ' ' {
			n++
			continue
		}
		if c == '\t' {
			n += 4
			continue
		}
		break
	}
	return n
}

func splitKey(line string) (string, string, bool) {
	idx := strings.Index(line, ":")
	if idx <= 0 {
		return "", "", false
	}
	key := strings.TrimSpace(line[:idx])
	rest := line[idx+1:]
	return key, rest, true
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		first, last := s[0], s[len(s)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return s[1 : len(s)-1]
		}
	}
	// Strip trailing comments only when preceded by a space (best-effort).
	if idx := strings.Index(s, " #"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	return s
}

// readBlockScalar reads following lines whose indentation is greater than 0
// and joins them with newlines (for `|`) or spaces (for `>`).
func readBlockScalar(lines []string) (string, int) {
	var captured []string
	consumed := 0
	for _, raw := range lines {
		if strings.TrimSpace(raw) == "" {
			captured = append(captured, "")
			consumed++
			continue
		}
		if indentOf(raw) == 0 {
			break
		}
		stripped := strings.TrimLeft(raw, " \t")
		captured = append(captured, stripped)
		consumed++
	}
	return strings.TrimSpace(strings.Join(captured, "\n")), consumed
}

// readContinuation reads the continuation lines of a scalar that starts
// with first (possibly empty, when the value starts on the next line): the
// following lines indented deeper than the key, up to the first blank,
// comment or top-level line. YAML folds such lines into one, so they are
// joined with single spaces. It returns the joined value and the number of
// lines consumed.
//
// Without this, "description: Extract text from PDFs." followed by an
// indented "Use when the user mentions PDFs." kept only the first line.
func readContinuation(first string, lines []string) (string, int) {
	parts := []string{}
	if first = strings.TrimSpace(first); first != "" {
		parts = append(parts, first)
	}
	consumed := 0
	for _, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || indentOf(raw) == 0 {
			break
		}
		parts = append(parts, trimmed)
		consumed++
	}
	return strings.Join(parts, " "), consumed
}

// readList reads the "- item" lines of a block list. It stops at the first
// line that is neither blank nor a list item, so a key with an empty value
// followed by another key consumes nothing.
func readList(lines []string) ([]string, int) {
	var items []string
	consumed := 0
	for _, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			consumed++
			continue
		}
		item, ok := strings.CutPrefix(trimmed, "-")
		if !ok {
			break
		}
		if v := unquote(item); v != "" {
			items = append(items, v)
		}
		consumed++
	}
	return items, consumed
}

// splitInlineList splits a flow-style list such as `[Read, "Bash(git:*)"]`.
func splitInlineList(s string) []string {
	s = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if v := unquote(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// readNestedMap reads a mapping with one level of indentation.
func readNestedMap(lines []string) (map[string]string, int) {
	out := map[string]string{}
	consumed := 0
	for _, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			consumed++
			continue
		}
		if indentOf(raw) == 0 {
			break
		}
		key, rest, ok := splitKey(trimmed)
		if !ok {
			consumed++
			continue
		}
		out[strings.ToLower(strings.TrimSpace(key))] = unquote(rest)
		consumed++
	}
	return out, consumed
}

// parseBool reads a YAML-ish boolean; anything unrecognized is false.
func parseBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "yes", "on", "1":
		return true
	}
	return false
}

// assignField stores one frontmatter field. Keys are matched in both the
// hyphenated spelling the spec uses and the underscored one some clients
// write. Unknown keys are kept in Metadata so /skill info can show them.
func assignField(s *Skill, key, value string) {
	switch strings.ReplaceAll(key, "_", "-") {
	case "name":
		s.Name = value
	case "description":
		s.Description = value
	case "when-to-use":
		s.WhenToUse = value
	case "license":
		s.License = value
	case "compatibility":
		s.Compatibility = value
	case "allowed-tools":
		s.AllowedTools = value
	case "argument-hint":
		s.ArgumentHint = value
	case "arguments":
		s.Arguments = strings.Fields(value)
	case "disable-model-invocation":
		s.ModelInvocationDisabled = parseBool(value)
	case "user-invocable":
		// Only an explicit false hides the skill; the default is invocable.
		s.UserInvocationDisabled = strings.TrimSpace(value) != "" && !parseBool(value)
	case "disabled", "disable":
		s.Disabled = parseBool(value)
	case "enabled":
		s.Disabled = !parseBool(value)
	default:
		if s.Metadata == nil {
			s.Metadata = map[string]string{}
		}
		s.Metadata[key] = value
	}
}
