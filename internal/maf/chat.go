package maf

import (
	"context"
	"crypto/rand"
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
// outer agent is the sole owner of the lifecycle. This exposes the provider
// boundary for the v0.1.0 stream compatibility fix and run-scoped image context,
// while MAF still owns request translation, invocation, and iteration limits.
//
// TODO(maf-upgrade): This package pins compatibility workarounds to MAF v0.1.0.
// When bumping github.com/microsoft/agent-framework-go, re-test each adapter
// below against the new version and delete whichever upstream has fixed; the
// goal is to converge back to a single openaiprovider.NewChatCompletionsAgent
// with fused autocall (the documented wiring) once it is correct.
//
// Verified against upstream source and issue tracker (Oct 2026):
//
//   - completionUpdates (streamed tool-call assembly): STILL REQUIRED.
//     provider/openaiprovider/chat.go on main still emits at most one
//     FunctionCallContent per chunk via acc.JustFinishedToolCall(); the
//     non-streaming path correctly ranges over all Message.ToolCalls. Repro
//     (scratch test, since deleted): a single chunk packing two tool calls
//     emitted ZERO FunctionCallContents under stock v0.1.0 wiring, so autocall
//     invoked neither. No upstream issue filed yet — consider filing one and
//     linking it here. Delete this adapter only when the streaming path emits
//     every accumulated call.
//
//   - toolLoop image retention: STILL REQUIRED. agent/messageinjection.go's
//     MessageInjector.run returns as soon as an actionable FunctionCallContent
//     appears, so injected messages reach only one downstream request. Our
//     run-scoped re-injection keeps fetched images in context for every
//     remaining round. Re-check MessageInjector on upgrade; if it gains
//     persistent/positional injection, drop the retention logic (keep the
//     drain hook shape).
//
//   - GuardTools (cancellation before dispatch): LIKELY REDUNDANT after upgrade.
//     Upstream issue #1076 / PR #1077 ("Stop tool invocation on request
//     cancellation", merged 2026-09-16, after v0.1.0) makes toolautocall check
//     ctx.Err() before provider/tool calls and bypass the recoverable-error
//     path on cancellation. Once we pin a release containing #1077, remove
//     GuardTools and rely on the framework; keep
//     TestChatCancellationStopsRemainingBatchAndKeepsResults as the guard.
//
//   - HistoryProvider/CheckpointMiddleware: NOT a workaround — this is our
//     intended persistence design (SQLite owns history, MAF sessions never do).
//     Keep regardless of upstream changes.
func NewChatAgent(client openai.Client, cfg ChatConfig) *agent.Agent {
	zero := 0
	provider := openaiprovider.NewChatCompletionsAgent(client, openaiprovider.AgentConfig{
		Model:        cfg.Model,
		ToolAutoCall: &toolautocall.Config{MaximumIterationsPerRequest: &zero},
		Config: agent.Config{
			HistoryProvider: agent.NewHistoryProvider(agent.HistoryProviderConfig{SourceID: "single-completion"}),
		},
	})
	runtime := agent.Config{Name: cfg.Name, Tools: GuardTools(cfg.Tools)}
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
			return completionUpdates(provider.Run(ctx, messages, opts...))
		},
		Middlewares: []agent.Middleware{toolLoop(cfg.MaxIterations, cfg.DrainImages)},
	}, runtime)
}

// completionUpdates works around MAF v0.1.0's use of JustFinishedToolCall,
// which can emit incomplete/duplicate calls for interleaved deltas and omit
// calls in the terminal chunk. Use the SDK's fully accumulated call set only
// after a successful stream. Text and usage still stream immediately. Remove
// this adapter when the upstream provider handles these cases itself.
func completionUpdates(stream agent.ResponseStream) iter.Seq2[*agent.ResponseUpdate, error] {
	return func(yield func(*agent.ResponseUpdate, error) bool) {
		var acc openai.ChatCompletionAccumulator
		var last *agent.ResponseUpdate
		for update, err := range stream {
			if err != nil {
				yield(nil, err)
				return
			}
			if update == nil {
				continue
			}
			out := *update
			out.Contents = nil
			chunk, streaming := update.RawRepresentation.(openai.ChatCompletionChunk)
			if streaming {
				acc.AddChunk(chunk)
				last = update
			}
			for _, c := range update.Contents {
				switch c.(type) {
				case *message.FunctionCallContent:
					if streaming {
						continue
					}
				case *message.ErrorContent:
					if streaming {
						continue // read the complete refusal from the accumulator below
					}
				}
				out.Contents = append(out.Contents, c)
			}
			if !yield(&out, nil) {
				return
			}
		}
		if last == nil || len(acc.Choices) == 0 {
			return
		}
		completed := *last
		completed.RawRepresentation = nil
		completed.Contents = nil
		msg := acc.Choices[0].Message
		if msg.Refusal != "" {
			completed.Contents = append(completed.Contents, &message.ErrorContent{Message: msg.Refusal, ErrorCode: "Refusal"})
		}
		for _, tc := range msg.ToolCalls {
			if tc.Type != "function" {
				continue
			}
			id := tc.ID
			if id == "" {
				id = "call_" + rand.Text()
			}
			completed.Contents = append(completed.Contents, &message.FunctionCallContent{
				CallID: id, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
			})
		}
		if len(completed.Contents) > 0 {
			yield(&completed, nil)
		}
	}
}

// toolLoop delegates the loop to MAF and retains injected images at their
// original positions for every remaining completion. MAF's MessageInjector
// in v0.1.0 only adds them to a single downstream request.
func toolLoop(maxIterations int, drain func() []string) agent.Middleware {
	return agent.MiddlewareFunc(func(next agent.RunFunc, ctx context.Context, messages []*message.Message, opts ...agent.Option) iter.Seq2[*agent.ResponseUpdate, error] {
		type injection struct {
			after int
			msg   *message.Message
		}
		var images []injection
		round := func(ctx context.Context, messages []*message.Message, opts ...agent.Option) iter.Seq2[*agent.ResponseUpdate, error] {
			if err := ctx.Err(); err != nil {
				return func(yield func(*agent.ResponseUpdate, error) bool) { yield(nil, err) }
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
		return loop.Run(round, ctx, messages, opts...)
	})
}

// GuardTools checks cancellation before every dispatch, including later calls
// in a serial MAF batch. Schema and return metadata are inherited unchanged.
func GuardTools(tools []tool.Tool) []tool.Tool {
	out := slices.Clone(tools)
	for i, t := range out {
		if fn, ok := t.(tool.FuncTool); ok {
			out[i] = guardedTool{fn}
		}
	}
	return out
}

type guardedTool struct{ tool.FuncTool }

func (t guardedTool) Call(ctx context.Context, args string) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return t.FuncTool.Call(ctx, args)
}
