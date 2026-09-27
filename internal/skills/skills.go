// Package skills implements the Agent Skills standard for Spettro.
//
// A skill is a directory containing a SKILL.md file with YAML frontmatter
// (name + description, plus optional fields) followed by Markdown
// instructions. Skills can also bundle scripts/, references/, and assets/
// subdirectories that the model reads on demand.
//
// # Discovery
//
// Spettro owns two skill directories, which are the only ones it ever writes
// to (install, uninstall):
//
//   - <project>/.spettro/skills/
//   - ~/.spettro/skills/
//
// It also reads, but never writes, the directories other agents use, so a
// skill installed for Claude Code or Codex works in Spettro without copying.
// These "compat" roots are on by default and can be switched off with the
// skills_compat_disabled user setting (see LookupOptions.IncludeCompat):
//
//   - .agents/skills/  (cross-client convention; current Codex location)
//   - .claude/skills/  (Claude Code)
//   - .codex/skills/   (legacy Codex location; ~/.codex honours $CODEX_HOME)
//   - .openai/skills/  (kept for configurations written before .codex)
//
// "<project>" is the working directory and each of its parents up to the
// repository root (the first directory holding .git), nearest first, so a
// skill checked in at the root of a monorepo is found from any package.
//
// # Precedence
//
// SearchRoots lists the roots in priority order and the first skill with a
// given name wins; later ones are kept in Catalog.Shadowed for /skills to
// show. The order is: project roots before user roots; within the project,
// the nearest directory first; within one directory, Spettro before the
// compat families in the order listed above. Names compare
// case-insensitively.
//
// See https://agentskills.io/specification for the format and docs/skills.md
// for the user-facing guide.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"spettro/internal/homedir"
)

// SkillFilename is the canonical name of the skill manifest file.
const SkillFilename = "SKILL.md"

// DisabledMarker is the legacy per-skill opt-out file. Older versions of
// /skill disable wrote it into the skill's directory; it is still honoured
// (the skill is marked Disabled) and /skill enable removes it, but new
// disables are recorded in the user config instead so the compat roots stay
// read-only.
const DisabledMarker = ".spettro-disabled"

// Source labels the directory family a skill was discovered in.
type Source string

const (
	// SourceSpettro is .spettro/skills, the only family Spettro writes to.
	SourceSpettro Source = "spettro"
	// SourceAgents is .agents/skills, the cross-client convention Codex uses.
	SourceAgents Source = "agents"
	// SourceClaude is .claude/skills (Claude Code).
	SourceClaude Source = "claude"
	// SourceCodex is .codex/skills, or $CODEX_HOME/skills for the user root.
	SourceCodex Source = "codex"
	// SourceOpenAI is .openai/skills, kept for older configurations.
	SourceOpenAI Source = "openai"
)

// compatFamilies are the read-only directory families, in precedence order,
// as the name of the directory that holds "skills" under a project or home.
var compatFamilies = []struct {
	dir    string
	source Source
}{
	{".agents", SourceAgents},
	{".claude", SourceClaude},
	{".codex", SourceCodex},
	{".openai", SourceOpenAI},
}

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
	// AllowedTools is recorded for display only. Spettro's permission model
	// is the agent manifest, so a skill cannot pre-approve tools.
	AllowedTools string `json:"allowed_tools,omitempty"`
	// WhenToUse (Claude Code's when_to_use) extends Description in the
	// model's skill list; see ListingDescription.
	WhenToUse string `json:"when_to_use,omitempty"`
	// ArgumentHint is shown next to the skill in the slash-command menu,
	// e.g. "[issue-number]".
	ArgumentHint string `json:"argument_hint,omitempty"`
	// Arguments names the positional arguments of a user invocation, so the
	// body can say $component instead of $0 (see RenderBody).
	Arguments []string `json:"arguments,omitempty"`
	// ModelInvocationDisabled (disable-model-invocation: true) keeps the skill
	// out of the model's list and the skill tool: only the user can run it.
	ModelInvocationDisabled bool `json:"model_invocation_disabled,omitempty"`
	// UserInvocationDisabled (user-invocable: false) keeps the skill out of
	// the slash-command menu and $-mentions: only the model can load it.
	UserInvocationDisabled bool `json:"user_invocation_disabled,omitempty"`
	// Disabled hides the skill from both the model and the user. It comes
	// from the frontmatter (disabled: true / enabled: false), the legacy
	// DisabledMarker file, or the user config (Catalog.WithDisabledNames).
	Disabled  bool     `json:"disabled,omitempty"`
	Location  string   `json:"location"`            // absolute path to SKILL.md
	Directory string   `json:"directory"`           // absolute path to the skill folder
	Source    Source   `json:"source"`              // discovery family
	Scope     Scope    `json:"scope"`               // project or user
	Resources []string `json:"resources,omitempty"` // bundled scripts/references/assets, relative to Directory
	Issues    []string `json:"issues,omitempty"`    // non-fatal validation warnings
}

// ModelInvocable reports whether the model may see and load the skill.
func (s Skill) ModelInvocable() bool { return !s.Disabled && !s.ModelInvocationDisabled }

