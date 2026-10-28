package maf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dkr290/agenticgo/internal/tools"
	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/functool"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func TestChatCancellationStopsRemainingBatchAndKeepsResults(t *testing.T) {
	st := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstRan := false
	first := functool.MustNew(functool.Config{Name: "first"}, func(context.Context, struct{}) (string, error) {
		firstRan = true
		cancel() // Cancellation happens during the first call, not before the batch.
		return "first side effect completed", nil
	})
	workspace := t.TempDir()
	write, err := tools.NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"first","arguments":"{}"}},{"index":1,"id":"b","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"after-cancel\",\"content\":\"must not run\"}"}}]},"finish_reason":"tool_calls"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	a := NewChatAgent(openai.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("unused")), ChatConfig{
		Model: "m", MaxIterations: 3, Tools: []tool.Tool{first, write}, History: NewHistoryProvider(st, "demo", "s"),
	})
	_, err = a.RunText(ctx, "two actions", agent.Stream(true)).Collect()
	if !errors.Is(err, context.Canceled) || !firstRan {
		t.Fatalf("firstRan=%v err=%v", firstRan, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "after-cancel")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("later write executed: %v", err)
	}
	rows, err := st.Messages(context.Background(), "demo", "s", 40)
	if err != nil || len(rows) != 4 {
		t.Fatalf("cancelled batch history: %+v %v", rows, err)
	}
	if rows[2].Name != "first" || rows[2].Content != "first side effect completed" || rows[3].Name != "write_file" || !strings.Contains(rows[3].Content, "context canceled") {
		t.Fatalf("completed/cancelled results not preserved: %+v", rows)
	}
	history, err := NewHistoryProvider(st, "demo", "s").Invoking(context.Background(), agent.InvokingContext{})
	if err != nil || len(history) != 4 {
		t.Fatalf("cancelled batch is not replayable: %v %v", history, err)
	}
}

func TestChatNonStreamingRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"r","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","refusal":"Cannot assist."},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	a := NewChatAgent(openai.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("unused")), ChatConfig{Model: "m"})
	response, err := a.RunText(context.Background(), "question").Collect()
	if err != nil {
		t.Fatal(err)
	}
	for c := range response.Contents() {
		if refusal, ok := c.(*message.ErrorContent); ok && refusal.Message == "Cannot assist." {
			return
		}
	}
	t.Fatalf("refusal content lost: %+v", response)
}

