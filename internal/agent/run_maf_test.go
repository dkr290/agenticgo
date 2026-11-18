package agent

// run_maf_test.go exercises the MAF execution path end to end
// against a mock OpenAI Chat Completions server: tool call round-trip, WS
// event stream, and history persistence in the legacy row format.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dkr290/agenticgo/internal/agents"
	"github.com/dkr290/agenticgo/internal/llm"
)

// chatCompletionsMock serves a scripted two-turn exchange: the first request
// (with tools) returns a memory_save tool call; the second (carrying the tool
// result) returns the final answer. Both stream SSE in the chunk shape the
// openai-go accumulator assembles: role chunk, tool-call header, arguments
// delta, finish chunk.
func chatCompletionsMock(t *testing.T) *httptest.Server {
	t.Helper()
	var requests int
	writeChunks := func(w http.ResponseWriter, chunks ...string) {
		for _, c := range chunks {
			_, _ = w.Write([]byte("data: " + c + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)

		if requests == 1 {
			writeChunks(w,
				`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
				`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"memory_save","arguments":""}}]},"finish_reason":null}]}`,
				`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"content\":\"User likes Go\"}"}}]},"finish_reason":null}]}`,
				`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			)
			return
		}
		writeChunks(w,
			`{"id":"c2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`{"id":"c2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Noted, you like Go."},"finish_reason":null}]}`,
			`{"id":"c2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		)
	}))
}