// UserInvocable reports whether the user may run the skill as /name or
// mention it as $name.
func (s Skill) UserInvocable() bool { return !s.Disabled && !s.UserInvocationDisabled }

// ListingDescription is the one-line text the model's skill list and the
// slash-command menu show: Description plus WhenToUse, whitespace collapsed.
func (s Skill) ListingDescription() string {
	text := s.Description
	if strings.TrimSpace(s.WhenToUse) != "" {
		text += " " + s.WhenToUse
	}
	return strings.Join(strings.Fields(text), " ")
}

// Catalog is the complete set of skills discovered for a session.
type Catalog struct {
	Skills   []Skill
	Shadowed []Skill // skills hidden by a higher-priority skill of the same name
	Issues   []string
}

// LookupOptions controls which directories Discover walks.
type LookupOptions struct {
	// IncludeProject scans the project roots (the working directory and its
	// parents up to the repository root).
	IncludeProject bool
	// IncludeUser scans the roots under the home directory.
	IncludeUser bool
	// IncludeCompat adds the read-only roots of other agents (.agents,
	// .claude, .codex, .openai) next to Spettro's own .spettro/skills.
	IncludeCompat bool
	// ExtraDirs are additional skill root directories to scan (each is a
	// directory expected to contain skill subfolders, e.g. ~/custom-skills).
	// They come last in precedence.
	ExtraDirs []string
}

// DefaultLookupOptions enables project, user and compat discovery.
func DefaultLookupOptions() LookupOptions {
	return LookupOptions{IncludeProject: true, IncludeUser: true, IncludeCompat: true}
}

// Root describes a single skill discovery directory.
type Root struct {
	Path   string `json:"path"`
	Source Source `json:"source"`
	Scope  Scope  `json:"scope"`
}

// ReadOnly reports whether Spettro treats the root as another agent's
// directory that it reads but never writes.
func (r Root) ReadOnly() bool { return r.Source != SourceSpettro }

// SearchRoots returns the skill root directories Discover scans, in
// precedence order (see the package documentation). A path is listed once
// even when it is reachable two ways (a working directory inside the home
// directory, say).
func SearchRoots(cwd string, opts LookupOptions) []Root {
	var roots []Root
	seen := map[string]bool{}
	add := func(r Root) {
		clean := filepath.Clean(r.Path)
		if seen[clean] {
			return
		}
		seen[clean] = true
		r.Path = clean
		roots = append(roots, r)
	}
	addFamilies := func(base string, scope Scope) {
		add(Root{Path: filepath.Join(base, ".spettro", "skills"), Source: SourceSpettro, Scope: scope})
		if !opts.IncludeCompat {
			return
		}
		for _, f := range compatFamilies {
			path := filepath.Join(base, f.dir, "skills")
			if f.source == SourceCodex && scope == ScopeUser {
				path = codexUserSkillsDir(base)
			}
			add(Root{Path: path, Source: f.source, Scope: scope})
		}
	}
	home, homeErr := homedir.Dir()
	if opts.IncludeProject && strings.TrimSpace(cwd) != "" {
		for _, dir := range projectDirs(cwd, home) {
			addFamilies(dir, ScopeProject)
		}
	}
	if opts.IncludeUser && homeErr == nil {
		addFamilies(home, ScopeUser)
	}
	for _, dir := range opts.ExtraDirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		add(Root{Path: dir, Source: SourceSpettro, Scope: ScopeUser})
	}
	return roots
}

// codexUserSkillsDir is Codex's user skill directory: $CODEX_HOME/skills when
// CODEX_HOME is set, ~/.codex/skills otherwise.
func codexUserSkillsDir(home string) string {
	if dir := strings.TrimSpace(os.Getenv("CODEX_HOME")); dir != "" {
		return filepath.Join(dir, "skills")
	}
	return filepath.Join(home, ".codex", "skills")
}

