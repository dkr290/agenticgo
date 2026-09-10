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
	"strings"
	"time"

	"github.com/dkr290/agenticgo/internal/agents"
	"github.com/dkr290/agenticgo/internal/config"
	"github.com/dkr290/agenticgo/internal/llm"
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

// Engine runs agent-scoped chat loops.
type Engine struct {
	cfg        *config.Config
	llm        llm.Provider
	tools      *tools.Registry
	store      *store.Store
	agents     *agents.Registry
	providers  ProviderLookup // optional: per-request provider override
	basePrompt string
}

// New creates an Engine.
func New(cfg *config.Config, p llm.Provider, reg *tools.Registry, st *store.Store, ar *agents.Registry) *Engine {
	return &Engine{cfg: cfg, llm: p, tools: reg, store: st, agents: ar, basePrompt: cfg.SystemPrompt}
}

// SetProviderLookup enables per-request provider overrides (used by the
// Providers UI; empty provider name falls back to the default provider).
func (e *Engine) SetProviderLookup(pl ProviderLookup) { e.providers = pl }

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

	// Skills.
	if sd, err := e.agents.SkillsDir(ag.Key); err == nil {
		if sks, err := skills.Load(sd); err == nil && len(sks) > 0 {
			if p := skills.Prompt(sks, nil); p != "" {
				b.WriteString(p)
				b.WriteString("\n\n")
			}
		}
	}

	// Knowledge-base documents (uploaded reference material). List titles so the
	// agent knows what's available; full content is fetched via search_docs.
	if docs, err := e.store.ListKnowledgeDocs(ctx, ag.Key); err == nil && len(docs) > 0 {
		b.WriteString("## Knowledge Base\n")
		b.WriteString("Reference documents are available. Use the search_docs tool to find " +
			"relevant content before answering on these topics:\n")
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
			b.WriteString(strings.TrimSpace(k))
			b.WriteString("\n")
		}
	}

	return strings.TrimSpace(b.String())
}

// Run processes one user message for a given agent+session, streaming events
// to emit. If providerName is non-empty and a provider lookup is configured,
// that provider is used instead of the default. It returns the final
// assistant reply. emit may be nil.
func (e *Engine) Run(ctx context.Context, agentKey, session, userMessage, providerName string, emit func(Event)) (string, error) {
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
	for _, m := range history {
		messages = append(messages, llm.Message{
			Role:    llm.Role(m.Role),
			Content: m.Content,
		})
	}

	// Give this run access to the per-agent memory tools (search curated
	// knowledge + knowledge-base docs, record timestamped observations).
	specs := append(e.tools.Specs(),
		toolSpec(tools.NewMemorySearch(e.store, ag.Key)),
		toolSpec(tools.NewRecordObservation(e.store, ag.Key)),
		toolSpec(tools.NewSearchDocs(e.docSearcher(ag.Key))),
	)

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

			result, callErr := e.callTool(ctx, ag.Key, tc.Name, json.RawMessage(tc.Arguments))
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

// toolSpec renders a single tool as an OpenAI-compatible spec.
func toolSpec(t tools.Tool) llm.ToolSpec {
	return llm.ToolSpec{
		Type: "function",
		Function: llm.FunctionSpec{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		},
	}
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

// docSearcher adapts store.SearchDocs into a tools.DocSearcher for an agent.
func (e *Engine) docSearcher(agentKey string) tools.DocSearcher {
	return func(ctx context.Context, query string, limit int) ([]string, error) {
		docs, err := e.store.SearchDocs(ctx, agentKey, query, limit)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(docs))
		for _, d := range docs {
			out = append(out, d.Title)
		}
		return out, nil
	}
}

// callTool routes a tool call: per-agent memory tools are constructed on the
// fly (scoped to the agent); everything else goes to the shared registry.
func (e *Engine) callTool(ctx context.Context, agentKey, name string, args json.RawMessage) (string, error) {
	switch name {
	case "memory_search":
		return tools.NewMemorySearch(e.store, agentKey).Call(ctx, args)
	case "record_observation":
		return tools.NewRecordObservation(e.store, agentKey).Call(ctx, args)
	case "search_docs":
		return tools.NewSearchDocs(e.docSearcher(agentKey)).Call(ctx, args)
	}
	return e.tools.Call(ctx, name, args)
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
