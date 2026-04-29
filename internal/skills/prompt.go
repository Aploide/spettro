package skills

import (
	"fmt"
	"path/filepath"
	"strings"
)

// CatalogPrompt renders the skill catalog as a labeled section the agent
// runtime can append to a system prompt. It includes a brief instruction block
// telling the model how to activate skills via file-read or the dedicated
// skill-read tool, then lists each skill's name + description + location.
//
// Returns "" when the catalog has no enabled skills.
func CatalogPrompt(c Catalog) string {
	skills := c.Active()
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nAvailable Agent Skills:\n")
	b.WriteString("Skills are specialized instruction packs (Anthropic Agent Skills format) for specific tasks.\n")
	b.WriteString("When a user request matches a skill's description below, load the skill before proceeding by either:\n")
	b.WriteString("- Calling the skill-read tool (preferred) with {\"name\":\"<skill-name>\"}, or\n")
	b.WriteString("- Reading the SKILL.md at the listed location with file-read.\n")
	b.WriteString("Resolve any relative paths inside the skill (e.g. scripts/, references/) against its directory.\n\n")
	b.WriteString("<available_skills>\n")
	for _, s := range skills {
		b.WriteString("  <skill>\n")
		b.WriteString(fmt.Sprintf("    <name>%s</name>\n", escapeXML(s.Name)))
		b.WriteString(fmt.Sprintf("    <description>%s</description>\n", escapeXML(s.Description)))
		b.WriteString(fmt.Sprintf("    <location>%s</location>\n", escapeXML(filepath.ToSlash(s.Location))))
		if strings.TrimSpace(s.Compatibility) != "" {
			b.WriteString(fmt.Sprintf("    <compatibility>%s</compatibility>\n", escapeXML(s.Compatibility)))
		}
		if len(s.Resources) > 0 {
			b.WriteString("    <resources>")
			b.WriteString(escapeXML(strings.Join(s.Resources, ", ")))
			b.WriteString("</resources>\n")
		}
		b.WriteString(fmt.Sprintf("    <scope>%s</scope>\n", s.Scope))
		b.WriteString("  </skill>\n")
	}
	b.WriteString("</available_skills>\n")
	return b.String()
}

// ActivationContent renders a skill's body wrapped in identifying tags so the
// agent runtime can detect skill context during compaction. It also surfaces
// bundled resources without eagerly reading them.
func ActivationContent(s Skill, body string) string {
	body = strings.TrimSpace(body)
	var b strings.Builder
	b.WriteString(fmt.Sprintf("<skill_content name=%q>\n", s.Name))
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

func escapeXML(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return r.Replace(s)
}
