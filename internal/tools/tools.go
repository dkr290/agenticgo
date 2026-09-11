// Package tools provides the tool registry and the built-in tool set.
// Tools are the agent's hands: the LLM requests them, the loop executes them.
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dkr290/agenticgo/internal/llm"
)

// Tool is a callable capability exposed to the agent.
type Tool interface {
	// Name is the identifier the model uses to call the tool.
	Name() string
	// Description explains what the tool does (shown to the model).
	Description() string
	// Parameters is a JSON Schema describing the arguments.
	Parameters() map[string]any
	// Call executes the tool with JSON-decoded arguments.
	Call(ctx context.Context, args json.RawMessage) (string, error)
}

// Registry holds the available tools and enforces the allow-list.
type Registry struct {
	tools   map[string]Tool
	allowed map[string]bool // empty => all allowed (config supplies a default)
}

// NewRegistry builds a registry. Only the named tools are exposed. An empty
// allowList exposes everything registered (config always passes a default).
func NewRegistry(allowList []string) *Registry {
	allowed := map[string]bool{}
	for _, n := range allowList {
		allowed[n] = true
	}
	return &Registry{tools: map[string]Tool{}, allowed: allowed}
}

// Register adds a tool. Tools not on the allow-list are skipped.
func (r *Registry) Register(t Tool) {
	if len(r.allowed) > 0 && !r.allowed[t.Name()] {
		return
	}
	r.tools[t.Name()] = t
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Specs returns the OpenAI-compatible tool specs for the model.
func (r *Registry) Specs() []llm.ToolSpec {
	out := make([]llm.ToolSpec, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, llm.ToolSpec{
			Type: "function",
			Function: llm.FunctionSpec{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Parameters(),
			},
		})
	}
	return out
}

// Call invokes a tool by name with raw JSON arguments.
func (r *Registry) Call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	t, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("unknown or disallowed tool: %q", name)
	}
	return t.Call(ctx, args)
}
