package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPruneKnowledge(t *testing.T) {
	dir, _ := os.MkdirTemp("", "st")
	defer os.RemoveAll(dir)
	st, err := Open(dir + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// Seed 5 entries for "default" and 2 for "other" (to prove per-agent scoping).
	for i := 1; i <= 5; i++ {
		if err := st.AddKnowledge(ctx, "default", fmt.Sprintf("fact %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 2; i++ {
		if err := st.AddKnowledge(ctx, "other", fmt.Sprintf("other fact %d", i)); err != nil {
			t.Fatal(err)
		}
	}

	// Count cap: keep newest 3 for "default" (no age prune). "other" untouched.
	if err := st.PruneKnowledge(ctx, "default", 0, 3); err != nil {
		t.Fatal(err)
	}
	got, err := st.Knowledge(ctx, "default", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 kept, got %d (%v)", len(got), got)
	}
	// Newest kept => facts 3,4,5.
	if got[0].Content != "fact 3" || got[2].Content != "fact 5" {
		t.Fatalf("expected newest facts kept, got %+v", got)
	}
	other, _ := st.Knowledge(ctx, "other", 100)
	if len(other) != 2 {
		t.Fatalf("prune must be per-agent; 'other' lost entries: %+v", other)
	}

	// Age prune: cutoff in the future deletes everything for "default".
	if err := st.PruneKnowledge(ctx, "default", time.Now().Unix()+10, 0); err != nil {
		t.Fatal(err)
	}
	got, _ = st.Knowledge(ctx, "default", 100)
	if len(got) != 0 {
		t.Fatalf("want 0 after age prune, got %d", len(got))
	}

	// Zero limits are no-ops: re-seed and prune with 0/0.
	if err := st.AddKnowledge(ctx, "default", "kept"); err != nil {
		t.Fatal(err)
	}
	if err := st.PruneKnowledge(ctx, "default", 0, 0); err != nil {
		t.Fatal(err)
	}
	got, _ = st.Knowledge(ctx, "default", 100)
	if len(got) != 1 {
		t.Fatalf("0/0 limits must delete nothing, got %d", len(got))
	}

	// FTS stays consistent after pruning: a pruned-away term must not be found.
	if err := st.AddKnowledge(ctx, "default", "zzz unique prune token"); err != nil {
		t.Fatal(err)
	}
	if err := st.PruneKnowledge(ctx, "default", time.Now().Unix()+10, 0); err != nil {
		t.Fatal(err)
	}
	hits, err := st.SearchKnowledge(ctx, "default", "prune token", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("FTS index still returns pruned knowledge: %v", hits)
	}
}
