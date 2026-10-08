package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkr290/agenticgo/internal/agents"
	"github.com/dkr290/agenticgo/internal/store"
)

// TestRunReplaysToolExchange drives two turns through the MAF loop against a
// mock server and asserts the second turn's first request replays the stored
// tool exchange (assistant tool call + tool result) from turn one — the
// HistoryProvider + replayHistory path end to end.
func TestRunReplaysToolExchange(t *testing.T) {
	e, _ := newTestEngine(t)
	ctx := context.Background()

	turn := 0
	e.SetProviderLookup(&trackingLookup{p: scriptedProvider(t, func(call int, req map[string]any) []string {
		if call == 1 {
			// Turn 1, request 1: ask for memory_save.
			return sseToolCall("memory-1", "memory_save", `{"content":"a fact"}`)
		}
		if call == 3 {
			// Turn 2, request 1: history replay must include the exchange.
			msgs, _ := req["messages"].([]any)
			var haveCall, haveResult bool
			for _, m := range msgs {
				msg, _ := m.(map[string]any)
				if msg["role"] == "assistant" {
					if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) == 1 {
						if tc, ok := tcs[0].(map[string]any); ok && tc["id"] == "memory-1" {
							haveCall = true
						}
					}
				}
				if msg["role"] == "tool" && msg["tool_call_id"] == "memory-1" {
					haveResult = true
				}
			}
			if !haveCall || !haveResult {
				t.Fatalf("turn 2 replay incomplete: call=%v result=%v in %v", haveCall, haveResult, req["messages"])
			}
		}
		return sseText("done")
	})})

	for _, message := range []string{"remember a fact", "what did you save?"} {
		turn++
		if _, err := e.Run(ctx, "demo", "conversation", message, "", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
}

// TestReplayDropsIncompleteToolExchanges pins the repair invariant at the
// storage layer: a stored assistant tool call whose results are missing (an
// interrupted run) must not reach the provider on the next turn.
func TestReplayDropsIncompleteToolExchanges(t *testing.T) {
	e, _ := newTestEngine(t)
	ctx := context.Background()

	calls, err := json.Marshal([]map[string]string{{"id": "a", "name": "read_file", "arguments": `{}`}, {"id": "b", "name": "read_file", "arguments": `{}`}})
	if err != nil {
		t.Fatal(err)
	}
	// Assistant invoked two tools; only one result got persisted.
	if err := e.store.AppendMessageWithToolCalls(ctx, "demo", "s", "assistant", "", string(calls), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.store.AppendMessageWithToolCalls(ctx, "demo", "s", "tool", "result-a", "", "a", "read_file"); err != nil {
		t.Fatal(err)
	}
	if err := e.store.AppendMessage(ctx, "demo", "s", "user", "continue"); err != nil {
		t.Fatal(err)
	}

	e.SetProviderLookup(&trackingLookup{p: scriptedProvider(t, func(_ int, req map[string]any) []string {
		msgs, _ := req["messages"].([]any)
		for _, m := range msgs {
			msg, _ := m.(map[string]any)
			if msg["role"] == "tool" || msg["tool_calls"] != nil {
				t.Fatalf("incomplete exchange leaked into request: %v", msg)
			}
		}
		return sseText("ok")
	})})

	if _, err := e.Run(ctx, "demo", "s", "continue", "", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestObservationRetentionZeroTTL(t *testing.T) {
	for _, keep := range []int{0, 2} {
		t.Run(fmt.Sprint(keep), func(t *testing.T) {
			e, ar := newTestEngine(t)
			e.cfg.ObservationKeepLatest = keep
			for _, content := range []string{"first", "second", "third"} {
				if err := e.store.AddObservation(context.Background(), "demo", content); err != nil {
					t.Fatal(err)
				}
			}
			db, err := sql.Open("sqlite", filepath.Join(filepath.Dir(ar.Root()), "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`UPDATE observations SET created_at=1`); err != nil {
				t.Fatal(err)
			}
			e.pruneRetention(context.Background(), "demo")
			got, err := e.store.ListObservations(context.Background(), "demo", 10)
			if err != nil {
				t.Fatal(err)
			}
			want := keep
			if keep == 0 {
				want = 3
			}
			if len(got) != want || got[0].Content != "third" {
				t.Fatalf("retained = %+v", got)
			}
		})
	}
}

func TestZeroKnowledgeInjectionKeepsSearchableMemory(t *testing.T) {
	e, ar := newTestEngine(t)
	ctx := context.Background()
	if err := e.store.AddKnowledge(ctx, "demo", "unique durable memory"); err != nil {
		t.Fatal(err)
	}
	ag, err := ar.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.buildSystemPrompt(ctx, ag), "unique durable memory") {
		t.Fatal("zero injection exposed memory in system prompt")
	}
	hits, err := e.store.SearchKnowledge(ctx, "demo", "unique", 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("memory lost: %v %v", hits, err)
	}
}

// TestRunGatesFetchedImages drives fetch_agent_image through the MAF loop and
// asserts the follow-up provider request carries the image part only when the
// effective model is vision-capable.
func TestRunGatesFetchedImages(t *testing.T) {
	for _, vision := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "vision"}[vision], func(t *testing.T) {
			e, ar := newTestEngine(t)
			if _, err := ar.UpdateConfig("demo", agents.AgentConfig{Vision: &vision}); err != nil {
				t.Fatal(err)
			}
			if _, err := ar.SaveImage("demo", "photo.png", []byte("fixture image")); err != nil {
				t.Fatal(err)
			}
			call := 0
			e.SetProviderLookup(&trackingLookup{p: scriptedProvider(t, func(c int, req map[string]any) []string {
				call = c
				if c == 1 {
					return sseToolCall("img", "fetch_agent_image", `{"name":"photo.png"}`)
				}
				if got := countImages(req) > 0; got != vision {
					t.Fatalf("vision=%v images in follow-up request = %v", vision, got)
				}
				return sseText("done")
			})})
			if _, err := e.Run(context.Background(), "demo", "s", "look", "", nil, nil); err != nil {
				t.Fatal(err)
			}
			if call < 2 {
				t.Fatalf("expected a follow-up request after the tool call, got %d calls", call)
			}
		})
	}
}

func TestConversationLockCancellation(t *testing.T) {
	e, _ := newTestEngine(t)
	key := conversation{"demo", "same"}
	release, err := e.lockConversation(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := e.lockConversation(ctx, key); err == nil {
		t.Fatal("overlapping turn acquired lock")
	}
	other, err := e.lockConversation(context.Background(), conversation{"demo", "other"})
	if err != nil {
		t.Fatal(err)
	}
	other()
	release()
	if len(e.runs) != 0 {
		t.Fatal("conversation locks leaked")
	}
}

// Ensure store import is retained for the helpers above.
var _ = store.ErrKnowledgeDuplicate
