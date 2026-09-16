package agents

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestSetEnabledToolsRoundTrip(t *testing.T) {
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Create("demo", "Demo", "test agent", "", AgentConfig{}); err != nil {
		t.Fatal(err)
	}

	// Empty by default.
	ag, err := reg.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.Config.EnabledTools) != 0 {
		t.Fatalf("EnabledTools default = %v", ag.Config.EnabledTools)
	}

	want := []string{"mcp_kube_pods_list", "mcp_kube_nodes_top"}
	if _, err := reg.SetEnabledTools("demo", want); err != nil {
		t.Fatal(err)
	}

	// Reload from disk and confirm persistence.
	ag, err = reg.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ag.Config.EnabledTools, want) {
		t.Fatalf("EnabledTools = %v, want %v", ag.Config.EnabledTools, want)
	}

	// Replacing preserves other config fields.
	mt := 512
	if _, err := reg.SetEnabledTools("demo", nil); err != nil {
		t.Fatal(err)
	}
	ag, _ = reg.Get("demo")
	ag.Config.MaxTokens = &mt
	if _, err := reg.UpdateConfig("demo", ag.Config); err != nil {
		t.Fatal(err)
	}
	ag, _ = reg.Get("demo")
	if len(ag.Config.EnabledTools) != 0 {
		t.Fatalf("EnabledTools after clear = %v", ag.Config.EnabledTools)
	}
	if ag.Config.MaxTokens == nil || *ag.Config.MaxTokens != mt {
		t.Fatalf("MaxTokens lost: %+v", ag.Config)
	}
}

func TestSetEnabledToolsUnknownAgent(t *testing.T) {
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.SetEnabledTools("nope", []string{"mcp_x_y"}); err == nil {
		t.Fatal("expected error for unknown agent")
	}
}
