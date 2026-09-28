package config

// Benchmarks for keys.enc loading (performance plan, Unit A1; corresponds to
// the startup harness perf/startup, timeline row "config.LoadFull", where the
// scrypt derivation cost 77 ms per process).

import "testing"

// BenchmarkLoadAPIKeysV2 measures a cold (uncached) v2 load, the cost every
// process pays once at startup (harness: perf/startup timeline row
// "config.LoadFull", 77 ms with scrypt).
func BenchmarkLoadAPIKeysV2(b *testing.B) {
	home := b.TempDir()
	b.Setenv("HOME", home)
	b.Setenv("SPETTRO_MASTER_KEY", "")
	invalidateKeysCache()
	if err := SaveAPIKey("anthropic", "sk"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		invalidateKeysCache()
		if _, err := LoadAPIKeys(); err != nil {
			b.Fatal(err)
		}
	}
}
