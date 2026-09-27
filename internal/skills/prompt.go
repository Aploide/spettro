package skills

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ToolName is the canonical name of the built-in tool the model calls to
// load a skill. The agent runtime keeps the older names (skill-read,
// skill-list, activate-skill, skill-activate) as hidden aliases, except
// while the operator's own tool holds the name skill: the old built-ins then
// keep their own names, and the skill list names skill-read instead (see
// CatalogPrompt's loadTool).
const ToolName = "skill"

// Budgets for the skill list in the system prompt. The list is part of the
// cached prompt prefix on every request, so it is kept small: one line per
// skill, each description cut to maxListingDescription characters, and the
// whole list to maxListingChars (Codex uses the same 8,000-character
// ceiling). Skills past the budget are still loadable; the note at the end
// tells the model how to list them.
const (
	maxListingDescription = 250
	maxListingChars       = 8000
)

// CatalogPrompt renders the model's skill list as a section to append to a
// system prompt: a short instruction to call loadTool, then one
// "- name: description" line per model-invocable skill, sorted by name.
//
// loadTool is the tool the agent the prompt is for loads skills with:
// ToolName normally, or an unfolded skill-read (see ToolName). An agent that
// holds no such tool gets no section at all ("" loadTool): the list would
// only tell it to call a tool it cannot use.
//
// The output depends only on the catalog and loadTool, never on the step or
// time, so the system prompt stays byte-identical across steps and keeps
// its provider prompt-cache prefix. Returns "" when no skill is
// model-invocable.
func CatalogPrompt(c Catalog, loadTool string) string {
	list := c.ForModel()
	if len(list) == 0 || loadTool == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nAgent Skills:\n")
	b.WriteString("Skills are instruction packs for specific tasks. When a request matches a skill below, call the `")
	b.WriteString(loadTool)
	b.WriteString("` tool with {\"name\": \"<skill>\"} before starting the work; it returns the instructions and the skill's directory. ")
	b.WriteString("Read the skill's bundled files (scripts/, references/, assets/) with file-read only when the instructions point to them. ")
	b.WriteString("Do not guess a skill's instructions from its description.\n")
	b.WriteString("<available_skills>\n")
	used := 0
	for i, s := range list {
		line := fmt.Sprintf("- %s: %s\n", escapeXML(s.Name), escapeXML(truncateRunes(s.ListingDescription(), maxListingDescription)))
		if used+len(line) > maxListingChars {
			fmt.Fprintf(&b, "- (%d more skills not listed; call `%s` with no name to list them all)\n", len(list)-i, loadTool)
			break
		}
		b.WriteString(line)
		used += len(line)
	}
	b.WriteString("</available_skills>\n")
	return b.String()
}

// ActivationContent wraps an already rendered skill body in identifying
// tags, so compaction and the transcript can tell skill instructions apart
// from ordinary text, and appends where the skill lives and which bundled
// files it has. The files are listed, not read: the model opens them with
// file-read when the instructions call for it.
func ActivationContent(s Skill, body string) string {
	body = strings.TrimSpace(body)
	var b strings.Builder
	fmt.Fprintf(&b, "<skill_content name=%q>\n", s.Name)
	b.WriteString(body)
	b.WriteString("\n\nSkill directory: ")
	b.WriteString(filepath.ToSlash(s.Directory))
	b.WriteString("\nRelative paths in this skill are relative to the skill directory.\n")
	if len(s.Resources) > 0 {
		b.WriteString("<skill_resources>\n")
		for _, r := range s.Resources {
			b.WriteString("  <file>")
			b.WriteString(escapeXML(r))
			b.WriteString("</file>\n")
		}
		b.WriteString("</skill_resources>\n")
	}
	b.WriteString("</skill_content>\n")
	return b.String()
}

// Activate loads a skill's body, substitutes args into it (see RenderBody)
// and wraps it with ActivationContent. It is what the skill tool returns and
// the core of every user invocation.
func Activate(s Skill, args string) (string, error) {
	body, err := LoadBody(s)
	if err != nil {
		return "", fmt.Errorf("load skill %q: %w", s.Name, err)
	}
	return ActivationContent(s, RenderBody(s, body, args)), nil
}

// UserInvocationPrompt builds the prompt sent to the agent when the user
// runs "/<name> [args]": a one-line statement that the user chose the skill,
// then the activated skill. Hosts show the user's own "/name args" in the
// transcript and send this text to the model.
func UserInvocationPrompt(s Skill, args string) (string, error) {
	content, err := Activate(s, args)
	if err != nil {
		return "", err
	}
	invocation := "/" + s.Name
	if args = strings.TrimSpace(args); args != "" {
		invocation += " " + args
	}
	return fmt.Sprintf("The user ran the %q skill (%s). Follow its instructions below for this request.\n\n%s", s.Name, invocation, content), nil
}

