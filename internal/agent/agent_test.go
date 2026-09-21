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
// here only exercise the per-run registry directly, so it never actually gets
// invoked.
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

// registryFor builds the per-run registry for an agent, failing the test on error.
func registryFor(t *testing.T, e *Engine, ar *agents.Registry, agentKey string) *tools.Registry {
	t.Helper()
	ag, err := ar.Get(agentKey)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := e.runRegistry(ag)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestCallToolGatesMCPToolsPerAgent(t *testing.T) {
	e, ar := newTestEngine(t)
	ctx := context.Background()

	// No MCP manager wired: mcp_* names are simply not in the registry.
	reg := registryFor(t, e, ar, "demo")
	if _, err := reg.Call(ctx, "mcp_kube_pods_list", nil); err == nil {
		t.Fatal("mcp tool without manager should fail")
	}

	// Wire a real (empty) MCP manager. With nothing enabled, the tool is not
	// registered, so calls are rejected as unknown.
	e.SetMCPManager(newTestMCPManager(t))
	reg = registryFor(t, e, ar, "demo")
	if _, err := reg.Call(ctx, "mcp_kube_pods_list", nil); err == nil {
		t.Fatal("mcp tool not enabled for agent should fail")
	} else if got := err.Error(); !containsAll(got, "unknown") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Enable it for the agent. The tool is still not discovered (no server
	// connected), so it stays out of the registry — the per-agent gate and the
	// discovery gate together keep it unavailable.
	if _, err := ar.SetEnabledTools("demo", []string{"mcp_kube_pods_list"}); err != nil {
		t.Fatal(err)
	}
	reg = registryFor(t, e, ar, "demo")
	if _, ok := reg.Get("mcp_kube_pods_list"); ok {
		t.Fatal("undiscovered mcp tool must not be registered")
	}
	if _, err := reg.Call(ctx, "mcp_kube_pods_list", nil); err == nil {
		t.Fatal("call to undiscovered mcp tool should fail")
	}

	// A different agent without the tool enabled stays blocked.
	if _, err := ar.Create("other", "Other", "test", "", agents.AgentConfig{}); err != nil {
		t.Fatal(err)
	}
	otherReg := registryFor(t, e, ar, "other")
	if _, err := otherReg.Call(ctx, "mcp_kube_pods_list", nil); err == nil {
		t.Fatal("tool enabled for demo must not leak to other agents")
	}
}

func TestMCPToolSpecsRespectsEnabledTools(t *testing.T) {
	e, ar := newTestEngine(t)
	e.SetMCPManager(newTestMCPManager(t))

	reg := registryFor(t, e, ar, "demo")
	if specs := mcpSpecs(reg.Specs()); len(specs) != 0 {
		t.Fatalf("specs without enabled tools = %v", specs)
	}

	// Enabled but not discovered (server offline): still skipped.
	if _, err := ar.SetEnabledTools("demo", []string{"mcp_kube_pods_list"}); err != nil {
		t.Fatal(err)
	}
	reg = registryFor(t, e, ar, "demo")
	if specs := mcpSpecs(reg.Specs()); len(specs) != 0 {
		t.Fatalf("specs with undiscovered tool = %v", specs)
	}
}

// mcpSpecs filters a spec list down to MCP tools.
func mcpSpecs(specs []llm.ToolSpec) []llm.ToolSpec {
	var out []llm.ToolSpec
	for _, sp := range specs {
		if mcp.IsMCPToolName(sp.Function.Name) {
			out = append(out, sp)
		}
	}
	return out
}

func TestCallToolMemorySavePersistsPerAgent(t *testing.T) {
	e, ar := newTestEngine(t)
	ctx := context.Background()

	reg := registryFor(t, e, ar, "demo")
	if _, err := reg.Call(ctx, "memory_save", json.RawMessage(`{"content":"demo durable fact"}`)); err != nil {
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
