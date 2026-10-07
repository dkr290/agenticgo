package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/microsoft/agent-framework-go/tool"
)

// funcToolAdapter adapts a MAF tool.FuncTool back to the tools.Tool interface
// so functool-built tools can live in the legacy Registry while the migration
// to MAF is in progress. It is the inverse of maf.AdaptTool and disappears
// when the registry itself becomes a []tool.Tool slice.
type funcToolAdapter struct {
	ft     tool.FuncTool
	schema map[string]any
}

// AdaptFuncTool wraps a MAF FuncTool as a tools.Tool. It panics if the
// tool's schema cannot be rendered as a JSON object map — functool returns
// its generated schema as a *jsonschema.Schema struct, so it is round-tripped
// through JSON into the map shape the legacy registry serves. The panic
// exists to catch exotic hand-rolled FuncTools at construction, not at
// request time.
func AdaptFuncTool(ft tool.FuncTool) Tool {
	schema, err := schemaToMap(ft.Schema())
	if err != nil {
		panic(fmt.Sprintf("tools.AdaptFuncTool: tool %q: %v", ft.Name(), err))
	}
	return &funcToolAdapter{ft: ft, schema: schema}
}

// schemaToMap normalizes a MAF tool schema (a map or a marshalable struct
// like *jsonschema.Schema) into the map[string]any the registry serves.
func schemaToMap(s any) (map[string]any, error) {
	if m, ok := s.(map[string]any); ok {
		return m, nil
	}
	data, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("marshal schema: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("schema is not a JSON object: %w", err)
	}
	return m, nil
}

func (a *funcToolAdapter) Name() string               { return a.ft.Name() }
func (a *funcToolAdapter) Description() string        { return a.ft.Description() }
func (a *funcToolAdapter) Parameters() map[string]any { return a.schema }

// Call forwards the raw JSON arguments. MAF functool results are any; our
// tools return strings, so non-string results are JSON-marshalled — functool
// tools in this codebase return strings, so this is a safety net only.
func (a *funcToolAdapter) Call(ctx context.Context, args json.RawMessage) (string, error) {
	result, err := a.ft.Call(ctx, string(args))
	if err != nil {
		return "", err
	}
	if s, ok := result.(string); ok {
		return s, nil
	}
	data, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal tool result: %w", err)
	}
	return string(data), nil
}

// UnwrapFuncTool returns the underlying MAF FuncTool when t was produced by
// AdaptFuncTool — used by the MAF engine path to recover the native tool from
// the legacy registry. ok is false for legacy tools.Tool implementations.
func UnwrapFuncTool(t Tool) (tool.FuncTool, bool) {
	if a, ok := t.(*funcToolAdapter); ok {
		return a.ft, true
	}
	return nil, false
}
