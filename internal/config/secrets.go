package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/scrypt"

	"spettro/internal/fsperm"
	"spettro/internal/homedir"
	"spettro/internal/safeio"
)

// Key-derivation functions a keys.enc envelope can name in its "kdf" field.
//
// kdfLegacyScrypt (the field is absent) is every file written before format
// v2: the file key is scrypt(secret, salt, N=32768, r=8, p=1). scrypt costs
// about 60 ms per derivation, which every process paid at startup.
//
// kdfHKDFv2 derives the file key with HKDF-SHA256 from the 32-byte random
// master secret in master.key. A slow KDF adds nothing there: the input is
// already uniformly random (nothing to brute-force), and master.key sits next
// to keys.enc, so an attacker who can read one can read the other. The kdf
// name is also bound into the ciphertext as AES-GCM additional data, so an
// edited "kdf" field fails authentication instead of selecting another
// derivation.
//
// A passphrase from SPETTRO_MASTER_KEY is not uniformly random, so files
// encrypted under it keep scrypt (see keySecret.lowEntropy).
const (
	kdfLegacyScrypt = ""
	kdfHKDFv2       = "hkdf-sha256-v2"
)

// hkdfInfo is the HKDF context string for keys.enc v2; changing it changes
// every derived key.
const hkdfInfo = "spettro keys.enc v2"

// legacyBackupSuffix names the copy of a pre-v2 keys.enc kept next to the
// migrated file (keys.enc.v1), so a user who downgrades to a build that only
// reads scrypt files can restore it. Removal timeline: the first release that
// writes v2 files creates the backup; the release after it stops creating it
// and deletes an existing one. The scrypt reader itself stays, so an install
// that skips a release still migrates.
const legacyBackupSuffix = ".v1"

// scryptKey is scrypt.Key behind a variable so tests can count derivations
// (a v2 file must never run scrypt).
var scryptKey = scrypt.Key

// replaceFile is the final, atomic step of every keys.enc write. Tests swap
// it to simulate a crash between writing the temp file and renaming it.
var replaceFile = safeio.Replace

