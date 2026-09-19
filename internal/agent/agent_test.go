package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkr290/agenticgo/internal/agents"
	"github.com/dkr290/agenticgo/internal/config"
	"github.com/dkr290/agenticgo/internal/llm"
	"github.com/dkr290/agenticgo/internal/mcp"
	"github.com/dkr290/agenticgo/internal/store"
	"github.com/dkr290/agenticgo/internal/tools"
)

// stubProvider satisfies llm.Provider without any network calls; the tests
// here only exercise callTool directly, so it never actually gets invoked.
type stubProvider struct{}

func (stubProvider) ChatCompletion(context.Context, llm.ChatRequest, llm.StreamFunc) (llm.Message, error) {
	return llm.Message{}, nil
}
func (stubProvider) Name() string  { return "stub" }
func (stubProvider) Model() string { return "stub" }

func newTestEngine(t *testing.T) (*Engine, *agents.Registry) {
	t.Helper()
	dir := t.TempDir()
	ar, err := agents.NewRegistry(filepath.Join(dir, "agents"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ar.Create("demo", "Demo", "test agent", "", agents.AgentConfig{}); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{MaxAgentIterations: 1}
	return New(cfg, stubProvider{}, tools.NewRegistry(nil), st, ar), ar
}

func TestCallToolGatesMCPToolsPerAgent(t *testing.T) {
	e, ar := newTestEngine(t)

	// No MCP manager wired: mcp_* names must be rejected, built-ins untouched.
	if _, err := e.callTool(context.Background(), "demo", "mcp_kube_pods_list", nil); err == nil {
		t.Fatal("mcp tool without manager should fail")
	}

	// Wire a real (empty) MCP manager. With nothing enabled, calls are blocked.
	e.SetMCPManager(newTestMCPManager(t))
	if _, err := e.callTool(context.Background(), "demo", "mcp_kube_pods_list", nil); err == nil {
		t.Fatal("mcp tool not enabled for agent should fail")
	} else if got := err.Error(); !containsAll(got, "not enabled", "demo") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Enable it for the agent: gating passes and the call reaches the manager
	// (which reports the tool as unknown since no server is connected).
	if _, err := ar.SetEnabledTools("demo", []string{"mcp_kube_pods_list"}); err != nil {
		t.Fatal(err)
	}
	_, err := e.callTool(context.Background(), "demo", "mcp_kube_pods_list", nil)
	if err == nil {
		t.Fatal("call should reach the manager and fail (no server connected)")
	}
	if !containsAll(err.Error(), "unknown mcp tool") {
		t.Fatalf("expected manager-level error, got: %v", err)
	}

	// A different agent without the tool enabled stays blocked.
	if _, err := ar.Create("other", "Other", "test", "", agents.AgentConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.callTool(context.Background(), "other", "mcp_kube_pods_list", nil); err == nil {
		t.Fatal("tool enabled for demo must not leak to other agents")
	}
}

func TestMCPToolSpecsRespectsEnabledTools(t *testing.T) {
	e, ar := newTestEngine(t)
	e.SetMCPManager(newTestMCPManager(t))

	ag, _ := ar.Get("demo")
	if specs := e.mcpToolSpecs(ag); len(specs) != 0 {
		t.Fatalf("specs without enabled tools = %v", specs)
	}

	// Enabled but not discovered (server offline): still skipped.
	if _, err := ar.SetEnabledTools("demo", []string{"mcp_kube_pods_list"}); err != nil {
		t.Fatal(err)
	}
	ag, _ = ar.Get("demo")
	if specs := e.mcpToolSpecs(ag); len(specs) != 0 {
		t.Fatalf("specs with undiscovered tool = %v", specs)
	}
}

func TestCallToolMemorySavePersistsPerAgent(t *testing.T) {
	e, _ := newTestEngine(t)
	ctx := context.Background()

	if _, err := e.callTool(ctx, "demo", "memory_save", json.RawMessage(`{"content":"demo durable fact"}`)); err != nil {
		t.Fatalf("memory_save: %v", err)
	}
	entries, err := e.store.Knowledge(ctx, "demo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Content != "demo durable fact" {
		t.Fatalf("memory_save should persist for demo, got %+v", entries)
	}
	// Scoped: nothing written for another agent.
	if other, _ := e.store.Knowledge(ctx, "other", 10); len(other) != 0 {
		t.Fatalf("memory_save leaked to other agent: %+v", other)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// newTestMCPManager returns an MCP manager with no servers (discovery catalog
// empty); used to verify per-agent gating without a real MCP server.
func newTestMCPManager(t *testing.T) *mcp.Manager {
	t.Helper()
	m, err := mcp.Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}