// placeholderRE matches the substitutions RenderBody understands, in the
// order it tries them: a skill-directory variable, $ARGUMENTS[N], $ARGUMENTS,
// $N, and $name (a named argument; left alone unless it is one).
var placeholderRE = regexp.MustCompile(`\$\{(CLAUDE_SKILL_DIR|SKILL_DIR|SPETTRO_SKILL_DIR)\}|\$ARGUMENTS\[(\d+)\]|\$ARGUMENTS|\$(\d+)|\$([A-Za-z_][A-Za-z0-9_-]*)`)

// RenderBody substitutes a user's arguments into a skill body, following
// Claude Code's rules so skills written for it behave the same:
//
//   - $ARGUMENTS is the whole argument string (empty when there is none).
//   - $ARGUMENTS[N] and $N are the N-th argument, counting from 0; one past
//     the last argument is left as written.
//   - $<name> is the argument the frontmatter "arguments" list names at that
//     position, or empty when the user gave fewer arguments; a $word that is
//     not a declared name is left as written.
//   - ${CLAUDE_SKILL_DIR}, ${SKILL_DIR} and ${SPETTRO_SKILL_DIR} are the
//     skill's directory.
//
// When args is non-empty but no argument placeholder took any of it, the
// body gets a final "ARGUMENTS: <args>" line so the model still sees what
// the user typed.
func RenderBody(s Skill, body, args string) string {
	args = strings.TrimSpace(args)
	positional := splitArgs(args)
	named := map[string]int{}
	for i, name := range s.Arguments {
		named[name] = i
	}
	argAt := func(i int) (string, bool) {
		if i < 0 || i >= len(positional) {
			return "", false
		}
		return positional[i], true
	}
	consumed := false
	out := placeholderRE.ReplaceAllStringFunc(body, func(match string) string {
		groups := placeholderRE.FindStringSubmatch(match)
		switch {
		case groups[1] != "":
			return filepath.ToSlash(s.Directory)
		case groups[2] != "" || groups[3] != "":
			digits := groups[2] + groups[3]
			n, err := strconv.Atoi(digits)
			if err != nil {
				return match
			}
			if v, ok := argAt(n); ok {
				consumed = true
				return v
			}
			return match
		case match == "$ARGUMENTS":
			consumed = true
			return args
		case groups[4] != "":
			i, ok := named[groups[4]]
			if !ok {
				return match
			}
			consumed = true
			v, _ := argAt(i)
			return v
		}
		return match
	})
	if args != "" && !consumed {
		out = strings.TrimRight(out, "\n") + "\n\nARGUMENTS: " + args
	}
	return out
}

// splitArgs splits an argument string on whitespace, keeping text inside
// single or double quotes together (the quotes are removed). It is not a
// shell parser: there are no escapes and an unclosed quote runs to the end.
func splitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	inWord := false
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
			inWord = true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

// maxMentions caps how many $-mentioned skills one prompt activates, so a
// pasted text full of dollar words cannot pull in every skill.
const maxMentions = 5

// mentionRE finds $name tokens at the start of the text or after
// whitespace. The trailing punctuation class lets "$deploy." or "$deploy,"
// still name the "deploy" skill.
var mentionRE = regexp.MustCompile(`(?:^|\s)\$([A-Za-z0-9][A-Za-z0-9._:-]*[A-Za-z0-9]|[A-Za-z0-9])`)

// MentionedSkills returns the user-invocable skills a prompt names with
// Codex-style $name mentions, in order of first mention, without
// duplicates and at most maxMentions. A $word that is not a skill name (a
// shell variable, a price) is ignored.
func MentionedSkills(prompt string, c Catalog) []Skill {
	var out []Skill
	seen := map[string]bool{}
	for _, m := range mentionRE.FindAllStringSubmatch(prompt, -1) {
		s, ok := c.Find(m[1])
		if !ok || !s.UserInvocable() || seen[strings.ToLower(s.Name)] {
			continue
		}
		seen[strings.ToLower(s.Name)] = true
		out = append(out, s)
		if len(out) == maxMentions {
			break
		}
	}
	return out
}

// ExpandMentions appends the activated instructions of every skill the
// prompt mentions as $name (see MentionedSkills) to the prompt, and returns
// the names it activated. The prompt text itself is kept as typed. A skill
// whose body cannot be read is skipped, since the mention was only a hint.
// With no mentions the prompt is returned unchanged.
func ExpandMentions(prompt string, c Catalog) (string, []string) {
	mentioned := MentionedSkills(prompt, c)
	if len(mentioned) == 0 {
		return prompt, nil
	}
	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\n\nThe user referenced these skills with $name; follow their instructions for this request:\n\n")
	var names []string
	for _, s := range mentioned {
		content, err := Activate(s, "")
		if err != nil {
			continue
		}
		b.WriteString(content)
		b.WriteString("\n")
		names = append(names, s.Name)
	}
	if len(names) == 0 {
		return prompt, nil
	}
	return strings.TrimRight(b.String(), "\n"), names
}

// truncateRunes cuts s to at most max runes, marking a cut with "...".
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 3 {
		return string(r[:max])
	}
	return string(r[:max-3]) + "..."
}

func escapeXML(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return r.Replace(s)
}
