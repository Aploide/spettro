package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

func trustedPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".spettro", "trusted.json"), nil
}

func loadTrusted() ([]string, error) {
	p, err := trustedPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read trusted: %w", err)
	}
	var paths []string
	if err := json.Unmarshal(data, &paths); err != nil {
		return nil, fmt.Errorf("decode trusted: %w", err)
	}
	return paths, nil
}

func saveTrusted(paths []string) error {
	p, err := trustedPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	raw, err := json.Marshal(paths)
	if err != nil {
		return fmt.Errorf("encode trusted: %w", err)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write trusted: %w", err)
	}
	return os.Rename(tmp, p)
}

// IsTrusted reports whether cwd has been permanently trusted.
func IsTrusted(cwd string) bool {
	paths, err := loadTrusted()
	if err != nil {
		return false
	}
	return slices.Contains(paths, cwd)
}

// AddTrusted permanently adds cwd to the trusted list.
func AddTrusted(cwd string) error {
	paths, err := loadTrusted()
	if err != nil {
		return err
	}
	if slices.Contains(paths, cwd) {
		return nil // already trusted
	}
	return saveTrusted(append(paths, cwd))
}
