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
	cfg := &config.Config{
		MaxAgentIterations: 1,
		// Mirror the production default: all built-ins on the global ceiling.
		ToolAllowList: []string{"read_file", "write_file", "list_files", "exec"},
		ExecAllowList: []string{"ls", "echo"},
	}
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

func specNames(reg *tools.Registry) map[string]bool {
	out := map[string]bool{}
	for _, sp := range reg.Specs() {
		out[sp.Function.Name] = true
	}
	return out
}

func TestRunRegistryBuiltinToolsInherit(t *testing.T) {
	e, ar := newTestEngine(t)
	reg := registryFor(t, e, ar, "demo")
	names := specNames(reg)
	for _, want := range []string{"read_file", "write_file", "list_files", "exec",
		"memory_search", "memory_save", "record_observation", "search_docs", "read_doc"} {
		if !names[want] {
			t.Errorf("inherited registry missing %q (got %v)", want, names)
		}
	}
}

func TestRunRegistryBuiltinToolsNarrowed(t *testing.T) {
	e, ar := newTestEngine(t)

	// Narrow the agent to read-only built-ins (no write_file, no exec).
	if _, err := ar.SetEnabledBuiltinTools("demo", &[]string{"read_file", "list_files"}); err != nil {
		t.Fatal(err)
	}
	reg := registryFor(t, e, ar, "demo")
	names := specNames(reg)
	if names["exec"] || names["write_file"] {
		t.Fatalf("narrowed registry must drop exec/write_file, got %v", names)
	}
	if !names["read_file"] || !names["list_files"] {
		t.Fatalf("narrowed registry must keep read_file/list_files, got %v", names)
	}
	// Core tools are always on regardless of narrowing.
	if !names["memory_search"] || !names["read_doc"] {
		t.Fatalf("core tools must survive narrowing, got %v", names)
	}
	// Dispatch enforces the same gate: exec is unknown to this agent.
	if _, err := reg.Call(context.Background(), "exec", json.RawMessage(`{"command":"ls"}`)); err == nil {
		t.Fatal("exec must not be callable when narrowed away")
	}

	// Empty list = no built-ins at all.
	if _, err := ar.SetEnabledBuiltinTools("demo", &[]string{}); err != nil {
		t.Fatal(err)
	}
	names = specNames(registryFor(t, e, ar, "demo"))
	if names["read_file"] || names["exec"] {
		t.Fatalf("empty enabled_builtin_tools must remove all built-ins, got %v", names)
	}

	// Reset to nil: inherits again.
	if _, err := ar.SetEnabledBuiltinTools("demo", nil); err != nil {
		t.Fatal(err)
	}
	names = specNames(registryFor(t, e, ar, "demo"))
	if !names["exec"] {
		t.Fatalf("reset to nil must restore inheritance, got %v", names)
	}
}

func TestRunRegistryExecExtraCommands(t *testing.T) {
	e, ar := newTestEngine(t)
	// A command guaranteed absent from PATH so the post-gate exec always fails.
	e.cfg.ExtraExecCommands = []string{"definitely-not-a-real-cmd-xyz"}

	// Not enabled for the agent: the command is not on exec's allow-list.
	reg := registryFor(t, e, ar, "demo")
	if _, err := reg.Call(context.Background(), "exec", json.RawMessage(`{"command":"definitely-not-a-real-cmd-xyz foo"}`)); err == nil {
		t.Fatal("extra command must not run until enabled per agent")
	} else if !strings.Contains(err.Error(), "not on the exec allow-list") {
		t.Fatalf("expected allow-list rejection, got: %v", err)
	}

	// Enabled: the gate passes (the command itself fails — it does not exist —
	// but the error must not be an allow-list rejection).
	if _, err := ar.SetEnabledCommands("demo", []string{"definitely-not-a-real-cmd-xyz"}); err != nil {
		t.Fatal(err)
	}
	reg = registryFor(t, e, ar, "demo")
	_, err := reg.Call(context.Background(), "exec", json.RawMessage(`{"command":"definitely-not-a-real-cmd-xyz foo"}`))
	if err == nil {
		t.Fatal("a nonexistent command should fail, but only after passing the gate")
	}
	if strings.Contains(err.Error(), "not on the exec allow-list") {
		t.Fatalf("enabled extra command must not hit the allow-list rejection: %v", err)
	}
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
