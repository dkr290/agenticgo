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
// invoked. It is handed out by fakeLookup.
type stubProvider struct{}

func (stubProvider) ChatCompletion(context.Context, llm.ChatRequest, llm.StreamFunc) (llm.Message, error) {
	return llm.Message{}, nil
}
func (stubProvider) Name() string  { return "stub" }
func (stubProvider) Model() string { return "stub" }

// stubLookup is a minimal ProviderLookup: any name (including "" = default)
// resolves to the stub provider.
type stubLookup struct{}

func (stubLookup) GetLLM(string) (llm.Provider, error) { return stubProvider{}, nil }

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
	e := New(cfg, tools.NewRegistry(nil), st, ar)
	e.SetProviderLookup(stubLookup{})
	return e, ar
}

// registryFor builds the per-run registry for an agent, failing the test on error.
func registryFor(t *testing.T, e *Engine, ar *agents.Registry, agentKey string) *tools.Registry {
	t.Helper()
	ag, err := ar.Get(agentKey)
	if err != nil {
		t.Fatal(err)
	}
	reg, _, err := e.runRegistry(ag)
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

// trackingLookup records the names it was asked to resolve.
type trackingLookup struct{ got []string }

func (f *trackingLookup) GetLLM(name string) (llm.Provider, error) {
	f.got = append(f.got, name)
	return stubProvider{}, nil
}

func TestResolveProviderRoutesEmptyNameToStoreDefault(t *testing.T) {
	e, ar := newTestEngine(t)
	tl := &trackingLookup{}
	e.SetProviderLookup(tl)

	ag, err := ar.Get("demo")
	if err != nil {
		t.Fatal(err)
	}

	// No override, agent pins nothing: empty name reaches the store, which
	// resolves it to its default provider.
	if _, err := e.resolveProvider(ag, ""); err != nil {
		t.Fatal(err)
	}
	// Agent config provider wins when set.
	if _, err := ar.UpdateConfig("demo", agents.AgentConfig{Provider: strPtr("pinned")}); err != nil {
		t.Fatal(err)
	}
	ag, err = ar.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.resolveProvider(ag, ""); err != nil {
		t.Fatal(err)
	}
	// Per-request override beats the agent pin.
	if _, err := e.resolveProvider(ag, "adhoc"); err != nil {
		t.Fatal(err)
	}

	want := []string{"", "pinned", "adhoc"}
	if len(tl.got) != len(want) {
		t.Fatalf("lookups = %v, want %v", tl.got, want)
	}
	for i := range want {
		if tl.got[i] != want[i] {
			t.Fatalf("lookups = %v, want %v", tl.got, want)
		}
	}

	// Without a provider lookup, resolution fails loudly instead of silently
	// falling back to an env-built provider.
	bare := New(e.cfg, tools.NewRegistry(nil), e.store, ar)
	if _, err := bare.resolveProvider(ag, ""); err == nil {
		t.Fatal("resolveProvider without lookup must fail")
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
		"memory_search", "memory_save", "record_observation", "search_docs", "read_doc",
		"list_agent_images", "fetch_agent_image"} {
		if !names[want] {
			t.Errorf("inherited registry missing %q (got %v)", want, names)
		}
	}
}

// Regression test: runRegistry must return the live imageSink so Run can drain
// images fetched by fetch_agent_image and inject them into the next LLM turn.
// Previously it returned a nil sink, so a fetched image never reached the model
// in a fresh conversation (only directly-attached images did) and the model
// hallucinated the content. Here we verify an image fetched via the tool lands
// in the returned sink.
func TestRunRegistryFetchAgentImageReachesSink(t *testing.T) {
	e, ar := newTestEngine(t)

	// Store a reference image for the agent (minimal PNG bytes; content is
	// irrelevant, only that it is a valid, readable image file).
	if _, err := ar.SaveImage("demo", "pic.png", []byte("\x89PNG\r\n\x1a\n")); err != nil {
		t.Fatal(err)
	}

	ag, err := ar.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	reg, sink, err := e.runRegistry(ag)
	if err != nil {
		t.Fatal(err)
	}
	if sink == nil {
		t.Fatal("runRegistry must return the live image sink, not nil")
	}

	// The agent sees the image name and fetches it by name.
	if _, err := reg.Call(context.Background(), "fetch_agent_image", json.RawMessage(`{"name":"pic.png"}`)); err != nil {
		t.Fatalf("fetch_agent_image: %v", err)
	}

	// The engine must be able to drain the fetched image for injection into the
	// next LLM turn.
	imgs := sink.DrainImages()
	if len(imgs) != 1 {
		t.Fatalf("expected 1 fetched image in sink, got %d", len(imgs))
	}
	if !strings.HasPrefix(imgs[0], "data:image/png;base64,") {
		t.Fatalf("expected a png data-URL, got %q", imgs[0])
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

// TestObservationInjectResolution verifies the precedence: the agent's own
// observation_inject override wins over the global env default; nil inherits.
func TestObservationInjectResolution(t *testing.T) {
	e, ar := newTestEngine(t)
	e.cfg.ObservationInject = 3 // global default (AGENTICGO_OBSERVATION_INJECT)

	ag, err := ar.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.observationInject(ag); got != 3 {
		t.Fatalf("nil override must inherit the global default, got %d", got)
	}

	// Per-agent override wins.
	seven := 7
	if _, err := ar.UpdateConfig("demo", agents.AgentConfig{ObservationInject: &seven}); err != nil {
		t.Fatal(err)
	}
	ag, err = ar.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.observationInject(ag); got != 7 {
		t.Fatalf("agent override must win, got %d", got)
	}

	// 0 disables injection for that agent.
	zero := 0
	if _, err := ar.UpdateConfig("demo", agents.AgentConfig{ObservationInject: &zero}); err != nil {
		t.Fatal(err)
	}
	ag, _ = ar.Get("demo")
	if got := e.observationInject(ag); got != 0 {
		t.Fatalf("0 must disable injection, got %d", got)
	}
}

// fixedProvider returns a canned assistant reply, used to drive Evolve without
// a network call.
type fixedProvider struct{ reply string }

func (p fixedProvider) ChatCompletion(context.Context, llm.ChatRequest, llm.StreamFunc) (llm.Message, error) {
	return llm.Message{Role: llm.RoleAssistant, Content: p.reply}, nil
}
func (p fixedProvider) Name() string  { return "fixed" }
func (p fixedProvider) Model() string { return "fixed" }

type fixedLookup struct{ p llm.Provider }

func (l fixedLookup) GetLLM(string) (llm.Provider, error) { return l.p, nil }

// Regression test: Evolve must skip duplicate learnings and keep saving the
// rest. Previously it returned ErrKnowledgeDuplicate on the first repeat and
// aborted the whole pass, silently dropping any new facts listed after it.
func TestEvolveSkipsDuplicatesAndKeepsRest(t *testing.T) {
	e, _ := newTestEngine(t)
	ctx := context.Background()

	// Pre-seed one fact so Evolve's extraction hits a duplicate mid-list.
	if err := e.store.AddKnowledge(ctx, "demo", "user prefers concise answers"); err != nil {
		t.Fatal(err)
	}

	// The model re-extracts the known fact plus a brand-new one after it.
	e.SetProviderLookup(fixedLookup{p: fixedProvider{reply: "- user prefers concise answers\n- deploys happen on Fridays\n"}})

	// Enough history for Evolve to consider the session worth learning from.
	for i := 0; i < 4; i++ {
		if err := e.store.AppendMessage(ctx, "demo", "s1", "user", "hello"); err != nil {
			t.Fatal(err)
		}
	}

	if err := e.Evolve(ctx, "demo", "s1"); err != nil {
		t.Fatalf("Evolve must not fail on a duplicate extraction: %v", err)
	}

	entries, err := e.store.Knowledge(ctx, "demo", 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, en := range entries {
		got[en.Content] = true
	}
	if !got["user prefers concise answers"] {
		t.Errorf("pre-seeded fact missing, got %v", got)
	}
	if !got["deploys happen on Fridays"] {
		t.Errorf("new fact after the duplicate must still be saved, got %v", got)
	}
	if len(entries) != 2 {
		t.Errorf("duplicate must not be re-inserted, got %d entries: %v", len(entries), got)
	}
}

// TestRetentionEnabledGating verifies the sweeper only makes sense (and only
// starts) when at least one retention knob is non-zero. With all of them at 0
// there is nothing to expire, so retention is disabled entirely.
func TestRetentionEnabledGating(t *testing.T) {
	e, _ := newTestEngine(t)

	// All knobs zero => nothing to expire.
	e.cfg.ObservationTTLDays, e.cfg.ObservationKeepLatest = 0, 0
	e.cfg.KnowledgeTTLDays, e.cfg.KnowledgeKeepLatest = 0, 0
	if e.retentionEnabled() {
		t.Fatal("all knobs at 0 must disable retention")
	}

	// Any single knob non-zero => retention is on.
	for _, set := range []func(){
		func() { e.cfg.KnowledgeTTLDays = 365 },
		func() { e.cfg.KnowledgeKeepLatest = 1000 },
		func() { e.cfg.ObservationTTLDays = 14 },
		func() { e.cfg.ObservationKeepLatest = 200 },
	} {
		e.cfg.ObservationTTLDays, e.cfg.ObservationKeepLatest = 0, 0
		e.cfg.KnowledgeTTLDays, e.cfg.KnowledgeKeepLatest = 0, 0
		set()
		if !e.retentionEnabled() {
			t.Fatal("a single non-zero knob must enable retention")
		}
	}
}

// TestRetentionSweeperSweepsAllAgents verifies the background sweeper applies
// the retention limits to every agent — including an idle one that never runs
// (whose rows the lazy per-Run prune would otherwise never touch).
func TestRetentionSweeperSweepsAllAgents(t *testing.T) {
	e, ar := newTestEngine(t)
	ctx := context.Background()

	// A second agent that never runs.
	if _, err := ar.Create("idle", "Idle", "never runs", "", agents.AgentConfig{}); err != nil {
		t.Fatal(err)
	}

	// Knowledge count cap: keep newest 2. Seed 3 facts for each agent.
	e.cfg.KnowledgeKeepLatest = 2
	for _, key := range []string{"demo", "idle"} {
		for i := 1; i <= 3; i++ {
			if err := e.store.AddKnowledge(ctx, key, key+" fact "+string(rune('0'+i))); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Run the sweep synchronously (one pass), as the goroutine would on tick.
	agents, err := ar.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, ag := range agents {
		e.pruneRetention(ctx, ag.Key)
	}

	// Both agents — including the idle one — are capped to the newest 2.
	for _, key := range []string{"demo", "idle"} {
		got, err := e.store.Knowledge(ctx, key, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("agent %q must be capped to 2 entries, got %d (%v)", key, len(got), got)
		}
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