// projectDirs returns the directories whose skill roots count as project
// scope: cwd, then each parent up to and including the repository root (the
// first directory that holds .git). When cwd is not inside a repository only
// cwd itself is used, so a stray skills folder high up the tree is never
// picked up. The walk also stops below the home directory, whose skill
// roots are the user scope.
func projectDirs(cwd, home string) []string {
	start, err := filepath.Abs(cwd)
	if err != nil {
		return []string{cwd}
	}
	home = filepath.Clean(home)
	var dirs []string
	for dir := start; ; {
		if dir == home && len(dirs) > 0 {
			break
		}
		dirs = append(dirs, dir)
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dirs
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// No repository root on the way up: the walk proves nothing about
	// which parents belong to the project, so only cwd counts.
	return []string{start}
}

// Discover scans every root SearchRoots returns and builds the catalog. The
// first skill of a name wins (see the package documentation); the others go
// to Catalog.Shadowed with an entry in Catalog.Issues. Skills are sorted by
// name. The error is always nil today and is kept for API stability;
// per-directory problems are reported in Catalog.Issues instead.
func Discover(cwd string, opts LookupOptions) (Catalog, error) {
	cat := Catalog{}
	seen := map[string]int{} // lower-cased name -> index in cat.Skills
	for _, root := range SearchRoots(cwd, opts) {
		entries, err := os.ReadDir(root.Path)
		if err != nil {
			if !os.IsNotExist(err) {
				cat.Issues = append(cat.Issues, fmt.Sprintf("read %s: %v", root.Path, err))
			}
			continue
		}
		for _, ent := range entries {
			dir := filepath.Join(root.Path, ent.Name())
			if !isDir(ent, dir) {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, SkillFilename)); err != nil {
				// A folder without SKILL.md (a shared scripts dir, say) is
				// not a skill; say nothing about it.
				continue
			}
			skill, err := Read(dir)
			if err != nil {
				cat.Issues = append(cat.Issues, fmt.Sprintf("%s: %v", dir, err))
				continue
			}
			skill.Source = root.Source
			skill.Scope = root.Scope
			key := strings.ToLower(skill.Name)
			if existing, ok := seen[key]; ok {
				cat.Shadowed = append(cat.Shadowed, skill)
				cat.Issues = append(cat.Issues,
					fmt.Sprintf("skill %q at %s is shadowed by %s",
						skill.Name, skill.Location, cat.Skills[existing].Location))
				continue
			}
			seen[key] = len(cat.Skills)
			cat.Skills = append(cat.Skills, skill)
		}
	}
	sort.SliceStable(cat.Skills, func(i, j int) bool {
		return cat.Skills[i].Name < cat.Skills[j].Name
	})
	return cat, nil
}

// isDir reports whether a directory entry is a directory, following a
// symlink (skills are often linked in from a shared checkout).
func isDir(ent os.DirEntry, path string) bool {
	if ent.IsDir() {
		return true
	}
	if ent.Type()&os.ModeSymlink == 0 {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// Read parses the SKILL.md file in dir and returns a populated Skill (without
// the body, which is loaded on demand via LoadBody). Parsing is tolerant: a
// missing name defaults to the directory name and a missing description to
// the first line of the body (see parse); only an unreadable file or a skill
// with no description and no body is an error.
func Read(dir string) (Skill, error) {
	manifest := filepath.Join(dir, SkillFilename)
	raw, err := os.ReadFile(manifest)
	if err != nil {
		return Skill{}, err
	}
	skill, err := parse(string(raw), filepath.Base(dir))
	if err != nil {
		return Skill{}, err
	}
	skill.Location = manifest
	skill.Directory = dir
	skill.Resources = enumerateResources(dir)
	if base := filepath.Base(dir); !strings.EqualFold(skill.Name, base) {
		skill.Issues = append(skill.Issues,
			fmt.Sprintf("name %q does not match parent directory %q", skill.Name, base))
	}
	if _, err := os.Stat(filepath.Join(dir, DisabledMarker)); err == nil {
		skill.Disabled = true
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

// Find returns a skill by name (case-insensitive), disabled ones included.
func (c Catalog) Find(name string) (Skill, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Skill{}, false
	}
	for _, s := range c.Skills {
		if strings.EqualFold(s.Name, name) {
			return s, true
		}
	}
	return Skill{}, false
}

// Active returns the skills that are not disabled.
func (c Catalog) Active() []Skill {
	return c.filter(func(s Skill) bool { return !s.Disabled })
}

// ForModel returns the skills the model may see and load.
func (c Catalog) ForModel() []Skill {
	return c.filter(Skill.ModelInvocable)
}

// ForUser returns the skills the user may run as /name or mention as $name.
func (c Catalog) ForUser() []Skill {
	return c.filter(Skill.UserInvocable)
}

func (c Catalog) filter(keep func(Skill) bool) []Skill {
	out := make([]Skill, 0, len(c.Skills))
	for _, s := range c.Skills {
		if keep(s) {
			out = append(out, s)
		}
	}
	return out
}

// WithDisabledNames returns a copy of the catalog with every skill whose name
// is in names (case-insensitive) marked Disabled. It is how the user
// config's disabled_skills list is applied; the receiver is not modified.
func (c Catalog) WithDisabledNames(names []string) Catalog {
	out := c.clone()
	if len(names) == 0 {
		return out
	}
	off := map[string]bool{}
	for _, n := range names {
		off[strings.ToLower(strings.TrimSpace(n))] = true
	}
	for i := range out.Skills {
		if off[strings.ToLower(out.Skills[i].Name)] {
			out.Skills[i].Disabled = true
		}
	}
	return out
}

// clone copies the catalog's slices so a caller can modify the skills
// without affecting a cached catalog. Skill values are copied; their
// Metadata maps and Resources slices are shared and must be treated as
// read-only.
func (c Catalog) clone() Catalog {
	return Catalog{
		Skills:   append([]Skill(nil), c.Skills...),
		Shadowed: append([]Skill(nil), c.Shadowed...),
		Issues:   append([]string(nil), c.Issues...),
	}
}

// maxResources caps the bundled files listed for one skill, so a skill that
// ships a large asset tree does not flood the activation message.
const maxResources = 50

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
	sort.Strings(out)
	if len(out) > maxResources {
		out = append(out[:maxResources], "... (truncated)")
	}
	return out
}
