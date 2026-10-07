package agent

// run_maf_test.go exercises the MAF execution path (cfg.UseMAF) end to end
// against a mock OpenAI Chat Completions server: tool call round-trip, WS
// event stream, and history persistence in the legacy row format.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

	e, ar := newTestEngine(t)
	e.cfg.UseMAF = true
	// Point the provider lookup at the mock server.
	e.SetProviderLookup(staticLookup{p: llm.NewOpenAI(srv.URL+"/v1", "", "test-model")})

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
	ag, _ := ar.Get("demo")
	_ = ag
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

// TestRunMAFStreamedTextMatchesReply: text deltas emitted during the run
// concatenate to the returned reply (the WS contract the SPA relies on).
func TestRunMAFStreamedTextMatchesReply(t *testing.T) {
	srv := chatCompletionsMock(t)
	defer srv.Close()

	e, _ := newTestEngine(t)
	e.cfg.UseMAF = true
	e.SetProviderLookup(staticLookup{p: llm.NewOpenAI(srv.URL+"/v1", "", "test-model")})

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

// staticLookup resolves every provider name to the same provider (the mock).
type staticLookup struct{ p llm.Provider }

func (s staticLookup) GetLLM(string) (llm.Provider, error) { return s.p, nil }

// jsonMarshal is a tiny helper to keep the mock payloads readable.
func jsonMarshal(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

var _ = jsonMarshal
