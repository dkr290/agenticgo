// Package llm defines the provider abstraction and message types shared by the
// agent loop. Providers talk to an LLM and stream back deltas plus tool calls.
package llm

import "context"

// Role identifies who produced a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall is a request from the model to invoke a tool.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // raw JSON
}

// Message is a single turn in the conversation.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"` // for RoleTool results
	Name       string     `json:"name,omitempty"`         // tool name for RoleTool results
}

// ToolSpec describes a tool to the model (OpenAI-compatible schema).
type ToolSpec struct {
	Type     string       `json:"type"` // always "function"
	Function FunctionSpec `json:"function"`
}

// FunctionSpec describes a callable function.
type FunctionSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"` // JSON Schema
}

// Delta is one streamed chunk from the provider.
type Delta struct {
	// Content is incremental assistant text (may be empty).
	Content string
	// ToolCalls accumulates completed tool calls as they finish streaming.
	// Providers may deliver tool calls incrementally; Complete marks the end.
	ToolCalls []ToolCall
	// Done is true on the final delta.
	Done bool
	// Err is set if the stream failed.
	Err error
}

// StreamFunc receives each Delta. Returning an error aborts the stream.
type StreamFunc func(Delta) error

// Provider is an LLM backend that supports streaming chat with tool use.
type Provider interface {
	// ChatCompletion streams a reply for the given messages and tools.
	// The model may emit tool calls instead of (or before) final text.
	ChatCompletion(ctx context.Context, req ChatRequest, onDelta StreamFunc) (Message, error)
	// Name identifies the provider for logging.
	Name() string
	// Model returns the model identifier requests are sent with.
	Model() string
}

// ChatRequest is a single completion request.
type ChatRequest struct {
	Model       string     `json:"model"`
	Messages    []Message  `json:"messages"`
	Tools       []ToolSpec `json:"tools,omitempty"`
	Temperature *float64   `json:"temperature,omitempty"`
	Stream      bool       `json:"stream"`
}
