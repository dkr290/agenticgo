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
//   - images become DataContent parts on the outgoing user message; the
//     fetch_agent_image imageSink is drained between provider calls via
//     agent.MessageInjector (the model sees the image, not a data-URL string).

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dkr290/agenticgo/internal/agents"
	"github.com/dkr290/agenticgo/internal/llm"
	"github.com/dkr290/agenticgo/internal/maf"
	mafgent "github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/agent/harness/toolautocall"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/microsoft/agent-framework-go/provider/openaiprovider"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// maxConsecutiveToolErrors mirrors the toolautocall default the legacy engine
// effectively had (unlimited retries within the iteration cap); 0 here would
// abort the run on the first failing tool, which is NOT the legacy behavior —
// tool errors are results the model reads and recovers from.
var maxConsecutiveToolErrors = 100

// runMAF executes one user turn through the MAF agent loop. Same signature
// and event contract as the legacy path in Run.
func (e *Engine) runMAF(ctx context.Context, ag *agents.Agent, agentKey, session, userMessage string, prov *llm.OpenAIProvider, images []string, vision bool, emit func(Event)) (string, error) {
	// Build the per-run tool set: the single source of truth for gating. MAF's
	// autocall invokes exactly this slice, so a tool absent from the set is
	// invisible to the model and unreachable at dispatch.
	mafTools, sink, err := e.runTools(ag)
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

	// Image injection between provider calls: fetch_agent_image fills the
	// sink; the injector drains it into the next provider call as a user
	// message with the image parts attached.
	injector := &mafgent.MessageInjector{}
	drain := func(s *mafgent.Session) {
		if !vision {
			return
		}
		if imgs := sink.DrainImages(); len(imgs) > 0 {
			_ = injector.EnqueueMessages(s, maf.UserMessage(
				"Describe what you see in the following image(s).", imgs))
		}
	}

	maxIter := e.cfg.MaxAgentIterations
	mafAgent := openaiprovider.NewChatCompletionsAgent(
		openai.NewClient(prov.RequestOptions()...),
		openaiprovider.AgentConfig{
			Model:        model,
			Instructions: e.buildSystemPrompt(ctx, ag),
			ToolAutoCall: &toolautocall.Config{
				MaximumIterationsPerRequest:                 &maxIter,
				MaximumConsecutiveErrorsPerRequest:          &maxConsecutiveToolErrors,
				IncludeDetailedErrors:                       true, // model reads tool errors, like the legacy loop
				TerminateOnUnknownCalls:                     false,
				AllowConcurrentInvocations:                  false,
				DisableApprovalResponseBinding:              true, // no approval flow in agenticgo
				DisableApprovalNotRequiredFunctionBypassing: true,
			},
			Config: mafgent.Config{
				Name:            ag.Key,
				HistoryProvider: maf.NewHistoryProvider(e.store, agentKey, session),
				Tools:           mafTools,
				MessageInjector: injector,
			},
		},
	)

	// Per-run MAF session: carries no history (the store owns it) but MAF
	// requires one for the injector; created fresh per run.
	mafSession, err := mafAgent.CreateSession(ctx)
	if err != nil {
		return "", fmt.Errorf("create maf session: %w", err)
	}
	drain(mafSession) // no-op on an empty sink; keeps the drain closure honest

	// Persist the user turn BEFORE the run, as the legacy path does: the
	// HistoryProvider prepends stored history, and a cancelled run still
	// leaves the user's message in history.
	if err := e.store.AppendMessage(ctx, agentKey, session, string(llm.RoleUser), userMessage); err != nil {
		return "", fmt.Errorf("save user message: %w", err)
	}

	opts := []mafgent.Option{mafgent.WithSession(mafSession), mafgent.Stream(true)}
	if params := chatParams(ag); params != nil {
		opts = append(opts, params)
	}

	var finalText strings.Builder
	var runErr error
	for update, err := range mafAgent.Run(ctx, []*message.Message{userMsg}, opts...) {
		if err != nil {
			runErr = err
			break
		}
		if update == nil {
			continue
		}
		for _, c := range update.Contents {
			switch c := c.(type) {
			case *message.TextContent:
				finalText.WriteString(c.Text)
				emit(Event{Kind: "text", Text: c.Text})
			case *message.FunctionCallContent:
				emit(Event{Kind: "tool_call", ToolName: c.Name, ToolArgs: c.Arguments})
			case *message.FunctionResultContent:
				emit(Event{Kind: "tool_result", ToolName: resultToolName(c), ToolResult: mafResultText(c)})
			}
		}
		drain(mafSession)
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

// resultToolName recovers the tool name for a result event; MAF does not
// carry it on the result content, so it is empty when unknown (the SPA
// tolerates this — the tool_call event immediately precedes it).
func resultToolName(c *message.FunctionResultContent) string {
	if fc, ok := c.RawRepresentation.(*message.FunctionCallContent); ok {
		return fc.Name
	}
	return ""
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
	a := openaiprovider.NewChatCompletionsAgent(
		openai.NewClient(prov.RequestOptions()...),
		openaiprovider.AgentConfig{
			Model:        model,
			Instructions: instructions,
		},
	)
	resp, err := a.RunText(ctx, prompt).Collect()
	if err != nil {
		return "", err
	}
	return resp.String(), nil
}

// ensure option import is used even if request options change shape
var _ = option.WithAPIKey
