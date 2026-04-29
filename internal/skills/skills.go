// Package skills implements the Agent Skills standard for Spettro.
//
// A skill is a directory containing a SKILL.md file with YAML frontmatter
// (name + description, plus optional fields) followed by Markdown instructions.
// Skills can also bundle scripts/, references/, and assets/ subdirectories.
//
// Spettro discovers skills from multiple well-known locations to remain
// compatible with the Claude Code and OpenAI skill ecosystems while still
// supporting its own native location:
//
//   - <project>/.spettro/skills/        (Spettro native, project scope)
//   - <project>/.agents/skills/         (cross-client convention)
//   - <project>/.claude/skills/         (Claude Code compatibility)
//   - <project>/.openai/skills/         (OpenAI tools compatibility)
//   - ~/.spettro/skills/                (Spettro native, user scope)
//   - ~/.agents/skills/                 (cross-client convention)
//   - ~/.claude/skills/                 (Claude Code compatibility)
//   - ~/.openai/skills/                 (OpenAI tools compatibility)
//
// See https://agentskills.io/specification for the format.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SkillFilename is the canonical name of the skill manifest file.
const SkillFilename = "SKILL.md"

// Source labels the discovery directory family a skill came from.
type Source string

const (
	SourceSpettro Source = "spettro"
	SourceAgents  Source = "agents"
	SourceClaude  Source = "claude"
	SourceOpenAI  Source = "openai"
)

// Scope is "project" (workspace-relative) or "user" (home-relative).
type Scope string

const (
	ScopeProject Scope = "project"
	ScopeUser    Scope = "user"
)

