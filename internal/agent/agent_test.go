package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkr290/agenticgo/internal/agents"
	"github.com/dkr290/agenticgo/internal/config"
	"github.com/dkr290/agenticgo/internal/llm"
	"github.com/dkr290/agenticgo/internal/mcp"
	"github.com/dkr290/agenticgo/internal/store"
	"github.com/microsoft/agent-framework-go/tool"
)

// scriptedProvider returns a llm.Provider whose "model" is served by a mock
// Chat Completions server. script is called per completion request with the
// 1-based request number and the decoded request body; it returns either SSE
// chunks (stream=true requests) or, for non-streaming requests (Evolve's
// Collect), a single JSON response body built from the first chunk's text.
func scriptedProvider(t *testing.T, script func(call int, req map[string]any) []string) llm.Provider {
	t.Helper()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		calls++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		chunks := script(calls, body)
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			for _, c := range chunks {
				_, _ = w.Write([]byte("data: " + c + "\n\n"))
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		// Non-streaming: fold the scripted text into a plain JSON response.
		var text string
		for _, c := range chunks {
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(c), &chunk); err == nil && len(chunk.Choices) > 0 {
				text += chunk.Choices[0].Delta.Content
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "c1", "object": "chat.completion",
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": text},
				"finish_reason": "stop",
			}},
		})
	}))
	t.Cleanup(srv.Close)
	return llm.NewOpenAI(srv.URL+"/v1", "", "test-model")
}

// sseText builds the chunk sequence for a plain assistant text reply.
func sseText(text string) []string {
	return []string{
		`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, text),
		`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}
}

// sseToolCall builds the chunk sequence for one tool call (no text).
func sseToolCall(id, name, args string) []string {
	return []string{
		`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":""}}]},"finish_reason":null}]}`, id, name),
		fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]},"finish_reason":null}]}`, args),
		`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
}

// toolNames extracts the names of the tools offered in a request body.
func toolNames(req map[string]any) map[string]bool {
	out := map[string]bool{}
	tools, _ := req["tools"].([]any)
	for _, t := range tools {
		if m, ok := t.(map[string]any); ok {
			if fn, ok := m["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok {
					out[name] = true
				}
			}
		}
	}
	return out
}

// countImages sums the image_url parts across the request's messages.
func countImages(req map[string]any) int {
	n := 0
	msgs, _ := req["messages"].([]any)
	for _, m := range msgs {
		msg, _ := m.(map[string]any)
		parts, _ := msg["content"].([]any)
		for _, p := range parts {
			if part, ok := p.(map[string]any); ok && part["type"] == "image_url" {
				n++
			}
		}
	}
	return n
}

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
		MaxAgentIterations: 3,
		// Mirror the production default: all built-ins on the global ceiling.
		ToolAllowList: []string{"read_file", "write_file", "list_files", "exec"},
		ExecAllowList: []string{"ls", "echo"},
	}
	e := New(cfg, st, ar)
	return e, ar
}