type encryptedSecrets struct {
	// KDF is kdfHKDFv2 for format v2 and absent for legacy scrypt files.
	KDF        string `json:"kdf,omitempty"`
	Salt       string `json:"salt"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// keySecret is the secret keys.enc is encrypted under.
type keySecret struct {
	value string
	// lowEntropy is true for a SPETTRO_MASTER_KEY passphrase, which keeps the
	// slow scrypt KDF; the random master.key secret uses HKDF.
	lowEntropy bool
}

// kdf returns the key-derivation function new files use for this secret.
func (s keySecret) kdf() string {
	if s.lowEntropy {
		return kdfLegacyScrypt
	}
	return kdfHKDFv2
}

// decryptedKeysCache holds the key map most recently decrypted from keys.enc,
// so the repeated LoadAPIKeys calls a session makes (every ACP request, every
// headless submission) skip the file decode and, for scrypt files, the 60 ms
// derivation.
//
// Key: the keys.enc path, the file's identity as os.SameFile compares it
// (device and inode on Unix, volume and file index on Windows), its size and
// modification time, and the secret used to decrypt it.
//
// Invalidation: every write in this package goes through writeKeysFile, which
// drops the entry. A write by another process (an ACP server next to the TUI)
// replaces the file by rename, which changes its identity and mtime, so the
// next lookup misses.
//
// Ownership: not owned by any goroutine; every access holds
// decryptedKeysCache.mu. Lookups return a copy, so callers may mutate what
// they get.
var decryptedKeysCache struct {
	mu     sync.Mutex
	path   string
	info   os.FileInfo
	secret string
	keys   map[string]string
}

// cachedKeys returns a copy of the cached key map when it was decrypted from
// the same file (path and FileInfo) under the same secret.
func cachedKeys(path string, info os.FileInfo, secret string) (map[string]string, bool) {
	c := &decryptedKeysCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.info == nil || c.path != path || c.secret != secret {
		return nil, false
	}
	if !os.SameFile(c.info, info) || c.info.Size() != info.Size() || !c.info.ModTime().Equal(info.ModTime()) {
		return nil, false
	}
	return maps.Clone(c.keys), true
}

// storeCachedKeys records keys as the decrypted content of path at info.
func storeCachedKeys(path string, info os.FileInfo, secret string, keys map[string]string) {
	c := &decryptedKeysCache
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path, c.info, c.secret, c.keys = path, info, secret, maps.Clone(keys)
}

// invalidateKeysCache drops the cached key map.
func invalidateKeysCache() {
	c := &decryptedKeysCache
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path, c.info, c.secret, c.keys = "", nil, "", nil
}

func keysPath() (string, error) {
	home, err := secretsHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".spettro", "keys.enc"), nil
}

// SecretsDir returns the directory that holds keys.enc and master.key. It is
// ~/.spettro, except under sudo, where it is the invoking user's ~/.spettro
// (see secretsHome).
func SecretsDir() (string, error) {
	path, err := keysPath()
	if err != nil {
		return "", err
	}
	return filepath.Dir(path), nil
}

// secretsHome returns the home directory key material lives under. Under
// sudo the effective user is root, which would move keys.enc and master.key;
// resolving SUDO_USER keeps secrets readable in elevated sessions. The user
// database is consulted only in that case: os/user.Current costs 0.6 to
// 0.9 ms per process on macOS and the home directory is all that is needed.
func secretsHome() (string, error) {
	if u, ok := sudoUser(); ok {
		return u.HomeDir, nil
	}
	home, err := homedir.Dir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return home, nil
}

// sudoUser returns the invoking user when running as root under sudo.
func sudoUser() (*user.User, bool) {
	name := os.Getenv("SUDO_USER")
	if name == "" || os.Geteuid() != 0 {
		return nil, false
	}
	u, err := user.Lookup(name)
	if err != nil || u.HomeDir == "" {
		return nil, false
	}
	return u, true
}

// secretsIdentity returns the username and home directory the legacy,
// identity-derived secrets were bound to (see legacySecrets).
func secretsIdentity() (username, home string, err error) {
	if u, ok := sudoUser(); ok {
		return u.Username, u.HomeDir, nil
	}
	current, err := user.Current()
	if err != nil {
		return "", "", fmt.Errorf("resolve current user: %w", err)
	}
	home, err = homedir.Dir()
	if err != nil {
		return "", "", fmt.Errorf("resolve home dir: %w", err)
	}
	return current.Username, home, nil
}

// LoadAPIKeys returns the provider API keys stored in ~/.spettro/keys.enc, or
// an empty map when the file does not exist. A legacy (pre-v2) file is
// migrated to v2 on the first successful decrypt; see migrateLegacyKeysFile.
func LoadAPIKeys() (map[string]string, error) {
	p, err := keysPath()
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("read encrypted keys: %w", err)
	}
	secret, err := machineSecret()
	if err != nil {
		return nil, err
	}
	if keys, ok := cachedKeys(p, info, secret.value); ok {
		return keys, nil
	}

	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read encrypted keys: %w", err)
	}
	var payload encryptedSecrets
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode encrypted keys: %w", err)
	}
	salt, nonce, ciphertext, err := payload.decode()
	if err != nil {
		return nil, err
	}

	plain, migrate, err := decryptKeysFile(payload.KDF, secret, salt, nonce, ciphertext)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if err := json.Unmarshal(plain, &out); err != nil {
		return nil, fmt.Errorf("decode key map: %w", err)
	}
	storeCachedKeys(p, info, secret.value, out)
	if migrate {
		migrateLegacyKeysFile(p, data, out)
	}
	return out, nil
}

// decryptKeysFile opens a keys.enc payload written with kdf. migrate reports
// that the file is in a legacy form and should be rewritten.
func decryptKeysFile(kdf string, secret keySecret, salt, nonce, ciphertext []byte) (plain []byte, migrate bool, err error) {
	switch kdf {
	case kdfHKDFv2:
		plain, err = decryptWithSecret(kdfHKDFv2, secret.value, salt, nonce, ciphertext)
		if err != nil {
			return nil, false, fmt.Errorf("decrypt keys: %w", err)
		}
		return plain, false, nil
	case kdfLegacyScrypt:
		return decryptLegacy(secret, salt, nonce, ciphertext)
	default:
		return nil, false, fmt.Errorf("decrypt keys: unsupported kdf %q", kdf)
	}
}

// decode base64-decodes the envelope's binary fields.
func (e encryptedSecrets) decode() (salt, nonce, ciphertext []byte, err error) {
	if salt, err = base64.StdEncoding.DecodeString(e.Salt); err != nil {
		return nil, nil, nil, fmt.Errorf("decode salt: %w", err)
	}
	if nonce, err = base64.StdEncoding.DecodeString(e.Nonce); err != nil {
		return nil, nil, nil, fmt.Errorf("decode nonce: %w", err)
	}
	if ciphertext, err = base64.StdEncoding.DecodeString(e.Ciphertext); err != nil {
		return nil, nil, nil, fmt.Errorf("decode ciphertext: %w", err)
	}
	return salt, nonce, ciphertext, nil
}

// decryptLegacy decrypts a pre-v2 (scrypt) file. It tries the persisted
// master secret first, then the legacy identity-derived secrets
// (username|hostname|home) so files written by older versions, possibly
// under a different hostname or via sudo, stay readable. migrate reports
// whether the file should be rewritten: always after an identity-secret
// decrypt, and after a master-secret decrypt when that secret moves to v2.
func decryptLegacy(secret keySecret, salt, nonce, ciphertext []byte) (plain []byte, migrate bool, err error) {
	plain, err = decryptWithSecret(kdfLegacyScrypt, secret.value, salt, nonce, ciphertext)
	if err == nil {
		return plain, !secret.lowEntropy, nil
	}
	for _, legacy := range legacySecrets() {
		if plain, legacyErr := decryptWithSecret(kdfLegacyScrypt, legacy, salt, nonce, ciphertext); legacyErr == nil {
			return plain, true, nil
		}
	}
	return nil, false, fmt.Errorf("decrypt keys: %w", err)
}

// migrateLegacyKeysFile rewrites a legacy keys.enc under the current secret
// (format v2 unless the secret is a passphrase). Before a v2 file replaces
// the legacy one, the legacy bytes are saved as keys.enc.v1 so a downgraded
// build can still be pointed at them; if that backup cannot be written the
// migration is skipped and the legacy file stays as it is. Failures are not
// reported: the keys were already decrypted, and the next load retries.
func migrateLegacyKeysFile(path string, legacyData []byte, keys map[string]string) {
	secret, err := machineSecret()
	if err != nil {
		return
	}
	if secret.kdf() == kdfHKDFv2 {
		if writeKeysFile(path+legacyBackupSuffix, legacyData) != nil {
			return
		}
	}
	_ = saveAPIKeys(keys)
}

// decryptWithSecret derives the file key for kdf and opens the ciphertext.
func decryptWithSecret(kdf, secret string, salt, nonce, ciphertext []byte) ([]byte, error) {
	aead, err := newAEAD(kdf, secret, salt)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, nonce, ciphertext, additionalData(kdf))
}

// additionalData is the AES-GCM additional data for kdf. Legacy files had
// none; v2 binds the kdf name so the field cannot be edited undetected.
func additionalData(kdf string) []byte {
	if kdf == kdfLegacyScrypt {
		return nil
	}
	return []byte(kdf)
}

// newAEAD returns the AES-256-GCM cipher for the key kdf derives from secret
// and salt.
func newAEAD(kdf, secret string, salt []byte) (cipher.AEAD, error) {
	key, err := deriveKey(kdf, secret, salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create gcm: %w", err)
	}
	return aead, nil
}

// deriveKey derives the 32-byte file key from secret and salt with kdf.
func deriveKey(kdf, secret string, salt []byte) ([]byte, error) {
	var (
		key []byte
		err error
	)
	switch kdf {
	case kdfHKDFv2:
		key, err = hkdf.Key(sha256.New, []byte(secret), salt, hkdfInfo, 32)
	case kdfLegacyScrypt:
		key, err = scryptKey([]byte(secret), salt, 32768, 8, 1, 32)
	default:
		return nil, fmt.Errorf("derive key: unsupported kdf %q", kdf)
	}
	if err != nil {
		return nil, fmt.Errorf("derive key: %w", err)
	}
	return key, nil
}

// legacySecrets returns key-derivation secrets used by older versions, which
// were bound to username|hostname|home. The hostname component drifts with
// the network on macOS, so both current and mDNS-style names are tried.
func legacySecrets() []string {
	username, home, err := secretsIdentity()
	if err != nil {
		return nil
	}
	hosts := map[string]bool{}
	if h, err := os.Hostname(); err == nil {
		hosts[h] = true
	}
	if out, err := exec.Command("scutil", "--get", "LocalHostName").Output(); err == nil {
		if h := strings.TrimSpace(string(out)); h != "" {
			hosts[h] = true
			hosts[h+".local"] = true
		}
	}
	var secrets []string
	for h := range hosts {
		hash := sha256.Sum256([]byte(username + "|" + h + "|" + home))
		secrets = append(secrets, base64.StdEncoding.EncodeToString(hash[:]))
	}
	return secrets
}

// SaveAPIKey stores apiKey for provider in keys.enc.
func SaveAPIKey(provider, apiKey string) error {
	keys, err := LoadAPIKeys()
	if err != nil {
		return err
	}
	keys[provider] = apiKey
	return saveAPIKeys(keys)
}

// RemoveAPIKey deletes provider's key from keys.enc.
func RemoveAPIKey(provider string) error {
	keys, err := LoadAPIKeys()
	if err != nil {
		return err
	}
	delete(keys, provider)
	return saveAPIKeys(keys)
}

func saveAPIKeys(keys map[string]string) error {
	p, err := keysPath()
	if err != nil {
		return err
	}
	plain, err := json.Marshal(keys)
	if err != nil {
		return fmt.Errorf("encode keys: %w", err)
	}
	secret, err := machineSecret()
	if err != nil {
		return err
	}
	raw, err := sealKeys(secret, plain)
	if err != nil {
		return err
	}
	return writeKeysFile(p, raw)
}

// sealKeys encrypts plain under secret and returns the keys.enc document.
func sealKeys(secret keySecret, plain []byte) ([]byte, error) {
	kdf := secret.kdf()
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generate salt: %w", err)
	}
	aead, err := newAEAD(kdf, secret.value, salt)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	payload := encryptedSecrets{
		KDF:        kdf,
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(aead.Seal(nil, nonce, plain, additionalData(kdf))),
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode encrypted payload: %w", err)
	}
	return raw, nil
}

// writeKeysFile atomically replaces path with data: a uniquely named temp
// file in the same directory, fsync, then rename. A crash at any point
// leaves either the old file or the new one, never a partial write. It
// drops the decrypted-keys cache.
func writeKeysFile(path string, data []byte) error {
	defer invalidateKeysCache()
	dir := filepath.Dir(path)
	if err := fsperm.SecureMkdirAll(dir); err != nil {
		return fmt.Errorf("create global config dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("write encrypted keys temp: %w", err)
	}
	tmpPath := tmp.Name()
	if err := writeAndSync(tmp, data); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write encrypted keys temp: %w", err)
	}
	if err := replaceFile(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace encrypted keys: %w", err)
	}
	return nil
}

// writeAndSync writes data to f, flushes it to stable storage and closes f.
func writeAndSync(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// machineSecret returns the key-encryption secret: SPETTRO_MASTER_KEY when
// set, otherwise a random value persisted in master.key alongside keys.enc
// (created on first use), so it survives hostname changes, sudo, and account
// renames; older versions derived it from mutable machine identity (see
// legacySecrets).
func machineSecret() (keySecret, error) {
	if v := os.Getenv("SPETTRO_MASTER_KEY"); v != "" {
		return keySecret{value: v, lowEntropy: true}, nil
	}
	home, err := secretsHome()
	if err != nil {
		return keySecret{}, err
	}
	p := filepath.Join(home, ".spettro", "master.key")
	if data, err := os.ReadFile(p); err == nil {
		if s := strings.TrimSpace(string(data)); s != "" {
			return keySecret{value: s}, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return keySecret{}, fmt.Errorf("read master key: %w", err)
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return keySecret{}, fmt.Errorf("generate master key: %w", err)
	}
	secret := base64.StdEncoding.EncodeToString(raw)
	if err := fsperm.SecureMkdirAll(filepath.Dir(p)); err != nil {
		return keySecret{}, fmt.Errorf("create global config dir: %w", err)
	}
	if err := os.WriteFile(p, []byte(secret), 0o600); err != nil {
		return keySecret{}, fmt.Errorf("write master key: %w", err)
	}
	return keySecret{value: secret}, nil
}
