package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/dkr290/agenticgo/internal/store"
)

func openTempStore(t *testing.T) *store.Store {
	t.Helper()
	dir, _ := os.MkdirTemp("", "srv")
	t.Cleanup(func() { os.RemoveAll(dir) })
	st, err := store.Open(dir + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestHandleListCoreTools(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/tools/core", nil)
	rec := httptest.NewRecorder()
	s.handleListCoreTools(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got []map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range got {
		names[c["name"]] = true
	}
	for _, want := range []string{"memory_search", "memory_save", "record_observation", "search_docs", "read_doc"} {
		if !names[want] {
			t.Errorf("core tools missing %q (got %v)", want, names)
		}
	}
}

func TestHandleDeleteKnowledge(t *testing.T) {
	st := openTempStore(t)
	s := &Server{store: st}
	ctx := context.Background()
	if err := st.AddKnowledge(ctx, "demo", "delete me"); err != nil {
		t.Fatal(err)
	}
	entries, _ := st.Knowledge(ctx, "demo", 10)
	if len(entries) != 1 {
		t.Fatalf("seed failed: %+v", entries)
	}
	id := entries[0].ID

	// Route through chi so {key}/{id} URL params are populated.
	r := chi.NewRouter()
	r.Delete("/api/agents/{key}/knowledge/{id}", s.handleDeleteKnowledge)

	req := httptest.NewRequest(http.MethodDelete, "/api/agents/demo/knowledge/"+strconv.FormatInt(id, 10), nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body=%s", rec.Code, rec.Body)
	}
	if got, _ := st.Knowledge(ctx, "demo", 10); len(got) != 0 {
		t.Fatalf("entry should be deleted, got %+v", got)
	}

	// Unknown id -> 404.
	req = httptest.NewRequest(http.MethodDelete, "/api/agents/demo/knowledge/99999", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id status = %d, want 404", rec.Code)
	}
}
