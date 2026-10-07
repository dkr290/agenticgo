package maf

// history.go implements the persistence bridge between agenticgo's SQLite
// message store and MAF's agent.HistoryProvider interface.
//
// The store remains the single source of truth for conversation history —
// MAF sessions are NOT used for history ownership (a second source of truth
// would fight the store). The engine wires one historyProvider per run,
// scoped to (agent, session), and MAF calls Provide before the provider run
// and Store after it completes.
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
	"strings"

	"github.com/dkr290/agenticgo/internal/llm"
	"github.com/dkr290/agenticgo/internal/store"
	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"
)

// historyWindow is how many stored messages a run loads, matching the
// window the legacy engine used.
const historyWindow = 40

// historyProvider implements agent.HistoryProvider over store.Store for one
// (agent, session) conversation.
type historyProvider struct {
	st      *store.Store
	agent   string
	session string
}

// NewHistoryProvider creates the MAF history bridge for one conversation.
func NewHistoryProvider(st *store.Store, agentKey, session string) agent.HistoryProvider {
	return &historyProvider{st: st, agent: agentKey, session: session}
}

// Invoking loads recent history and returns it as additional messages to
// prepend (the default NewHistoryProvider semantics: Provide results are
// prepended to caller-supplied request messages).
func (h *historyProvider) Invoking(ctx context.Context, _ agent.InvokingContext) ([]*message.Message, error) {
	rows, err := h.st.Messages(ctx, h.agent, h.session, historyWindow)
	if err != nil {
		return nil, fmt.Errorf("load history: %w", err)
	}
	replayed, err := replayHistory(rows)
	if err != nil {
		return nil, err
	}
	out := make([]*message.Message, 0, len(replayed))
	for _, m := range replayed {
		out = append(out, llmToMAF(m))
	}
	return out, nil
}

// Invoked persists newly produced messages. MAF hands us the request and
// response messages; only the ones that are not already in the store get
// appended. The engine persists the user turn itself before the run, so the
// provider stores only response messages (assistant text, tool calls, tool
// results) — anything else would double-write.
func (h *historyProvider) Invoked(ctx context.Context, invoked agent.InvokedContext) error {
	if invoked.Err != nil {
		return nil // failed runs persist nothing (matches the legacy engine)
	}
	for _, m := range invoked.ResponseMessages {
		if err := h.storeMessage(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// storeMessage persists one response message in the store's flat-row format,
// matching the rows the legacy engine wrote so Chat History renders unchanged.
func (h *historyProvider) storeMessage(ctx context.Context, m *message.Message) error {
	text, calls, results := splitContents(m.Contents)

	switch {
	case len(calls) > 0:
		// Assistant turn that invoked tools: text + tool_calls metadata.
		tcJSON, err := json.Marshal(calls)
		if err != nil {
			return fmt.Errorf("marshal tool calls: %w", err)
		}
		return h.st.AppendMessageWithToolCalls(ctx, h.agent, h.session,
			string(llm.RoleAssistant), text, string(tcJSON), "", "")
	case len(results) > 0:
		// Tool results: one row per result, linked by tool_call_id.
		for _, r := range results {
			if err := h.st.AppendMessageWithToolCalls(ctx, h.agent, h.session,
				string(llm.RoleTool), r.content, "", r.callID, r.name); err != nil {
				return err
			}
		}
		return nil
	default:
		role := string(m.Role)
		if role == "" {
			role = string(llm.RoleAssistant)
		}
		return h.st.AppendMessage(ctx, h.agent, h.session, role, text)
	}
}

// toolResult is one pending row for a tool-result message.
type toolResult struct {
	callID  string
	name    string
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
		case *message.FunctionCallContent:
			calls = append(calls, llm.ToolCall{ID: c.CallID, Name: c.Name, Arguments: c.Arguments})
		case *message.FunctionResultContent:
			content := resultText(c.Result)
			if c.Error != nil {
				content = "error: " + c.Error.Error()
			}
			results = append(results, toolResult{callID: c.CallID, name: resultName(c), content: content})
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

// resultName recovers the tool name MAF stashed in the result's raw
// representation when available; empty otherwise (the store tolerates this —
// replay links by call ID, and the UI only uses name for display).
func resultName(c *message.FunctionResultContent) string {
	if fc, ok := c.RawRepresentation.(*message.FunctionCallContent); ok {
		return fc.Name
	}
	return ""
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
