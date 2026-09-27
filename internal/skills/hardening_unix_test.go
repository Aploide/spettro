//go:build unix

package skills

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A SKILL.md that is a FIFO (possible in any cloned repository) must be
// reported and skipped, never opened: the open would block until a writer
// appears, and discovery runs under the cache lock on the TUI's input path.
func TestDiscoverRefusesFIFOManifest(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	root := filepath.Join(cwd, ".claude", "skills")
	fifoDir := filepath.Join(root, "fifo")
	if err := os.MkdirAll(fifoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(fifoDir, SkillFilename), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	writeRaw(t, root, "fine", "---\nname: fine\ndescription: d\n---\nb\n")

	done := make(chan Catalog, 1)
	go func() {
		cat, _ := Discover(cwd, DefaultLookupOptions())
		done <- cat
	}()
	select {
	case cat := <-done:
		if len(cat.Skills) != 1 || cat.Skills[0].Name != "fine" {
			t.Errorf("skills = %+v, want fine alone", cat.Skills)
		}
		if len(cat.Issues) != 1 {
			t.Errorf("issues = %v, want the FIFO reported", cat.Issues)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Discover blocked on a FIFO SKILL.md")
	}
}
