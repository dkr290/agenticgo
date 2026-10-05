package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkr290/agenticgo/internal/agents"
	"github.com/dkr290/agenticgo/internal/llm"
	"github.com/dkr290/agenticgo/internal/store"
)

type checkingProvider struct {
	complete func(llm.ChatRequest) (llm.Message, error)
}

func (p checkingProvider) Model() string { return "test" }
func (p checkingProvider) Name() string  { return "test" }
func (p checkingProvider) ChatCompletion(_ context.Context, r llm.ChatRequest, _ llm.StreamFunc) (llm.Message, error) {
	return p.complete(r)
}

func TestRunReplaysToolExchange(t *testing.T) {
	e, _ := newTestEngine(t)
	e.cfg.MaxAgentIterations = 3
	ctx := context.Background()
	call := 0
	e.SetProviderLookup(fixedLookup{p: checkingProvider{complete: func(r llm.ChatRequest) (llm.Message, error) {
		call++
		if call == 1 {
			return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "memory-1", Name: "memory_save", Arguments: `{"content":"a fact"}`}}}, nil
		}
		if call == 3 {
			if len(r.Messages) != 6 || len(r.Messages[2].ToolCalls) != 1 || r.Messages[2].ToolCalls[0].ID != "memory-1" || r.Messages[3].ToolCallID != "memory-1" || r.Messages[3].Name != "memory_save" {
				t.Fatalf("invalid replay: %+v", r.Messages)
			}
		}
		return llm.Message{Role: llm.RoleAssistant, Content: "done"}, nil
	}}})
	for _, message := range []string{"remember a fact", "what did you save?"} {
		if _, err := e.Run(ctx, "demo", "conversation", message, "", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if call != 3 {
		t.Fatalf("completion calls = %d", call)
	}
}

func TestReplayDropsIncompleteToolExchanges(t *testing.T) {
	calls, err := json.Marshal([]llm.ToolCall{{ID: "a", Name: "read_file", Arguments: `{}`}, {ID: "b", Name: "read_file", Arguments: `{}`}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		history []store.Message
	}{
		{"window starts in results", []store.Message{{Role: "tool", ToolCallID: "a"}, {Role: "user", Content: "next"}}},
		{"interrupted parallel calls", []store.Message{{Role: "assistant", ToolCalls: string(calls)}, {Role: "tool", ToolCallID: "a"}, {Role: "user", Content: "next"}}},
		{"missing all results", []store.Message{{Role: "assistant", ToolCalls: string(calls)}, {Role: "user", Content: "next"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := replayHistory(tc.history)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Content != "next" {
				t.Fatalf("replay = %+v", got)
			}
		})
	}
}

func TestObservationRetentionZeroTTL(t *testing.T) {
	for _, keep := range []int{0, 2} {
		t.Run(string(rune('0'+keep)), func(t *testing.T) {
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

func TestRunGatesFetchedImages(t *testing.T) {
	for _, vision := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "vision"}[vision], func(t *testing.T) {
			e, ar := newTestEngine(t)
			e.cfg.MaxAgentIterations = 2
			if _, err := ar.UpdateConfig("demo", agents.AgentConfig{Vision: &vision}); err != nil {
				t.Fatal(err)
			}
			if _, err := ar.SaveImage("demo", "photo.png", []byte("fixture image")); err != nil {
				t.Fatal(err)
			}
			calls := 0
			e.SetProviderLookup(fixedLookup{p: checkingProvider{complete: func(r llm.ChatRequest) (llm.Message, error) {
				calls++
				if calls == 1 {
					return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "img", Name: "fetch_agent_image", Arguments: `{"name":"photo.png"}`}}}, nil
				}
				images := 0
				for _, m := range r.Messages {
					images += len(m.Images)
				}
				if (images > 0) != vision {
					t.Fatalf("vision=%v images=%d", vision, images)
				}
				return llm.Message{Role: llm.RoleAssistant, Content: "done"}, nil
			}}})
			if _, err := e.Run(context.Background(), "demo", "s", "look", "", nil, nil); err != nil {
				t.Fatal(err)
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
