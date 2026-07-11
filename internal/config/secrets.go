package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/scrypt"
)

type encryptedSecrets struct {
	Salt       string `json:"salt"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

func keysPath() (string, error) {
	_, home, err := secretsIdentity()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".spettro", "keys.enc"), nil
}

// secretsIdentity returns the username and home directory that key material
// is bound to. Under sudo the effective user is root, which would change both
// the derived encryption key and the keys.enc location; resolving SUDO_USER
// keeps secrets readable in elevated sessions.
func secretsIdentity() (username, home string, err error) {
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" && os.Geteuid() == 0 {
		if u, lookupErr := user.Lookup(sudoUser); lookupErr == nil && u.HomeDir != "" {
			return u.Username, u.HomeDir, nil
		}
	}
	current, err := user.Current()
	if err != nil {
		return "", "", fmt.Errorf("resolve current user: %w", err)
	}
	home, err = os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("resolve home dir: %w", err)
	}
	return current.Username, home, nil
}

func LoadAPIKeys() (map[string]string, error) {
	p, err := keysPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("read encrypted keys: %w", err)
	}

	var payload encryptedSecrets
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode encrypted keys: %w", err)
	}

	salt, err := base64.StdEncoding.DecodeString(payload.Salt)
	if err != nil {
		return nil, fmt.Errorf("decode salt: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(payload.Nonce)
	if err != nil {
		return nil, fmt.Errorf("decode nonce: %w", err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(payload.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decode ciphertext: %w", err)
	}

	plain, err := decryptWithSecrets(salt, nonce, ciphertext)
	if err != nil {
		return nil, err
	}

	out := map[string]string{}
	if err := json.Unmarshal(plain, &out); err != nil {
		return nil, fmt.Errorf("decode key map: %w", err)
	}
	return out, nil
}

// decryptWithSecrets tries the persisted master secret first, then falls back
// to legacy identity-derived secrets (username|hostname|home) so files written
// by older versions — possibly under a different hostname or via sudo — stay
// readable. A successful legacy decrypt is migrated to the master secret.
func decryptWithSecrets(salt, nonce, ciphertext []byte) ([]byte, error) {
	secret, err := machineSecret()
	if err != nil {
		return nil, err
	}
	plain, err := decryptWithSecret(secret, salt, nonce, ciphertext)
	if err == nil {
		return plain, nil
	}

	for _, legacy := range legacySecrets() {
		if plain, legacyErr := decryptWithSecret(legacy, salt, nonce, ciphertext); legacyErr == nil {
			var keys map[string]string
			if jsonErr := json.Unmarshal(plain, &keys); jsonErr == nil {
				_ = saveAPIKeys(keys)
			}
			return plain, nil
		}
	}
	return nil, fmt.Errorf("decrypt keys: %w", err)
}

func decryptWithSecret(secret string, salt, nonce, ciphertext []byte) ([]byte, error) {
	key, err := scrypt.Key([]byte(secret), salt, 32768, 8, 1, 32)
	if err != nil {
		return nil, fmt.Errorf("derive key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create gcm: %w", err)
	}
	return aead.Open(nil, nonce, ciphertext, nil)
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

func SaveAPIKey(provider, apiKey string) error {
	keys, err := LoadAPIKeys()
	if err != nil {
		return err
	}
	keys[provider] = apiKey
	return saveAPIKeys(keys)
}

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
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("create global config dir: %w", err)
	}

	plain, err := json.Marshal(keys)
	if err != nil {
		return fmt.Errorf("encode keys: %w", err)
	}

	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("generate salt: %w", err)
	}
	key, err := deriveKey(salt)
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return fmt.Errorf("create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("create gcm: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("generate nonce: %w", err)
	}

	payload := encryptedSecrets{
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(aead.Seal(nil, nonce, plain, nil)),
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("encode encrypted payload: %w", err)
	}

	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write encrypted keys temp: %w", err)
	}
	return os.Rename(tmp, p)
}

func deriveKey(salt []byte) ([]byte, error) {
	secret, err := machineSecret()
	if err != nil {
		return nil, err
	}
	return scrypt.Key([]byte(secret), salt, 32768, 8, 1, 32)
}

// machineSecret returns the key-encryption secret. It is a random value
// persisted alongside keys.enc so it survives hostname changes, sudo, and
// account renames; older versions derived it from mutable machine identity
// (see legacySecrets).
func machineSecret() (string, error) {
	if v := os.Getenv("SPETTRO_MASTER_KEY"); v != "" {
		return v, nil
	}
	_, home, err := secretsIdentity()
	if err != nil {
		return "", err
	}
	p := filepath.Join(home, ".spettro", "master.key")
	if data, err := os.ReadFile(p); err == nil {
		if s := strings.TrimSpace(string(data)); s != "" {
			return s, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read master key: %w", err)
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate master key: %w", err)
	}
	secret := base64.StdEncoding.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", fmt.Errorf("create global config dir: %w", err)
	}
	if err := os.WriteFile(p, []byte(secret), 0o600); err != nil {
		return "", fmt.Errorf("write master key: %w", err)
	}
	return secret, nil
}
