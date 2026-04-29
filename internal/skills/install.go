package skills

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// InstallOptions controls how a source is installed into the skill library.
type InstallOptions struct {
	// Source can be a local directory containing a SKILL.md (or a parent of
	// several skill directories), an https://... git URL, or an
	// "owner/repo[/subdir]" GitHub shorthand.
	Source string
	// Scope chooses between project (cwd/.spettro/skills) and user
	// (~/.spettro/skills). Default is user.
	Scope Scope
	// CWD is the project working directory used to resolve project-scope
	// installs. Required when Scope == ScopeProject.
	CWD string
	// Name overrides the destination directory name. Defaults to the
	// SKILL.md `name` field, falling back to the source directory's basename.
	Name string
	// Force, when true, replaces an existing skill with the same name.
	Force bool
	// SubPath restricts a git/local install to the given relative directory
	// inside the source.
	SubPath string
}

// InstallResult describes a successful install.
type InstallResult struct {
	Skill       Skill
	Destination string
	Replaced    bool
	Source      string
}

// InstallTimeout caps how long a remote git clone may run.
const InstallTimeout = 90 * time.Second

// Install fetches a skill from a source and copies it into the appropriate
// skills directory. Returns the installed skill metadata.
func Install(ctx context.Context, opts InstallOptions) (InstallResult, error) {
	src := strings.TrimSpace(opts.Source)
	if src == "" {
		return InstallResult{}, fmt.Errorf("install: source is required")
	}
	scope := opts.Scope
	if scope == "" {
		scope = ScopeUser
	}
	root, err := installRoot(scope, opts.CWD)
	if err != nil {
		return InstallResult{}, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return InstallResult{}, fmt.Errorf("install: create skills dir: %w", err)
	}

	stagingParent, err := os.MkdirTemp("", "spettro-skill-install-*")
	if err != nil {
		return InstallResult{}, fmt.Errorf("install: temp dir: %w", err)
	}
	defer os.RemoveAll(stagingParent)

	stagingDir := filepath.Join(stagingParent, "src")
	if err := stageSource(ctx, src, stagingDir); err != nil {
		return InstallResult{}, err
	}

	skillDir := stagingDir
	if opts.SubPath != "" {
		skillDir = filepath.Join(stagingDir, filepath.FromSlash(opts.SubPath))
	}
	if !pathExists(filepath.Join(skillDir, SkillFilename)) {
		// Allow `Install` against a parent that contains a single skill
		// subdirectory. Pick the subdirectory if exactly one contains
		// SKILL.md and SubPath was not specified.
		if opts.SubPath == "" {
			candidates := findSkillSubdirs(stagingDir)
			if len(candidates) == 1 {
				skillDir = candidates[0]
			} else if len(candidates) > 1 {
				rels := make([]string, 0, len(candidates))
				for _, c := range candidates {
					rel, _ := filepath.Rel(stagingDir, c)
					rels = append(rels, rel)
				}
				return InstallResult{}, fmt.Errorf("install: multiple SKILL.md candidates found, pass --path=<one of: %s>", strings.Join(rels, ", "))
			}
		}
	}
	if !pathExists(filepath.Join(skillDir, SkillFilename)) {
		return InstallResult{}, fmt.Errorf("install: no SKILL.md found in %s", src)
	}

	parsed, err := Read(skillDir)
	if err != nil {
		return InstallResult{}, fmt.Errorf("install: parse skill: %w", err)
	}

	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = parsed.Name
	}
	if name == "" {
		name = filepath.Base(skillDir)
	}
	if !nameRE.MatchString(name) {
		return InstallResult{}, fmt.Errorf("install: invalid destination name %q (use lowercase letters, numbers, hyphens)", name)
	}

	dest := filepath.Join(root, name)
	replaced := false
	if pathExists(dest) {
		if !opts.Force {
			return InstallResult{}, fmt.Errorf("install: %q already exists at %s (use --force to overwrite)", name, dest)
		}
		if err := os.RemoveAll(dest); err != nil {
			return InstallResult{}, fmt.Errorf("install: remove existing %s: %w", dest, err)
		}
		replaced = true
	}

	if err := copyDir(skillDir, dest); err != nil {
		return InstallResult{}, fmt.Errorf("install: copy %s -> %s: %w", skillDir, dest, err)
	}

	installed, err := Read(dest)
	if err != nil {
		return InstallResult{}, fmt.Errorf("install: re-read installed skill: %w", err)
	}
	installed.Source = SourceSpettro
	installed.Scope = scope
	return InstallResult{
		Skill:       installed,
		Destination: dest,
		Replaced:    replaced,
		Source:      src,
	}, nil
}

