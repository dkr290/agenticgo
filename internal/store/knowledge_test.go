package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	dir, _ := os.MkdirTemp("", "st")
	t.Cleanup(func() { os.RemoveAll(dir) })
	st, err := Open(dir + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestKnowledgeReturnsEntriesWithIDs(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	if err := st.AddKnowledge(ctx, "default", "alpha fact"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddKnowledge(ctx, "default", "beta fact"); err != nil {
		t.Fatal(err)
	}
	entries, err := st.Knowledge(ctx, "default", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %v", entries)
	}
	for _, e := range entries {
		if e.ID <= 0 || e.Content == "" {
			t.Errorf("entry missing id/content: %+v", e)
		}
	}
}

func TestAddKnowledgeDedupesExactMatch(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	if err := st.AddKnowledge(ctx, "default", "same fact"); err != nil {
		t.Fatal(err)
	}
	err := st.AddKnowledge(ctx, "default", "same fact")
	if !errors.Is(err, ErrKnowledgeDuplicate) {
		t.Fatalf("want ErrKnowledgeDuplicate, got %v", err)
	}
	entries, _ := st.Knowledge(ctx, "default", 10)
	if len(entries) != 1 {
		t.Fatalf("duplicate should be a no-op, got %d entries", len(entries))
	}
	// A different agent may hold the same fact independently.
	if err := st.AddKnowledge(ctx, "other", "same fact"); err != nil {
		t.Fatalf("same content for another agent should be allowed: %v", err)
	}
}

func TestAddKnowledgeRejectsOverlong(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	if err := st.AddKnowledge(ctx, "default", strings.Repeat("x", maxKnowledgeBytes+1)); err == nil {
		t.Fatal("over-long knowledge should be rejected")
	}
}

func TestDeleteKnowledgeRemovesRowAndFTS(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	if err := st.AddKnowledge(ctx, "default", "ephemeral OOM detail"); err != nil {
		t.Fatal(err)
	}
	entries, _ := st.Knowledge(ctx, "default", 10)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %v", entries)
	}
	id := entries[0].ID

	if err := st.DeleteKnowledge(ctx, id, "default"); err != nil {
		t.Fatal(err)
	}
	// Gone from the list.
	entries, _ = st.Knowledge(ctx, "default", 10)
	if len(entries) != 0 {
		t.Fatalf("entry should be deleted, got %v", entries)
	}
	// Gone from FTS search (knowledge_ad trigger keeps the index in sync).
	hits, _ := st.SearchKnowledge(ctx, "default", "ephemeral OOM", 5)
	if len(hits) != 0 {
		t.Fatalf("deleted entry should not be searchable, got %v", hits)
	}
}

func TestDeleteKnowledgeScopedAndMissing(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	if err := st.AddKnowledge(ctx, "default", "agent-specific fact"); err != nil {
		t.Fatal(err)
	}
	entries, _ := st.Knowledge(ctx, "default", 10)
	id := entries[0].ID

	// Unknown id -> ErrNoRows.
	if err := st.DeleteKnowledge(ctx, 99999, "default"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("want sql.ErrNoRows for unknown id, got %v", err)
	}
	// Another agent cannot delete it.
	if err := st.DeleteKnowledge(ctx, id, "other"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("want sql.ErrNoRows when scoped to another agent, got %v", err)
	}
	// Still present for the owning agent.
	if entries, _ := st.Knowledge(ctx, "default", 10); len(entries) != 1 {
		t.Fatal("entry must survive a cross-agent delete attempt")
	}
}
