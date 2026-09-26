package storage

import (
	"fmt"
	"os"
	"path/filepath"

	"spettro/internal/fsperm"
	"spettro/internal/homedir"
)

type Store struct {
	ProjectDir string
	GlobalDir  string
}

func New(cwd string) (*Store, error) {
	home, err := homedir.Dir()
	if err != nil {
		return nil, fmt.Errorf("resolve home dir: %w", err)
	}

	s := &Store{
		ProjectDir: filepath.Join(cwd, ".spettro"),
		GlobalDir:  filepath.Join(home, ".spettro"),
	}
	if err := s.Ensure(); err != nil {
		return nil, err
	}
	return s, nil
}

// Ensure creates the global storage dir. The project dir (<cwd>/.spettro) is
// created lazily, by the first write that needs it: an empty one left in every
// directory Spettro runs in is clutter, and the model reads it as part of the
// project.
func (s *Store) Ensure() error {
	if err := fsperm.SecureMkdirAll(s.GlobalDir); err != nil {
		return fmt.Errorf("create global storage dir: %w", err)
	}
	return nil
}

// EnsureProjectDir creates the project storage dir if it is missing.
func (s *Store) EnsureProjectDir() error {
	if err := os.MkdirAll(s.ProjectDir, 0o755); err != nil {
		return fmt.Errorf("create project storage dir: %w", err)
	}
	return nil
}

func (s *Store) WriteProjectFile(name, content string) error {
	if err := s.EnsureProjectDir(); err != nil {
		return err
	}
	target := filepath.Join(s.ProjectDir, name)
	return os.WriteFile(target, []byte(content), 0o644)
}

func (s *Store) AppendProjectFile(name, content string) error {
	if err := s.EnsureProjectDir(); err != nil {
		return err
	}
	target := filepath.Join(s.ProjectDir, name)
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(content)
	return err
}
