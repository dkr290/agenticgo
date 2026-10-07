package maf

import (
	"context"
	"encoding/json"

	"github.com/dkr290/agenticgo/internal/mcp"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/microsoft/agent-framework-go/tool"
)

// mcpFuncTool adapts one discovered MCP tool to MAF's tool.FuncTool.
//
// MAF ships a native mcptool package, but it wraps a live session snapshot
// (ListTools) with fixed names and Go-error failures. agenticgo instead needs
// its own invariants on top of the same go-sdk session the Manager owns:
//
//   - namespacing: the tool is exposed as mcp_<server>_<tool> (Manager-side
//     longest-prefix resolution), so two servers can never collide and the
//     agent's enabled_tools list matches config.json entries verbatim;
//   - lazy reconnect: calls go through Manager.CallTool, which re-dials a
//     configured-but-disconnected server on demand;
//   - soft-fail: a dead/unreachable server produces an error STRING the model
//     can read, not a Go error — a dead server never breaks the chat.
type mcpFuncTool struct {
	name   string // namespaced: mcp_<server>_<tool>
	desc   string
	schema map[string]any
	mgr    *mcp.Manager
}

// NewMCPTool wraps one discovered MCP tool (from Manager.Tools/Lookup) as a
// MAF FuncTool bound to the manager for dispatch.
func NewMCPTool(name string, info mcp.ToolInfo, mgr *mcp.Manager) tool.FuncTool {
	desc := info.Description
	if desc == "" {
		desc = "Tool from MCP server " + info.Server
	}
	return &mcpFuncTool{name: name, desc: desc, schema: info.Schema, mgr: mgr}
}

func (t *mcpFuncTool) Name() string        { return t.name }
func (t *mcpFuncTool) Description() string { return t.desc }

// Schema returns the input schema captured at discovery. MAF accepts *any*
// here but the OpenAI provider serializes structs by JSON-marshalling them
// first (see provider/openaiprovider strict_schema.go), so the discovered map
// is converted to a *jsonschema.Schema via a JSON round-trip — same mechanism
// as tools.AdaptFuncTool, in reverse.
func (t *mcpFuncTool) Schema() any {
	data, err := json.Marshal(t.schema)
	if err != nil {
		return map[string]any{"type": "object"}
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(data, &s); err != nil {
		return map[string]any{"type": "object"}
	}
	return &s
}

// ReturnSchema is nil: MCP results are flattened to text.
func (t *mcpFuncTool) ReturnSchema() any { return nil }

// Call forwards to the MCP manager, which lazily reconnects a configured but
// disconnected server. Failures come back as error strings the model can read
// (soft-fail), so a dead server never breaks the chat.
func (t *mcpFuncTool) Call(ctx context.Context, args string) (any, error) {
	result, err := t.mgr.CallTool(ctx, t.name, json.RawMessage(args))
	if err != nil {
		return "error: " + err.Error(), nil
	}
	return result, nil
}

// compile-time assertion
var _ tool.FuncTool = (*mcpFuncTool)(nil)
