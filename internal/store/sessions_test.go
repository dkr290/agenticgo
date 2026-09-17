package store

import (
	"context"
	"os"
	"testing"
)

func TestListSessionsAndDelete(t *testing.T) {
	dir, _ := os.MkdirTemp("", "st")
	defer os.RemoveAll(dir)
	st, err := Open(dir + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// Two agents, three conversations.
	seed := []struct{ agent, session string }{
		{"a1", "default"},
		{"a1", "default"},
		{"a1", "prod-watch"},
		{"a2", "default"},
	}
	for _, m := range seed {
		if err := st.AppendMessage(ctx, m.agent, m.session, "user", "hi"); err != nil {
			t.Fatal(err)
		}
	}

	all, err := st.ListSessions(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("all sessions = %d, want 3", len(all))
	}
	for _, s := range all {
		if s.Agent == "a1" && s.Session == "default" && s.Messages != 2 {
			t.Errorf("a1/default messages = %d, want 2", s.Messages)
		}
	}

	// Agent-scoped listing must not leak other agents' conversations.
	a1, err := st.ListSessions(ctx, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(a1) != 2 {
		t.Fatalf("a1 sessions = %d, want 2", len(a1))
	}
	for _, s := range a1 {
		if s.Agent != "a1" {
			t.Errorf("filtered list contains agent %q", s.Agent)
		}
	}

	// Deleting removes only that conversation.
	if err := st.DeleteSession(ctx, "a1", "default"); err != nil {
		t.Fatal(err)
	}
	left, err := st.ListSessions(ctx, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].Session != "prod-watch" {
		t.Fatalf("after delete: %+v", left)
	}
	if msgs, err := st.Messages(ctx, "a1", "default", 10); err != nil || len(msgs) != 0 {
		t.Errorf("deleted session messages = %v, %v", msgs, err)
	}

	// Deleting again (or an unknown session) must fail.
	if err := st.DeleteSession(ctx, "a1", "default"); err == nil {
		t.Error("second delete should fail")
	}
	if err := st.DeleteSession(ctx, "a2", "nope"); err == nil {
		t.Error("delete of unknown session should fail")
	}
}
