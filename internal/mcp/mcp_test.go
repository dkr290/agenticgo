package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolNameAndPrefix(t *testing.T) {
	if got := ToolName("kube", "pods_list"); got != "mcp_kube_pods_list" {
		t.Fatalf("ToolName = %q", got)
	}
	if !IsMCPToolName("mcp_kube_pods_list") {
		t.Fatal("IsMCPToolName should be true")
	}
	if IsMCPToolName("read_file") {
		t.Fatal("IsMCPToolName should be false for built-ins")
	}
}

func TestValidateServer(t *testing.T) {
	cases := []struct {
		name    string
		srv     ServerConfig
		wantErr bool
	}{
		{"stdio ok", ServerConfig{Name: "kube", Transport: "stdio", Command: "npx"}, false},
		{"stdio no command", ServerConfig{Name: "kube", Transport: "stdio"}, true},
		{"http ok", ServerConfig{Name: "remote", Transport: "http", URL: "http://x/mcp"}, false},
		{"http no url", ServerConfig{Name: "remote", Transport: "http"}, true},
		{"bad transport", ServerConfig{Name: "x", Transport: "carrier-pigeon"}, true},
		{"bad name", ServerConfig{Name: "Bad Name!", Transport: "stdio", Command: "x"}, true},
		{"empty name", ServerConfig{Transport: "stdio", Command: "x"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateServer(&tc.srv); (err != nil) != tc.wantErr {
				t.Fatalf("validateServer() err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestAddListDeleteRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp_servers.json")
	m, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(ServerConfig{Name: "kube", Transport: "stdio", Command: "npx", Args: []string{"kubernetes-mcp-server@latest"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(ServerConfig{Name: "kube", Transport: "stdio", Command: "x"}); err == nil {
		t.Fatal("duplicate name should fail")
	}
	list := m.ListServers()
	if len(list) != 1 || list[0].Name != "kube" || list[0].Connected {
		t.Fatalf("ListServers = %+v", list)
	}

	// Persisted: reopen and confirm.
	m2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	list2 := m2.ListServers()
	if len(list2) != 1 || list2[0].Name != "kube" {
		t.Fatalf("reopened ListServers = %+v", list2)
	}

	if err := m2.Delete(list2[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := m2.ListServers(); len(got) != 0 {
		t.Fatalf("after delete ListServers = %+v", got)
	}
}

func TestResolveWithUnderscoreNames(t *testing.T) {
	m, err := Open(filepath.Join(t.TempDir(), "x.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(ServerConfig{Name: "kube", Transport: "stdio", Command: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(ServerConfig{Name: "kube_system", Transport: "stdio", Command: "x"}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		discovered string
		wantServer string
		wantTool   string
		wantOK     bool
	}{
		{"mcp_kube_pods_list", "kube", "pods_list", true},
		{"mcp_kube_system_get", "kube_system", "get", true}, // longest prefix wins
		{"mcp_kube_", "kube", "", false},                    // empty tool part
		{"mcp_unknown_tool", "", "", false},                 // no matching server
		{"read_file", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.discovered, func(t *testing.T) {
			id, tool, ok := m.resolveLocked(tc.discovered)
			if ok != tc.wantOK {
				t.Fatalf("resolveLocked(%q) ok = %v, want %v", tc.discovered, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got := m.servers[id].Name; got != tc.wantServer {
				t.Fatalf("server = %q, want %q", got, tc.wantServer)
			}
			if tool.Name != tc.wantTool {
				t.Fatalf("tool = %q, want %q", tool.Name, tc.wantTool)
			}
		})
	}
}

// TestEndToEnd spins up an in-memory MCP server with one tool and exercises
// Connect → Tools → Lookup → CallTool through the real SDK.
func TestEndToEnd(t *testing.T) {
	// In-memory server with one tool that echoes its input.
	server := sdk.NewServer(&sdk.Implementation{Name: "test-server", Version: "0.0.1"}, nil)
	type echoIn struct {
		Text string `json:"text"`
	}
	sdk.AddTool[echoIn, any](server, &sdk.Tool{
		Name:        "echo_text",
		Description: "Echo the input text",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in echoIn) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: "echo: " + in.Text}},
		}, nil, nil
	})

	m, err := Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Inject the in-memory transport so no process/network is needed.
	m.dial = func(ctx context.Context, srv *ServerConfig) (sdk.Transport, error) {
		clientT, serverT := sdk.NewInMemoryTransports()
		if _, err := server.Connect(ctx, serverT, nil); err != nil {
			return nil, err
		}
		return clientT, nil
	}

	srv, err := m.Add(ServerConfig{Name: "test", Transport: "stdio", Command: "unused"})
	if err != nil {
		t.Fatal(err)
	}

	st, err := m.Connect(context.Background(), srv.ID)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !st.Connected || st.ToolCount != 1 {
		t.Fatalf("status after connect = %+v", st)
	}

	tools := m.Tools()
	if len(tools) != 1 || tools[0].DiscoveredName() != "mcp_test_echo_text" {
		t.Fatalf("Tools = %+v", tools)
	}
	ti, ok := m.Lookup("mcp_test_echo_text")
	if !ok || ti.Description != "Echo the input text" {
		t.Fatalf("Lookup = %+v, %v", ti, ok)
	}
	if ti.Schema == nil {
		t.Fatal("schema should be captured at discovery")
	}

	out, err := m.CallTool(context.Background(), "mcp_test_echo_text", json.RawMessage(`{"text":"hello"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !strings.Contains(out, "echo: hello") {
		t.Fatalf("CallTool output = %q", out)
	}

	if _, err := m.CallTool(context.Background(), "mcp_test_missing", nil); err == nil {
		t.Fatal("CallTool for unknown tool should fail")
	}

	m.CloseAll()
	if got := m.Tools(); len(got) != 0 {
		t.Fatalf("Tools after CloseAll = %+v", got)
	}
}
