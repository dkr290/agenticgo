// Package providers manages named OpenAI-compatible LLM provider
// configurations stored on disk as JSON. Exactly one provider may be marked
// default; the default backs the agent engine unless a chat request names a
// specific provider.
package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/dkr290/agenticgo/internal/crypto"
	"github.com/dkr290/agenticgo/internal/llm"
	"github.com/dkr290/agenticgo/internal/logger"
)

// redact masks a secret for logs, showing only the last 4 characters.
func redact(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 4 {
		return "****"
	}
	return "****" + s[len(s)-4:]
}

// Provider is a named OpenAI-compatible endpoint configuration.
type Provider struct {
	Name        string `json:"name"`         // unique key, e.g. "ollama-local"
	DisplayName string `json:"display_name"` // shown in the UI
	Type        string `json:"type"`         // always "openai-compatible" for now
	BaseURL     string `json:"base_url"`     // e.g. http://localhost:11434/v1
	APIKey      string `json:"api_key"`      // often unused for local servers
	Model       string `json:"model"`        // default model for this provider
	Default     bool   `json:"default"`      // exactly one should be default
	// Vision marks this provider's model as able to understand images
	// (screenshots, pictures). There is no reliable API to detect this, so it
	// is set manually. An agent's config may override it (agents.AgentConfig.Vision).
	Vision bool `json:"vision"`
}

// LLM builds an llm.Provider for this configuration.
func (p *Provider) LLM() llm.Provider {
	return llm.NewOpenAI(p.BaseURL, p.APIKey, p.Model)
}

// Store is a thread-safe JSON-file-backed provider store. API keys are
// encrypted at rest (encKey); they are decrypted transparently on read so
// callers and the UI keep working with plaintext keys.
type Store struct {
	path   string
	mu     sync.RWMutex
	list   []*Provider
	log    logger.Logger
	encKey crypto.Key
}

// Open loads (or initializes) the provider store at path. encKey encrypts
// provider API keys at rest; legacy plaintext keys are migrated on load.
func Open(path string, encKey crypto.Key) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create providers dir: %w", err)
	}
	s := &Store{path: path, log: logger.Nop(), encKey: encKey}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.log.Debug("providers store: no file yet, starting empty", "path", path)
			return s, nil // empty store; caller seeds a default
		}
		return nil, fmt.Errorf("read providers: %w", err)
	}
	// Keys are stored encrypted; decrypt for in-memory use.
	var stored []*Provider
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("parse providers: %w", err)
	}
	s.list = stored
	migrated := false
	for _, p := range s.list {
		pt, err := s.encKey.Decrypt(p.APIKey)
		if err != nil {
			return nil, fmt.Errorf("decrypt api key for %q: %w", p.Name, err)
		}
		if p.APIKey != "" && !crypto.IsEncrypted(p.APIKey) { // legacy plaintext
			migrated = true
		}
		p.APIKey = pt
	}
	if migrated {
		s.log.Debug("providers store: migrating plaintext api keys to encrypted")
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	s.log.Debug("providers store: loaded", "path", path, "count", len(s.list))
	return s, nil
}

// SetLogger wires verbose logging into the store (nil keeps a no-op logger).
func (s *Store) SetLogger(l logger.Logger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l == nil {
		l = logger.Nop()
	}
	s.log = l
}

// SeedDefault adds a default provider from the env config if the store is empty.
func (s *Store) SeedDefault(baseURL, apiKey, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.list) > 0 {
		s.log.Debug("providers: store not empty, skipping env seed", "count", len(s.list))
		return
	}
	s.log.Debug("providers: seeding default from env", "base_url", baseURL, "model", model)
	s.list = append(s.list, &Provider{
		Name:        "default",
		DisplayName: "Default (env)",
		Type:        "openai-compatible",
		BaseURL:     baseURL,
		APIKey:      apiKey,
		Model:       model,
		Default:     true,
	})
	_ = s.saveLocked()
}

