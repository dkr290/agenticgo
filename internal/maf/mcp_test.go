package maf

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkr290/agenticgo/internal/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// testMCPServer is an in-process MCP server with one echo tool, wired to a
// Manager over an in-memory transport so the real discovery path runs.
func testMCPServer(t *testing.T) *mcp.Manager {
	t.Helper()

	server := sdk.NewServer(&sdk.Implementation{Name: "test-server", Version: "v0.0.1"}, nil)
	type echoIn struct {
		Text string `json:"text" jsonschema:"Text to echo back"`
	}
	sdk.AddTool[echoIn, any](server, &sdk.Tool{
		Name:        "echo",
		Description: "Echoes the input text",
	}, func(_ context.Context, _ *sdk.CallToolRequest, args echoIn) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: "echo: " + args.Text}},
		}, nil, nil
	})

	mgr, err := mcp.Open(filepath.Join(t.TempDir(), "mcp_servers.json"))
	if err != nil {
		t.Fatalf("mcp.Open: %v", err)
	}
	t.Cleanup(mgr.CloseAll)

	// In-memory transport: client connects to the in-process server.
	mgr.SetDial(func(ctx context.Context, _ *mcp.ServerConfig) (sdk.Transport, error) {
		clientT, serverT := sdk.NewInMemoryTransports()
		if _, err := server.Connect(ctx, serverT, nil); err != nil {
			return nil, err
		}
		return clientT, nil
	})

	added, err := mgr.Add(mcp.ServerConfig{Name: "test", Transport: "stdio", Command: "true"})
	if err != nil {
		t.Fatalf("mgr.Add: %v", err)
	}
	if _, err := mgr.Connect(context.Background(), added.ID); err != nil {
		t.Fatalf("mgr.Connect: %v", err)
	}
	return mgr
}

func TestMCPTool_WrapsAndCalls(t *testing.T) {
	mgr := testMCPServer(t)

	info, ok := mgr.Lookup("mcp_test_echo")
	if !ok {
		t.Fatalf("tool not discovered; have %v", mgr.Tools())
	}
	ft := NewMCPTool("mcp_test_echo", info, mgr)

	if ft.Name() != "mcp_test_echo" {
		t.Errorf("Name() = %q", ft.Name())
	}
	if ft.Description() != "Echoes the input text" {
		t.Errorf("Description() = %q", ft.Description())
	}

	// Schema must survive the round-trip as a JSON object with a text property.
	schemaJSON, err := json.Marshal(ft.Schema())
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	if !strings.Contains(string(schemaJSON), `"text"`) {
		t.Errorf("schema missing text property: %s", schemaJSON)
	}

	// Call through the manager: real MCP round-trip over the in-memory transport.
	result, err := ft.Call(context.Background(), `{"text":"hello"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	s, ok := result.(string)
	if !ok || !strings.Contains(s, "echo: hello") {
		t.Errorf("Call result = %v", result)
	}
}

func TestMCPTool_SoftFailsWhenServerGone(t *testing.T) {
	mgr := testMCPServer(t)

	info, ok := mgr.Lookup("mcp_test_echo")
	if !ok {
		t.Fatal("tool not discovered")
	}
	ft := NewMCPTool("mcp_test_echo", info, mgr)

	// Kill the server-side connection, then prevent lazy reconnect by breaking
	// the dial: the call must return an error STRING, not a Go error.
	mgr.CloseAll()
	mgr.SetDial(func(context.Context, *mcp.ServerConfig) (sdk.Transport, error) {
		return nil, context.DeadlineExceeded
	})

	result, err := ft.Call(context.Background(), `{"text":"hello"}`)
	if err != nil {
		t.Fatalf("soft-fail violated: Call returned Go error: %v", err)
	}
	s, _ := result.(string)
	if !strings.HasPrefix(s, "error:") {
		t.Errorf("result = %q, want error string", s)
	}
}
