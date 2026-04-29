package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"spettro/internal/skills"
)

// runSkillRead handles the `skill-read` (alias `activate-skill`) builtin tool.
// It accepts {"name":"<skill-name>"} and returns the wrapped SKILL.md body.
func (r *toolRuntime) runSkillRead(rawArgs []byte) (string, error) {
	var args struct {
		Name     string `json:"name"`
		Skill    string `json:"skill"`
		Location string `json:"location"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("skill-read args: %w", err)
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		name = strings.TrimSpace(args.Skill)
	}
	if name == "" && args.Location != "" {
		// Location-based activation: parse SKILL.md directly. Rely on the
		// catalog as a safety check so models can't activate arbitrary files.
		for _, s := range r.skillsCatalog.Skills {
			if s.Location == args.Location {
				name = s.Name
				break
			}
		}
		if name == "" {
			return "", fmt.Errorf("skill-read: location %q is not a known skill location (use {\"name\":\"<skill>\"})", args.Location)
		}
	}
	if name == "" {
		return "", fmt.Errorf("skill-read: name is required")
	}
	skill, ok := r.skillsCatalog.Find(name)
	if !ok {
		available := make([]string, 0, len(r.skillsCatalog.Skills))
		for _, s := range r.skillsCatalog.Skills {
			available = append(available, s.Name)
		}
		sort.Strings(available)
		if len(available) == 0 {
			return "", fmt.Errorf("skill-read: skill %q not found (no skills installed; use /skill install)", name)
		}
		return "", fmt.Errorf("skill-read: skill %q not found (available: %s)", name, strings.Join(available, ", "))
	}
	body, err := skills.LoadBody(skill)
	if err != nil {
		return "", fmt.Errorf("skill-read: load body for %q: %w", name, err)
	}
	return skills.ActivationContent(skill, body), nil
}

// runSkillList returns a JSON-encoded list of installed skills.
func (r *toolRuntime) runSkillList(rawArgs []byte) (string, error) {
	var args struct {
		Query string `json:"query"`
	}
	if len(rawArgs) > 0 {
		_ = decodeJSONStrict(rawArgs, &args)
	}
	q := strings.ToLower(strings.TrimSpace(args.Query))
	type row struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Source      string `json:"source"`
		Scope       string `json:"scope"`
		Location    string `json:"location"`
	}
	var rows []row
	for _, s := range r.skillsCatalog.Skills {
		if s.Disabled {
			continue
		}
		hay := strings.ToLower(s.Name + " " + s.Description)
		if q != "" && !strings.Contains(hay, q) {
			continue
		}
		rows = append(rows, row{
			Name:        s.Name,
			Description: s.Description,
			Source:      string(s.Source),
			Scope:       string(s.Scope),
			Location:    s.Location,
		})
	}
	if len(rows) == 0 {
		return "[]", nil
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return "", fmt.Errorf("skill-list: marshal: %w", err)
	}
	return string(raw), nil
}
