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

// Engine runs agent-scoped chat loops.
type Engine struct {
	cfg        *config.Config
	llm        llm.Provider
	tools      *tools.Registry
	store      *store.Store
	agents     *agents.Registry
	basePrompt string
}

// New creates an Engine.
func New(cfg *config.Config, p llm.Provider, reg *tools.Registry, st *store.Store, ar *agents.Registry) *Engine {
	return &Engine{cfg: cfg, llm: p, tools: reg, store: st, agents: ar, basePrompt: cfg.SystemPrompt}
}

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

	// Accumulated knowledge (self-evolution memory), per agent.
	if knowledge, err := e.store.Knowledge(ctx, ag.Key, 50); err == nil && len(knowledge) > 0 {
		b.WriteString("## Accumulated Knowledge\n")
		b.WriteString("Facts and learnings captured from prior sessions. Use them when relevant:\n")
		for _, k := range knowledge {
			b.WriteString("- ")
			b.WriteString(strings.TrimSpace(k))
			b.WriteString("\n")
		}
	}

	return strings.TrimSpace(b.String())
}

// Run processes one user message for a given agent+session, streaming events
// to emit. It returns the final assistant reply. emit may be nil.
func (e *Engine) Run(ctx context.Context, agentKey, session, userMessage string, emit func(Event)) (string, error) {
	if emit == nil {
		emit = func(Event) {}
	}

	ag, err := e.agents.Get(agentKey)
	if err != nil {
		return "", err
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

	specs := e.tools.Specs()

	for iter := 0; iter < e.cfg.MaxAgentIterations; iter++ {
		req := llm.ChatRequest{
			Model:    e.cfg.LLMModel,
			Messages: messages,
			Tools:    specs,
			Stream:   true,
		}

		var turnText strings.Builder
		assistantMsg, err := e.llm.ChatCompletion(ctx, req, func(d llm.Delta) error {
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

			result, callErr := e.tools.Call(ctx, tc.Name, json.RawMessage(tc.Arguments))
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
