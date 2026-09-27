package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"spettro/internal/skills"
)

// skillArgs are the arguments of the skill tool. name and args are the
// advertised ones; the rest keep calls written against older spellings
// working:
//
//   - skill: Claude Code's Skill tool names the skill "skill", and skill-read
//     accepted it too.
//   - location: skill-read accepted a SKILL.md path from the old catalog.
//   - query: skill-list's filter, used when no skill is named.
type skillArgs struct {
	Name     string `json:"name"`
	Args     string `json:"args"`
	Query    string `json:"query"`
	Skill    string `json:"skill"`
	Location string `json:"location"`
}

// runSkill handles the skill tool (and its hidden aliases skill-read,
// skill-list, activate-skill and skill-activate). With a skill named it
// returns the activated skill (skills.Activate): the body with args
// substituted, wrapped in <skill_content> tags, plus the skill directory
// and its bundled files. With no name it lists the skills the model may
// load, as JSON.
func (r *toolRuntime) runSkill(rawArgs []byte) (string, error) {
	var args skillArgs
	if len(bytes.TrimSpace(rawArgs)) > 0 {
		if err := decodeJSONStrict(rawArgs, &args); err != nil {
			return "", fmt.Errorf("skill args: %w", err)
		}
	}
	name, err := r.skillNameFromArgs(args)
	if err != nil {
		return "", err
	}
	if name == "" {
		return r.listSkills(args.Query)
	}
	skill, ok := r.skillsCatalog.Find(name)
	switch {
	case !ok:
		return "", r.skillNotFound(name)
	case skill.Disabled:
		return "", fmt.Errorf("skill: %q is disabled by the user", skill.Name)
	case skill.ModelInvocationDisabled:
		// disable-model-invocation: the skill has side effects the user
		// wants to trigger themselves (a deploy, a release).
		return "", fmt.Errorf("skill: %q can only be run by the user (/%s); tell them it exists instead of running it", skill.Name, skill.Name)
	}
	return skills.Activate(skill, args.Args)
}

// skillNameFromArgs resolves the skill a call names: name, else skill, else
// the skill whose SKILL.md is at location. Only catalog locations are
// accepted, so the tool cannot be pointed at an arbitrary file.
func (r *toolRuntime) skillNameFromArgs(args skillArgs) (string, error) {
	if name := strings.TrimSpace(args.Name); name != "" {
		return name, nil
	}
	if name := strings.TrimSpace(args.Skill); name != "" {
		return name, nil
	}
	if loc := strings.TrimSpace(args.Location); loc != "" {
		for _, s := range r.skillsCatalog.Skills {
			if s.Location == loc {
				return s.Name, nil
			}
		}
		return "", fmt.Errorf("skill: location %q is not a known skill location (use {\"name\":\"<skill>\"})", loc)
	}
	return "", nil
}

// skillNotFound builds the error for an unknown skill name, listing what the
// model may load instead so it can correct itself in one step.
func (r *toolRuntime) skillNotFound(name string) error {
	var available []string
	for _, s := range r.skillsCatalog.ForModel() {
		available = append(available, s.Name)
	}
	if len(available) == 0 {
		return fmt.Errorf("skill: %q not found (no skills are installed)", name)
	}
	return fmt.Errorf("skill: %q not found (available: %s)", name, strings.Join(available, ", "))
}

// skillListRow is one entry of the skill tool's listing.
type skillListRow struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	ArgumentHint string `json:"argument_hint,omitempty"`
	Source       string `json:"source"`
	Scope        string `json:"scope"`
	Location     string `json:"location"`
}

// listSkills returns the model-invocable skills whose name or description
// contains query (all of them for an empty query) as a JSON array, "[]" when
// none match.
func (r *toolRuntime) listSkills(query string) (string, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	rows := []skillListRow{}
	for _, s := range r.skillsCatalog.ForModel() {
		if q != "" && !strings.Contains(strings.ToLower(s.Name+" "+s.ListingDescription()), q) {
			continue
		}
		rows = append(rows, skillListRow{
			Name:         s.Name,
			Description:  s.ListingDescription(),
			ArgumentHint: s.ArgumentHint,
			Source:       string(s.Source),
			Scope:        string(s.Scope),
			Location:     s.Location,
		})
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return "", fmt.Errorf("skill: marshal list: %w", err)
	}
	return string(raw), nil
}
