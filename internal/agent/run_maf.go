package agent

// run_maf.go is the agent execution path, built on the Microsoft Agent
// Framework (github.com/microsoft/agent-framework-go):
//
//   - the composed system prompt becomes AgentConfig.Instructions,
//   - the per-run tool set (runTools output — core memory/docs tools,
//     jailed built-ins, enabled MCP tools) becomes Config.Tools, so a tool
//     absent from the set is invisible to the model and unreachable by
//     construction (MAF's autocall only invokes what it was handed),
//   - history stays in SQLite via maf.NewHistoryProvider — no MAF sessions
//     own conversation state,
//   - the tool loop runs as toolautocall middleware with
//     MaximumIterationsPerRequest = cfg.MaxAgentIterations; unknown tool
//     calls soft-fail into a "not found" result,
//   - WS events (text / tool_call / tool_result / done) are derived from the
//     ResponseStream, keeping the SPA contract byte-identical,
//   - cancellation is the run context: WS kind:cancel and shutdown propagate
//     through the stream unchanged,
//   - images become DataContent parts on the outgoing user message; MAF bridge
//     middleware retains fetched images throughout the remaining tool rounds.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dkr290/agenticgo/internal/agents"
	"github.com/dkr290/agenticgo/internal/llm"
	"github.com/dkr290/agenticgo/internal/maf"
	mafgent "github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/microsoft/agent-framework-go/provider/openaiprovider"
	"github.com/openai/openai-go/v3"
)

// runMAF executes one user turn through the MAF agent loop. Same signature
// and event contract as the legacy path in Run.
func (e *Engine) runMAF(ctx context.Context, ag *agents.Agent, agentKey, session, userMessage string, prov *llm.OpenAIProvider, images []string, vision bool, emit func(Event)) (string, error) {
	// Build the per-run tool set: the single source of truth for gating. MAF's
	// autocall invokes exactly this slice, so a tool absent from the set is
	// invisible to the model and unreachable at dispatch.
	mafTools, sink, err := e.runTools(ag, vision)
	if err != nil {
		return "", fmt.Errorf("build tool set: %w", err)
	}

	// Model + sampling settings: agent config pins win over provider defaults.
	model := prov.Model()
	if ag.Config.Model != nil && *ag.Config.Model != "" {
		model = *ag.Config.Model
	}

	// The outgoing user message: text plus image parts when vision is on.
	userMsg := maf.UserMessage(userMessage, images)

	mafAgent := maf.NewChatAgent(
		openai.NewClient(prov.RequestOptions()...),
		maf.ChatConfig{
			Name:          ag.Key,
			Model:         model,
			Instructions:  e.buildSystemPrompt(ctx, ag),
			MaxIterations: e.cfg.MaxAgentIterations,
			History:       maf.NewHistoryProvider(e.store, agentKey, session),
			Tools:         mafTools,
			DrainImages:   sink.DrainImages,
		},
	)

	// MAF creates a fresh session; the custom history provider owns SQLite
	// persistence and preserves the original multimodal input.
	opts := []mafgent.Option{mafgent.Stream(true)}
	if params := chatParams(ag); params != nil {
		opts = append(opts, params)
	}

	var finalText strings.Builder
	names := make(map[string]string)
	var runErr error
	for update, err := range mafAgent.Run(ctx, []*message.Message{userMsg}, opts...) {
		if err != nil {
			runErr = err
			break
		}
		if update == nil {
			continue
		}
		// A tool result ends the intermediate assistant message. Stream all
		// narration to the UI, but return only the final answer to REST callers.
		if update.Role == message.RoleTool {
			finalText.Reset()
		}
		for _, c := range update.Contents {
			switch c := c.(type) {
			case *message.TextContent:
				finalText.WriteString(c.Text)
				emit(Event{Kind: "text", Text: c.Text})
			case *message.ErrorContent:
				// MAF represents provider refusals as content rather than Go errors.
				finalText.WriteString(c.Message)
				emit(Event{Kind: "text", Text: c.Message})
			case *message.FunctionCallContent:
				names[c.CallID] = c.Name
				emit(Event{Kind: "tool_call", ToolName: c.Name, ToolArgs: c.Arguments})
			case *message.FunctionResultContent:
				emit(Event{Kind: "tool_result", ToolName: names[c.CallID], ToolResult: mafResultText(c)})
			}
		}
	}
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			emit(Event{Kind: "cancelled"})
			return "", context.Canceled
		}
		emit(Event{Kind: "error", Err: runErr})
		return "", fmt.Errorf("maf run: %w", runErr)
	}
	if err := ctx.Err(); err != nil {
		emit(Event{Kind: "cancelled"})
		return "", err
	}

	emit(Event{Kind: "done"})
	return finalText.String(), nil
}

// chatParams passes per-agent sampling settings through to the Chat
// Completions request when set.
func chatParams(ag *agents.Agent) mafgent.Option {
	if ag.Config.Temperature == nil && ag.Config.MaxTokens == nil {
		return nil
	}
	p := openai.ChatCompletionNewParams{}
	if ag.Config.Temperature != nil {
		p.Temperature = openai.Float(*ag.Config.Temperature)
	}
	if ag.Config.MaxTokens != nil {
		p.MaxTokens = openai.Int(int64(*ag.Config.MaxTokens))
	}
	return openaiprovider.ChatCompletionNewParams(p)
}

// mafResultText flattens a function result for the WS event.
func mafResultText(c *message.FunctionResultContent) string {
	if c.Error != nil {
		return "error: " + c.Error.Error()
	}
	switch r := c.Result.(type) {
	case nil:
		return ""
	case string:
		return r
	case message.Contents:
		return r.Text()
	default:
		return fmt.Sprintf("%v", r)
	}
}

// runOneShotMAF runs a single non-streaming MAF completion with no tools and
// no history provider — used by Evolve, where the transcript is baked into
// the prompt and there is nothing to persist. Returns the reply text.
func runOneShotMAF(ctx context.Context, prov *llm.OpenAIProvider, ag *agents.Agent, instructions, prompt string) (string, error) {
	model := prov.Model()
	if ag.Config.Model != nil && *ag.Config.Model != "" {
		model = *ag.Config.Model
	}
	a := maf.NewChatAgent(
		openai.NewClient(prov.RequestOptions()...),
		maf.ChatConfig{
			Model:        model,
			Instructions: instructions,
		},
	)
	resp, err := a.RunText(ctx, prompt).Collect()
	if err != nil {
		return "", err
	}
	for c := range resp.Contents() {
		if failure, ok := c.(*message.ErrorContent); ok {
			return "", fmt.Errorf("model declined extraction: %s", failure.Message)
		}
	}
	return resp.String(), nil
}
