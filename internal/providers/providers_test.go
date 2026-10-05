package providers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkr290/agenticgo/internal/crypto"
)

func TestEncryptedPersistenceAndFailedSaveRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	key := crypto.Key{1, 2, 3}
	s, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	p := Provider{Name: "first", BaseURL: "http://example.invalid/v1", Model: "test", APIKey: "private-api-key", Default: true}
	if err := s.Upsert(p); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), p.APIKey) || !strings.Contains(string(data), crypto.Prefix) {
		t.Fatal("key not encrypted")
	}
	reopened, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Get(p.Name)
	if err != nil || got.APIKey != p.APIKey {
		t.Fatalf("reopen: %+v %v", got, err)
	}
	// Replacing a directory with a regular file must fail even when tests run as root.
	s.path = t.TempDir()
	if err := s.Upsert(Provider{Name: "second", BaseURL: p.BaseURL, Model: p.Model, Default: true}); err == nil {
		t.Fatal("expected save failure")
	}
	if len(s.List()) != 1 || !s.Default().Default || s.Default().Name != "first" {
		t.Fatal("failed save changed provider state")
	}
	if err := s.Delete("first"); err == nil {
		t.Fatal("expected delete persistence failure")
	}
	if len(s.List()) != 1 {
		t.Fatal("failed deletion lost provider")
	}
	data, err = os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), crypto.Prefix) {
		t.Fatal("original persisted file damaged", err)
	}
}
