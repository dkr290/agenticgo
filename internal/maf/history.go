package maf

// history.go implements the persistence bridge between agenticgo's SQLite
// message store and MAF's agent.HistoryProvider interface.
//
// The store remains the single source of truth for conversation history —
// MAF sessions are NOT used for history ownership (a second source of truth
// would fight the store). The engine wires one HistoryProvider per run,
// scoped to (agent, session). Invoking loads history and saves the incoming
// user turn; middleware checkpoints completed tool rounds; Invoked saves the
// remaining successful response without duplicating those checkpoints.
//
// The translation boundary: the store keeps its existing flat rows (role,
// content, tool_calls JSON, tool_call_id, name); MAF speaks message.Message
// with typed content parts (TextContent, FunctionCallContent,
// FunctionResultContent, DataContent). Both directions are lossless for the
// shapes agenticgo persists.
//
// replayHistory moves here from internal/agent unchanged in behavior: tool
// metadata is preserved and incomplete tool exchanges (history-window
// boundary or an interrupted run) are dropped before translation, so the
// provider never sees a dangling tool call or an orphaned result.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"time"

	"github.com/dkr290/agenticgo/internal/llm"
	"github.com/dkr290/agenticgo/internal/store"
	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"
)

// historyWindow is how many stored messages a run loads, matching the
// window the legacy engine used.
const historyWindow = 40

// HistoryProvider implements agent.HistoryProvider over store.Store for one
// serialized (agent, session) turn. It must be paired with CheckpointMiddleware
// to preserve completed tool rounds when a later request fails or is cancelled.
type HistoryProvider struct {
	st      *store.Store
	agent   string
	session string
	saved   int // response messages already checkpointed during this turn
}

// NewHistoryProvider creates the MAF history bridge for one conversation.
func NewHistoryProvider(st *store.Store, agentKey, session string) *HistoryProvider {
	return &HistoryProvider{st: st, agent: agentKey, session: session}
}

// Invoking returns the full input, including the caller's original multimodal
// messages. Load history before storing the incoming user text to avoid adding
// that turn twice, including when the user repeats an identical prompt.
func (h *HistoryProvider) Invoking(ctx context.Context, invoking agent.InvokingContext) ([]*message.Message, error) {
	h.saved = 0
	rows, err := h.st.Messages(ctx, h.agent, h.session, historyWindow)
	if err != nil {
		return nil, fmt.Errorf("load history: %w", err)
	}
	replayed, err := replayHistory(rows)
	if err != nil {
		return nil, err
	}
	out := make([]*message.Message, 0, len(replayed)+len(invoking.Messages))
	for _, m := range replayed {
		out = append(out, llmToMAF(m))
	}
	var incoming []store.Message
	for _, m := range invoking.Messages {
		if m == nil {
			continue
		}
		out = append(out, m)
		incoming = append(incoming, store.Message{Role: string(m.Role), Content: m.String()})
	}
	if err := h.st.AppendMessages(ctx, h.agent, h.session, incoming); err != nil {
		return nil, fmt.Errorf("save incoming messages: %w", err)
	}
	return out, nil
}

// Invoked stores only the successful tail. On failure the completed tool
// rounds are already durable; unfinished assistant text is not replayed.
func (h *HistoryProvider) Invoked(ctx context.Context, invoked agent.InvokedContext) error {
	if invoked.Err != nil {
		return nil
	}
	return h.checkpoint(ctx, invoked.ResponseMessages)
}

// CheckpointMiddleware saves each completed MAF tool round before the next
// provider request. Cleanup has a bounded independent context so cancellation
// cannot erase results of side effects that have already happened.
func (h *HistoryProvider) CheckpointMiddleware() agent.Middleware {
	return agent.MiddlewareFunc(func(next agent.RunFunc, ctx context.Context, messages []*message.Message, opts ...agent.Option) iter.Seq2[*agent.ResponseUpdate, error] {
		return func(yield func(*agent.ResponseUpdate, error) bool) {
			var response agent.Response
			for update, err := range next(ctx, messages, opts...) {
				response.Update(update)
				if update != nil && hasToolResults(update.Contents) {
					saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
					saveErr := h.checkpoint(saveCtx, response.Messages)
					cancel()
					if saveErr != nil {
						yield(nil, saveErr)
						return
					}
				}
				if !yield(update, err) || err != nil {
					return
				}
			}
		}
	})
}

func hasToolResults(contents message.Contents) bool {
	for _, c := range contents {
		if _, ok := c.(*message.FunctionResultContent); ok {
			return true
		}
	}
	return false
}

