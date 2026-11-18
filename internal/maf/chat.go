package maf

import (
	"context"
	"iter"
	"slices"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/agent/harness/toolautocall"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/microsoft/agent-framework-go/provider/openaiprovider"
	"github.com/microsoft/agent-framework-go/tool"
	"github.com/openai/openai-go/v3"
)

// ChatConfig configures agenticgo's MAF chat agent. History is scoped to one
// serialized conversation turn; DrainImages supplies reference images fetched
// by tools during that turn.
type ChatConfig struct {
	Name          string
	Model         string
	Instructions  string
	Tools         []tool.Tool
	History       *HistoryProvider
	MaxIterations int
	DrainImages   func() []string
}

// NewChatAgent composes MAF's OpenAI provider with its toolautocall middleware.
// The single-completion agent has autocall and history storage disabled so the
// outer agent is the sole owner of the lifecycle. MAF owns request translation,
// streamed tool-call assembly, invocation, iteration limits, and cancellation —
// since MAF v0.3.0 (upstream #1349) the provider assembles packed, interleaved,
// and terminal-chunk tool calls itself and preserves completed tool outcomes on
// cancellation, so the v0.2.1-era stream-repair and batch-recovery adapters are
// gone. What remains agenticgo-specific:
//
//   - toolLoop image retention: injected images must remain at their original
//     positions across subsequent model calls. MAF's MessageInjector alone
//     does not retain them without adopting per-service-call history storage.
//   - HistoryProvider/CheckpointMiddleware: the SQLite persistence design.
//
// The Run passthrough exists because provider.Run returns the named
// agent.ResponseStream type while ProviderConfig.Run wants plain iter.Seq2.
func NewChatAgent(client openai.Client, cfg ChatConfig) *agent.Agent {
	zero := 0
	provider := openaiprovider.NewChatCompletionsAgent(client, openaiprovider.AgentConfig{
		Model:        cfg.Model,
		ToolAutoCall: &toolautocall.Config{MaximumIterationsPerRequest: &zero},
		Config: agent.Config{
			HistoryProvider: agent.NewHistoryProvider(agent.HistoryProviderConfig{SourceID: "single-completion"}),
		},
	})
	runtime := agent.Config{
		Name: cfg.Name, Tools: cfg.Tools,
	}
	if cfg.Instructions != "" {
		runtime.RunOptions = []agent.Option{agent.WithInstructions(cfg.Instructions)}
	}
	if cfg.History != nil {
		runtime.HistoryProvider = cfg.History
		runtime.Middlewares = []agent.Middleware{cfg.History.CheckpointMiddleware()}
	}
	return agent.New(agent.ProviderConfig{
		ProviderName: "openai",
		Run: func(ctx context.Context, messages []*message.Message, opts ...agent.Option) iter.Seq2[*agent.ResponseUpdate, error] {
			return func(yield func(*agent.ResponseUpdate, error) bool) {
				for update, err := range provider.Run(ctx, messages, opts...) {
					if !yield(update, err) {
						return
					}
				}
			}
		},
		Middlewares: []agent.Middleware{toolLoop(cfg.MaxIterations, cfg.DrainImages)},
	}, runtime)
}

// toolLoop delegates the loop to MAF and retains injected images at their
// original positions for every remaining completion. MAF's MessageInjector
// alone only adds them to a single downstream request with our history design.
func toolLoop(maxIterations int, drain func() []string) agent.Middleware {
	return agent.MiddlewareFunc(func(next agent.RunFunc, ctx context.Context, messages []*message.Message, opts ...agent.Option) iter.Seq2[*agent.ResponseUpdate, error] {
		type injection struct {
			after int
			msg   *message.Message
		}
		var images []injection
		streaming, _ := agent.GetOption(opts, agent.Stream)
		round := func(ctx context.Context, messages []*message.Message, opts ...agent.Option) iter.Seq2[*agent.ResponseUpdate, error] {
			// Stream tool-round updates even for non-streaming model requests,
			// so completed rounds can be checkpointed before a later failure.
			if !streaming {
				opts = append(slices.Clone(opts), agent.Stream(false))
			}
			if drain != nil {
				if fetched := drain(); len(fetched) > 0 {
					images = append(images, injection{len(messages), UserMessage("Describe what you see in the following image(s).", fetched)})
				}
			}
			if len(images) > 0 {
				out := make([]*message.Message, 0, len(messages)+len(images))
				start := 0
				for _, image := range images {
					out = append(out, messages[start:image.after]...)
					out = append(out, image.msg)
					start = image.after
				}
				messages = append(out, messages[start:]...)
			}
			return next(ctx, messages, opts...)
		}
		// Tool failures remain readable results throughout the iteration budget.
		maxErrors := max(0, maxIterations)
		loop := toolautocall.New(toolautocall.Config{
			MaximumIterationsPerRequest:                 &maxIterations,
			MaximumConsecutiveErrorsPerRequest:          &maxErrors,
			IncludeDetailedErrors:                       true,
			TerminateOnUnknownCalls:                     false,
			AllowConcurrentInvocations:                  false,
			DisableApprovalResponseBinding:              true,
			DisableApprovalNotRequiredFunctionBypassing: true,
		})
		return loop.Run(round, ctx, messages, append(slices.Clone(opts), agent.Stream(true))...)
	})
}
