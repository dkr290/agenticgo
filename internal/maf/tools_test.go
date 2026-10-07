package maf

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dkr290/agenticgo/internal/tools"
)

// fakeTool is a minimal tools.Tool implementation for adapter tests.
type fakeTool struct {
	name    string
	desc    string
	params  map[string]any
	gotArgs json.RawMessage
	result  string
	err     error
}

func (f *fakeTool) Name() string               { return f.name }
func (f *fakeTool) Description() string        { return f.desc }
func (f *fakeTool) Parameters() map[string]any { return f.params }
func (f *fakeTool) Call(_ context.Context, args json.RawMessage) (string, error) {
	f.gotArgs = args
	return f.result, f.err
}

func TestAdaptTool_Passthrough(t *testing.T) {
	params := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string"},
		},
	}
	ft := &fakeTool{name: "read_file", desc: "read a file", params: params, result: "contents"}

	at := AdaptTool(ft)

	if at.Name() != "read_file" {
		t.Errorf("Name() = %q, want %q", at.Name(), "read_file")
	}
	if at.Description() != "read a file" {
		t.Errorf("Description() = %q, want %q", at.Description(), "read a file")
	}
	gotSchema, err := json.Marshal(at.Schema())
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	wantSchema, _ := json.Marshal(params)
	if string(gotSchema) != string(wantSchema) {
		t.Errorf("Schema() = %s, want %s", gotSchema, wantSchema)
	}
	if at.ReturnSchema() != nil {
		t.Errorf("ReturnSchema() = %v, want nil", at.ReturnSchema())
	}
}

func TestAdaptTool_CallForwardsRawArgs(t *testing.T) {
	ft := &fakeTool{name: "exec", result: "ok"}
	at := AdaptTool(ft)

	args := `{"cmd":"ls","args":["-la"]}`
	res, err := at.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res != "ok" {
		t.Errorf("Call result = %v, want %q", res, "ok")
	}
	if string(ft.gotArgs) != args {
		t.Errorf("forwarded args = %s, want %s", ft.gotArgs, args)
	}
}

func TestAdaptTools_PreservesOrderAndCount(t *testing.T) {
	a := &fakeTool{name: "a"}
	b := &fakeTool{name: "b"}
	c := &fakeTool{name: "c"}

	adapted := AdaptTools([]tools.Tool{a, b, c})
	if len(adapted) != 3 {
		t.Fatalf("AdaptTools returned %d tools, want 3", len(adapted))
	}
	for i, want := range []string{"a", "b", "c"} {
		if adapted[i].Name() != want {
			t.Errorf("adapted[%d].Name() = %q, want %q", i, adapted[i].Name(), want)
		}
	}
}
