// Package agent implements the tool-use agent loop: send messages to the LLM,
// execute any requested tool calls, feed results back, and repeat until the
// model produces a final answer (or the iteration cap is hit).
//
// The Engine resolves named agents (their context files + skills + per-agent
// knowledge) and runs the loop scoped to that agent and session.
package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dkr290/agenticgo/internal/agents"
	"github.com/dkr290/agenticgo/internal/config"
	"github.com/dkr290/agenticgo/internal/llm"
	"github.com/dkr290/agenticgo/internal/mcp"
	"github.com/dkr290/agenticgo/internal/skills"
	"github.com/dkr290/agenticgo/internal/store"
	"github.com/dkr290/agenticgo/internal/tools"
)

// Event is emitted during a run so callers can stream progress.
type Event struct {
	// Kind is "text", "tool_call", "tool_result", "error", or "done".
	Kind string
	// Text holds assistant text for Kind "text".
	Text string
	// ToolName / ToolArgs / ToolResult describe tool activity.
	ToolName   string
	ToolArgs   string
	ToolResult string
	// Err is set for Kind "error".
	Err error
}

// ProviderLookup resolves a named provider configuration into an
// llm.Provider. Implemented by internal/providers.Store.
type ProviderLookup interface {
	GetLLM(name string) (llm.Provider, error)
}

// VisionLookup is an optional extension of ProviderLookup that also reports
// whether the named provider's model supports images. Implemented by
// internal/providers.Store.
type VisionLookup interface {
	// VisionCapable reports whether the provider ("" = default) is vision-capable.
	VisionCapable(name string) bool
}

// Engine runs agent-scoped chat loops.
type Engine struct {
	cfg        *config.Config
	tools      *tools.Registry
	store      *store.Store
	agents     *agents.Registry
	providers  ProviderLookup // required for chat: resolves the default and named providers
	mcp        *mcp.Manager   // optional: custom (MCP) tools, enabled per agent
	basePrompt string
	runMu      sync.Mutex
	runs       map[conversation]*runGate
}

type conversation struct{ agent, session string }
type runGate struct {
	token chan struct{}
	refs  int
}

