package agent

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

type wireCall struct {
	index          int
	id, name, args string
}

func callsChunk(t *testing.T, finish string, calls ...wireCall) string {
	t.Helper()
	var deltas []any
	for _, call := range calls {
		fn := map[string]any{"arguments": call.args}
		delta := map[string]any{"index": call.index, "function": fn}
		if call.id != "" {
			delta["id"], delta["type"] = call.id, "function"
		}
		if call.name != "" {
			fn["name"] = call.name
		}
		deltas = append(deltas, delta)
	}
	delta := map[string]any{"role": "assistant"}
	if len(deltas) > 0 {
		delta["tool_calls"] = deltas
	}
	data, err := json.Marshal(map[string]any{
		"id": "tools", "object": "chat.completion.chunk",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRunStreamingToolCallShapes(t *testing.T) {
	a := wireCall{0, "save", "memory_save", `{"content":"first fact"}`}
	b := wireCall{1, "observe", "record_observation", `{"content":"second observation"}`}
	for _, tc := range []struct {
		name   string
		chunks []string
	}{
		{"packed", []string{callsChunk(t, "", a, b), callsChunk(t, "tool_calls")}},
		{"terminal", []string{callsChunk(t, "tool_calls", a, b)}},
		{"interleaved", []string{
			callsChunk(t, "", wireCall{0, "save", "memory_save", `{"content":`}),
			callsChunk(t, "", wireCall{1, "observe", "record_observation", `{"content":`}),
			callsChunk(t, "", wireCall{0, "", "", `"first fact"}`}),
			callsChunk(t, "", wireCall{1, "", "", `"second observation"}`}),
			callsChunk(t, "tool_calls"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := newTestEngine(t)
			requests := 0
			e.SetProviderLookup(&trackingLookup{p: scriptedProvider(t, func(call int, req map[string]any) []string {
				requests = call
				if call == 1 {
					return tc.chunks
				}
				// Both calls and both results must reach the next completion.
				var calls, results int
				for _, raw := range req["messages"].([]any) {
					m := raw.(map[string]any)
					if tcs, ok := m["tool_calls"].([]any); ok {
						calls += len(tcs)
					}
					if m["role"] == "tool" {
						results++
					}
				}
				if calls != 2 || results != 2 {
					t.Errorf("follow-up has %d calls / %d results: %v", calls, results, req["messages"])
				}
				return sseText("Saved both.")
			})})
			var events []Event
			reply, err := e.Run(context.Background(), "demo", "s", "save both", "", nil, func(ev Event) { events = append(events, ev) })
			if err != nil || reply != "Saved both." || requests != 2 {
				t.Fatalf("reply=%q requests=%d err=%v", reply, requests, err)
			}
			knowledge, err := e.store.Knowledge(context.Background(), "demo", 10)
			if err != nil || len(knowledge) != 1 || knowledge[0].Content != "first fact" {
				t.Fatalf("memory tool: %+v %v", knowledge, err)
			}
			observations, err := e.store.ListObservations(context.Background(), "demo", 10)
			if err != nil || len(observations) != 1 || observations[0].Content != "second observation" {
				t.Fatalf("observation tool: %+v %v", observations, err)
			}
			rows, err := e.store.Messages(context.Background(), "demo", "s", 40)
			if err != nil || len(rows) != 5 {
				t.Fatalf("history: %+v %v", rows, err)
			}
			if rows[2].Name != "memory_save" || rows[3].Name != "record_observation" {
				t.Errorf("result names lost: %+v", rows)
			}
			var resultNames []string
			var callCount int
			for _, ev := range events {
				if ev.Kind == "tool_result" {
					resultNames = append(resultNames, ev.ToolName)
				}
				if ev.Kind == "tool_call" {
					callCount++
				}
			}
			if callCount != 2 || strings.Join(resultNames, ",") != "memory_save,record_observation" {
				t.Errorf("duplicate/missing tool events: %+v", events)
			}
		})
	}
}

func TestRunAttachmentsPreserveInputOnce(t *testing.T) {
	for _, vision := range []bool{false, true} {
		t.Run(fmt.Sprint(vision), func(t *testing.T) {
			e, ar := newTestEngine(t)
			if _, err := ar.UpdateConfig("demo", agents.AgentConfig{Vision: &vision}); err != nil {
				t.Fatal(err)
			}
			e.SetProviderLookup(&trackingLookup{p: scriptedProvider(t, func(call int, req map[string]any) []string {
				wantImages := 0
				if vision && call < 3 {
					wantImages = 1
				}
				if got := countImages(req); got != wantImages {
					t.Errorf("request %d images=%d, want %d", call, got, wantImages)
				}
				users := 0
				for _, raw := range req["messages"].([]any) {
					if raw.(map[string]any)["role"] == "user" {
						users++
					}
				}
				wantUsers := 1
				if call == 3 {
					wantUsers = 2
				}
				if users != wantUsers {
					t.Errorf("request %d has %d user messages, want %d", call, users, wantUsers)
				}
				if call == 1 {
					return sseToolCall("save", "memory_save", `{"content":"a fact"}`)
				}
				return sseText("done")
			})})
			for _, images := range [][]string{{"data:image/png;base64,aW1hZ2U="}, nil} {
				// Identical prompts are separate turns, not content-deduplicated.
				if _, err := e.Run(context.Background(), "demo", "s", "look", "", images, nil); err != nil {
					t.Fatal(err)
				}
			}
			rows, err := e.store.Messages(context.Background(), "demo", "s", 40)
			if err != nil || len(rows) != 6 {
				t.Fatalf("duplicated or missing persisted turns: %+v %v", rows, err)
			}
		})
	}
}

func TestRunFetchedImagesSurviveLaterRounds(t *testing.T) {
	e, ar := newTestEngine(t)
	vision := true
	if _, err := ar.UpdateConfig("demo", agents.AgentConfig{Vision: &vision}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.png", "b.png"} {
		if _, err := ar.SaveImage("demo", name, []byte(name)); err != nil {
			t.Fatal(err)
		}
	}
	want := []int{0, 1, 2, 2}
	e.SetProviderLookup(&trackingLookup{p: scriptedProvider(t, func(call int, req map[string]any) []string {
		if call > len(want) {
			t.Errorf("unexpected extra completion %d", call)
			return sseText("done")
		}
		if got := countImages(req); got != want[call-1] {
			t.Errorf("request %d images=%d, want %d", call, got, want[call-1])
		}
		switch call {
		case 1:
			return sseToolCall("a", "fetch_agent_image", `{"name":"a.png"}`)
		case 2:
			return sseToolCall("b", "fetch_agent_image", `{"name":"b.png"}`)
		case 3:
			return sseToolCall("save", "memory_save", `{"content":"visual finding"}`)
		default:
			return sseText("done")
		}
	})})
	if _, err := e.Run(context.Background(), "demo", "s", "compare", "", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRunFailedFollowupKeepsCheckpoint(t *testing.T) {
	e, _ := newTestEngine(t)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, chunk := range sseToolCall("save", "memory_save", `{"content":"tool already ran"}`) {
				fmt.Fprintf(w, "data: %s\n\n", chunk)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		// The checkpoint must be visible while the follow-up is still running.
		rows, err := e.store.Messages(context.Background(), "demo", "s", 40)
		if err != nil || len(rows) != 3 || rows[2].Name != "memory_save" {
			t.Errorf("checkpoint before follow-up: %+v %v", rows, err)
		}
		http.Error(w, `{"error":{"message":"follow-up failed"}}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	e.SetProviderLookup(&trackingLookup{p: llm.NewOpenAI(srv.URL+"/v1", "", "m")})
	if _, err := e.Run(context.Background(), "demo", "s", "save", "", nil, nil); err == nil {
		t.Fatal("expected follow-up error")
	}
	// A later successful turn must replay the executed tool, with no duplicate
	// checkpoint or assistant answer invented for the failed request.
	e.SetProviderLookup(&trackingLookup{p: scriptedProvider(t, func(_ int, req map[string]any) []string {
		results := 0
		for _, raw := range req["messages"].([]any) {
			if raw.(map[string]any)["tool_call_id"] == "save" {
				results++
			}
		}
		if results != 1 {
			t.Errorf("replayed %d completed results, want 1", results)
		}
		return sseText("Already saved.")
	})})
	if _, err := e.Run(context.Background(), "demo", "s", "continue", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := e.store.Messages(context.Background(), "demo", "s", 40)
	if err != nil || len(rows) != 5 {
		t.Fatalf("history after recovery: %+v %v", rows, err)
	}
}

func TestRunReturnsFinalAnswerOnly(t *testing.T) {
	e, _ := newTestEngine(t)
	e.SetProviderLookup(&trackingLookup{p: scriptedProvider(t, func(call int, _ map[string]any) []string {
		if call == 1 {
			chunks := sseToolCall("save", "memory_save", `{"content":"fact"}`)
			return append(sseText("Let me save that.")[:2], chunks...)
		}
		return sseText(`{"saved":true}`)
	})})
	var streamed strings.Builder
	reply, err := e.Run(context.Background(), "demo", "s", "save", "", nil, func(ev Event) {
		if ev.Kind == "text" {
			streamed.WriteString(ev.Text)
		}
	})
	if err != nil || reply != `{"saved":true}` {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if !strings.Contains(streamed.String(), "Let me save that.") {
		t.Fatal("intermediate narration should still stream to the UI")
	}
}

func TestRunRefusalIsVisibleAndPersisted(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			e, _ := newTestEngine(t)
			const refusal = "I cannot assist with that request."
			e.SetProviderLookup(&trackingLookup{p: scriptedProvider(t, func(_ int, _ map[string]any) []string {
				chunks := []string{`{"id":"r","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","refusal":"I cannot assist with that request."},"finish_reason":"stop"}]}`}
				if !terminal {
					chunks = append(chunks, `{"id":"r","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
				}
				return chunks
			})})
			var streamed strings.Builder
			reply, err := e.Run(context.Background(), "demo", "s", "question", "", nil, func(ev Event) {
				if ev.Kind == "text" {
					streamed.WriteString(ev.Text)
				}
			})
			if err != nil || reply != refusal || streamed.String() != refusal {
				t.Fatalf("reply=%q streamed=%q err=%v", reply, streamed.String(), err)
			}
			rows, err := e.store.Messages(context.Background(), "demo", "s", 40)
			if err != nil || len(rows) != 2 || rows[1].Content != refusal {
				t.Fatalf("refusal history: %+v %v", rows, err)
			}
		})
	}
}

