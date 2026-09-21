package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/dkr290/agenticgo/internal/agents"
	"github.com/dkr290/agenticgo/internal/config"
)

func newAgentServer(t *testing.T, toolAllowList []string) (*Server, *agents.Registry) {
	t.Helper()
	ar, err := agents.NewRegistry(filepath.Join(t.TempDir(), "agents"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ar.Create("demo", "Demo", "test agent", "", agents.AgentConfig{}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ToolAllowList: toolAllowList}
	return &Server{cfg: cfg, agents: ar}, ar
}

// builtinToolsRouter mounts the built-in-tools endpoints on a chi router.
func builtinToolsRouter(s *Server) chi.Router {
	r := chi.NewRouter()
	r.Get("/api/agents/{key}/builtin-tools", s.handleListAgentBuiltinTools)
	r.Put("/api/agents/{key}/builtin-tools/{name}", s.handleEnableBuiltinTool)
	r.Delete("/api/agents/{key}/builtin-tools/{name}", s.handleDisableBuiltinTool)
	r.Delete("/api/agents/{key}/builtin-tools", s.handleResetBuiltinTools)
	return r
}

func TestListAgentBuiltinToolsInherit(t *testing.T) {
	s, _ := newAgentServer(t, []string{"read_file", "write_file", "list_files", "exec"})
	r := builtinToolsRouter(s)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents/demo/builtin-tools", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	var got []builtinToolWithState
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("want 4 built-in tools, got %d", len(got))
	}
	for _, bt := range got {
		if !bt.Allowed || !bt.Enabled {
			t.Errorf("%s should be allowed+enabled when inheriting, got %+v", bt.Name, bt)
		}
	}
}

func TestListAgentBuiltinToolsRespectsGlobalCeiling(t *testing.T) {
	// exec is not on the global allow-list: it must show as not allowed and
	// not enabled, and enabling it must be refused.
	s, _ := newAgentServer(t, []string{"read_file", "list_files"})
	r := builtinToolsRouter(s)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents/demo/builtin-tools", nil))
	var got []builtinToolWithState
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	states := map[string]builtinToolWithState{}
	for _, bt := range got {
		states[bt.Name] = bt
	}
	if states["exec"].Allowed || states["exec"].Enabled {
		t.Fatalf("exec must be disallowed when off the global allow-list: %+v", states["exec"])
	}
	if !states["read_file"].Enabled {
		t.Fatalf("read_file must be enabled: %+v", states["read_file"])
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/agents/demo/builtin-tools/exec", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("enabling a tool off the global allow-list must be 409, got %d", rec.Code)
	}
}

func TestBuiltinToolNarrowingRoundTrip(t *testing.T) {
	s, ar := newAgentServer(t, []string{"read_file", "write_file", "list_files", "exec"})
	r := builtinToolsRouter(s)

	// Disable exec for this agent only.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/agents/demo/builtin-tools/exec", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status = %d, body=%s", rec.Code, rec.Body)
	}

	ag, err := ar.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if ag.Config.EnabledBuiltinTools == nil {
		t.Fatal("narrowing should materialize the effective set, not stay nil")
	}
	set := map[string]bool{}
	for _, n := range *ag.Config.EnabledBuiltinTools {
		set[n] = true
	}
	if set["exec"] || len(set) != 3 {
		t.Fatalf("exec should be removed, set = %v", set)
	}

	// Listing reflects the narrowed state.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents/demo/builtin-tools", nil))
	var got []builtinToolWithState
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	states := map[string]builtinToolWithState{}
	for _, bt := range got {
		states[bt.Name] = bt
	}
	if states["exec"].Enabled {
		t.Fatalf("exec should be disabled for the agent: %+v", states["exec"])
	}

	// Re-enable, then reset to inherit.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/agents/demo/builtin-tools/exec", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("enable status = %d, body=%s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/agents/demo/builtin-tools", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, body=%s", rec.Code, rec.Body)
	}
	ag, _ = ar.Get("demo")
	if ag.Config.EnabledBuiltinTools != nil {
		t.Fatalf("reset should clear the override, got %v", *ag.Config.EnabledBuiltinTools)
	}
}

func TestEnableBuiltinToolRejectsUnknown(t *testing.T) {
	s, _ := newAgentServer(t, nil)
	r := builtinToolsRouter(s)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/agents/demo/builtin-tools/nuke", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown tool must be 400, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents/nope/builtin-tools", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown agent must be 404, got %d", rec.Code)
	}
}
