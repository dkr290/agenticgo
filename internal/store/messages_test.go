package store

import (
	"context"
	"os"
	"testing"
)

// TestMessagesLegacyNullToolColumns is a regression test: the
// tool_calls/tool_call_id/name columns were added by a later ALTER TABLE
// migration without defaults, so rows written before that have NULL there.
// Messages() must tolerate those NULLs instead of failing the scan (which
// surfaced as "Internal Server Error" when loading old chat history).
func TestMessagesLegacyNullToolColumns(t *testing.T) {
	dir, _ := os.MkdirTemp("", "st")
	defer os.RemoveAll(dir)
	st, err := Open(dir + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// Simulate a pre-migration row: explicit NULLs in the tool columns.
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO messages (agent, session, role, content, tool_calls, tool_call_id, name, created_at)
		 VALUES (?,?,?,?,NULL,NULL,NULL,?)`,
		"demo", "old", "user", "hello from before the migration", 1234); err != nil {
		t.Fatal(err)
	}
	// A modern row with tool metadata set.
	if err := st.AppendMessageWithToolCalls(ctx, "demo", "old", "assistant", "calling a tool",
		`[{"name":"exec"}]`, "", ""); err != nil {
		t.Fatal(err)
	}

	msgs, err := st.Messages(ctx, "demo", "old", 10)
	if err != nil {
		t.Fatalf("Messages with NULL tool columns: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].Content != "hello from before the migration" {
		t.Fatalf("wrong order/content: %+v", msgs[0])
	}
	if msgs[0].ToolCalls != "" || msgs[0].ToolCallID != "" || msgs[0].Name != "" {
		t.Fatalf("NULL columns must scan as empty strings, got %+v", msgs[0])
	}
	if msgs[1].ToolCalls != `[{"name":"exec"}]` {
		t.Fatalf("tool_calls lost: %+v", msgs[1])
	}
}
