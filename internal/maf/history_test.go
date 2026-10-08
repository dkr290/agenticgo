package maf

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/dkr290/agenticgo/internal/llm"
	"github.com/dkr290/agenticgo/internal/store"
	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestHistoryRoundTrip persists a conversation through Invoked, loads it back
// through Invoking, and asserts the MAF messages carry the same roles, text,
// and tool linkage.
func TestHistoryRoundTrip(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	hp := NewHistoryProvider(st, "demo", "s1").(*historyProvider)

	// The engine persists the user turn itself before the run.
	if err := st.AppendMessage(ctx, "demo", "s1", "user", "list the files"); err != nil {
		t.Fatal(err)
	}

	// Simulate a completed run: assistant text+tool call, then a tool result.
	assistantMsg := message.New(
		&message.TextContent{Text: "let me look"},
		&message.FunctionCallContent{CallID: "call_1", Name: "list_files", Arguments: `{"path":"."}`},
	)
	assistantMsg.Role = message.RoleAssistant
	toolMsg := message.New(&message.FunctionResultContent{CallID: "call_1", Result: "file\ta.txt\n"})
	toolMsg.Role = message.RoleTool
	finalMsg := message.NewText("there is one file: a.txt")
	finalMsg.Role = message.RoleAssistant

	err := hp.Invoked(ctx, agent.InvokedContext{ResponseMessages: []*message.Message{assistantMsg, toolMsg, finalMsg}})
	if err != nil {
		t.Fatalf("Invoked: %v", err)
	}

	// The stored rows must match the legacy engine's format exactly: the Chat
	// History UI reads them raw.
	rows, err := st.Messages(ctx, "demo", "s1", 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("stored %d rows, want 4 (user, assistant+calls, tool, assistant)", len(rows))
	}
	if rows[1].Role != "assistant" || rows[1].ToolCalls == "" {
		t.Errorf("row 1 should be assistant with tool_calls: %+v", rows[1])
	}
	if rows[2].Role != "tool" || rows[2].ToolCallID != "call_1" {
		t.Errorf("row 2 should be tool result linked to call_1: %+v", rows[2])
	}

	// Invoking returns the same conversation as MAF messages.
	msgs, err := hp.Invoking(ctx, agent.InvokingContext{})
	if err != nil {
		t.Fatalf("Invoking: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("Invoking returned %d messages, want 4", len(msgs))
	}
	if msgs[0].Role != message.RoleUser || msgs[0].String() != "list the files" {
		t.Errorf("msg 0 = %v %q", msgs[0].Role, msgs[0].String())
	}
	// Assistant message must carry the function call with ID and args.
	var foundCall *message.FunctionCallContent
	for _, c := range msgs[1].Contents {
		if fc, ok := c.(*message.FunctionCallContent); ok {
			foundCall = fc
		}
	}
	if foundCall == nil || foundCall.CallID != "call_1" || foundCall.Name != "list_files" {
		t.Errorf("assistant tool call lost: %+v", foundCall)
	}
	// Tool result must be linked by call ID.
	var foundResult *message.FunctionResultContent
	for _, c := range msgs[2].Contents {
		if fr, ok := c.(*message.FunctionResultContent); ok {
			foundResult = fr
		}
	}
	if foundResult == nil || foundResult.CallID != "call_1" {
		t.Errorf("tool result lost or unlinked: %+v", foundResult)
	}
	if msgs[3].String() != "there is one file: a.txt" {
		t.Errorf("final message = %q", msgs[3].String())
	}
}

// TestHistoryInvokedSkipsFailedRuns: a run that errored persists nothing,
// matching the legacy engine (a partial exchange must not corrupt history).
func TestHistoryInvokedSkipsFailedRuns(t *testing.T) {
	st := openTestStore(t)
	hp := NewHistoryProvider(st, "demo", "s1")

	err := hp.Invoked(context.Background(), agent.InvokedContext{
		ResponseMessages: []*message.Message{message.NewText("partial")},
		Err:              fmt.Errorf("boom"),
	})
	if err != nil {
		t.Fatalf("Invoked with Err should not fail: %v", err)
	}
	rows, _ := st.Messages(context.Background(), "demo", "s1", 40)
	if len(rows) != 0 {
		t.Errorf("failed run persisted %d rows", len(rows))
	}
}

// TestReplayHistoryDroppedExchanges pins the repair invariants: dangling tool
// calls (no result) and orphaned results (no call) are dropped, so the
// provider never sees an exchange it would reject.
func TestReplayHistoryDroppedExchanges(t *testing.T) {
	rows := []store.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "calling", ToolCalls: `[{"id":"c1","name":"list_files","arguments":"{}"}]`},
		// tool result for c1 missing (interrupted run) — whole exchange drops
		{Role: "user", Content: "again"},
		{Role: "tool", Content: "orphan", ToolCallID: "c_unknown", Name: "x"}, // orphaned result
		{Role: "assistant", Content: "done"},
	}
	out, err := replayHistory(rows)
	if err != nil {
		t.Fatal(err)
	}
	// Expect: user hi, user again, assistant done — the broken exchange and
	// the orphan vanish.
	if len(out) != 3 {
		t.Fatalf("replay returned %d messages, want 3: %+v", len(out), out)
	}
	if out[2].Content != "done" {
		t.Errorf("last message = %q", out[2].Content)
	}
}

// TestLLMToMAFImages: a user message with data-URL images becomes a message
// with text + DataContent parts (vision path).
func TestLLMToMAFImages(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("fake-png"))
	m := llmToMAF(llm.Message{
		Role:    llm.RoleUser,
		Content: "what is this",
		Images:  []string{"data:image/png;base64," + png},
	})
	var texts, images int
	for _, c := range m.Contents {
		switch c := c.(type) {
		case *message.TextContent:
			texts++
		case *message.DataContent:
			images++
			if c.MediaType != "image/png" {
				t.Errorf("media type = %q", c.MediaType)
			}
			data, err := c.Bytes()
			if err != nil || string(data) != "fake-png" {
				t.Errorf("image bytes round-trip failed: %v", err)
			}
		}
	}
	if texts != 1 || images != 1 {
		t.Errorf("contents: %d texts, %d images", texts, images)
	}
}

// TestSplitContentsToolError: a FunctionResultContent carrying an error is
// persisted as an "error: ..." string, matching the legacy engine's tool
// failure rows.
func TestSplitContentsToolError(t *testing.T) {
	_, _, results := splitContents(message.Contents{
		&message.FunctionResultContent{CallID: "c1", Error: fmt.Errorf("tool exploded")},
	})
	if len(results) != 1 || !strings.HasPrefix(results[0].content, "error: tool exploded") {
		t.Errorf("results = %+v", results)
	}
}
