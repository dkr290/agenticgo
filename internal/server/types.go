// Shared request/response types for the Huma-registered API operations.
// Types carry `doc`/`example` annotations so the generated OpenAPI spec and
// the /docs UI are self-describing.
package server

import (
	"github.com/dkr290/agenticgo/internal/agents"
	"github.com/dkr290/agenticgo/internal/mcp"
	"github.com/dkr290/agenticgo/internal/providers"
	"github.com/dkr290/agenticgo/internal/skills"
)

// --- Chat (non-streaming REST) ---

// chatInput is the request for POST /api/chat — one agent turn over plain
// HTTP (the WebSocket /ws endpoint remains the streaming variant).
type chatInput struct {
	Body struct {
		Agent   string `json:"agent,omitempty" example:"demo" doc:"Agent key (default: default)"`
		Session string `json:"session,omitempty" example:"my-script" doc:"Conversation/session name (default: default)"`
		// Message is the user's message for this turn.
		Message  string   `json:"message" example:"Summarize the workspace" doc:"User message (required)"`
		Provider string   `json:"provider,omitempty" example:"ollama-local" doc:"Optional provider override"`
		Images   []string `json:"images,omitempty" doc:"Names of the agent's stored images to attach (vision models only)"`
		Evolve   bool     `json:"evolve,omitempty" doc:"Run the self-evolution pass after the reply"`
	}
}

// chatToolCall is one tool invocation from the turn (tool trace summary).
type chatToolCall struct {
	Name string `json:"name" example:"read_file"`
	Args string `json:"args,omitempty" example:"{\"path\":\".\"}" doc:"Raw JSON arguments"`
}

// chatOutput is the synchronous chat reply.
type chatOutput struct {
	Body struct {
		Reply     string         `json:"reply" doc:"Final assistant text"`
		Agent     string         `json:"agent" example:"demo"`
		Session   string         `json:"session" example:"my-script"`
		ToolsUsed []chatToolCall `json:"tools_used" doc:"Tool calls made during the turn (in order)"`
		Evolved   bool           `json:"evolved" doc:"Whether the optional Evolve pass ran successfully"`
	}
}

// --- Generic responses ---

// statusBody is the generic {"status": "..."} response.
type statusBody struct {
	Status string `json:"status" example:"ok" doc:"Operation status"`
}

// statusOutput wraps statusBody for huma responses.
type statusOutput struct {
	Body statusBody
}

// --- Health ---

type healthOutput struct {
	Body struct {
		Status string `json:"status" example:"ok" doc:"Service health status"`
	}
}

// --- Agents ---

type agentLLMConfig struct {
	Provider          *string  `json:"provider,omitempty"`
	Model             *string  `json:"model,omitempty"`
	Temperature       *float64 `json:"temperature,omitempty" minimum:"0" maximum:"2"`
	MaxTokens         *int     `json:"max_tokens,omitempty" minimum:"1"`
	Vision            *bool    `json:"vision,omitempty"`
	ObservationInject *int     `json:"observation_inject,omitempty" minimum:"0"`
}

type createAgentInput struct {
	Body struct {
		Key         string              `json:"key" example:"demo" doc:"Agent key (a-z, 0-9, -, _); becomes the directory name"`
		Name        string              `json:"name" example:"Demo Agent" doc:"Display name"`
		Description string              `json:"description,omitempty" doc:"Short description shown in the UI"`
		Soul        string              `json:"soul,omitempty" doc:"Optional persona text (SOUL.md)"`
		Config      *agents.AgentConfig `json:"config,omitempty" doc:"Optional per-agent LLM settings (nil = inherit)"`
	}
}

// --- Context files ---

type fileContentOutput struct {
	Body struct {
		Name    string `json:"name" example:"SOUL.md" doc:"Context file name"`
		Content string `json:"content" doc:"File contents (empty when the file does not exist yet)"`
	}
}

type writeFileInput struct {
	Key  string `path:"key"`
	Name string `path:"name"`
	Body struct {
		Content string `json:"content" doc:"Full new content of the context file"`
	}
}

// --- Skills ---

// skillWithState is a library skill plus whether the agent in context has it
// enabled.
type skillWithState struct {
	skills.Skill
	Enabled bool `json:"enabled" doc:"Whether the agent has this skill enabled"`
}

type skillStateOutput struct {
	Body struct {
		Agent   string `json:"agent" example:"demo"`
		Skill   string `json:"skill" example:"code-review"`
		Enabled bool   `json:"enabled" doc:"Resulting enabled state"`
	}
}

// --- Agent images (vision) ---

type visionOutput struct {
	Body struct {
		Vision bool `json:"vision" doc:"Whether the effective provider/model accepts image attachments"`
	}
}

// --- Evolve ---

