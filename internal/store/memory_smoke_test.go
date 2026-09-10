package store

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestMemorySmoke(t *testing.T) {
	dir, _ := os.MkdirTemp("", "st")
	defer os.RemoveAll(dir)
	st, err := Open(dir + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	st.AddKnowledge(ctx, "default", "pod payments-api OOMs after traffic spikes")
	st.AddKnowledge(ctx, "default", "user prefers concise answers")
	hits, _ := st.SearchKnowledge(ctx, "default", "OOM traffic", 5)
	t.Logf("search 'OOM traffic': %v", hits)
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %v", hits)
	}
	hits2, _ := st.SearchKnowledge(ctx, "default", "concise", 5)
	if len(hits2) != 1 {
		t.Fatalf("want concise hit, got %v", hits2)
	}

	st.AddObservation(ctx, "default", "old state: pod X crashlooping")
	time.Sleep(1100 * time.Millisecond)
	st.AddObservation(ctx, "default", "new state: all healthy")
	latest, _ := st.LatestObservation(ctx, "default")
	if latest == nil || latest.Content != "new state: all healthy" {
		t.Fatalf("wrong latest: %+v", latest)
	}

	// Retention cap: keep only the latest 1 (cutoff in the past => TTL deletes nothing).
	st.PruneObservations(ctx, "default", time.Now().AddDate(0, 0, -1).Unix(), 1)
	obs, _ := st.ListObservations(ctx, "default", 10)
	if len(obs) != 1 || obs[0].Content != "new state: all healthy" {
		t.Fatalf("want latest 1 after cap, got %+v", obs)
	}

	// TTL prune: cutoff in the future => deletes everything older than now.
	st.PruneObservations(ctx, "default", time.Now().Unix()+10, 0)
	obs, _ = st.ListObservations(ctx, "default", 10)
	if len(obs) != 0 {
		t.Fatalf("want 0 after TTL prune, got %d", len(obs))
	}
}
