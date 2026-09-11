package agent

import (
	"testing"

	"github.com/dkr290/agenticgo/internal/agents"
	"github.com/dkr290/agenticgo/internal/llm"
)

// fakeLookup implements both ProviderLookup and VisionLookup for tests.
type fakeLookup struct{ vision map[string]bool }

func (f fakeLookup) GetLLM(name string) (llm.Provider, error) { return nil, nil }
func (f fakeLookup) VisionCapable(name string) bool           { return f.vision[name] }

func boolPtr(b bool) *bool    { return &b }
func strPtr(s string) *string { return &s }

func TestEffectiveVision(t *testing.T) {
	e := &Engine{providers: fakeLookup{vision: map[string]bool{"vis": true, "plain": false}}}

	cases := []struct {
		name       string
		cfg        agents.AgentConfig
		perReq     string
		wantVision bool
	}{
		{"inherit from vision provider (agent pins provider)", agents.AgentConfig{Provider: strPtr("vis")}, "", true},
		{"inherit from non-vision provider", agents.AgentConfig{Provider: strPtr("plain")}, "", false},
		{"per-request override to vision provider", agents.AgentConfig{}, "vis", true},
		{"per-request override to non-vision provider", agents.AgentConfig{}, "plain", false},
		{"agent forces on over non-vision provider", agents.AgentConfig{Provider: strPtr("plain"), Vision: boolPtr(true)}, "", true},
		{"agent forces off over vision provider", agents.AgentConfig{Provider: strPtr("vis"), Vision: boolPtr(false)}, "", false},
		{"agent forces off over per-request vision provider", agents.AgentConfig{Vision: boolPtr(false)}, "vis", false},
		{"no provider, no override -> false", agents.AgentConfig{}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ag := &agents.Agent{Key: "a", Config: c.cfg}
			if got := e.EffectiveVision(ag, c.perReq); got != c.wantVision {
				t.Errorf("EffectiveVision = %v, want %v", got, c.wantVision)
			}
		})
	}
}
