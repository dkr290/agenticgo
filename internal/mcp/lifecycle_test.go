package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestFailedConnectionCanBeDeletedOrUpdated(t *testing.T) {
	for _, action := range []string{"delete", "rename"} {
		t.Run(action, func(t *testing.T) {
			m, err := Open(filepath.Join(t.TempDir(), "mcp.json"))
			if err != nil {
				t.Fatal(err)
			}
			defer m.CloseAll()
			srv, err := m.Add(ServerConfig{Name: "broken", Transport: "stdio", Command: "nonexistent-agenticgo-mcp-command"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.Connect(context.Background(), srv.ID); err == nil {
				t.Fatal("expected connection failure")
			}
			if action == "delete" {
				err = m.Delete(srv.ID)
			} else {
				_, err = m.Update(srv.ID, ServerConfig{Name: "renamed", Transport: "stdio", Command: "still-missing"})
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.Connect(context.Background(), "unknown"); err == nil {
				t.Fatal("expected unknown ID error")
			}
		})
	}
}

// Run a real stdio MCP server in a child copy of the test binary.
func TestStdioHelper(t *testing.T) {
	if os.Getenv("AGENTICGO_MCP_HELPER") != "1" {
		return
	}
	s := sdk.NewServer(&sdk.Implementation{Name: "fixture", Version: "1"}, nil)
	sdk.AddTool(s, &sdk.Tool{Name: "echo", Description: "fixture echo"}, func(ctx context.Context, _ *sdk.CallToolRequest, in struct {
		Text string `json:"text"`
	}) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: in.Text}}}, nil, nil
	})
	if err := s.Run(context.Background(), &sdk.StdioTransport{}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestStdioSurvivesConnectContextCancellation(t *testing.T) {
	t.Setenv("AGENTICGO_MCP_HELPER", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m, err := Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.CloseAll()
	srv, err := m.Add(ServerConfig{Name: "stdio", Transport: "stdio", Command: exe, Args: []string{"-test.run=^TestStdioHelper$"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := m.Connect(ctx, srv.ID); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	callCtx, callCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer callCancel()
	for range 3 {
		out, err := m.CallTool(callCtx, "mcp_stdio_echo", json.RawMessage(`{"text":"still alive"}`))
		if err != nil || !strings.Contains(out, "still alive") {
			t.Fatalf("connection died after Connect: %q %v", out, err)
		}
	}
}
