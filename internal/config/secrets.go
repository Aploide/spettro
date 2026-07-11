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
	"os/user"
	"path/filepath"

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

	key, err := deriveKey(salt)
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
	plain, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt keys: %w", err)
	}

	out := map[string]string{}
	if err := json.Unmarshal(plain, &out); err != nil {
		return nil, fmt.Errorf("decode key map: %w", err)
	}
	return out, nil
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

func machineSecret() (string, error) {
	if v := os.Getenv("SPETTRO_MASTER_KEY"); v != "" {
		return v, nil
	}
	username, home, err := secretsIdentity()
	if err != nil {
		return "", err
	}
	host, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("resolve hostname: %w", err)
	}
	hash := sha256.Sum256([]byte(username + "|" + host + "|" + home))
	return base64.StdEncoding.EncodeToString(hash[:]), nil
}