// List returns all providers, sorted by name.
func (s *Store) List() []Provider {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Provider, 0, len(s.list))
	for _, p := range s.list {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns a provider by name.
func (s *Store) Get(name string) (*Provider, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.list {
		if p.Name == name {
			cp := *p
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("provider %q not found", name)
}

// Default returns the default provider, or nil if none exists.
func (s *Store) Default() *Provider {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.list {
		if p.Default {
			cp := *p
			return &cp
		}
	}
	if len(s.list) > 0 {
		cp := *s.list[0]
		return &cp
	}
	return nil
}

// GetLLM resolves a provider name into an llm.Provider. An empty name
// resolves to the default provider. It implements agent.ProviderLookup.
func (s *Store) GetLLM(name string) (llm.Provider, error) {
	var p *Provider
	var err error
	if name == "" {
		p = s.Default()
		if p == nil {
			return nil, fmt.Errorf("no providers configured")
		}
		name = p.Name
	} else {
		p, err = s.Get(name)
		if err != nil {
			s.log.Debug("providers: GetLLM lookup failed", "name", name, "error", err)
			return nil, err
		}
	}
	s.log.Debug("providers: resolved LLM", "name", name, "base_url", p.BaseURL, "model", p.Model)
	return p.LLM(), nil
}

// VisionCapable reports whether the named provider ("" = default) is marked
// as supporting images. It implements agent.VisionLookup. Unknown names and
// an empty store report false.
func (s *Store) VisionCapable(name string) bool {
	var p *Provider
	if name == "" {
		p = s.Default()
	} else {
		var err error
		p, err = s.Get(name)
		if err != nil {
			return false
		}
	}
	return p != nil && p.Vision
}

// Upsert creates or updates a provider. If p.Default is set, all other
// providers are un-defaulted.
func (s *Store) Upsert(p Provider) error {
	if p.Name == "" {
		return fmt.Errorf("provider name is required")
	}
	if p.BaseURL == "" {
		return fmt.Errorf("provider base_url is required")
	}
	if p.Type == "" {
		p.Type = "openai-compatible"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.list {
		if existing.Name == p.Name {
			cp := p
			s.list[i] = &cp
			if p.Default {
				s.clearDefaultLocked(p.Name)
			}
			s.log.Debug("providers: updated",
				"name", p.Name, "base_url", p.BaseURL, "model", p.Model,
				"vision", p.Vision, "default", p.Default, "api_key", redact(p.APIKey))
			return s.saveLocked()
		}
	}
	cp := p
	s.list = append(s.list, &cp)
	if p.Default {
		s.clearDefaultLocked(p.Name)
	}
	s.log.Debug("providers: created",
		"name", p.Name, "base_url", p.BaseURL, "model", p.Model,
		"vision", p.Vision, "default", p.Default, "api_key", redact(p.APIKey))
	return s.saveLocked()
}

// Delete removes a provider by name.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.list {
		if p.Name == name {
			s.list = append(s.list[:i], s.list[i+1:]...)
			s.log.Debug("providers: deleted", "name", name, "remaining", len(s.list))
			return s.saveLocked()
		}
	}
	s.log.Debug("providers: delete failed, not found", "name", name)
	return fmt.Errorf("provider %q not found", name)
}

// TestConnection checks a provider by listing its models; it returns the
// discovered model IDs on success.
//
// If adhoc is non-nil it is tested as-is — this lets the UI test the
// provider form's current (possibly unsaved) values. Otherwise the saved
// provider named `name` is looked up and tested.
func (s *Store) TestConnection(ctx context.Context, name string, adhoc *Provider) ([]string, error) {
	var p Provider
	switch {
	case adhoc != nil:
		p = *adhoc
		s.log.Debug("providers: testing ad-hoc (unsaved) config",
			"name", p.Name, "base_url", p.BaseURL, "model", p.Model, "api_key", redact(p.APIKey))
	default:
		saved, err := s.Get(name)
		if err != nil {
			s.log.Debug("providers: test failed, not found", "name", name)
			return nil, err
		}
		p = *saved
		s.log.Debug("providers: testing saved provider",
			"name", p.Name, "base_url", p.BaseURL, "model", p.Model, "api_key", redact(p.APIKey))
	}
	if p.BaseURL == "" {
		return nil, fmt.Errorf("provider base_url is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	op, ok := p.LLM().(*llm.OpenAIProvider)
	if !ok {
		return nil, fmt.Errorf("provider %q is not OpenAI-compatible", p.Name)
	}
	op.SetLogger(s.log)
	models, err := op.ListModels(ctx)
	if err != nil {
		s.log.Debug("providers: test connection failed", "name", p.Name, "error", err)
		return nil, err
	}
	s.log.Debug("providers: test connection ok", "name", p.Name, "models", len(models))
	return models, nil
}

func (s *Store) clearDefaultLocked(except string) {
	for _, p := range s.list {
		if p.Name != except {
			p.Default = false
		}
	}
}

// saveLocked persists the store with API keys encrypted at rest. In-memory
// keys stay plaintext; only the JSON written to disk is encrypted.
func (s *Store) saveLocked() error {
	stored := make([]Provider, 0, len(s.list))
	for _, p := range s.list {
		cp := *p
		enc, err := s.encKey.Encrypt(p.APIKey)
		if err != nil {
			return fmt.Errorf("encrypt api key for %q: %w", p.Name, err)
		}
		cp.APIKey = enc
		stored = append(stored, cp)
	}
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal providers: %w", err)
	}
	if err := os.WriteFile(s.path, data, 0o600); err != nil {
		return fmt.Errorf("write providers: %w", err)
	}
	return nil
}