// Skill is a discovered skill ready for disclosure to the model.
type Skill struct {
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	License       string            `json:"license,omitempty"`
	Compatibility string            `json:"compatibility,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	AllowedTools  string            `json:"allowed_tools,omitempty"`
	Disabled      bool              `json:"disabled,omitempty"`
	Location      string            `json:"location"`           // absolute path to SKILL.md
	Directory     string            `json:"directory"`          // absolute path to the skill folder
	Source        Source            `json:"source"`             // discovery family
	Scope         Scope              `json:"scope"`              // project or user
	Resources     []string          `json:"resources,omitempty"` // bundled scripts/references/assets, relative to Directory
	Issues        []string          `json:"issues,omitempty"`   // non-fatal validation warnings
}

// Catalog is the complete set of skills discovered for a session.
type Catalog struct {
	Skills    []Skill
	Shadowed  []Skill // skills hidden by name collision
	Issues    []string
}

// LookupOptions controls how Discover walks the well-known directories.
type LookupOptions struct {
	// IncludeProject limits discovery to user-level when false.
	IncludeProject bool
	// IncludeUser limits discovery to project-level when false.
	IncludeUser bool
	// ExtraDirs are additional skill root directories to scan (each is a
	// directory expected to contain skill subfolders, e.g. ~/custom-skills).
	ExtraDirs []string
}

// DefaultLookupOptions enables both project and user discovery.
func DefaultLookupOptions() LookupOptions {
	return LookupOptions{IncludeProject: true, IncludeUser: true}
}

// Root describes a single skill discovery directory.
type Root struct {
	Path   string `json:"path"`
	Source Source `json:"source"`
	Scope  Scope  `json:"scope"`
}

// SearchRoots returns the list of skill root directories Discover will scan,
// in deterministic order. The first occurrence of a given skill name wins,
// and project roots are listed before user roots so they take precedence.
func SearchRoots(cwd string, opts LookupOptions) []Root {
	var roots []Root
	if opts.IncludeProject && strings.TrimSpace(cwd) != "" {
		roots = append(roots,
			Root{Path: filepath.Join(cwd, ".spettro", "skills"), Source: SourceSpettro, Scope: ScopeProject},
			Root{Path: filepath.Join(cwd, ".agents", "skills"), Source: SourceAgents, Scope: ScopeProject},
			Root{Path: filepath.Join(cwd, ".claude", "skills"), Source: SourceClaude, Scope: ScopeProject},
			Root{Path: filepath.Join(cwd, ".openai", "skills"), Source: SourceOpenAI, Scope: ScopeProject},
		)
	}
	if opts.IncludeUser {
		if home, err := os.UserHomeDir(); err == nil {
			roots = append(roots,
				Root{Path: filepath.Join(home, ".spettro", "skills"), Source: SourceSpettro, Scope: ScopeUser},
				Root{Path: filepath.Join(home, ".agents", "skills"), Source: SourceAgents, Scope: ScopeUser},
				Root{Path: filepath.Join(home, ".claude", "skills"), Source: SourceClaude, Scope: ScopeUser},
				Root{Path: filepath.Join(home, ".openai", "skills"), Source: SourceOpenAI, Scope: ScopeUser},
			)
		}
	}
	for _, dir := range opts.ExtraDirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		roots = append(roots, Root{Path: dir, Source: SourceSpettro, Scope: ScopeUser})
	}
	return roots
}

// Discover scans all well-known skill roots and returns a deduplicated catalog.
// Project-scope skills override user-scope skills with the same name; shadowed
// skills are returned in Catalog.Shadowed for diagnostics.
func Discover(cwd string, opts LookupOptions) (Catalog, error) {
	cat := Catalog{}
	seen := map[string]int{} // name -> index in cat.Skills
	for _, root := range SearchRoots(cwd, opts) {
		entries, err := os.ReadDir(root.Path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			cat.Issues = append(cat.Issues, fmt.Sprintf("read %s: %v", root.Path, err))
			continue
		}
		for _, ent := range entries {
			if !ent.IsDir() {
				continue
			}
			dir := filepath.Join(root.Path, ent.Name())
			skill, err := Read(dir)
			if err != nil {
				cat.Issues = append(cat.Issues, fmt.Sprintf("%s: %v", dir, err))
				continue
			}
			skill.Source = root.Source
			skill.Scope = root.Scope
			if existing, ok := seen[skill.Name]; ok {
				cat.Shadowed = append(cat.Shadowed, skill)
				cat.Issues = append(cat.Issues,
					fmt.Sprintf("skill %q shadowed by %s (existing: %s)",
						skill.Name, skill.Location, cat.Skills[existing].Location))
				continue
			}
			seen[skill.Name] = len(cat.Skills)
			cat.Skills = append(cat.Skills, skill)
		}
	}
	sort.SliceStable(cat.Skills, func(i, j int) bool {
		return cat.Skills[i].Name < cat.Skills[j].Name
	})
	return cat, nil
}

// Read parses the SKILL.md file in dir and returns a populated Skill (without
// the body, which can be loaded on demand via LoadBody).
func Read(dir string) (Skill, error) {
	manifest := filepath.Join(dir, SkillFilename)
	raw, err := os.ReadFile(manifest)
	if err != nil {
		return Skill{}, err
	}
	skill, err := parse(string(raw))
	if err != nil {
		return Skill{}, err
	}
	skill.Location = manifest
	skill.Directory = dir
	skill.Resources = enumerateResources(dir)
	if base := filepath.Base(dir); skill.Name != "" && skill.Name != base {
		skill.Issues = append(skill.Issues,
			fmt.Sprintf("name %q does not match parent directory %q", skill.Name, base))
	}
	return skill, nil
}

// LoadBody reads SKILL.md from disk and returns the markdown content following
// the YAML frontmatter. Use this at activation time (tier 2 disclosure).
func LoadBody(s Skill) (string, error) {
	if strings.TrimSpace(s.Location) == "" {
		return "", fmt.Errorf("skill %q has no location", s.Name)
	}
	raw, err := os.ReadFile(s.Location)
	if err != nil {
		return "", err
	}
	_, body := splitFrontmatter(string(raw))
	return strings.TrimSpace(body), nil
}

// Find returns a skill by name from the catalog.
func (c Catalog) Find(name string) (Skill, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Skill{}, false
	}
	lower := strings.ToLower(name)
	for _, s := range c.Skills {
		if strings.EqualFold(s.Name, name) {
			return s, true
		}
		if strings.ToLower(s.Name) == lower {
			return s, true
		}
	}
	return Skill{}, false
}

// Active returns enabled (non-disabled) skills, suitable for prompting.
func (c Catalog) Active() []Skill {
	out := make([]Skill, 0, len(c.Skills))
	for _, s := range c.Skills {
		if s.Disabled {
			continue
		}
		out = append(out, s)
	}
	return out
}

func enumerateResources(dir string) []string {
	var out []string
	for _, sub := range []string{"scripts", "references", "assets"} {
		root := filepath.Join(dir, sub)
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, rerr := filepath.Rel(dir, path)
			if rerr != nil {
				return nil
			}
			out = append(out, filepath.ToSlash(rel))
			return nil
		})
	}
	const cap = 50
	if len(out) > cap {
		out = append(out[:cap], "... (truncated)")
	}
	sort.Strings(out)
	return out
}