func (h *HistoryProvider) checkpoint(ctx context.Context, messages []*message.Message) error {
	names := make(map[string]string)
	var rows []store.Message
	for i, m := range messages {
		text, calls, results := splitContents(m.Contents)
		for _, call := range calls {
			names[call.ID] = call.Name
		}
		if i < h.saved {
			continue
		}
		switch {
		case len(calls) > 0:
			tcJSON, err := json.Marshal(calls)
			if err != nil {
				return fmt.Errorf("marshal tool calls: %w", err)
			}
			rows = append(rows, store.Message{Role: string(llm.RoleAssistant), Content: text, ToolCalls: string(tcJSON)})
		case len(results) > 0:
			for _, r := range results {
				rows = append(rows, store.Message{Role: string(llm.RoleTool), Content: r.content, ToolCallID: r.callID, Name: names[r.callID]})
			}
		case text != "":
			role := string(m.Role)
			if role == "" {
				role = string(llm.RoleAssistant)
			}
			rows = append(rows, store.Message{Role: role, Content: text})
		}
	}
	if err := h.st.AppendMessages(ctx, h.agent, h.session, rows); err != nil {
		return fmt.Errorf("save response messages: %w", err)
	}
	h.saved = len(messages)
	return nil
}

// toolResult is one pending row for a tool-result message.
type toolResult struct {
	callID  string
	content string
}

// splitContents flattens MAF content parts into the shapes the store
// persists: concatenated text, a []llm.ToolCall for function calls, and one
// toolResult per function result.
func splitContents(cs message.Contents) (text string, calls []llm.ToolCall, results []toolResult) {
	var b strings.Builder
	for _, c := range cs {
		switch c := c.(type) {
		case *message.TextContent:
			b.WriteString(c.Text)
		case *message.ErrorContent:
			b.WriteString(c.Message)
		case *message.FunctionCallContent:
			calls = append(calls, llm.ToolCall{ID: c.CallID, Name: c.Name, Arguments: c.Arguments})
		case *message.FunctionResultContent:
			content := resultText(c.Result)
			if c.Error != nil {
				content = "error: " + c.Error.Error()
			}
			results = append(results, toolResult{callID: c.CallID, content: content})
		}
	}
	return b.String(), calls, results
}

// resultText flattens a FunctionResultContent Result (string, Contents, or
// other JSON-able value) to a plain string for storage.
func resultText(result any) string {
	switch r := result.(type) {
	case nil:
		return ""
	case string:
		return r
	case message.Contents:
		return r.Text()
	case []message.Content:
		return message.Contents(r).Text()
	default:
		data, err := json.Marshal(r)
		if err != nil {
			return fmt.Sprintf("%v", r)
		}
		return string(data)
	}
}

// --- llm.Message ↔ message.Message translation ---

// llmToMAF converts one replayed llm.Message into a MAF message.
func llmToMAF(m llm.Message) *message.Message {
	switch m.Role {
	case llm.RoleAssistant:
		var cs message.Contents
		if m.Content != "" {
			cs = append(cs, &message.TextContent{Text: m.Content})
		}
		for _, tc := range m.ToolCalls {
			cs = append(cs, &message.FunctionCallContent{CallID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
		}
		msg := message.New(cs...)
		msg.Role = message.RoleAssistant
		return msg
	case llm.RoleTool:
		msg := message.New(&message.FunctionResultContent{
			CallID: m.ToolCallID,
			Result: m.Content,
		})
		msg.Role = message.RoleTool
		return msg
	case llm.RoleSystem:
		msg := message.NewText(m.Content)
		msg.Role = message.RoleSystem
		return msg
	default: // user
		var cs message.Contents
		cs = append(cs, &message.TextContent{Text: m.Content})
		for _, img := range m.Images {
			if dc := dataURLContent(img); dc != nil {
				cs = append(cs, dc)
			}
		}
		msg := message.New(cs...)
		msg.Role = message.RoleUser
		return msg
	}
}

// dataURLContent converts a base64 data-URL into a DataContent part.
func dataURLContent(dataURL string) *message.DataContent {
	data, mediaType, err := message.DecodeDataURI(dataURL)
	if err != nil {
		return nil
	}
	return &message.DataContent{
		Data:      base64.StdEncoding.EncodeToString(data),
		MediaType: mediaType,
	}
}

// UserMessage builds a MAF user message from text plus optional base64
// data-URL images (the chat attachment / fetch_agent_image vision path).
// Invalid data-URLs are dropped silently — the engine only forwards images
// for vision-capable providers anyway.
func UserMessage(text string, images []string) *message.Message {
	cs := message.Contents{&message.TextContent{Text: text}}
	for _, img := range images {
		if dc := dataURLContent(img); dc != nil {
			cs = append(cs, dc)
		}
	}
	msg := message.New(cs...)
	msg.Role = message.RoleUser
	return msg
}

// replayHistory preserves tool metadata and drops incomplete tool exchanges
// caused by a history-window boundary or an interrupted run. Moved verbatim
// from internal/agent — the correctness invariants (dangling calls, orphaned
// results, window truncation) live here now, ahead of MAF translation.
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
