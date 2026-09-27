package agent

import (
	"spettro/internal/config"
	"spettro/internal/skills"
)

// This file is the one place that turns the user's settings into a skill
// catalog. Every consumer goes through it, so the agent's system prompt, the
// skill tool, the TUI slash menu, /skills, ACP's advertised commands and
// headless invocations always agree on which skills exist:
//
//   - discovery is memoised in skills.Shared, so the list (and therefore the
//     system prompt) stays identical from run to run until a skill changes
//     on disk (the cache notices that by itself; see skills.Cache);
//   - skills_compat_disabled in the user config switches the Claude Code and
//     Codex directories off;
//   - disabled_skills in the user config marks skills Disabled.

// SkillCatalogFor returns the skill catalog for the workspace cwd under the
// given user config. Hosts that already hold a loaded config (the TUI, ACP)
// call this to avoid re-reading it.
func SkillCatalogFor(cwd string, cfg config.UserConfig) skills.Catalog {
	return skills.Shared.Get(cwd, SkillLookupOptions(cfg)).WithDisabledNames(cfg.DisabledSkills)
}

// SkillCatalog is SkillCatalogFor with the user config read from disk. A
// config that cannot be read counts as the defaults (compat discovery on,
// nothing disabled), so a broken config never hides every skill.
func SkillCatalog(cwd string) skills.Catalog {
	cfg, _ := config.Load()
	return SkillCatalogFor(cwd, cfg)
}

// SkillLookupOptions maps the user config onto discovery options.
func SkillLookupOptions(cfg config.UserConfig) skills.LookupOptions {
	opts := skills.DefaultLookupOptions()
	opts.IncludeCompat = !cfg.SkillsCompatDisabled
	return opts
}

// ReloadSkills forgets every cached catalog, so the next SkillCatalog call
// rescans the skill directories. The cache already notices changes on disk
// by itself; /skill install, uninstall, enable and reload call this anyway
// so their result shows even when a change falls within the file system's
// timestamp resolution.
func ReloadSkills() {
	skills.Shared.Invalidate()
}
