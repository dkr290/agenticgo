package crypto

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func testKey(t *testing.T) Key {
	t.Helper()
	var k Key
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	k := testKey(t)
	cases := []string{
		"",
		"a",
		"test-api-key-not-real",
		"example-longer-fake-key-0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		"unicode-✓-key-🔑",
	}
	for _, in := range cases {
		enc, err := k.Encrypt(in)
		if err != nil {
			t.Fatalf("Encrypt(%q): %v", in, err)
		}
		if in == "" && enc != "" {
			t.Fatalf("empty input should stay empty, got %q", enc)
		}
		dec, err := k.Decrypt(enc)
		if err != nil {
			t.Fatalf("Decrypt(Encrypt(%q)): %v", in, err)
		}
		if dec != in {
			t.Fatalf("round trip mismatch: got %q want %q", dec, in)
		}
	}
}

func TestDecryptLegacyPlaintext(t *testing.T) {
	k := testKey(t)
	// Values without the enc:v1: prefix must come back unchanged (back-compat).
	got, err := k.Decrypt("sk-plaintext-legacy")
	if err != nil {
		t.Fatalf("Decrypt(plaintext): %v", err)
	}
	if got != "sk-plaintext-legacy" {
		t.Fatalf("legacy plaintext altered: %q", got)
	}
}

func TestEncryptProducesPrefixedCiphertext(t *testing.T) {
	k := testKey(t)
	enc, err := k.Encrypt("secret")
	if err != nil {
		t.Fatal(err)
	}
	if enc == "secret" {
		t.Fatal("ciphertext equals plaintext")
	}
	if len(enc) < len(Prefix) || enc[:len(Prefix)] != Prefix {
		t.Fatalf("missing %q prefix: %q", Prefix, enc)
	}
}

func TestEncryptRandomNonce(t *testing.T) {
	k := testKey(t)
	a, _ := k.Encrypt("same")
	b, _ := k.Encrypt("same")
	if a == b {
		t.Fatal("same plaintext produced identical ciphertext (nonce not random)")
	}
}

func TestLoadKeyFromSecret(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = 0xAB
	}
	k, err := LoadKey(base64.StdEncoding.EncodeToString(raw), filepath.Join(t.TempDir(), "ignored.key"))
	if err != nil {
		t.Fatal(err)
	}
	if k[0] != 0xAB {
		t.Fatalf("secret key not loaded: %v", k[:4])
	}
}

func TestLoadKeyInvalidSecret(t *testing.T) {
	if _, err := LoadKey("not-base64!!!", filepath.Join(t.TempDir(), "ignored.key")); err == nil {
		t.Fatal("expected error for non-base64 secret")
	}
	// Valid base64 but wrong length must also fail loudly.
	if _, err := LoadKey(base64.StdEncoding.EncodeToString([]byte("short")), filepath.Join(t.TempDir(), "ignored.key")); err == nil {
		t.Fatal("expected error for wrong-length secret")
	}
}

func TestLoadKeyGeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	kf := filepath.Join(dir, "secret.key")
	k1, err := LoadKey("", kf)
	if err != nil {
		t.Fatal(err)
	}
	// Second load must return the SAME persisted key.
	k2, err := LoadKey("", kf)
	if err != nil {
		t.Fatal(err)
	}
	if k1 != k2 {
		t.Fatal("persisted key changed between loads")
	}
	// File must exist with 0600 perms.
	fi, err := os.Stat(kf)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file perms = %o, want 600", perm)
	}
}