func TestChatCancellationPreservesPartialBatchAndEarlierRounds(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, outcome := range []string{"success", "error", "panic"} {
			t.Run(fmt.Sprintf("stream=%t/%s", streaming, outcome), func(t *testing.T) {
				st := openTestStore(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				effects := 0
				effect := functool.MustNew(functool.Config{Name: "effect"}, func(callCtx context.Context, _ struct{}) (string, error) {
					if callCtx.Done() != ctx.Done() {
						t.Error("tool lost the original cancellation context")
					}
					effects++
					if effects < 3 {
						return fmt.Sprintf("completed effect %d", effects), nil
					}
					cancel()
					switch outcome {
					case "error":
						return "", errors.New("tool interrupted")
					case "panic":
						panic("tool panicked")
					default:
						return "completed effect 3", nil
					}
				})
				var requests atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var request struct {
						Stream   bool             `json:"stream"`
						Messages []map[string]any `json:"messages"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						return
					}
					if request.Stream != streaming {
						t.Errorf("model stream=%t, want %t", request.Stream, streaming)
					}
					var ids []string
					switch requests.Add(1) {
					case 1:
						ids = []string{"earlier"}
					case 2:
						ids = []string{"first", "second", "skipped"}
					default:
						results := 0
						for _, m := range request.Messages {
							if m["role"] == "tool" {
								results++
							}
						}
						if results != 4 {
							t.Errorf("replayed %d results, want 4", results)
						}
					}
					writeChatTestResponse(t, w, streaming, ids)
				}))
				defer srv.Close()
				a := NewChatAgent(openai.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("unused")), ChatConfig{
					Model: "m", MaxIterations: 4, Tools: []tool.Tool{effect}, History: NewHistoryProvider(st, "demo", "s"),
				})
				var emittedResults int
				var runErr error
				for update, err := range a.RunText(ctx, "perform actions", agent.Stream(streaming)) {
					if update != nil {
						for _, c := range update.Contents {
							if _, ok := c.(*message.FunctionResultContent); ok {
								emittedResults++
							}
						}
					}
					if err != nil {
						runErr = err
						break
					}
				}
				if ctx.Err() == nil || runErr == nil || effects != 3 || requests.Load() != 2 || emittedResults != 4 {
					t.Fatalf("err=%v effects=%d requests=%d results=%d", runErr, effects, requests.Load(), emittedResults)
				}
				rows, err := st.Messages(context.Background(), "demo", "s", 40)
				if err != nil || len(rows) != 7 {
					t.Fatalf("history: %+v %v", rows, err)
				}
				want := map[string]string{"success": "completed effect 3", "error": "tool interrupted", "panic": "tool panicked"}[outcome]
				if rows[2].Content != "completed effect 1" || rows[4].Content != "completed effect 2" ||
					!strings.Contains(rows[5].Content, want) || !strings.Contains(rows[6].Content, "context canceled") {
					t.Fatalf("lost partial batch or earlier round: %+v", rows)
				}
				// Reusing the agent starts a fresh result journal and replays every
				// completed/cancelled result without checkpointing it a second time.
				if _, err := a.RunText(context.Background(), "continue", agent.Stream(streaming)).Collect(); err != nil {
					t.Fatal(err)
				}
				rows, err = st.Messages(context.Background(), "demo", "s", 40)
				if err != nil || len(rows) != 9 {
					t.Fatalf("duplicate or missing history on reuse: %+v %v", rows, err)
				}
			})
		}
	}
}

func TestChatCancellationBeforeToolDispatch(t *testing.T) {
	st := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	effect := functool.MustNew(functool.Config{Name: "effect"}, func(context.Context, struct{}) (string, error) {
		t.Error("tool dispatched after cancellation")
		return "unexpected", nil
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeChatTestResponse(t, w, true, []string{"first", "second"})
	}))
	defer srv.Close()
	a := NewChatAgent(openai.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("unused")), ChatConfig{
		Model: "m", MaxIterations: 2, Tools: []tool.Tool{effect}, History: NewHistoryProvider(st, "demo", "s"),
	})
	var runErr error
	for update, err := range a.RunText(ctx, "cancel before tools", agent.Stream(true)) {
		if update != nil {
			for _, c := range update.Contents {
				if _, ok := c.(*message.FunctionCallContent); ok {
					cancel()
				}
			}
		}
		if err != nil {
			runErr = err
			break
		}
	}
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", runErr)
	}
	rows, err := st.Messages(context.Background(), "demo", "s", 40)
	if err != nil || len(rows) != 4 {
		t.Fatalf("history: %+v %v", rows, err)
	}
	for _, row := range rows[2:] {
		if !strings.Contains(row.Content, "context canceled") {
			t.Fatalf("unexecuted call reported as completed: %+v", row)
		}
	}
}

func writeChatTestResponse(t *testing.T, w http.ResponseWriter, streaming bool, ids []string) {
	t.Helper()
	content := map[string]any{"role": "assistant"}
	finish := "stop"
	if len(ids) == 0 {
		content["content"] = "done"
	} else {
		finish = "tool_calls"
		calls := make([]map[string]any, 0, len(ids))
		for i, id := range ids {
			call := map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "effect", "arguments": "{}"}}
			if streaming {
				call["index"] = i
			}
			calls = append(calls, call)
		}
		content["tool_calls"] = calls
	}
	choice := map[string]any{"index": 0, "finish_reason": finish}
	object := "chat.completion"
	if streaming {
		choice["delta"] = content
		object += ".chunk"
	} else {
		choice["message"] = content
	}
	data, err := json.Marshal(map[string]any{"id": "response", "object": object, "choices": []any{choice}})
	if err != nil {
		t.Fatal(err)
	}
	if streaming {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
	} else {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, string(data))
	}
}
