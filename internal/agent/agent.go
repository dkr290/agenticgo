// Package agent implements the tool-use agent loop: send messages to the LLM,
// execute any requested tool calls, feed results back, and repeat until the
// model produces a final answer (or the iteration cap is hit).
//
// The Engine resolves named agents (their context files + skills + per-agent
// knowledge) and runs the loop scoped to that agent and session.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
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
	llm        llm.Provider
	tools      *tools.Registry
	store      *store.Store
	agents     *agents.Registry
	providers  ProviderLookup // optional: per-request provider override
	mcp        *mcp.Manager   // optional: custom (MCP) tools, enabled per agent
	basePrompt string
}

// New creates an Engine.
func New(cfg *config.Config, p llm.Provider, reg *tools.Registry, st *store.Store, ar *agents.Registry) *Engine {
	return &Engine{cfg: cfg, llm: p, tools: reg, store: st, agents: ar, basePrompt: cfg.SystemPrompt}
}

// SetProviderLookup enables per-request provider overrides (used by the
// Providers UI; empty provider name falls back to the default provider).
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

	// Latest observation (e.g. from a recurring monitor). Only the most recent
	// snapshot is injected — stale observations are pruned and searchable, never
	// bulk-loaded, so outdated state does not mislead the model.
	if obs, err := e.store.LatestObservation(ctx, ag.Key); err == nil && obs != nil {
		b.WriteString("## Latest Observation\n")
		b.WriteString("Most recent recorded state (may be outdated — re-check before acting on it):\n")
		b.WriteString(strings.TrimSpace(obs.Content))
		b.WriteString("\n\n")
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

	// Curated knowledge (self-evolution memory), per agent. Capped and labelled
	// as historical; the agent should use memory_search to find relevant facts
	// rather than treating everything here as current.
	if knowledge, err := e.store.Knowledge(ctx, ag.Key, 25); err == nil && len(knowledge) > 0 {
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

	return strings.TrimSpace(b.String())
}

// Run processes one user message for a given agent+session, streaming events
// to emit. If providerName is non-empty and a provider lookup is configured,
// that provider is used instead of the default. images are base64 data-URLs
// attached to this user message, included only when the effective provider is
// vision-capable (otherwise they are dropped, so a non-vision model never
// errors). It returns the final assistant reply. emit may be nil.
func (e *Engine) Run(ctx context.Context, agentKey, session, userMessage, providerName string, images []string, emit func(Event)) (string, error) {
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
	if !e.EffectiveVision(ag, providerName) {
		images = nil
	}

	// Retention: prune stale observations so recurring agents don't accumulate
	// misleading historical state. Best-effort; failures are non-fatal.
	if e.cfg.ObservationTTLDays > 0 || e.cfg.ObservationKeepLatest > 0 {
		cutoff := time.Now().AddDate(0, 0, -e.cfg.ObservationTTLDays).Unix()
		_ = e.store.PruneObservations(ctx, agentKey, cutoff, e.cfg.ObservationKeepLatest)
	}

	// Persist the user turn.
	if err := e.store.AppendMessage(ctx, agentKey, session, string(llm.RoleUser), userMessage); err != nil {
		emit(Event{Kind: "error", Err: err})
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
	for i, m := range history {
		msg := llm.Message{
			Role:    llm.Role(m.Role),
			Content: m.Content,
		}
		// Attach images to the current user turn (the last message) only.
		if i == len(history)-1 && len(images) > 0 && m.Role == string(llm.RoleUser) {
			msg.Images = images
		}
		messages = append(messages, msg)
	}

	// Build the per-run tool registry: the single source of truth for both the
	// tool specs offered to the model and the dispatch of its tool calls.
	// It composes the always-on core memory/docs tools (scoped to this agent),
	// the built-in fs/exec tools (jailed to the agent's own workspace), and the
	// MCP tools this agent has enabled.
	reg, err := e.runRegistry(ag)
	if err != nil {
		return "", fmt.Errorf("build tool registry: %w", err)
	}
	specs := reg.Specs()

	for iter := 0; iter < e.cfg.MaxAgentIterations; iter++ {
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
				emit(Event{Kind: "error", Err: err})
			}
			emit(Event{Kind: "done"})
			return finalText, nil
		}

		// Record the assistant turn that requested tools.
		messages = append(messages, assistantMsg)

		// Execute each requested tool and append results.
		for _, tc := range assistantMsg.ToolCalls {
			emit(Event{Kind: "tool_call", ToolName: tc.Name, ToolArgs: tc.Arguments})

			result, callErr := reg.Call(ctx, tc.Name, json.RawMessage(tc.Arguments))
			if callErr != nil {
				result = "error: " + callErr.Error()
			}
			emit(Event{Kind: "tool_result", ToolName: tc.Name, ToolResult: result})

			messages = append(messages, llm.Message{
				Role:       llm.RoleTool,
				Content:    result,
				ToolCallID: tc.ID,
				Name:       tc.Name,
			})
		}
		// Loop: let the model observe tool results and continue.
	}

	return "", fmt.Errorf("agent reached max iterations (%d) without a final answer", e.cfg.MaxAgentIterations)
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
func (e *Engine) runRegistry(ag *agents.Agent) (*tools.Registry, error) {
	// The registry itself allows everything registered into it; gating happens
	// when composing it (below). This keeps the always-on core tools and the
	// per-agent MCP tools out of the global AGENTICGO_TOOL_ALLOWLIST, which
	// only governs the built-in fs/exec tools.
	registry := tools.NewRegistry(nil)

	// Core memory/knowledge tools: always on, scoped to the calling agent.
	registry.Register(tools.NewMemorySearch(e.store, ag.Key))
	registry.Register(tools.NewMemorySave(e.store, ag.Key))
	registry.Register(tools.NewRecordObservation(e.store, ag.Key))
	registry.Register(tools.NewSearchDocs(e.docSearcher(ag.Key)))
	registry.Register(tools.NewReadDoc(e.docReader(ag.Key)))

	// Built-in filesystem tools, jailed to the agent's workspace. The global
	// AGENTICGO_TOOL_ALLOWLIST is the ceiling; the agent may narrow it further
	// (config.json enabled_builtin_tools; nil = inherit the global allow-list).
	// Narrowing can only remove tools, never add ones the ceiling doesn't permit.
	builtin := e.builtinAllowed(ag)
	workspace := e.workspaceFor(ag.Key)
	if builtin["read_file"] {
		t, err := tools.NewReadFile(workspace)
		if err != nil {
			return nil, err
		}
		registry.Register(t)
	}
	if builtin["write_file"] {
		t, err := tools.NewWriteFile(workspace)
		if err != nil {
			return nil, err
		}
		registry.Register(t)
	}
	if builtin["list_files"] {
		t, err := tools.NewListFiles(workspace)
		if err != nil {
			return nil, err
		}
		registry.Register(t)
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
		registry.Register(tools.NewExecWithExtra(workspace, execAllow, execExtra))
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

	return registry, nil
}

// workspaceFor resolves the agent's workspace jail, falling back to the
// global workspace when the per-agent one cannot be resolved.
func (e *Engine) workspaceFor(agentKey string) string {
	workspace, err := e.agents.WorkspaceDir(agentKey)
	if err != nil {
		return e.cfg.WorkspaceDir
	}
	return workspace
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
// then the agent's configured provider (if any), then the engine default.
// If no named provider is requested and no provider lookup is configured,
// the default engine provider is returned.
func (e *Engine) resolveProvider(ag *agents.Agent, perRequest string) (llm.Provider, error) {
	name := perRequest
	if name == "" && ag.Config.Provider != nil {
		name = *ag.Config.Provider
	}
	if name == "" || e.providers == nil {
		return e.llm, nil
	}
	p, err := e.providers.GetLLM(name)
	if err != nil {
		return nil, err
	}
	return p, nil
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

// Evolve summarizes the session and appends learnings to the agent's knowledge
// store. This is the simplified self-evolution: a background pass extracts
// durable facts/preferences from the transcript for future prompts.
func (e *Engine) Evolve(ctx context.Context, agentKey, session string) error {
	history, err := e.store.Messages(ctx, agentKey, session, 60)
	if err != nil || len(history) < 4 {
		return err // not enough to learn from
	}

	var transcript strings.Builder
	for _, m := range history {
		fmt.Fprintf(&transcript, "%s: %s\n", m.Role, m.Content)
	}

	prompt := "From the following conversation, extract at most 3 durable, reusable facts, " +
		"user preferences, or lessons worth remembering for future sessions. " +
		"Write each as a single concise line prefixed with '- '. If nothing is worth " +
		"remembering, reply with exactly: NONE\n\n" + transcript.String()

	req := llm.ChatRequest{
		Model: e.cfg.LLMModel,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "You extract concise, reusable learnings from conversations."},
			{Role: llm.RoleUser, Content: prompt},
		},
		Stream: false,
	}

	// Use a non-streaming call via the streaming API by discarding deltas.
	msg, err := e.llm.ChatCompletion(ctx, req, func(llm.Delta) error { return nil })
	if err != nil {
		return err
	}

	content := strings.TrimSpace(msg.Content)
	if content == "" || content == "NONE" {
		return nil
	}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "- "))
		if line != "" && line != "NONE" {
			if err := e.store.AddKnowledge(ctx, agentKey, line); err != nil {
				return err
			}
		}
	}
	return nil
}