type evolveInput struct {
	Body struct {
		Agent   string `json:"agent,omitempty" example:"default" doc:"Agent key (default: default)"`
		Session string `json:"session,omitempty" example:"default" doc:"Conversation/session name (default: default)"`
	}
}

// --- Sessions ---

type deleteSessionOutput struct {
	Body struct {
		Deleted string `json:"deleted" example:"my-conversation" doc:"Deleted session name"`
	}
}

// --- Built-in tools ---

type builtinToolInfo struct {
	Name        string `json:"name" example:"read_file"`
	Description string `json:"description" example:"Read a file from the workspace"`
}

type listToolsOutput struct {
	Body []builtinToolInfo
}

// --- Providers ---

// testProviderInput is the input for POST /api/providers/{name}/test. The
// body is a pointer so an empty body (test the saved provider) is allowed.
type testProviderInput struct {
	Name string              `path:"name" doc:"Saved provider name"`
	Body *providers.Provider `doc:"Optional ad-hoc provider config to test instead of the saved one"`
}

// testProviderAdhocInput is the input for POST /api/providers/test (ad-hoc
// config from the provider form, tested before saving).
type testProviderAdhocInput struct {
	Body *providers.Provider `doc:"Ad-hoc provider config to test"`
}

type testProviderOutput struct {
	Body struct {
		Status string   `json:"status" example:"ok"`
		Models []string `json:"models" doc:"Model IDs reported by the provider"`
	}
}

// --- MCP servers ---

// connectErrorBody is the 502 body shape when connecting fails: the error
// message plus (when available) the server status snapshot so the UI can show
// the error next to the server entry.
type connectErrorBody struct {
	Error  string            `json:"error" example:"dial stdio server: executable file not found"`
	Server *mcp.ServerStatus `json:"server,omitempty"`
}

// connectError is a huma.StatusError whose body matches the pre-Huma shape
// ({"error","server"}) the UI's api() helper expects.
type connectError struct {
	connectErrorBody
	status int
}

func (e *connectError) Error() string  { return e.connectErrorBody.Error }
func (e *connectError) GetStatus() int { return e.status }

// ContentType implements huma.ContentTypeFilter so the UI keeps receiving
// plain application/json (not application/problem+json).
func (e *connectError) ContentType(string) string { return "application/json" }

type connectOutput struct {
	Body *mcp.ServerStatus
}

// --- MCP tools: per-agent enablement ---

// customToolWithState is a discovered MCP tool plus whether the agent in
// context has it enabled.
type customToolWithState struct {
	mcp.ToolInfo
	// Discovered is the namespaced name (mcp_<server>_<tool>) used as the key.
	Discovered string `json:"discovered" example:"mcp_kubernetes_get_pods"`
	Enabled    bool   `json:"enabled"`
}

type customToolStateOutput struct {
	Body struct {
		Agent   string `json:"agent" example:"demo"`
		Tool    string `json:"tool" example:"mcp_kubernetes_get_pods"`
		Enabled bool   `json:"enabled"`
	}
}

// --- Extra (dangerous) exec commands ---

// extraCommandWithState is one AGENTICGO_EXTRA_EXEC_COMMANDS entry plus
// whether the agent in context has it enabled.
type extraCommandWithState struct {
	Name    string `json:"name" example:"kubectl"`
	Enabled bool   `json:"enabled"`
}

type extraCommandStateOutput struct {
	Body struct {
		Agent   string `json:"agent" example:"demo"`
		Command string `json:"command" example:"kubectl"`
		Enabled bool   `json:"enabled"`
	}
}

// --- Built-in tools: per-agent narrowing ---

// builtinTools lists every built-in tool name (the ceiling is the global
// AGENTICGO_TOOL_ALLOWLIST; this is the full set the UI can render).
var builtinTools = []string{"exec", "list_files", "read_file", "write_file"}

// builtinToolWithState is one built-in tool plus the agent's state for it.
type builtinToolWithState struct {
	Name string `json:"name" example:"exec"`
	// Allowed reports whether the global AGENTICGO_TOOL_ALLOWLIST ceiling
	// permits the tool at all. A tool not allowed globally can never be
	// enabled per agent.
	Allowed bool `json:"allowed"`
	// Enabled reports whether the tool is effective for the agent right now
	// (allowed globally and not narrowed away per agent).
	Enabled bool `json:"enabled"`
}

type builtinToolStateOutput struct {
	Body struct {
		Agent   string `json:"agent" example:"demo"`
		Tool    string `json:"tool" example:"exec"`
		Enabled bool   `json:"enabled"`
	}
}

type resetBuiltinToolsOutput struct {
	Body struct {
		Agent        string `json:"agent" example:"demo"`
		BuiltinTools string `json:"builtin_tools" example:"inherit" doc:"Reset result; always 'inherit'"`
	}
}
