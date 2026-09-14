package llm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseServer replays a canned SSE stream for /chat/completions.
func sseServer(t *testing.T, frames []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runStream executes a streaming ChatCompletion against srv and returns the
// final message plus the concatenated text forwarded to onDelta.
func runStream(t *testing.T, srv *httptest.Server) (Message, string) {
	t.Helper()
	p := NewOpenAI(srv.URL, "", "m")
	var deltas strings.Builder
	msg, err := p.ChatCompletion(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Stream:   true,
	}, func(d Delta) error {
		deltas.WriteString(d.Content)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	return msg, deltas.String()
}

// A single tool call streamed in argument fragments must be reassembled.
func TestStreamSingleToolCall(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Let me check."}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"pa"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"x\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	})
	msg, text := runStream(t, srv)

	if text != "Let me check." || msg.Content != "Let me check." {
		t.Errorf("content = %q (deltas %q)", msg.Content, text)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d: %+v", len(msg.ToolCalls), msg.ToolCalls)
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "read_file" || tc.Arguments != `{"path":"x"}` {
		t.Errorf("bad tool call: %+v", tc)
	}
}

// Two parallel tool calls streamed one delta per chunk must stay separated —
// regression test for the old bug where slice-position keying merged them.
func TestStreamParallelToolCalls(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"list_files","arguments":"{\"dir\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\".\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	})
	msg, _ := runStream(t, srv)

	if len(msg.ToolCalls) != 2 {
		t.Fatalf("want 2 tool calls, got %d: %+v", len(msg.ToolCalls), msg.ToolCalls)
	}
	a, b := msg.ToolCalls[0], msg.ToolCalls[1]
	if a.Name != "read_file" || a.Arguments != `{"path":"a.txt"}` || a.ID != "call_a" {
		t.Errorf("bad first tool call: %+v", a)
	}
	if b.Name != "list_files" || b.Arguments != `{"dir":"."}` || b.ID != "call_b" {
		t.Errorf("bad second tool call: %+v", b)
	}
}

// A tool call that streams no argument bytes must still be emitted.
func TestStreamEmptyArgsToolCall(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"list_files","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	})
	msg, _ := runStream(t, srv)

	if len(msg.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d: %+v", len(msg.ToolCalls), msg.ToolCalls)
	}
	if msg.ToolCalls[0].Name != "list_files" || msg.ToolCalls[0].Arguments != "" {
		t.Errorf("bad tool call: %+v", msg.ToolCalls[0])
	}
}

// A mid-stream error payload must surface as an error, not an empty reply.
func TestStreamErrorPayload(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"index":0,"delta":{"content":"partial"}}]}`,
		`{"error":{"message":"boom: model exploded","type":"server_error"}}`,
	})
	p := NewOpenAI(srv.URL, "", "m")
	_, err := p.ChatCompletion(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Stream:   true,
	}, func(Delta) error { return nil })
	if err == nil {
		t.Fatal("expected an error from the mid-stream error payload")
	}
	if !strings.Contains(err.Error(), "boom: model exploded") {
		t.Errorf("error should carry the provider message, got: %v", err)
	}
}

// Plain text streaming without tools must forward all deltas and finish done.
func TestStreamTextOnly(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	})
	msg, text := runStream(t, srv)

	if text != "Hello" || msg.Content != "Hello" {
		t.Errorf("content = %q (deltas %q)", msg.Content, text)
	}
	if len(msg.ToolCalls) != 0 {
		t.Errorf("unexpected tool calls: %+v", msg.ToolCalls)
	}
}
