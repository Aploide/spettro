package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"spettro/internal/config"
	"spettro/internal/storage"
)

// runClean implements `spettro clean`: report what Spettro stores on disk and
// reclaim the safe-default set. Dry-run by default — without --yes it only
// prints the plan.
func runClean(args []string) {
	fs := flag.NewFlagSet("clean", flag.ExitOnError)
	yes := fs.Bool("yes", false, "actually delete (default is dry-run: print the plan only)")
	days := fs.Int("days", 0, "session age threshold in days (default 30, or clean_session_age_days from config)")
	keep := fs.Int("keep", 0, "most recent sessions to keep per project regardless of age (default 5, or clean_keep_sessions)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: spettro clean [--yes] [--days N] [--keep N]")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	cwd, err := os.Getwd()
	if err != nil {
		fatal("cwd error: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fatal("home dir error: %v", err)
	}
	globalDir := filepath.Join(home, ".spettro")
	projectDir := filepath.Join(cwd, ".spettro")

	opts := storage.CleanOptions{SessionAgeDays: *days, KeepSessions: *keep}
	if cfg, err := config.LoadFull(); err == nil {
		if opts.SessionAgeDays <= 0 {
			opts.SessionAgeDays = cfg.CleanSessionAgeDays
		}
		if opts.KeepSessions <= 0 {
			opts.KeepSessions = cfg.CleanKeepSessions
		}
	}

	report := storage.Inventory(globalDir, projectDir, opts)
	// The project-cache row depends on where clean is run from; print the
	// scanned roots so a report taken from another directory is explainable.
	fmt.Printf("global:  %s\nproject: %s\n\n", globalDir, projectDir)
	fmt.Println(storage.RenderReport(report))

	plan := report.PreselectedItems()
	if len(plan) == 0 {
		fmt.Println("\nnothing to delete with the safe defaults — widen with --days/--keep, or pick items interactively with /storage clean in the TUI.")
		return
	}

	fmt.Printf("\nplan (%s):\n", storage.FormatBytes(report.TotalReclaimable()))
	for _, it := range plan {
		fmt.Printf("  [%s] %s (%s)\n", it.ClassName, it.Label, storage.FormatBytes(it.Size))
	}

	if !*yes {
		fmt.Println("\ndry run — nothing deleted. Re-run with --yes to execute.")
		return
	}
	freed, err := storage.Clean(plan)
	if err != nil {
		fmt.Fprintf(os.Stderr, "clean finished with errors: %v\n", err)
	}
	fmt.Printf("\nfreed %s\n", storage.FormatBytes(freed))
}