// lockConversation keeps complete turns together across REST, WS, and cron.
func (e *Engine) lockConversation(ctx context.Context, key conversation) (func(), error) {
	e.runMu.Lock()
	if e.runs == nil {
		e.runs = make(map[conversation]*runGate)
	}
	g := e.runs[key]
	if g == nil {
		g = &runGate{token: make(chan struct{}, 1)}
		e.runs[key] = g
	}
	g.refs++
	e.runMu.Unlock()
	drop := func() {
		e.runMu.Lock()
		defer e.runMu.Unlock()
		g.refs--
		if g.refs == 0 {
			delete(e.runs, key)
		}
	}
	select {
	case g.token <- struct{}{}:
		return func() { <-g.token; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

// New creates an Engine.
func New(cfg *config.Config, reg *tools.Registry, st *store.Store, ar *agents.Registry) *Engine {
	return &Engine{cfg: cfg, tools: reg, store: st, agents: ar, basePrompt: cfg.SystemPrompt}
}

// SetProviderLookup wires the provider store (used by the Providers UI).
// An empty provider name resolves to the store's default provider, so marking
// a provider as default in the UI takes effect immediately.
func (e *Engine) SetProviderLookup(pl ProviderLookup) { e.providers = pl }

// SetMCPManager wires the MCP manager so agents can use MCP tools they
// have enabled (config.json enabled_tools, the MCP Tools tab).
func (e *Engine) SetMCPManager(m *mcp.Manager) { e.mcp = m }

// buildSystemPrompt composes: base prompt + agent context files + skills +
// accumulated per-agent knowledge.
func (e *Engine) buildSystemPrompt(ctx context.Context, ag *agents.Agent) string {
	var b strings.Builder

	if p := strings.TrimSpace(e.basePrompt); p != "" {
		b.WriteString(p)
		b.WriteString("\n\n")
	}

	// Agent identity + persona + instructions from context files.
	if p := ag.SystemPrompt(); p != "" {
		b.WriteString(p)
		b.WriteString("\n\n")
	}

	// Skills: from the global library, but only those the agent has enabled.
	// Skills are never inherited implicitly — an empty EnabledSkills means the
	// agent gets no skills at all.
	if len(ag.Config.EnabledSkills) > 0 {
		if lib, err := e.agents.SkillsLibraryDir(); err == nil {
			if sks, err := skills.Load(lib); err == nil && len(sks) > 0 {
				if p := skills.Prompt(sks, ag.Config.EnabledSkills); p != "" {
					b.WriteString(p)
					b.WriteString("\n\n")
				}
			}
		}
	}

	// Knowledge-base documents (uploaded reference material). List titles so the
	// agent knows what's available; content is fetched via search_docs + read_doc.
	if docs, err := e.store.ListKnowledgeDocs(ctx, ag.Key); err == nil && len(docs) > 0 {
		b.WriteString("## Knowledge Base\n")
		b.WriteString("Reference documents are available. Use the search_docs tool to find " +
			"relevant documents, then read_doc with the document id to read the content " +
			"before answering on these topics:\n")
		for _, d := range docs {
			b.WriteString("- ")
			b.WriteString(strings.TrimSpace(d.Title))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// Reference images stored for this agent. List names so the agent knows
	// what images exist; content is fetched via list_agent_images + fetch_agent_image.
	if images, err := e.agents.ListImages(ag.Key); err == nil && len(images) > 0 {
		b.WriteString("## Reference Images\n")
		b.WriteString("Reference images are available. Use the list_agent_images tool to " +
			"see what images exist, then fetch_agent_image with the image name to " +
			"retrieve one before answering on these topics:\n")
		for _, im := range images {
			b.WriteString("- ")
			b.WriteString(strings.TrimSpace(im.Name))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// Recent observations (e.g. from a recurring monitor). How many are
	// injected is configurable per agent (observation_inject) over the global
	// default — a monitor that diffs run-over-run wants the last few snapshots,
	// not just the latest. Stale observations are pruned by retention and never
	// bulk-loaded, so outdated state does not mislead the model.
	switch n := e.observationInject(ag); {
	case n <= 0:
		// Observations disabled for this agent.
	case n == 1:
		if obs, err := e.store.LatestObservation(ctx, ag.Key); err == nil && obs != nil {
			b.WriteString("## Latest Observation\n")
			b.WriteString("Most recent recorded state (may be outdated — re-check before acting on it):\n")
			b.WriteString(strings.TrimSpace(obs.Content))
			b.WriteString("\n\n")
		}
	default:
		if obs, err := e.store.ListObservations(ctx, ag.Key, n); err == nil && len(obs) > 0 {
			b.WriteString("## Recent Observations\n")
			b.WriteString("Most recent recorded states, newest first (may be outdated — re-check before acting on them):\n")
			for _, o := range obs {
				b.WriteString("- ")
				b.WriteString(strings.TrimSpace(o.Content))
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}
	}

	// Extra (dangerous) exec commands this agent has enabled, beyond the safe
	// global allow-list. Listing them tells the model it may run them via exec.
	if len(ag.Config.EnabledCommands) > 0 {
		b.WriteString("## Extra Exec Commands\n")
		b.WriteString("In addition to the standard allow-listed commands, you may run the " +
			"following commands with the exec tool (use them with care — they can " +
			"modify or delete real state):\n")
		for _, c := range ag.Config.EnabledCommands {
			b.WriteString("- ")
			b.WriteString(c)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// Curated knowledge (self-evolution memory), per agent. Capped (env
	// AGENTICGO_KNOWLEDGE_INJECT) and labelled as historical; the agent should
	// use memory_search to find relevant facts rather than treating everything
	// here as current.
	if e.cfg.KnowledgeInject > 0 {
		if knowledge, err := e.store.Knowledge(ctx, ag.Key, e.cfg.KnowledgeInject); err == nil && len(knowledge) > 0 {
			b.WriteString("## Accumulated Knowledge\n")
			b.WriteString("Learnings captured from prior sessions. These are historical and may be " +
				"outdated — use the memory_search tool to look up specifics, and verify " +
				"before relying on them:\n")
			for _, k := range knowledge {
				b.WriteString("- ")
				b.WriteString(strings.TrimSpace(k.Content))
				b.WriteString("\n")
			}
		}
	}

	return strings.TrimSpace(b.String())
}

// pruneRetention applies the configured retention limits for one agent:
// age-based expiry (TTL) and/or a count cap on the newest entries, for both
// observations and self-educated knowledge. Best-effort — failures are
// non-fatal. Each knob at 0 disables that limit. Shared by Run (lazy, per
// running agent) and StartRetentionSweeper (background, all agents).
func (e *Engine) pruneRetention(ctx context.Context, agentKey string) {
	if e.cfg.ObservationTTLDays > 0 || e.cfg.ObservationKeepLatest > 0 {
		var cutoff int64
		if e.cfg.ObservationTTLDays > 0 {
			cutoff = time.Now().AddDate(0, 0, -e.cfg.ObservationTTLDays).Unix()
		}
		_ = e.store.PruneObservations(ctx, agentKey, cutoff, e.cfg.ObservationKeepLatest)
	}
	if e.cfg.KnowledgeTTLDays > 0 || e.cfg.KnowledgeKeepLatest > 0 {
		var cutoff int64 // 0 = no age-based prune
		if e.cfg.KnowledgeTTLDays > 0 {
			cutoff = time.Now().AddDate(0, 0, -e.cfg.KnowledgeTTLDays).Unix()
		}
		_ = e.store.PruneKnowledge(ctx, agentKey, cutoff, e.cfg.KnowledgeKeepLatest)
	}
}

// retentionEnabled reports whether any retention knob is non-zero. When all
// are 0 there is nothing to expire, so neither Run nor the sweeper prunes.
func (e *Engine) retentionEnabled() bool {
	return e.cfg.ObservationTTLDays > 0 || e.cfg.ObservationKeepLatest > 0 ||
		e.cfg.KnowledgeTTLDays > 0 || e.cfg.KnowledgeKeepLatest > 0
}

// StartRetentionSweeper launches a background goroutine that periodically
// applies the retention limits to EVERY agent, so expired knowledge and
// observations are deleted on a schedule even for agents that are idle (the
// lazy per-Run prune only fires when an agent actually runs). It returns
// immediately without starting anything when disabled — either
// RetentionSweepMinutes <= 0, or every retention knob is 0 (in which case
// there is nothing to expire and a sweeper makes no sense). The goroutine
// stops when ctx is cancelled.
func (e *Engine) StartRetentionSweeper(ctx context.Context) {
	if e.cfg.RetentionSweepMinutes <= 0 || !e.retentionEnabled() {
		return
	}
	interval := time.Duration(e.cfg.RetentionSweepMinutes) * time.Minute
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				agents, err := e.agents.List()
				if err != nil {
					continue
				}
				for _, ag := range agents {
					e.pruneRetention(ctx, ag.Key)
				}
			}
		}
	}()
}

// Run processes one user message for a given agent+session, streaming events
// to emit. If providerName is non-empty and a provider lookup is configured,
// that provider is used instead of the default. images are base64 data-URLs
// attached to this user message, included only when the effective provider is
// vision-capable (otherwise they are dropped, so a non-vision model never
// errors). It returns the final assistant reply. emit may be nil.
func (e *Engine) Run(ctx context.Context, agentKey, session, userMessage, providerName string, images []string, emit func(Event)) (string, error) {
	unlock, err := e.lockConversation(ctx, conversation{agentKey, session})
	if err != nil {
		return "", err
	}
	defer unlock()
	if emit == nil {
		emit = func(Event) {}
	}

	ag, err := e.agents.Get(agentKey)
	if err != nil {
		return "", err
	}

	// Provider precedence: per-request override > agent's configured provider
	// > engine default.
	provider, err := e.resolveProvider(ag, providerName)
	if err != nil {
		return "", err
	}

	// Only forward images when the effective model can actually see them.
	vision := e.EffectiveVision(ag, providerName)
	if !vision {
		images = nil
	}

	// Retention: prune stale observations so recurring agents don't accumulate
	// misleading historical state, and bound self-educated knowledge so a
	// long-lived agent can't grow it without limit. Best-effort; failures are
	// non-fatal. Each knob at 0 disables that limit.
	e.pruneRetention(ctx, agentKey)

	// Persist the user turn.
	if err := e.store.AppendMessage(ctx, agentKey, session, string(llm.RoleUser), userMessage); err != nil {
		return "", fmt.Errorf("save user message: %w", err)
	}

	// Load recent history for this agent+session.
	history, err := e.store.Messages(ctx, agentKey, session, 40)
	if err != nil {
		return "", fmt.Errorf("load history: %w", err)
	}

	messages := make([]llm.Message, 0, len(history)+1)
	messages = append(messages, llm.Message{
		Role:    llm.RoleSystem,
		Content: e.buildSystemPrompt(ctx, ag),
	})
	replayed, err := replayHistory(history)
	if err != nil {
		return "", err
	}
	if len(replayed) > 0 && replayed[len(replayed)-1].Role == llm.RoleUser {
		replayed[len(replayed)-1].Images = images
	}
	messages = append(messages, replayed...)

	// Build the per-run tool registry: the single source of truth for both the
	// tool specs offered to the model and the dispatch of its tool calls.
	// It composes the always-on core memory/docs tools (scoped to this agent),
	// the built-in fs/exec tools (jailed to the agent's own workspace), and the
	// MCP tools this agent has enabled.
	reg, sink, err := e.runRegistry(ag)
	if err != nil {
		return "", fmt.Errorf("build tool registry: %w", err)
	}
	specs := reg.Specs()

	for iter := 0; iter < e.cfg.MaxAgentIterations; iter++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		req := llm.ChatRequest{
			Model:       provider.Model(),
			Messages:    messages,
			Tools:       specs,
			Temperature: ag.Config.Temperature,
			MaxTokens:   ag.Config.MaxTokens,
			Stream:      true,
		}
		if ag.Config.Model != nil && *ag.Config.Model != "" {
			req.Model = *ag.Config.Model
		}

		var turnText strings.Builder
		assistantMsg, err := provider.ChatCompletion(ctx, req, func(d llm.Delta) error {
			if d.Content != "" {
				turnText.WriteString(d.Content)
				emit(Event{Kind: "text", Text: d.Content})
			}
			return nil
		})
		if err != nil {
			emit(Event{Kind: "error", Err: err})
			return "", fmt.Errorf("llm completion: %w", err)
		}

		// No tool calls => we have the final answer.
		if len(assistantMsg.ToolCalls) == 0 {
			finalText := assistantMsg.Content
			if finalText == "" {
				finalText = turnText.String()
			}
			if err := e.store.AppendMessage(ctx, agentKey, session, string(llm.RoleAssistant), finalText); err != nil {
				return "", fmt.Errorf("save assistant reply: %w", err)
			}
			emit(Event{Kind: "done"})
			return finalText, nil
		}

		// Serialize tool calls for persistence.
		toolCallsJSON, err := json.Marshal(assistantMsg.ToolCalls)
		if err != nil {
			emit(Event{Kind: "error", Err: err})
			return "", fmt.Errorf("marshal tool calls: %w", err)
		}

		// Persist the assistant turn that requested tools (with tool_calls metadata).
		if err := e.store.AppendMessageWithToolCalls(ctx, agentKey, session,
			string(llm.RoleAssistant), assistantMsg.Content, string(toolCallsJSON), "", ""); err != nil {
			return "", fmt.Errorf("save assistant tool calls: %w", err)
		}

		// Record the assistant turn that requested tools.
		messages = append(messages, assistantMsg)

		// Execute each requested tool and append results.
		for _, tc := range assistantMsg.ToolCalls {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			emit(Event{Kind: "tool_call", ToolName: tc.Name, ToolArgs: tc.Arguments})

			var result string
			var callErr error
			if tc.Name == "fetch_agent_image" && !vision {
				callErr = fmt.Errorf("the effective model does not support images")
			} else {
				result, callErr = reg.Call(ctx, tc.Name, json.RawMessage(tc.Arguments))
			}
			if callErr != nil {
				result = "error: " + callErr.Error()
			}
			emit(Event{Kind: "tool_result", ToolName: tc.Name, ToolResult: result})

			// Persist the tool result as a separate message row.
			if err := e.store.AppendMessageWithToolCalls(ctx, agentKey, session,
				string(llm.RoleTool), result, "", tc.ID, tc.Name); err != nil {
				return "", fmt.Errorf("save tool result: %w", err)
			}

			messages = append(messages, llm.Message{
				Role:       llm.RoleTool,
				Content:    result,
				ToolCallID: tc.ID,
				Name:       tc.Name,
			})
		}

		// Drain any images fetched by fetch_agent_image and inject them as a
		// user message so the model can actually see them in the next LLM turn.
		if imgs := sink.DrainImages(); vision && len(imgs) > 0 {
			messages = append(messages, llm.Message{
				Role:    llm.RoleUser,
				Content: "Describe what you see in the following image(s).",
				Images:  imgs,
			})
		}

		// Loop: let the model observe tool results and continue.
	}

	return "", fmt.Errorf("agent reached max iterations (%d) without a final answer", e.cfg.MaxAgentIterations)
}

// replayHistory preserves tool metadata and drops incomplete tool exchanges
// caused by a history-window boundary or an interrupted run.
func replayHistory(history []store.Message) ([]llm.Message, error) {
	out := make([]llm.Message, 0, len(history))
	for i := 0; i < len(history); i++ {
		m := history[i]
		if m.Role == string(llm.RoleTool) {
			continue
		}
		msg := llm.Message{Role: llm.Role(m.Role), Content: m.Content, Name: m.Name, ToolCallID: m.ToolCallID}
		if m.ToolCalls != "" {
			if err := json.Unmarshal([]byte(m.ToolCalls), &msg.ToolCalls); err != nil {
				return nil, fmt.Errorf("decode stored tool calls: %w", err)
			}
		}
		if len(msg.ToolCalls) > 0 {
			pending := make(map[string]bool, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				pending[tc.ID] = true
			}
			results := []llm.Message{}
			valid := len(pending) == len(msg.ToolCalls) && !pending[""]
			for i+1 < len(history) && history[i+1].Role == string(llm.RoleTool) {
				i++
				r := history[i]
				if !pending[r.ToolCallID] {
					valid = false
				}
				delete(pending, r.ToolCallID)
				results = append(results, llm.Message{Role: llm.RoleTool, Content: r.Content, ToolCallID: r.ToolCallID, Name: r.Name})
			}
			if !valid || len(pending) > 0 {
				continue
			}
			out = append(out, msg)
			out = append(out, results...)
		} else {
			out = append(out, msg)
		}
	}
	return out, nil
}

// runRegistry builds the per-run tool registry for an agent: the single
// source of truth for both the tool specs offered to the model and the
// dispatch of tool calls. It composes, in one place:
//
//   - the always-on core memory/docs tools (scoped to this agent),
//   - the built-in fs tools (read_file/write_file/list_files) and exec,
//     jailed to the agent's own workspace and gated by the global
//     AGENTICGO_TOOL_ALLOWLIST; exec's allow-list additionally includes the
//     agent's enabled extra (dangerous) commands,
//   - the MCP tools this agent has enabled (config.json enabled_tools),
//     soft-failing per call when their server is unreachable.
//
// Building this per run means every tool is constructed once (not per call)
// and specs can never drift from dispatch, which the old callTool switch
// allowed. Tools absent from the registry are invisible to the model and
// rejected by Registry.Call, so per-agent gating is enforced by construction.
func (e *Engine) runRegistry(ag *agents.Agent) (*tools.Registry, *imageSink, error) {
	// The registry itself allows everything registered into it; gating happens
	// when composing it (below). This keeps the always-on core tools and the
	// per-agent MCP tools out of the global AGENTICGO_TOOL_ALLOWLIST, which
	// only governs the built-in fs/exec tools.
	registry := tools.NewRegistry(nil)

	// Core memory/knowledge tools: always on, scoped to the calling agent.
	registry.Register(tools.AdaptFuncTool(tools.NewMemorySearch(e.store, ag.Key)))
	registry.Register(tools.AdaptFuncTool(tools.NewMemorySave(e.store, ag.Key)))
	registry.Register(tools.AdaptFuncTool(tools.NewRecordObservation(e.store, ag.Key)))
	registry.Register(tools.AdaptFuncTool(tools.NewSearchDocs(e.docSearcher(ag.Key))))
	registry.Register(tools.AdaptFuncTool(tools.NewReadDoc(e.docReader(ag.Key))))
	registry.Register(tools.AdaptFuncTool(tools.NewListAgentImages(e.imageLister(ag.Key))))

	// Image sink: collects images fetched by fetch_agent_image so the engine
	// can inject them into the next LLM turn (the model can actually see them,
	// not just the data-URL string).
	sink := newImageSink()
	registry.Register(tools.AdaptFuncTool(tools.NewFetchAgentImage(e.imageFetcher(ag.Key), sink)))

	// Built-in filesystem tools, jailed to the agent's workspace. The global
	// AGENTICGO_TOOL_ALLOWLIST is the ceiling; the agent may narrow it further
	// (config.json enabled_builtin_tools; nil = inherit the global allow-list).
	// Narrowing can only remove tools, never add ones the ceiling doesn't permit.
	builtin := e.builtinAllowed(ag)
	workspace, err := e.agents.WorkspaceDir(ag.Key)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve agent workspace: %w", err)
	}
	if builtin["read_file"] {
		t, err := tools.NewReadFile(workspace)
		if err != nil {
			return nil, nil, err
		}
		registry.Register(tools.AdaptFuncTool(t))
	}
	if builtin["write_file"] {
		t, err := tools.NewWriteFile(workspace)
		if err != nil {
			return nil, nil, err
		}
		registry.Register(tools.AdaptFuncTool(t))
	}
	if builtin["list_files"] {
		t, err := tools.NewListFiles(workspace)
		if err != nil {
			return nil, nil, err
		}
		registry.Register(tools.AdaptFuncTool(t))
	}

	// Exec: safe commands from the global AGENTICGO_EXEC_ALLOWLIST plus the
	// agent's enabled extra (dangerous) commands, jailed to the workspace.
	// The extras are named in the tool description so the model can see which
	// dangerous commands it may run.
	if builtin["exec"] {
		execAllow := append([]string{}, e.cfg.ExecAllowList...)
		var execExtra []string
		for _, cmd := range e.cfg.ExtraExecCommands {
			if slices.Contains(ag.Config.EnabledCommands, cmd) {
				execAllow = append(execAllow, cmd)
				execExtra = append(execExtra, cmd)
			}
		}
		registry.Register(tools.AdaptFuncTool(tools.NewExecWithExtra(workspace, execAllow, execExtra)))
	}

	// Custom (MCP) tools enabled for this agent. Tools whose server is not
	// connected are silently skipped — they become visible once the server is
	// connected and the tool is discovered. A tool that fails at call time
	// returns its error as a string the model can read (soft-fail per tool).
	if e.mcp != nil {
		for _, name := range ag.Config.EnabledTools {
			t, ok := e.mcp.Lookup(name)
			if !ok {
				continue // not discovered (server offline or tool removed)
			}
			registry.Register(newMCPTool(name, t, e.mcp))
		}
	}

	return registry, sink, nil
}

// observationInject resolves how many recent observations to inject into the
// agent's system prompt: the agent's own override wins, else the global
// AGENTICGO_OBSERVATION_INJECT default. 0 disables injection.
func (e *Engine) observationInject(ag *agents.Agent) int {
	if ag.Config.ObservationInject != nil {
		return *ag.Config.ObservationInject
	}
	return e.cfg.ObservationInject
}

// builtinAllowed resolves which built-in tools the agent may use: the global
// AGENTICGO_TOOL_ALLOWLIST ceiling, narrowed by the agent's
// enabled_builtin_tools when that override is set (nil = inherit). Narrowing
// can only remove tools, never add ones the ceiling does not permit.
func (e *Engine) builtinAllowed(ag *agents.Agent) map[string]bool {
	ceiling := map[string]bool{}
	for _, n := range e.cfg.ToolAllowList {
		ceiling[n] = true
	}
	permitted := func(n string) bool { return len(ceiling) == 0 || ceiling[n] }

	if ag.Config.EnabledBuiltinTools == nil {
		// Inherit the global ceiling.
		out := map[string]bool{}
		for _, n := range []string{"read_file", "write_file", "list_files", "exec"} {
			out[n] = permitted(n)
		}
		return out
	}
	out := map[string]bool{}
	for _, n := range *ag.Config.EnabledBuiltinTools {
		if permitted(n) {
			out[n] = true
		}
	}
	return out
}

// mcpTool adapts one discovered MCP tool to the tools.Tool interface so it
// can live in the per-run registry alongside the built-in and core tools.
type mcpTool struct {
	name   string // namespaced: mcp_<server>_<tool>
	desc   string
	schema map[string]any
	mgr    *mcp.Manager
}

func newMCPTool(name string, info mcp.ToolInfo, mgr *mcp.Manager) tools.Tool {
	desc := info.Description
	if desc == "" {
		desc = "Tool from MCP server " + info.Server
	}
	return &mcpTool{name: name, desc: desc, schema: info.Schema, mgr: mgr}
}

func (t *mcpTool) Name() string               { return t.name }
func (t *mcpTool) Description() string        { return t.desc }
func (t *mcpTool) Parameters() map[string]any { return t.schema }

// Call forwards to the MCP manager, which lazily reconnects a configured but
// disconnected server. Errors are returned for the engine loop to wrap into
// tool-result strings, so a dead server never breaks the chat.
func (t *mcpTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	return t.mgr.CallTool(ctx, t.name, args)
}

// resolveProvider picks the provider for a run: a per-request override wins,
// then the agent's configured provider (if any), then the provider store's
// default. The store must be wired (SetProviderLookup) before chat runs.
func (e *Engine) resolveProvider(ag *agents.Agent, perRequest string) (llm.Provider, error) {
	if e.providers == nil {
		return nil, fmt.Errorf("no provider lookup configured")
	}
	name := perRequest
	if name == "" && ag.Config.Provider != nil {
		name = *ag.Config.Provider
	}
	// Empty name resolves to the store's default provider.
	return e.providers.GetLLM(name)
}

// EffectiveVision reports whether the agent may attach images, given an
// optional per-request provider override. Precedence mirrors resolveProvider:
// the agent's explicit Vision override wins; otherwise the resolved provider's
// Vision flag. The env-built default provider (no lookup) reports false unless
// the agent overrides it.
func (e *Engine) EffectiveVision(ag *agents.Agent, perRequest string) bool {
	if ag.Config.Vision != nil {
		return *ag.Config.Vision
	}
	vl, ok := e.providers.(VisionLookup)
	if !ok {
		return false
	}
	name := perRequest
	if name == "" && ag.Config.Provider != nil {
		name = *ag.Config.Provider
	}
	return vl.VisionCapable(name)
}

// docSearcher adapts store.SearchDocs into a tools.DocSearcher for an agent.
// Returns "id — title" lines so the agent can follow up with read_doc.
func (e *Engine) docSearcher(agentKey string) tools.DocSearcher {
	return func(ctx context.Context, query string, limit int) ([]string, error) {
		docs, err := e.store.SearchDocs(ctx, agentKey, query, limit)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(docs))
		for _, d := range docs {
			out = append(out, fmt.Sprintf("%d — %s", d.ID, strings.TrimSpace(d.Title)))
		}
		return out, nil
	}
}

// docReader adapts store.GetKnowledgeDocForAgent into a tools.DocReader for
// an agent — scoped so one agent can never read another agent's documents.
func (e *Engine) docReader(agentKey string) tools.DocReader {
	return func(ctx context.Context, id int64) (string, string, error) {
		d, err := e.store.GetKnowledgeDocForAgent(ctx, id, agentKey)
		if err != nil {
			return "", "", err
		}
		return d.Title, d.Content, nil
	}
}

// imageLister adapts agents.ListImages into a tools.ImageLister for an agent.
// Returns just the filenames so the agent can follow up with fetch_agent_image.
func (e *Engine) imageLister(agentKey string) tools.ImageLister {
	return func(ctx context.Context) ([]string, error) {
		images, err := e.agents.ListImages(agentKey)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(images))
		for _, im := range images {
			names = append(names, im.Name)
		}
		return names, nil
	}
}

// imageFetcher adapts agents.ReadImage into a tools.ImageFetcher for an agent.
// It reads the raw bytes, converts them to a base64 data-URL, and returns it.
func (e *Engine) imageFetcher(agentKey string) tools.ImageFetcher {
	return func(ctx context.Context, name string) (string, error) {
		data, mime, err := e.agents.ReadImage(agentKey, name)
		if err != nil {
			return "", err
		}
		return encodeDataURL(mime, data), nil
	}
}

// imageSink is a thread-safe buffer that collects image data-URLs fetched by
// the fetch_agent_image tool during an iteration. After all tool calls complete,
// the engine drains the buffer and injects the images into the next LLM turn
// so the model can actually see them (not just the data-URL string).
type imageSink struct {
	mu   sync.Mutex
	imgs []string
}

func newImageSink() *imageSink {
	return &imageSink{}
}

func (s *imageSink) AddImage(dataURL string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.imgs = append(s.imgs, dataURL)
}

func (s *imageSink) DrainImages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	imgs := s.imgs
	s.imgs = nil
	return imgs
}

// Evolve summarizes the session and appends learnings to the agent's knowledge
// store. This is the simplified self-evolution: a background pass extracts
// durable facts/preferences from the transcript for future prompts. It uses the
// same provider resolution as Run (agent config > provider store default).
func (e *Engine) Evolve(ctx context.Context, agentKey, session string) error {
	unlock, err := e.lockConversation(ctx, conversation{agentKey, session})
	if err != nil {
		return err
	}
	defer unlock()
	history, err := e.store.Messages(ctx, agentKey, session, 60)
	if err != nil || len(history) < 4 {
		return err // not enough to learn from
	}

	ag, err := e.agents.Get(agentKey)
	if err != nil {
		return err
	}
	provider, err := e.resolveProvider(ag, "")
	if err != nil {
		return fmt.Errorf("resolve provider: %w", err)
	}

	var transcript strings.Builder
	for _, m := range history {
		if m.ToolCalls != "" {
			// Assistant turn that invoked tools — include the tool calls for context.
			fmt.Fprintf(&transcript, "%s: %s [tool_calls: %s]\n", m.Role, m.Content, m.ToolCalls)
		} else if m.ToolCallID != "" {
			// Tool result — include the tool name and call ID for context.
			fmt.Fprintf(&transcript, "%s [%s]: %s\n", m.Role, m.Name, m.Content)
		} else {
			fmt.Fprintf(&transcript, "%s: %s\n", m.Role, m.Content)
		}
	}

	prompt := "From the following conversation, extract at most 3 durable, reusable facts, " +
		"user preferences, or lessons worth remembering for future sessions. " +
		"Write each as a single concise line prefixed with '- '. If nothing is worth " +
		"remembering, reply with exactly: NONE\n\n" + transcript.String()

	req := llm.ChatRequest{
		Model: provider.Model(),
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "You extract concise, reusable learnings from conversations."},
			{Role: llm.RoleUser, Content: prompt},
		},
		Stream: false,
	}
	if ag.Config.Model != nil && *ag.Config.Model != "" {
		req.Model = *ag.Config.Model
	}

	// Use a non-streaming call via the streaming API by discarding deltas.
	msg, err := provider.ChatCompletion(ctx, req, func(llm.Delta) error { return nil })
	if err != nil {
		return err
	}

	content := strings.TrimSpace(msg.Content)
	if content == "" || content == "NONE" {
		return nil
	}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "- "))
		if line == "" || line == "NONE" {
			continue
		}
		// A repeat extraction of an already-known fact is a no-op, not a
		// failure: skip it and keep saving the remaining new learnings.
		// Without this, one duplicate would abort the whole pass and drop
		// any new facts listed after it.
		if err := e.store.AddKnowledge(ctx, agentKey, line); err != nil && !errors.Is(err, store.ErrKnowledgeDuplicate) {
			return err
		}
	}
	return nil
}

// encodeDataURL converts raw image bytes into a base64 data-URL with the
// given MIME type, matching the format the LLM provider expects.
func encodeDataURL(mime string, data []byte) string {
	return fmt.Sprintf("data:%s;base64,%s", mime, base64.StdEncoding.EncodeToString(data))
}
