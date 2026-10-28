package maf

// tool_results.go preserves what the agent's tools actually did when a run is
// cancelled partway through a tool batch. Stopping a run does not undo work
// that a tool already completed.
//
// Example: a Kubernetes observer runs on cron and also accepts chat requests.
// It uses MCP tools to inspect unhealthy pods and query their Loki logs, then
// asks to record its finding and perform another diagnostic check:
//
//  1. The Kubernetes/Loki checks find a pod repeatedly restarting with OOM errors.
//  2. In the next tool batch, record_observation successfully saves that finding.
//  3. The user presses Stop in chat, or shutdown cancels the cron run, before
//     the remaining diagnostic check executes.
//
// MAF v0.2.1 stops further dispatch, but returns the cancellation error without
// emitting that batch's results. The observation is already saved, yet the
// conversation history would lose the result confirming it. A later turn could
// treat the work as unfinished, repeat the observation, or give an incomplete
// account of what was checked.
//
// This adapter captures outcomes through MAF's function-invocation middleware.
// On cancellation it emits the completed results and marks unexecuted calls as
// cancelled, so the existing SQLite checkpoint can save an accurate exchange.
// The next turn in that session can distinguish finished work from pending work.
// Normal successful batches use MAF's ordinary results; MAF still owns execution
// and cancellation. This records finished work rather than continuing the run.
//
// Recovery is scoped to the current agent/session. Cron jobs and chat have
// separate conversation histories; observations and memory share findings
// between those sessions. These are historical findings, not proof that the
// cluster's current state is unchanged, so later runs may still need fresh checks.

import (
	"context"
	"crypto/rand"
	"fmt"
	"iter"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"
)

type toolResultsKey struct{}

// toolResults belongs to one run, not the reusable agent. Dispatch is serial
// (AllowConcurrentInvocations is false), so recording and streaming share it
// without synchronization. Normal result updates retire the current batch.
type toolResults struct {
	pending []*message.FunctionCallContent
	results map[string]*message.FunctionResultContent
}

// captureToolResult records outcomes before MAF's post-dispatch cancellation
// check can discard them. Cancellation and dispatch remain owned by MAF.
func captureToolResult(next func(context.Context, *agent.FunctionInvocationContext) (any, error), ctx context.Context, invocation *agent.FunctionInvocationContext) (result any, err error) {
	batch, _ := ctx.Value(toolResultsKey{}).(*toolResults)
	if batch == nil {
		return next(ctx, invocation)
	}
	defer func() {
		if p := recover(); p != nil {
			// MAF catches tool panics outside function middleware. Record the
			// same failure before handing the panic back to that handler.
			if e, ok := p.(error); ok {
				err = fmt.Errorf("tool execution panic: %w", e)
			} else {
				err = fmt.Errorf("tool execution panic: %v", p)
			}
			batch.record(invocation.CallID, nil, err)
			panic(p)
		}
		batch.record(invocation.CallID, result, err)
	}()
	return next(ctx, invocation)
}

func (b *toolResults) record(callID string, result any, err error) {
	if err == nil {
		if content, ok := result.(*message.FunctionResultContent); ok && content != nil && content.CallID == callID {
			b.results[callID] = content
			return
		}
		if result == nil {
			result = "Success: Function completed."
		}
	}
	b.results[callID] = &message.FunctionResultContent{CallID: callID, Result: result, Error: err}
}

func (b *toolResults) observe(update *agent.ResponseUpdate) {
	if update == nil {
		return
	}
	for _, content := range update.Contents {
		switch c := content.(type) {
		case *message.FunctionCallContent:
			if !c.InformationalOnly {
				b.pending = append(b.pending, c)
			}
		case *message.FunctionResultContent:
			for i, call := range b.pending {
				if call.CallID == c.CallID {
					b.pending = append(b.pending[:i], b.pending[i+1:]...)
					break
				}
			}
			delete(b.results, c.CallID)
		}
	}
}

func (b *toolResults) cancelled(err error) *agent.ResponseUpdate {
	if len(b.pending) == 0 {
		return nil
	}
	contents := make(message.Contents, 0, len(b.pending))
	for _, call := range b.pending {
		result := b.results[call.CallID]
		if result == nil {
			result = &message.FunctionResultContent{CallID: call.CallID, Error: err}
		}
		contents = append(contents, result)
	}
	return &agent.ResponseUpdate{Role: message.RoleTool, MessageID: "cancelled_" + rand.Text(), Contents: contents}
}

// preserveToolResults repairs a cancelled batch before exposing the error. The
// history checkpoint middleware then atomically persists the tool exchange with
// its bounded, uncancelled context, just as it does for ordinary result updates.
func preserveToolResults() agent.Middleware {
	return agent.MiddlewareFunc(func(next agent.RunFunc, ctx context.Context, messages []*message.Message, opts ...agent.Option) iter.Seq2[*agent.ResponseUpdate, error] {
		return func(yield func(*agent.ResponseUpdate, error) bool) {
			batch := &toolResults{results: make(map[string]*message.FunctionResultContent)}
			runCtx := context.WithValue(ctx, toolResultsKey{}, batch)
			for update, err := range next(runCtx, messages, opts...) {
				batch.observe(update)
				if update != nil && !yield(update, nil) {
					return
				}
				if err != nil {
					if runCtx.Err() != nil {
						if recovered := batch.cancelled(runCtx.Err()); recovered != nil && !yield(recovered, nil) {
							return
						}
					}
					yield(nil, err)
					return
				}
			}
		}
	})
}