func TestRunStreamErrorDoesNotExecuteBufferedCalls(t *testing.T) {
	e, _ := newTestEngine(t)
	e.SetProviderLookup(&trackingLookup{p: scriptedProvider(t, func(_ int, _ map[string]any) []string {
		return []string{
			callsChunk(t, "", wireCall{0, "save", "memory_save", `{"content":"must not be saved"}`}),
			`{"error":{"message":"stream interrupted","type":"server_error"}}`,
		}
	})})
	if _, err := e.Run(context.Background(), "demo", "s", "save", "", nil, nil); err == nil || !strings.Contains(err.Error(), "stream interrupted") {
		t.Fatalf("stream failure lost: %v", err)
	}
	knowledge, err := e.store.Knowledge(context.Background(), "demo", 10)
	if err != nil || len(knowledge) != 0 {
		t.Fatalf("tool executed from failed stream: %+v %v", knowledge, err)
	}
	rows, err := e.store.Messages(context.Background(), "demo", "s", 40)
	if err != nil || len(rows) != 1 || rows[0].Role != "user" {
		t.Fatalf("incomplete stream persisted: %+v %v", rows, err)
	}
}

func TestRunMAFRejectsDisabledToolAtDispatch(t *testing.T) {
	e, ar := newTestEngine(t)
	if _, err := ar.SetEnabledBuiltinTools("demo", &[]string{}); err != nil {
		t.Fatal(err)
	}
	e.SetProviderLookup(&trackingLookup{p: scriptedProvider(t, func(call int, req map[string]any) []string {
		if toolNames(req)["write_file"] {
			t.Error("disabled tool advertised")
		}
		if call == 1 {
			return sseToolCall("write", "write_file", `{"path":"blocked","content":"must not run"}`)
		}
		msgs := req["messages"].([]any)
		if last := msgs[len(msgs)-1].(map[string]any); !strings.Contains(fmt.Sprint(last["content"]), "not found") {
			t.Errorf("disabled call was not rejected: %v", last)
		}
		return sseText("Tool unavailable.")
	})})
	if _, err := e.Run(context.Background(), "demo", "s", "write", "", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRunMAFPreservesOptionsAndIterationLimit(t *testing.T) {
	e, ar := newTestEngine(t)
	e.cfg.MaxAgentIterations = 1
	e.basePrompt = "unique system instruction"
	model, temperature, tokens := "pinned-model", 0.0, 128
	if _, err := ar.UpdateConfig("demo", agents.AgentConfig{Model: &model, Temperature: &temperature, MaxTokens: &tokens}); err != nil {
		t.Fatal(err)
	}
	requests := 0
	e.SetProviderLookup(&trackingLookup{p: scriptedProvider(t, func(call int, req map[string]any) []string {
		requests = call
		if req["model"] != model || req["temperature"] != temperature || req["max_tokens"] != float64(tokens) {
			t.Errorf("sampling/model options lost: %v", req)
		}
		system := 0
		for _, raw := range req["messages"].([]any) {
			m := raw.(map[string]any)
			if m["role"] == "system" && strings.Contains(fmt.Sprint(m["content"]), e.basePrompt) {
				system++
			}
		}
		if system != 1 {
			t.Errorf("system prompt appeared %d times", system)
		}
		if call == 1 {
			if len(toolNames(req)) == 0 {
				t.Error("first completion must have tools")
			}
			return sseToolCall("save", "memory_save", `{"content":"fact"}`)
		}
		if len(toolNames(req)) != 0 {
			t.Error("MAF's final completion after the tool budget must omit tools")
		}
		return sseText("done")
	})})
	if _, err := e.Run(context.Background(), "demo", "s", "save", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests=%d, want one tool round plus final completion", requests)
	}
}

func TestEvolveDoesNotSaveRefusalAsKnowledge(t *testing.T) {
	e, _ := newTestEngine(t)
	for range 4 {
		if err := e.store.AppendMessage(context.Background(), "demo", "s", "user", "hello"); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"r","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","refusal":"Cannot assist."},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	e.SetProviderLookup(&trackingLookup{p: llm.NewOpenAI(srv.URL, "", "m")})
	if err := e.Evolve(context.Background(), "demo", "s"); err == nil || !strings.Contains(err.Error(), "Cannot assist.") {
		t.Fatalf("refusal should be reported: %v", err)
	}
	knowledge, err := e.store.Knowledge(context.Background(), "demo", 10)
	if err != nil || len(knowledge) != 0 {
		t.Fatalf("refusal saved as knowledge: %+v %v", knowledge, err)
	}
}
