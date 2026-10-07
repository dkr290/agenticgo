// Package maf isolates every integration with the Microsoft Agent Framework
// (github.com/microsoft/agent-framework-go) behind agenticgo-owned types, so a
// preview-API upgrade touches one package and the rest of the app never sees
// framework types.
//
// This file adapts the existing tools.Tool interface to MAF's tool.FuncTool.
// All of agenticgo's tools — the workspace-jailed filesystem/exec built-ins,
// the core memory/docs tools, and MCP tools — implement tools.Tool with a
// hand-written JSON schema and a Call(ctx, json.RawMessage) dispatch.
// The adapter passes both through unchanged, so no tool implementation is
// rewritten and the Engine's per-run gating (only enabled tools get wrapped)
// remains the single dispatch rule: a tool that is not adapted is invisible
// to the model and unreachable at dispatch.
package maf

import (
	"context"
	"encoding/json"

	"github.com/dkr290/agenticgo/internal/tools"
	"github.com/microsoft/agent-framework-go/tool"
)

// funcTool adapts a tools.Tool to MAF's tool.FuncTool interface.
type funcTool struct {
	t tools.Tool
}

// AdaptTool wraps one tools.Tool as a MAF tool.FuncTool.
func AdaptTool(t tools.Tool) tool.FuncTool {
	return funcTool{t: t}
}

// AdaptTools wraps a slice of tools.Tool, preserving order.
func AdaptTools(ts []tools.Tool) []tool.Tool {
	out := make([]tool.Tool, 0, len(ts))
	for _, t := range ts {
		out = append(out, AdaptTool(t))
	}
	return out
}

func (a funcTool) Name() string        { return a.t.Name() }
func (a funcTool) Description() string { return a.t.Description() }

// Schema returns the tool's input JSON Schema. MAF accepts "any" here and
// serializes it verbatim into the provider request, so the existing
// hand-written schemas pass through untouched.
func (a funcTool) Schema() any { return a.t.Parameters() }

// ReturnSchema returns nil: results are plain strings (text content), not
// structured outputs.
func (a funcTool) ReturnSchema() any { return nil }

// Call invokes the underlying tool. MAF delivers arguments as a raw JSON
// string, which is exactly what tools.Tool.Call already takes.
func (a funcTool) Call(ctx context.Context, args string) (any, error) {
	return a.t.Call(ctx, json.RawMessage(args))
}
