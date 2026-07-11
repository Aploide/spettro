// Package commands implements user-defined custom slash commands: reusable
// prompts saved as TOML or markdown files that expand into a prompt sent to
// the agent. Files are discovered from the global ~/.spettro/commands/
// directory and the project <root>/.spettro/commands/ directory; on a name
// conflict the project definition wins.
package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// Command is one user-defined slash command.
type Command struct {
	// Name is the slash-command name without the leading "/". Subdirectories
	// become namespace separators: <root>/git/pr.toml → "git:pr".
	Name        string
	Description string
	Prompt      string
	// Scope is "project" or "user" depending on which commands dir defined it.
	Scope string
	// Path is the absolute path of the source file.
	Path string
}

// Root is one directory scanned for command files.
type Root struct {
	Path  string
	Scope string
}

// Roots returns the command discovery roots in priority order (user first,
// project last so project entries override on conflict).
func Roots(cwd string) []Root {
	var out []Root
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		out = append(out, Root{Path: filepath.Join(home, ".spettro", "commands"), Scope: "user"})
	}
	if cwd != "" {
		out = append(out, Root{Path: filepath.Join(cwd, ".spettro", "commands"), Scope: "project"})
	}
	return out
}

// Discover scans the global and project command directories and returns the
// effective command set (project overrides user on name conflict), sorted by
// name, plus human-readable issues for files that could not be parsed.
func Discover(cwd string) ([]Command, []string) {
	return DiscoverRoots(Roots(cwd))
}

// DiscoverRoots is Discover with explicit roots; later roots override earlier
// ones on name conflict.
func DiscoverRoots(roots []Root) ([]Command, []string) {
	byName := map[string]Command{}
	var issues []string
	for _, root := range roots {
		_ = filepath.WalkDir(root.Path, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil //nolint:nilerr // a missing root or unreadable entry is not an error
			}
			ext := strings.ToLower(filepath.Ext(path))
			if ext != ".toml" && ext != ".md" {
				return nil
			}
			rel, relErr := filepath.Rel(root.Path, path)
			if relErr != nil {
				return nil
			}
			name := strings.TrimSuffix(rel, filepath.Ext(rel))
			name = strings.ReplaceAll(name, string(filepath.Separator), ":")
			cmd, parseErr := parseFile(path, ext)
			if parseErr != nil {
				issues = append(issues, fmt.Sprintf("%s: %v", path, parseErr))
				return nil
			}
			cmd.Name = name
			cmd.Scope = root.Scope
			cmd.Path = path
			byName[strings.ToLower(name)] = cmd
			return nil
		})
	}
	out := make([]Command, 0, len(byName))
	for _, c := range byName {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, issues
}

type tomlCommand struct {
	Prompt      string `toml:"prompt"`
	Description string `toml:"description"`
}

func parseFile(path, ext string) (Command, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Command{}, err
	}
	switch ext {
	case ".toml":
		var tc tomlCommand
		if err := toml.Unmarshal(raw, &tc); err != nil {
			return Command{}, fmt.Errorf("invalid TOML: %w", err)
		}
		if strings.TrimSpace(tc.Prompt) == "" {
			return Command{}, fmt.Errorf("missing required field: prompt")
		}
		return Command{Prompt: tc.Prompt, Description: strings.TrimSpace(tc.Description)}, nil
	default: // .md
		front, body := splitFrontmatter(string(raw))
		if strings.TrimSpace(body) == "" {
			return Command{}, fmt.Errorf("empty prompt body")
		}
		return Command{Prompt: strings.TrimSpace(body), Description: frontmatterValue(front, "description")}, nil
	}
}

// splitFrontmatter returns (frontmatter, body). If the content does not start
// with a `---` line, frontmatter is empty and the whole content is the body.
func splitFrontmatter(content string) (string, string) {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return "", content
	}
	rest := normalized[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", content
	}
	body := rest[end+len("\n---"):]
	if i := strings.Index(body, "\n"); i >= 0 {
		body = body[i+1:]
	} else {
		body = ""
	}
	return rest[:end], body
}

// frontmatterValue extracts a simple `key: value` line from frontmatter.
func frontmatterValue(front, key string) string {
	for _, line := range strings.Split(front, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(strings.ToLower(k)) == key {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// shellInterp matches !`command` interpolations inside a prompt template.
var shellInterp = regexp.MustCompile("!`([^`]+)`")

// shellTimeout bounds each interpolated shell command.
const shellTimeout = 15 * time.Second

// Expand substitutes {{args}} with args and, when allowShell is true, runs
// each !`command` interpolation via the shell (in dir) and splices in its
// output. When the prompt contains shell interpolation and allowShell is
// false, Expand returns an error instead of silently dropping it — callers
// gate allowShell on the existing permission policy.
func Expand(c Command, args, dir string, allowShell bool) (string, error) {
	out := strings.ReplaceAll(c.Prompt, "{{args}}", args)
	matches := shellInterp.FindAllStringSubmatch(out, -1)
	if len(matches) == 0 {
		return out, nil
	}
	if !allowShell {
		return "", fmt.Errorf("command %q uses shell interpolation (!`...`), which requires yolo permission (/permission yolo)", c.Name)
	}
	for _, mt := range matches {
		ctx, cancel := context.WithTimeout(context.Background(), shellTimeout)
		cmd := exec.CommandContext(ctx, "sh", "-c", mt[1])
		cmd.Dir = dir
		raw, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			return "", fmt.Errorf("interpolated command %q failed: %v\n%s", mt[1], err, strings.TrimSpace(string(raw)))
		}
		out = strings.Replace(out, mt[0], strings.TrimSpace(string(raw)), 1)
	}
	return out, nil
}
