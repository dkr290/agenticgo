package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/dkr290/agenticgo/internal/store"
	"github.com/dkr290/agenticgo/internal/tools"
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
	r := newTestAPI(t, func(api huma.API) {
		huma.Register(api, huma.Operation{
			OperationID: "list-core-tools",
			Method:      http.MethodGet,
			Path:        "/api/tools/core",
		}, func(ctx context.Context, _ *struct{}) (*struct{ Body []tools.CoreTool }, error) {
			return &struct{ Body []tools.CoreTool }{Body: tools.CoreTools()}, nil
		})
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tools/core", nil))

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

	// Route through huma so {key}/{id} path params are populated.
	r := newTestAPI(t, func(api huma.API) {
		huma.Register(api, huma.Operation{
			OperationID: "delete-knowledge",
			Method:      http.MethodDelete,
			Path:        "/api/agents/{key}/knowledge/{id}",
		}, func(ctx context.Context, input *struct {
			Key string `path:"key"`
			ID  int64  `path:"id"`
		}) (*statusOutput, error) {
			if input.ID <= 0 {
				return nil, huma.Error400BadRequest("invalid knowledge id")
			}
			if err := s.store.DeleteKnowledge(ctx, input.ID, input.Key); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return nil, huma.Error404NotFound("knowledge entry not found")
				}
				return nil, huma.Error500InternalServerError(err.Error())
			}
			return &statusOutput{Body: statusBody{Status: "deleted"}}, nil
		})
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/agents/demo/knowledge/"+strconv.FormatInt(id, 10), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body=%s", rec.Code, rec.Body)
	}
	if got, _ := st.Knowledge(ctx, "demo", 10); len(got) != 0 {
		t.Fatalf("entry should be deleted, got %+v", got)
	}

	// Unknown id -> 404.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/agents/demo/knowledge/99999", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id status = %d, want 404 (body=%s)", rec.Code, rec.Body)
	}
}
