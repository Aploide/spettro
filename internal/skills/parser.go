package skills

import (
	"fmt"
	"maps"
	"regexp"
	"strings"
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

// parse extracts metadata from SKILL.md content.
func parse(content string) (Skill, error) {
	front, _ := splitFrontmatter(content)
	if strings.TrimSpace(front) == "" {
		return Skill{}, fmt.Errorf("missing YAML frontmatter delimited by ---")
	}
	skill, err := parseFrontmatter(front)
	if err != nil {
		return Skill{}, err
	}
	if strings.TrimSpace(skill.Name) == "" {
		return Skill{}, fmt.Errorf("frontmatter missing required field: name")
	}
	if strings.TrimSpace(skill.Description) == "" {
		return Skill{}, fmt.Errorf("frontmatter missing required field: description")
	}
	skill.Name = strings.TrimSpace(skill.Name)
	skill.Description = strings.TrimSpace(skill.Description)
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

// parseFrontmatter is a deliberately small YAML subset parser tailored to
// the SKILL.md frontmatter shape. It supports:
//
//   - Top-level `key: value` pairs (string scalars, optionally quoted).
//   - Multi-line block scalars with `|` and `>` indicators.
//   - One level of nesting under `metadata:` with `key: value` pairs.
//
// It is deliberately lenient with unquoted colons inside values, matching the
// fallback behavior recommended by the Agent Skills spec for cross-client
// compatibility.
func parseFrontmatter(front string) (Skill, error) {
	lines := strings.Split(front, "\n")
	var skill Skill
	skill.Metadata = map[string]string{}

	i := 0
	for i < len(lines) {
		raw := lines[i]
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			i++
			continue
		}
		// Determine indentation level.
		indent := indentOf(line)
		if indent > 0 {
			// Stray indented line at top level — skip.
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

		// Block scalar (| or >) consumes following indented lines.
		if rest == "|" || rest == ">" || strings.HasPrefix(rest, "|") || strings.HasPrefix(rest, ">") {
			value, consumed := readBlockScalar(lines[i+1:])
			i += 1 + consumed
			assignField(&skill, key, value)
			continue
		}

		// metadata: nested map.
		if key == "metadata" && rest == "" {
			meta, consumed := readNestedMap(lines[i+1:])
			maps.Copy(skill.Metadata, meta)
			i += 1 + consumed
			continue
		}

		assignField(&skill, key, unquote(rest))
		i++
	}
	return skill, nil
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

func assignField(s *Skill, key, value string) {
	switch key {
	case "name":
		s.Name = value
	case "description":
		s.Description = value
	case "license":
		s.License = value
	case "compatibility":
		s.Compatibility = value
	case "allowed-tools", "allowed_tools":
		s.AllowedTools = value
	case "disabled", "disable", "enabled":
		// Treat enabled: false / disabled: true as disabling the skill.
		v := strings.ToLower(strings.TrimSpace(value))
		flag := v == "true" || v == "yes" || v == "1"
		if key == "enabled" {
			s.Disabled = !flag
		} else {
			s.Disabled = flag
		}
	default:
		// Unknown top-level key — store under metadata for round-tripping.
		if s.Metadata == nil {
			s.Metadata = map[string]string{}
		}
		s.Metadata[key] = value
	}
}
