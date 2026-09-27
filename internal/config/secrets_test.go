package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// secretsHomeForTest points HOME at a fresh directory with a random
// master.key and no SPETTRO_MASTER_KEY, the default install layout. It
// returns the keys.enc path.
func secretsHomeForTest(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SPETTRO_MASTER_KEY", "")
	t.Setenv("SUDO_USER", "")
	invalidateKeysCache()
	t.Cleanup(invalidateKeysCache)
	if _, err := machineSecret(); err != nil {
		t.Fatalf("create master key: %v", err)
	}
	return filepath.Join(home, ".spettro", "keys.enc")
}

// countScrypt replaces scryptKey with a counting wrapper for the test.
func countScrypt(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	orig := scryptKey
	scryptKey = func(password, salt []byte, N, r, p, keyLen int) ([]byte, error) {
		n.Add(1)
		return orig(password, salt, N, r, p, keyLen)
	}
	t.Cleanup(func() { scryptKey = orig })
	return &n
}

// writeLegacyKeysFile writes keys the way builds before format v2 did:
// scrypt under the master secret, no kdf field, no additional data.
func writeLegacyKeysFile(t *testing.T, path string, keys map[string]string) []byte {
	t.Helper()
	secret, err := machineSecret()
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := json.Marshal(keys)
	raw, err := sealKeys(keySecret{value: secret.value, lowEntropy: true}, plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"kdf"`)) {
		t.Fatalf("legacy file must not carry a kdf field: %s", raw)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return raw
}

func readEnvelope(t *testing.T, path string) encryptedSecrets {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var e encryptedSecrets
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestKeysV2RoundTripNeverRunsScrypt(t *testing.T) {
	path := secretsHomeForTest(t)
	scrypts := countScrypt(t)

	if err := SaveAPIKey("anthropic", "sk-ant"); err != nil {
		t.Fatal(err)
	}
	if err := SaveAPIKey("openai", "sk-oai"); err != nil {
		t.Fatal(err)
	}
	invalidateKeysCache() // force a real decrypt below
	keys, err := LoadAPIKeys()
	if err != nil {
		t.Fatal(err)
	}
	if keys["anthropic"] != "sk-ant" || keys["openai"] != "sk-oai" {
		t.Fatalf("keys = %v", keys)
	}
	if got := readEnvelope(t, path).KDF; got != kdfHKDFv2 {
		t.Fatalf("kdf = %q, want %q", got, kdfHKDFv2)
	}
	if n := scrypts.Load(); n != 0 {
		t.Fatalf("scrypt ran %d times for a v2 file, want 0", n)
	}
}

func TestLegacyKeysFileMigratesToV2WithBackup(t *testing.T) {
	path := secretsHomeForTest(t)
	legacy := writeLegacyKeysFile(t, path, map[string]string{"anthropic": "sk-legacy"})
	scrypts := countScrypt(t)

	keys, err := LoadAPIKeys()
	if err != nil {
		t.Fatal(err)
	}
	if keys["anthropic"] != "sk-legacy" {
		t.Fatalf("keys = %v", keys)
	}
	if got := readEnvelope(t, path).KDF; got != kdfHKDFv2 {
		t.Fatalf("after migration kdf = %q, want %q", got, kdfHKDFv2)
	}
	backup, err := os.ReadFile(path + legacyBackupSuffix)
	if err != nil {
		t.Fatalf("legacy backup missing: %v", err)
	}
	if !bytes.Equal(backup, legacy) {
		t.Fatal("keys.enc.v1 is not a byte-for-byte copy of the legacy file")
	}

	scrypts.Store(0)
	invalidateKeysCache()
	keys, err = LoadAPIKeys()
	if err != nil || keys["anthropic"] != "sk-legacy" {
		t.Fatalf("reload after migration: %v, %v", keys, err)
	}
	if n := scrypts.Load(); n != 0 {
		t.Fatalf("scrypt ran %d times after migration, want 0", n)
	}
}

func TestPassphraseSecretKeepsScrypt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SPETTRO_MASTER_KEY", "a passphrase")
	invalidateKeysCache()
	t.Cleanup(invalidateKeysCache)

	if err := SaveAPIKey("anthropic", "sk"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".spettro", "keys.enc")
	if got := readEnvelope(t, path).KDF; got != kdfLegacyScrypt {
		t.Fatalf("passphrase file kdf = %q, want scrypt (empty)", got)
	}
	if _, err := os.Stat(path + legacyBackupSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no backup expected for a passphrase file, stat err = %v", err)
	}
}

func TestTamperedKDFFieldIsRejected(t *testing.T) {
	path := secretsHomeForTest(t)
	if err := SaveAPIKey("anthropic", "sk"); err != nil {
		t.Fatal(err)
	}
	for _, kdf := range []string{"hkdf-sha256-v3", ""} {
		e := readEnvelope(t, path)
		e.KDF = kdf
		raw, _ := json.Marshal(e)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		invalidateKeysCache()
		if keys, err := LoadAPIKeys(); err == nil {
			t.Fatalf("kdf %q: expected an error, got keys %v", kdf, keys)
		}
		// Restore a valid v2 file for the next case.
		if err := SaveAPIKey("anthropic", "sk"); err == nil {
			t.Fatalf("kdf %q: save over an unreadable file must fail", kdf)
		}
		e.KDF = kdfHKDFv2
		raw, _ = json.Marshal(e)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestKeysCacheHitsAndInvalidatesOnExternalWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A passphrase keeps scrypt, so the scrypt counter shows cache hits.
	t.Setenv("SPETTRO_MASTER_KEY", "cache test passphrase")
	invalidateKeysCache()
	t.Cleanup(invalidateKeysCache)
	path := filepath.Join(home, ".spettro", "keys.enc")
	if err := SaveAPIKey("anthropic", "one"); err != nil {
		t.Fatal(err)
	}
	scrypts := countScrypt(t)

	if _, err := LoadAPIKeys(); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadAPIKeys()
	if err != nil {
		t.Fatal(err)
	}
	if n := scrypts.Load(); n != 1 {
		t.Fatalf("two loads of an unchanged file ran scrypt %d times, want 1", n)
	}
	keys["anthropic"] = "mutated by caller"
	if again, _ := LoadAPIKeys(); again["anthropic"] != "one" {
		t.Fatalf("cache handed out a shared map: %v", again)
	}

	// Another process rewrites the file in place (same inode) and the
	// modification time moves forward.
	secret, _ := machineSecret()
	plain, _ := json.Marshal(map[string]string{"anthropic": "two"})
	raw, err := sealKeys(secret, plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if keys, err := LoadAPIKeys(); err != nil || keys["anthropic"] != "two" {
		t.Fatalf("after external write: %v, %v", keys, err)
	}

	// Another process replaces the file by rename (new inode).
	plain, _ = json.Marshal(map[string]string{"anthropic": "three"})
	raw, _ = sealKeys(secret, plain)
	tmp := path + ".other-process"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if keys, err := LoadAPIKeys(); err != nil || keys["anthropic"] != "three" {
		t.Fatalf("after external rename: %v, %v", keys, err)
	}
}

// A crash between writing the new file and renaming it over keys.enc must
// leave the old file intact and readable, and no temp file behind.
func TestInterruptedMigrationLeavesLegacyFileIntact(t *testing.T) {
	path := secretsHomeForTest(t)
	legacy := writeLegacyKeysFile(t, path, map[string]string{"anthropic": "sk-legacy"})

	orig := replaceFile
	replaceFile = func(tmp, dst string) error {
		if dst == path {
			return errors.New("simulated crash before rename")
		}
		return orig(tmp, dst)
	}
	t.Cleanup(func() { replaceFile = orig })

	keys, err := LoadAPIKeys()
	if err != nil || keys["anthropic"] != "sk-legacy" {
		t.Fatalf("load during failed migration: %v, %v", keys, err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, legacy) {
		t.Fatal("keys.enc changed although the rename never happened")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}

	// The next start, without the crash, completes the migration.
	replaceFile = orig
	invalidateKeysCache()
	if keys, err := LoadAPIKeys(); err != nil || keys["anthropic"] != "sk-legacy" {
		t.Fatalf("load after recovery: %v, %v", keys, err)
	}
	if got := readEnvelope(t, path).KDF; got != kdfHKDFv2 {
		t.Fatalf("kdf after recovery = %q", got)
	}
}

func TestMachineSecretSkipsUserDatabaseWithoutSudo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SPETTRO_MASTER_KEY", "")
	t.Setenv("SUDO_USER", "")
	got, err := secretsHome()
	if err != nil || got != home {
		t.Fatalf("secretsHome = %q, %v; want %q", got, err, home)
	}
}

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
