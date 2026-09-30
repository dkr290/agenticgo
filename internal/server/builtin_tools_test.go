package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/dkr290/agenticgo/internal/agents"
	"github.com/dkr290/agenticgo/internal/config"
)

// newAgentServer builds a server with just the deps the built-in-tools routes
// need, plus a prepared "demo" agent.
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

// newTestAPI mounts a huma API on a stdlib mux for tests and returns the mux.
func newTestAPI(t *testing.T, register func(api huma.API)) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("agenticgo-test", "0.0.0"))
	register(api)
	return mux
}

// builtinToolsMux mounts only the built-in-tools endpoints.
func builtinToolsMux(t *testing.T, s *Server) *http.ServeMux {
	return newTestAPI(t, func(api huma.API) {
		huma.Register(api, huma.Operation{
			OperationID: "list-agent-builtin-tools",
			Method:      http.MethodGet,
			Path:        "/api/agents/{key}/builtin-tools",
		}, func(ctx context.Context, input *struct {
			Key string `path:"key"`
		}) (*struct{ Body []builtinToolWithState }, error) {
			out, err := s.listAgentBuiltinTools(input.Key)
			if err != nil {
				return nil, err
			}
			return &struct{ Body []builtinToolWithState }{Body: out}, nil
		})

		set := func(on bool) func(context.Context, *struct {
			Key  string `path:"key"`
			Name string `path:"name"`
		}) (*builtinToolStateOutput, error) {
			return func(ctx context.Context, input *struct {
				Key  string `path:"key"`
				Name string `path:"name"`
			}) (*builtinToolStateOutput, error) {
				if err := s.setBuiltinToolEnabled(input.Key, input.Name, on); err != nil {
					return nil, err
				}
				out := &builtinToolStateOutput{}
				out.Body.Agent = input.Key
				out.Body.Tool = input.Name
				out.Body.Enabled = on
				return out, nil
			}
		}
		huma.Register(api, huma.Operation{
			OperationID: "enable-builtin-tool",
			Method:      http.MethodPut,
			Path:        "/api/agents/{key}/builtin-tools/{name}",
		}, set(true))
		huma.Register(api, huma.Operation{
			OperationID: "disable-builtin-tool",
			Method:      http.MethodDelete,
			Path:        "/api/agents/{key}/builtin-tools/{name}",
		}, set(false))
		huma.Register(api, huma.Operation{
			OperationID: "reset-builtin-tools",
			Method:      http.MethodDelete,
			Path:        "/api/agents/{key}/builtin-tools",
		}, func(ctx context.Context, input *struct {
			Key string `path:"key"`
		}) (*resetBuiltinToolsOutput, error) {
			if _, err := s.agents.SetEnabledBuiltinTools(input.Key, nil); err != nil {
				return nil, huma.Error404NotFound(err.Error())
			}
			out := &resetBuiltinToolsOutput{}
			out.Body.Agent = input.Key
			out.Body.BuiltinTools = "inherit"
			return out, nil
		})
	})
}

func TestListAgentBuiltinToolsInherit(t *testing.T) {
	s, _ := newAgentServer(t, []string{"read_file", "write_file", "list_files", "exec"})
	r := builtinToolsMux(t, s)

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
	r := builtinToolsMux(t, s)

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
	r := builtinToolsMux(t, s)

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
	r := builtinToolsMux(t, s)

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
