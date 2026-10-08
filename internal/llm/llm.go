// Package llm defines the provider abstraction and message types shared by
// the agent engine and the persistence bridge. The agent loop runs on the
// Microsoft Agent Framework (see internal/agent/run_maf.go); what remains
// here is the named-provider seam (providers.Store resolves to a Provider,
// concretely *OpenAIProvider) plus the flat message shape the SQLite history
// store persists and internal/maf translates.
package llm

// Role identifies who produced a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall is a request from the model to invoke a tool, as persisted in
// history rows.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // raw JSON
}

// Message is a single turn in the conversation as persisted by the history
// store (flat rows; internal/maf translates these to and from MAF
// content-part messages).
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"` // for RoleTool results
	Name       string     `json:"name,omitempty"`         // tool name for RoleTool results
	// Images are base64 data-URLs ("data:image/png;base64,...") attached to a
	// user message for vision-capable models.
	Images []string `json:"images,omitempty"`
}

// Provider is a named LLM backend configuration. The engine builds its MAF
// client from the provider's endpoint settings (see
// OpenAIProvider.RequestOptions).
type Provider interface {
	// Name identifies the provider for logging.
	Name() string
	// Model returns the model identifier requests are sent with.
	Model() string
}