// Uninstall removes a skill from the user or project skill library. It only
// removes skills installed under the spettro-native directories.
func Uninstall(name string, scope Scope, cwd string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("uninstall: skill name is required")
	}
	root, err := installRoot(scope, cwd)
	if err != nil {
		return err
	}
	dest := filepath.Join(root, name)
	if !pathExists(dest) {
		return fmt.Errorf("uninstall: skill %q not found at %s", name, dest)
	}
	return os.RemoveAll(dest)
}

func installRoot(scope Scope, cwd string) (string, error) {
	switch scope {
	case ScopeProject:
		if strings.TrimSpace(cwd) == "" {
			return "", fmt.Errorf("install: project scope requires a working directory")
		}
		return filepath.Join(cwd, ".spettro", "skills"), nil
	case ScopeUser, "":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("install: resolve home dir: %w", err)
		}
		return filepath.Join(home, ".spettro", "skills"), nil
	default:
		return "", fmt.Errorf("install: unsupported scope %q", scope)
	}
}

func stageSource(ctx context.Context, source, dest string) error {
	if isLocalDir(source) {
		abs, err := filepath.Abs(source)
		if err != nil {
			return fmt.Errorf("install: resolve %s: %w", source, err)
		}
		if !pathExists(abs) {
			return fmt.Errorf("install: %s does not exist", abs)
		}
		return copyDir(abs, dest)
	}
	if isGitURL(source) {
		return gitClone(ctx, source, dest)
	}
	if shorthand := normalizeGitShorthand(source); shorthand != "" {
		return gitClone(ctx, shorthand, dest)
	}
	return fmt.Errorf("install: unsupported source %q (use a local path, https git URL, or owner/repo)", source)
}

func isLocalDir(source string) bool {
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") || strings.HasPrefix(source, "git@") {
		return false
	}
	if strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../") || strings.HasPrefix(source, "/") {
		return true
	}
	if strings.HasPrefix(source, "~") {
		return true
	}
	if u, err := url.Parse(source); err == nil && u.Scheme != "" {
		return false
	}
	if pathExists(source) {
		return true
	}
	return false
}

func isGitURL(source string) bool {
	if strings.HasPrefix(source, "git@") {
		return true
	}
	if strings.HasSuffix(source, ".git") {
		return true
	}
	if strings.HasPrefix(source, "https://") || strings.HasPrefix(source, "http://") {
		return strings.HasSuffix(source, ".git") || isLikelyGitHost(source)
	}
	return false
}

func isLikelyGitHost(source string) bool {
	u, err := url.Parse(source)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Host)
	switch host {
	case "github.com", "gitlab.com", "bitbucket.org", "codeberg.org":
		return true
	}
	return false
}

// normalizeGitShorthand converts an "owner/repo" or "owner/repo/path" shorthand
// into an https GitHub URL. Returns "" if the input doesn't match.
func normalizeGitShorthand(source string) string {
	if strings.Contains(source, " ") || strings.Contains(source, ":") {
		return ""
	}
	parts := strings.Split(source, "/")
	if len(parts) < 2 {
		return ""
	}
	for _, p := range parts {
		if strings.TrimSpace(p) == "" {
			return ""
		}
	}
	owner, repo := parts[0], parts[1]
	if owner == "" || repo == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
}

func gitClone(ctx context.Context, repo, dest string) error {
	cctx, cancel := context.WithTimeout(ctx, InstallTimeout)
	defer cancel()
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("install: git is required to fetch %s (%w)", repo, err)
	}
	cmd := exec.CommandContext(cctx, "git", "clone", "--depth", "1", "--", repo, dest)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("install: git clone failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func findSkillSubdirs(root string) []string {
	var out []string
	entries, err := os.ReadDir(root)
	if err != nil {
		return out
	}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		candidate := filepath.Join(root, ent.Name())
		if pathExists(filepath.Join(candidate, SkillFilename)) {
			out = append(out, candidate)
		}
	}
	return out
}

func copyDir(src, dst string) error {
	src = filepath.Clean(src)
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			if filepath.Base(path) == ".git" {
				return filepath.SkipDir
			}
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, lerr := os.Readlink(path)
			if lerr != nil {
				return lerr
			}
			return os.Symlink(link, target)
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func copyFile(src, dst string, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