func TestRunMAFEndToEnd(t *testing.T) {
	srv := chatCompletionsMock(t)
	defer srv.Close()

	e, _ := newTestEngine(t)
	// Point the provider lookup at the mock server.
	e.SetProviderLookup(&trackingLookup{p: llm.NewOpenAI(srv.URL+"/v1", "", "test-model")})

	var events []Event
	reply, err := e.Run(context.Background(), "demo", "s1", "remember that I like Go", "", nil, func(ev Event) {
		events = append(events, ev)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(reply, "Noted, you like Go") {
		t.Errorf("reply = %q", reply)
	}

	// Event stream: at minimum a tool_call, a tool_result, and done.
	var kinds []string
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
	}
	joined := strings.Join(kinds, ",")
	if !strings.Contains(joined, "tool_call") || !strings.Contains(joined, "tool_result") || !strings.Contains(joined, "done") {
		t.Errorf("event kinds = %v", kinds)
	}

	// History persisted in the legacy row format: user, assistant+tool_calls,
	// tool result, final assistant.
	rows, err := e.store.Messages(context.Background(), "demo", "s1", 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("stored %d rows, want 4: %+v", len(rows), rows)
	}
	if rows[0].Role != "user" {
		t.Errorf("row 0 = %q", rows[0].Role)
	}
	if rows[1].Role != "assistant" || !strings.Contains(rows[1].ToolCalls, "memory_save") {
		t.Errorf("row 1 = %+v", rows[1])
	}
	if rows[2].Role != "tool" || rows[2].ToolCallID != "call_1" {
		t.Errorf("row 2 = %+v", rows[2])
	}
	if rows[3].Role != "assistant" || !strings.Contains(rows[3].Content, "Noted") {
		t.Errorf("row 3 = %+v", rows[3])
	}

	// The tool actually ran: memory_save persisted to the knowledge store.
	knowledge, err := e.store.Knowledge(context.Background(), "demo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(knowledge) != 1 || !strings.Contains(knowledge[0].Content, "User likes Go") {
		t.Errorf("knowledge = %+v", knowledge)
	}
}

// TestRunMAFDisableReasoning asserts the per-agent disable_reasoning toggle
// puts reasoning_effort="none" on the wire, and that the field is omitted
// entirely when the toggle is unset (ordinary chat models must see no
// reasoning parameters at all).
func TestRunMAFDisableReasoning(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, c := range []string{
			`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		} {
			_, _ = w.Write([]byte("data: " + c + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	e, ar := newTestEngine(t)
	e.SetProviderLookup(&trackingLookup{p: llm.NewOpenAI(srv.URL+"/v1", "", "test-model")})

	run := func(session string) map[string]any {
		t.Helper()
		before := len(bodies)
		if _, err := e.Run(context.Background(), "demo", session, "hi", "", nil, func(Event) {}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(bodies) != before+1 {
			t.Fatalf("expected 1 request, got %d", len(bodies)-before)
		}
		return bodies[len(bodies)-1]
	}

	// Unset toggle: no reasoning fields on the wire.
	if body := run("plain"); body["reasoning_effort"] != nil {
		t.Errorf("unset toggle sent reasoning_effort: %v", body["reasoning_effort"])
	}

	// Enabled toggle: reasoning_effort="none".
	on := true
	if _, err := ar.UpdateConfig("demo", agents.AgentConfig{DisableReasoning: &on}); err != nil {
		t.Fatal(err)
	}
	if body := run("disabled"); body["reasoning_effort"] != "none" {
		t.Errorf("enabled toggle: reasoning_effort = %v, want \"none\"", body["reasoning_effort"])
	}

	// Explicit false: omitted again.
	off := false
	if _, err := ar.UpdateConfig("demo", agents.AgentConfig{DisableReasoning: &off}); err != nil {
		t.Fatal(err)
	}
	if body := run("explicit-off"); body["reasoning_effort"] != nil {
		t.Errorf("explicit false sent reasoning_effort: %v", body["reasoning_effort"])
	}
}

// TestRunMAFResponsesAPI: a provider set to the Responses API sends agent
// runs to /responses, statelessly, with per-agent settings mapped to the
// Responses parameter names.
func TestRunMAFResponsesAPI(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		resp := `{"id":"resp_1","object":"response","created_at":1,"status":"%s","model":"m","output":%s}`
		msg := `[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}]}]`
		for _, ev := range []string{
			`{"type":"response.created","sequence_number":0,"response":` + fmt.Sprintf(resp, "in_progress", "[]") + `}`,
			`{"type":"response.output_text.delta","sequence_number":1,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"ok"}`,
			`{"type":"response.completed","sequence_number":2,"response":` + fmt.Sprintf(resp, "completed", msg) + `}`,
		} {
			_, _ = w.Write([]byte("data: " + ev + "\n\n"))
		}
	}))
	defer srv.Close()

	e, ar := newTestEngine(t)
	prov := llm.NewOpenAI(srv.URL+"/v1", "", "test-model")
	prov.SetAPI(llm.APIResponses)
	e.SetProviderLookup(&trackingLookup{p: prov})

	temp, maxTok := 0.5, 100
	if _, err := ar.UpdateConfig("demo", agents.AgentConfig{Temperature: &temp, MaxTokens: &maxTok}); err != nil {
		t.Fatal(err)
	}
	reply, err := e.Run(context.Background(), "demo", "s", "hi", "", nil, func(Event) {})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(reply, "ok") {
		t.Errorf("reply = %q, want ok", reply)
	}
	if len(bodies) != 1 {
		t.Fatalf("expected 1 /responses request, got %d", len(bodies))
	}
	b := bodies[0]
	if b["store"] != false {
		t.Errorf("store = %v, want false (history is owned by SQLite)", b["store"])
	}
	if b["previous_response_id"] != nil {
		t.Errorf("unexpected previous_response_id %v", b["previous_response_id"])
	}
	if b["max_output_tokens"] != float64(100) || b["temperature"] != 0.5 {
		t.Errorf("sampling params not mapped: %v", b)
	}
	if b["reasoning"] != nil {
		t.Errorf("unset disable_reasoning sent reasoning: %v", b["reasoning"])
	}
}

// TestRunMAFAPIErrorDetail: the endpoint's error message reaches the caller
// instead of a bare "400 Bad Request".
func TestRunMAFAPIErrorDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"Unsupported value: 'reasoning_effort'","type":"invalid_request_error","param":"reasoning_effort","code":"unsupported_value"}}`))
	}))
	defer srv.Close()

	e, _ := newTestEngine(t)
	e.SetProviderLookup(&trackingLookup{p: llm.NewOpenAI(srv.URL+"/v1", "", "test-model")})
	var evErr error
	_, err := e.Run(context.Background(), "demo", "s", "hi", "", nil, func(ev Event) {
		if ev.Kind == "error" {
			evErr = ev.Err
		}
	})
	if err == nil || !strings.Contains(err.Error(), "Unsupported value: 'reasoning_effort' (param: reasoning_effort)") {
		t.Fatalf("err = %v, want endpoint message", err)
	}
	if evErr == nil || !strings.Contains(evErr.Error(), "Unsupported value") {
		t.Errorf("error event = %v, want endpoint message", evErr)
	}
}

// TestRunMAFStreamedTextMatchesReply: when there is no intermediate narration,
// text deltas concatenate to the final reply.
func TestRunMAFStreamedTextMatchesReply(t *testing.T) {
	srv := chatCompletionsMock(t)
	defer srv.Close()

	e, _ := newTestEngine(t)
	e.SetProviderLookup(&trackingLookup{p: llm.NewOpenAI(srv.URL+"/v1", "", "test-model")})

	var streamed strings.Builder
	reply, err := e.Run(context.Background(), "demo", "s2", "hi", "", nil, func(ev Event) {
		if ev.Kind == "text" {
			streamed.WriteString(ev.Text)
		}
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply == "" || streamed.String() != reply {
		t.Errorf("streamed %q != reply %q", streamed.String(), reply)
	}
}
