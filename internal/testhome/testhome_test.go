package testhome

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(Main(m)) }

func TestHomeIsIsolated(t *testing.T) { Check(t) }

func TestSameDir(t *testing.T) {
	dir := t.TempDir()
	if !sameDir(dir, dir+string(os.PathSeparator)) {
		t.Fatal("a directory and its path with a trailing separator differ")
	}
	if sameDir(dir, t.TempDir()) {
		t.Fatal("two temporary directories compare equal")
	}
}
