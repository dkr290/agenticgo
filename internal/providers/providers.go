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

	"github.com/dkr290/agenticgo/internal/llm"
)

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

// Store is a thread-safe JSON-file-backed provider store.
type Store struct {
	path string
	mu   sync.RWMutex
	list []*Provider
}

// Open loads (or initializes) the provider store at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create providers dir: %w", err)
	}
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil // empty store; caller seeds a default
		}
		return nil, fmt.Errorf("read providers: %w", err)
	}
	if err := json.Unmarshal(data, &s.list); err != nil {
		return nil, fmt.Errorf("parse providers: %w", err)
	}
	return s, nil
}

// SeedDefault adds a default provider from the env config if the store is empty.
func (s *Store) SeedDefault(baseURL, apiKey, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.list) > 0 {
		return
	}
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
	} else {
		p, err = s.Get(name)
		if err != nil {
			return nil, err
		}
	}
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
			return s.saveLocked()
		}
	}
	cp := p
	s.list = append(s.list, &cp)
	if p.Default {
		s.clearDefaultLocked(p.Name)
	}
	return s.saveLocked()
}

// Delete removes a provider by name.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.list {
		if p.Name == name {
			s.list = append(s.list[:i], s.list[i+1:]...)
			return s.saveLocked()
		}
	}
	return fmt.Errorf("provider %q not found", name)
}

// TestConnection checks the provider by listing its models; it returns the
// discovered model IDs on success.
func (s *Store) TestConnection(ctx context.Context, name string) ([]string, error) {
	p, err := s.Get(name)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	op, ok := p.LLM().(*llm.OpenAIProvider)
	if !ok {
		return nil, fmt.Errorf("provider %q is not OpenAI-compatible", name)
	}
	return op.ListModels(ctx)
}

func (s *Store) clearDefaultLocked(except string) {
	for _, p := range s.list {
		if p.Name != except {
			p.Default = false
		}
	}
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.list, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal providers: %w", err)
	}
	if err := os.WriteFile(s.path, data, 0o600); err != nil {
		return fmt.Errorf("write providers: %w", err)
	}
	return nil
}
