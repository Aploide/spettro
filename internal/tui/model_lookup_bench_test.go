package tui

// Benchmarks for the per-frame model metadata reads (performance plan,
// integration step I5). The status bar's context gauge (contextWindow, via
// evaluateCompact) and the header's model label run on every chrome render.

import (
	"strings"
	"testing"

	"spettro/internal/models"
)

// lookupBenchModel is a TUI model whose provider manager holds the embedded
// catalog snapshot (the list a keyed launch starts with) and whose active
// model is in it.
func lookupBenchModel(tb testing.TB) Model {
	tb.Helper()
	m := footerModel(140, 40)
	cat, err := models.Snapshot()
	if err != nil {
		tb.Fatal(err)
	}
	m.providers.SetCatalog(cat)
	all := m.providers.Models()
	if len(all) == 0 {
		tb.Fatal("empty catalog snapshot")
	}
	last := all[len(all)-1] // the worst case for a linear scan
	m.cfg.ActiveProvider, m.cfg.ActiveModel = last.Provider, last.Name
	return m
}

// BenchmarkContextWindow is the context gauge's model lookup.
func BenchmarkContextWindow(b *testing.B) {
	m := lookupBenchModel(b)
	b.ReportAllocs()
	for b.Loop() {
		_ = m.contextWindow()
	}
}

// BenchmarkViewHeader is one header render, which labels the active model.
func BenchmarkViewHeader(b *testing.B) {
	m := lookupBenchModel(b)
	b.ReportAllocs()
	for b.Loop() {
		_ = m.viewHeader()
	}
}

// The context gauge reads the model metadata without allocating, however
// long the catalog is.
func TestContextWindowDoesNotAllocate(t *testing.T) {
	m := lookupBenchModel(t)
	if m.contextWindow() == 0 {
		t.Fatal("active catalog model has no context window")
	}
	if allocs := testing.AllocsPerRun(100, func() { _ = m.contextWindow() }); allocs != 0 {
		t.Fatalf("contextWindow allocates %.0f times per call, want 0", allocs)
	}
}

// The header labels the active model with its catalog names.
func TestViewHeaderUsesModelDisplayName(t *testing.T) {
	m := lookupBenchModel(t)
	mod, _ := m.providers.Lookup(m.cfg.ActiveProvider, m.cfg.ActiveModel)
	want := mod.DisplayName
	if want == "" {
		want = mod.Name
	}
	if len(want) > 12 {
		want = want[:12]
	}
	if header := m.viewHeader(); !strings.Contains(header, want) {
		t.Fatalf("header %q does not show the model label %q", header, want)
	}
}
