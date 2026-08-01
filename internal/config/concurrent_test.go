package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Concurrent config access used to corrupt itself: every writer built its temp
// file at the fixed path "config.json.tmp", so the first rename moved the file
// out from under the others, which then failed with ENOENT — taking healthy
// *reads* down with them, because LoadOrCreate saves when it normalizes.
//
// The visible symptom was the macOS app locking the user into provider setup
// on launch: it fires account/status, providers/list and models/list at once,
// and whichever lost the race reported "could not read configuration", which
// the app read as "nothing is configured".
func TestConcurrentUpdatesDoNotCollide(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const writers = 8
	const reads = 8

	var wg sync.WaitGroup
	errs := make(chan error, (writers+reads)*4)

	for i := range writers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for round := range 4 {
				if _, err := Update(func(c *UserConfig) error {
					c.ActiveModel = fmt.Sprintf("model-%d-%d", n, round)
					return nil
				}); err != nil {
					errs <- fmt.Errorf("writer %d round %d: %w", n, round, err)
				}
			}
		}(i)
	}

	for i := range reads {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for round := range 4 {
				if _, err := LoadFull(); err != nil {
					errs <- fmt.Errorf("reader %d round %d: %w", n, round, err)
				}
			}
		}(i)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent config access failed: %v", err)
	}
}

// Independent fields written concurrently must all survive: a load/mutate/save
// cycle that doesn't hold the lock for its whole duration silently drops the
// changes made by whoever committed first.
func TestConcurrentUpdatesDoNotLoseFields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		_, _ = Update(func(c *UserConfig) error { c.SpettroEmail = "user@example.com"; return nil })
	}()
	go func() {
		defer wg.Done()
		_, _ = Update(func(c *UserConfig) error { c.SpettroPlan = "max"; return nil })
	}()
	go func() {
		defer wg.Done()
		_, _ = Update(func(c *UserConfig) error { c.ActiveProvider = "anthropic"; return nil })
	}()
	wg.Wait()

	cfg, err := LoadFull()
	if err != nil {
		t.Fatalf("LoadFull: %v", err)
	}
	if cfg.SpettroEmail != "user@example.com" {
		t.Errorf("email was lost: %q", cfg.SpettroEmail)
	}
	if cfg.SpettroPlan != "max" {
		t.Errorf("plan was lost: %q", cfg.SpettroPlan)
	}
	if cfg.ActiveProvider != "anthropic" {
		t.Errorf("active provider was lost: %q", cfg.ActiveProvider)
	}
}

// A failed write must not leave temp files behind in ~/.spettro.
func TestSaveLeavesNoTempFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	for range 5 {
		if err := Save(Default()); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	dir, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(dir))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}
