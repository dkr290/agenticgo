// Package crypto provides symmetric authenticated encryption for secrets at
// rest (provider API keys). It uses AES-256-GCM with a random nonce.
//
// The key comes from the AGENTICGO_SECRET_KEY env var; when unset, a random
// 32-byte key is generated once and persisted to a 0600 file next to the data
// (e.g. data/secret.key). The on-disk file is still readable by anyone who can
// read the data dir, so this protects against casual inspection / backups of
// providers.json — for stronger isolation, supply the key via env.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	cryptorand "crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
)

// Prefix marks an encrypted value so it is distinguishable from plaintext and
// so legacy (plaintext) values still load.
const Prefix = "enc:v1:"

// IsEncrypted reports whether value carries the encrypted-value prefix.
func IsEncrypted(value string) bool {
	return len(value) >= len(Prefix) && value[:len(Prefix)] == Prefix
}

// Key is a 32-byte AES-256 key.
type Key [32]byte

// LoadKey resolves the encryption key: secretB64 (base64, 32 bytes — typically
// from config, e.g. AGENTICGO_SECRET_KEY) first, then the persisted key file
// (created on first use). An empty secretB64 skips straight to the key file.
func LoadKey(secretB64, keyFile string) (Key, error) {
	var k Key
	if secretB64 != "" {
		b, err := base64.StdEncoding.DecodeString(secretB64)
		if err == nil && len(b) == 32 {
			copy(k[:], b)
			return k, nil
		}
		// Be strict so a mistyped key is loud rather than silently
		// deriving a different key and failing to decrypt later.
		if err != nil {
			return k, fmt.Errorf("secret key is not valid base64: %w", err)
		}
		return k, fmt.Errorf("secret key must decode to 32 bytes, got %d", len(b))
	}
	// Fall back to (or create) the persisted key file.
	if b, err := os.ReadFile(keyFile); err == nil && len(b) == 32 {
		copy(k[:], b)
		return k, nil
	}
	if _, err := cryptorand.Read(k[:]); err != nil {
		return k, fmt.Errorf("generate key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o755); err != nil {
		return k, fmt.Errorf("create key dir: %w", err)
	}
	if err := os.WriteFile(keyFile, k[:], 0o600); err != nil {
		return k, fmt.Errorf("write key file: %w", err)
	}
	return k, nil
}

// Encrypt encrypts plaintext and returns an "enc:v1:<base64(nonce+ciphertext)>"
// string. Empty input returns empty (no envelope) so empty keys stay empty.
func (k Key) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	gcm, err := newGCM(k)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := cryptorand.Read(nonce); err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}
	ct := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return Prefix + base64.StdEncoding.EncodeToString(ct), nil
}

// Decrypt reverses Encrypt. Values without the enc:v1: prefix are returned
// as-is (backwards compatibility with plaintext).
func (k Key) Decrypt(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !IsEncrypted(value) {
		return value, nil // legacy plaintext
	}
	raw, err := base64.StdEncoding.DecodeString(value[len(Prefix):])
	if err != nil {
		return "", fmt.Errorf("decode encrypted value: %w", err)
	}
	gcm, err := newGCM(k)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("encrypted value too short")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt value: %w", err)
	}
	return string(pt), nil
}

func newGCM(k Key) (cipher.AEAD, error) {
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return gcm, nil
}