// toolSetFor builds the per-run tool set for an agent, failing on error.
func toolSetFor(t *testing.T, e *Engine, ar *agents.Registry, agentKey string) []tool.Tool {
	t.Helper()
	ag, err := ar.Get(agentKey)
	if err != nil {
		t.Fatal(err)
	}
	ts, _, err := e.runTools(ag)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// toolSetNames indexes a tool set by name.
func toolSetNames(ts []tool.Tool) map[string]tool.Tool {
	out := map[string]tool.Tool{}
	for _, t := range ts {
		out[t.Name()] = t
	}
	return out
}

func TestRunToolsGatesMCPToolsPerAgent(t *testing.T) {
	e, ar := newTestEngine(t)

	// No MCP manager wired: no mcp_* tools in the set.
	if _, ok := toolSetNames(toolSetFor(t, e, ar, "demo"))["mcp_kube_pods_list"]; ok {
		t.Fatal("mcp tool without manager must be absent")
	}

	// Wired but nothing enabled: still absent.
	e.SetMCPManager(newTestMCPManager(t))
	if _, ok := toolSetNames(toolSetFor(t, e, ar, "demo"))["mcp_kube_pods_list"]; ok {
		t.Fatal("mcp tool not enabled for agent must be absent")
	}

	// Enabled but not discovered (no server connected): still absent — the
	// per-agent gate and the discovery gate together keep it unavailable.
	if _, err := ar.SetEnabledTools("demo", []string{"mcp_kube_pods_list"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := toolSetNames(toolSetFor(t, e, ar, "demo"))["mcp_kube_pods_list"]; ok {
		t.Fatal("undiscovered mcp tool must be absent")
	}

	// A different agent without the tool enabled stays blocked.
	if _, err := ar.Create("other", "Other", "test", "", agents.AgentConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := toolSetNames(toolSetFor(t, e, ar, "other"))["mcp_kube_pods_list"]; ok {
		t.Fatal("tool enabled for demo must not leak to other agents")
	}
}

func TestRunToolsMemorySavePersistsPerAgent(t *testing.T) {
	e, ar := newTestEngine(t)
	ctx := context.Background()

	ms, ok := toolSetNames(toolSetFor(t, e, ar, "demo"))["memory_save"]
	if !ok {
		t.Fatal("memory_save missing from tool set")
	}
	ft, ok := ms.(tool.FuncTool)
	if !ok {
		t.Fatalf("memory_save is %T, want tool.FuncTool", ms)
	}
	if _, err := ft.Call(ctx, `{"content":"demo durable fact"}`); err != nil {
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
type trackingLookup struct {
	got []string
	p   llm.Provider
}

func (f *trackingLookup) GetLLM(name string) (llm.Provider, error) {
	f.got = append(f.got, name)
	return f.p, nil
}

func TestResolveProviderRoutesEmptyNameToStoreDefault(t *testing.T) {
	e, ar := newTestEngine(t)
	tl := &trackingLookup{p: llm.NewOpenAI("http://unused", "", "m")}
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

	// Without a provider lookup, resolution fails loudly.
	bare := New(e.cfg, e.store, ar)
	if _, err := bare.resolveProvider(ag, ""); err == nil {
		t.Fatal("resolveProvider without lookup must fail")
	}
}

func TestRunToolsBuiltinToolsInherit(t *testing.T) {
	e, ar := newTestEngine(t)
	names := toolSetNames(toolSetFor(t, e, ar, "demo"))
	for _, want := range []string{"read_file", "write_file", "list_files", "exec",
		"memory_search", "memory_save", "record_observation", "search_docs", "read_doc",
		"list_agent_images", "fetch_agent_image"} {
		if names[want] == nil {
			t.Errorf("inherited tool set missing %q", want)
		}
	}
}

// Regression test: runTools must return the live imageSink so the run can
// drain images fetched by fetch_agent_image and inject them into the next
// provider call. Previously a nil sink was returned, so a fetched image never
// reached the model and it hallucinated the content.
func TestRunToolsFetchAgentImageReachesSink(t *testing.T) {
	e, ar := newTestEngine(t)

	// Store a reference image for the agent (minimal PNG bytes).
	if _, err := ar.SaveImage("demo", "pic.png", []byte("\x89PNG\r\n\x1a\n")); err != nil {
		t.Fatal(err)
	}

	ag, err := ar.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	ts, sink, err := e.runTools(ag)
	if err != nil {
		t.Fatal(err)
	}
	if sink == nil {
		t.Fatal("runTools must return the live image sink, not nil")
	}

	fetch, ok := toolSetNames(ts)["fetch_agent_image"]
	if !ok {
		t.Fatal("fetch_agent_image missing")
	}
	if _, err := fetch.(tool.FuncTool).Call(context.Background(), `{"name":"pic.png"}`); err != nil {
		t.Fatalf("fetch_agent_image: %v", err)
	}

	imgs := sink.DrainImages()
	if len(imgs) != 1 {
		t.Fatalf("expected 1 fetched image in sink, got %d", len(imgs))
	}
	if !strings.HasPrefix(imgs[0], "data:image/png;base64,") {
		t.Fatalf("expected a png data-URL, got %q", imgs[0])
	}
}

func TestRunToolsBuiltinToolsNarrowed(t *testing.T) {
	e, ar := newTestEngine(t)

	// Narrow the agent to read-only built-ins (no write_file, no exec).
	if _, err := ar.SetEnabledBuiltinTools("demo", &[]string{"read_file", "list_files"}); err != nil {
		t.Fatal(err)
	}
	names := toolSetNames(toolSetFor(t, e, ar, "demo"))
	if names["exec"] != nil || names["write_file"] != nil {
		t.Fatalf("narrowed tool set must drop exec/write_file")
	}
	if names["read_file"] == nil || names["list_files"] == nil {
		t.Fatalf("narrowed tool set must keep read_file/list_files")
	}
	// Core tools are always on regardless of narrowing.
	if names["memory_search"] == nil || names["read_doc"] == nil {
		t.Fatalf("core tools must survive narrowing")
	}
	// MAF's autocall can only invoke what it was handed: exec absent from the
	// slice means unreachable at dispatch.

	// Empty list = no built-ins at all.
	if _, err := ar.SetEnabledBuiltinTools("demo", &[]string{}); err != nil {
		t.Fatal(err)
	}
	names = toolSetNames(toolSetFor(t, e, ar, "demo"))
	if names["read_file"] != nil || names["exec"] != nil {
		t.Fatalf("empty enabled_builtin_tools must remove all built-ins")
	}

	// Reset to nil: inherits again.
	if _, err := ar.SetEnabledBuiltinTools("demo", nil); err != nil {
		t.Fatal(err)
	}
	if toolSetNames(toolSetFor(t, e, ar, "demo"))["exec"] == nil {
		t.Fatalf("reset to nil must restore inheritance")
	}
}

func TestRunToolsExecExtraCommands(t *testing.T) {
	e, ar := newTestEngine(t)
	// A command guaranteed absent from PATH so the post-gate exec always fails.
	e.cfg.ExtraExecCommands = []string{"definitely-not-a-real-cmd-xyz"}

	callExec := func() (any, error) {
		ex, ok := toolSetNames(toolSetFor(t, e, ar, "demo"))["exec"]
		if !ok {
			t.Fatal("exec missing")
		}
		return ex.(tool.FuncTool).Call(context.Background(), `{"command":"definitely-not-a-real-cmd-xyz foo"}`)
	}

	// Not enabled for the agent: the command is not on exec's allow-list.
	if _, err := callExec(); err == nil {
		t.Fatal("extra command must not run until enabled per agent")
	} else if !strings.Contains(err.Error(), "not on the exec allow-list") {
		t.Fatalf("expected allow-list rejection, got: %v", err)
	}

	// Enabled: the gate passes (the command itself fails — it does not exist —
	// but the error must not be an allow-list rejection).
	if _, err := ar.SetEnabledCommands("demo", []string{"definitely-not-a-real-cmd-xyz"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callExec(); err == nil {
		t.Fatal("a nonexistent command should fail, but only after passing the gate")
	} else if strings.Contains(err.Error(), "not on the exec allow-list") {
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

// TestEvolveSkipsDuplicatesAndKeepsRest is the regression test for the
// duplicate-extraction bug: Evolve must skip duplicate learnings and keep
// saving the rest (previously one duplicate aborted the whole pass, silently
// dropping any new facts listed after it).
func TestEvolveSkipsDuplicatesAndKeepsRest(t *testing.T) {
	e, _ := newTestEngine(t)
	ctx := context.Background()

	// Pre-seed one fact so Evolve's extraction hits a duplicate mid-list.
	if err := e.store.AddKnowledge(ctx, "demo", "user prefers concise answers"); err != nil {
		t.Fatal(err)
	}

	// The model re-extracts the known fact plus a brand-new one after it.
	e.SetProviderLookup(&trackingLookup{p: scriptedProvider(t, func(int, map[string]any) []string {
		return sseText("- user prefers concise answers\n- deploys happen on Fridays\n")
	})})

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
			if err := e.store.AddKnowledge(ctx, key, fmt.Sprintf("%s fact %d", key, i)); err != nil {
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
// empty), so mcp_* tools are never discovered.
func newTestMCPManager(t *testing.T) *mcp.Manager {
	t.Helper()
	m, err := mcp.Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.CloseAll)
	return m
}
