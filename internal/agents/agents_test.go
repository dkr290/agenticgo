package agents

import (
	"path/filepath"
	"slices"
	"strings"
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

func TestSetEnabledBuiltinToolsRoundTrip(t *testing.T) {
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Create("demo", "Demo", "test agent", "", AgentConfig{}); err != nil {
		t.Fatal(err)
	}

	// nil by default: inherit the global allow-list.
	ag, err := reg.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if ag.Config.EnabledBuiltinTools != nil {
		t.Fatalf("EnabledBuiltinTools default = %v, want nil", *ag.Config.EnabledBuiltinTools)
	}

	// Narrow to read-only; persisted to disk.
	if _, err := reg.SetEnabledBuiltinTools("demo", &[]string{"read_file", "list_files"}); err != nil {
		t.Fatal(err)
	}
	ag, _ = reg.Get("demo")
	if ag.Config.EnabledBuiltinTools == nil ||
		!slices.Equal(*ag.Config.EnabledBuiltinTools, []string{"read_file", "list_files"}) {
		t.Fatalf("EnabledBuiltinTools = %+v", ag.Config.EnabledBuiltinTools)
	}

	// Empty list (not nil) = no built-ins at all; the distinction must survive
	// the JSON round trip.
	if _, err := reg.SetEnabledBuiltinTools("demo", &[]string{}); err != nil {
		t.Fatal(err)
	}
	ag, _ = reg.Get("demo")
	if ag.Config.EnabledBuiltinTools == nil || len(*ag.Config.EnabledBuiltinTools) != 0 {
		t.Fatalf("EnabledBuiltinTools empty-vs-nil lost: %+v", ag.Config.EnabledBuiltinTools)
	}

	// nil again = inherit.
	if _, err := reg.SetEnabledBuiltinTools("demo", nil); err != nil {
		t.Fatal(err)
	}
	ag, _ = reg.Get("demo")
	if ag.Config.EnabledBuiltinTools != nil {
		t.Fatalf("EnabledBuiltinTools after reset = %v", *ag.Config.EnabledBuiltinTools)
	}
}

// TestCreateSeedsContextFilesFromTemplates verifies that Create writes all
// seven context files with the embedded template content, substituting the
// agent name/role into SOUL.md and IDENTITY.md.
func TestCreateSeedsContextFilesFromTemplates(t *testing.T) {
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Create("demo", "Demo", "test agent", "", AgentConfig{}); err != nil {
		t.Fatal(err)
	}

	wantFiles := []string{
		"AGENTS.md", "SOUL.md", "IDENTITY.md", "USER.md",
		"USER_PREDEFINED.md", "CAPABILITIES.md", "HEARTBEAT.md",
	}
	ag, err := reg.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range ag.Files {
		got[f.Name] = f.Content
	}
	for _, name := range wantFiles {
		if _, ok := got[name]; !ok {
			t.Fatalf("context file %s not listed", name)
		}
	}
	if !strings.Contains(got["SOUL.md"], "You are Demo,") {
		t.Fatalf("SOUL.md not seeded from template:\n%s", got["SOUL.md"])
	}
	if !strings.Contains(got["IDENTITY.md"], "**Name:** Demo") ||
		!strings.Contains(got["IDENTITY.md"], "**Role:** test agent") {
		t.Fatalf("IDENTITY.md not seeded from template:\n%s", got["IDENTITY.md"])
	}
	if !strings.Contains(got["AGENTS.md"], "How You Operate") {
		t.Fatalf("AGENTS.md not seeded from template:\n%s", got["AGENTS.md"])
	}
}

// TestCreateSoulOverrideBeatsTemplate verifies a non-empty soul argument wins
// over the SOUL.md template.
func TestCreateSoulOverrideBeatsTemplate(t *testing.T) {
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents"))
	if err != nil {
		t.Fatal(err)
	}
	custom := "# Custom Soul\n\nHand-written persona.\n"
	if _, err := reg.Create("demo", "Demo", "test agent", custom, AgentConfig{}); err != nil {
		t.Fatal(err)
	}
	ag, err := reg.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range ag.Files {
		if f.Name == "SOUL.md" {
			if f.Content != custom {
				t.Fatalf("SOUL.md = %q, want custom override %q", f.Content, custom)
			}
			return
		}
	}
	t.Fatal("SOUL.md not found")
}
