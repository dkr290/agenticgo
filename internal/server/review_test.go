package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/dkr290/agenticgo/internal/agent"
	"github.com/dkr290/agenticgo/internal/agents"
	"github.com/dkr290/agenticgo/internal/llm"
	"github.com/dkr290/agenticgo/internal/logger"
	"github.com/dkr290/agenticgo/internal/tools"
)

func TestLLMConfigPreservesCapabilities(t *testing.T) {
	s, ar := newAgentServer(t, nil)
	s.log = logger.Nop()
	initial := agents.AgentConfig{EnabledTools: []string{"mcp_test_a"}, EnabledSkills: []string{"review"}, EnabledCommands: []string{"kubectl"}, EnabledBuiltinTools: &[]string{}}
	if _, err := ar.UpdateConfig("demo", initial); err != nil {
		t.Fatal(err)
	}
	mux := newTestAPI(t, s.registerAgentRoutes)
	for _, body := range []string{`{"model":"new-model","temperature":0}`, `{}`} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/agents/demo/llm-config", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		mux.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("save: %d %s", rec.Code, rec.Body)
		}
		ag, err := ar.Get("demo")
		if err != nil {
			t.Fatal(err)
		}
		if len(ag.Config.EnabledTools) != 1 || len(ag.Config.EnabledSkills) != 1 || len(ag.Config.EnabledCommands) != 1 || ag.Config.EnabledBuiltinTools == nil || len(*ag.Config.EnabledBuiltinTools) != 0 {
			t.Fatalf("lost capabilities: %+v", ag.Config)
		}
		if body == `{}` && (ag.Config.Model != nil || ag.Config.Temperature != nil) {
			t.Fatal("settings did not reset to inherit")
		}
	}
}

func TestConcurrentCapabilityToggles(t *testing.T) {
	s, ar := newAgentServer(t, nil)
	for i := range 20 {
		s.cfg.ExtraExecCommands = append(s.cfg.ExtraExecCommands, fmt.Sprintf("command-%d", i))
	}
	var wg sync.WaitGroup
	for _, name := range s.cfg.ExtraExecCommands {
		wg.Go(func() {
			if err := s.setExtraCommandEnabled("demo", name, true); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	ag, err := ar.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.Config.EnabledCommands) != 20 {
		t.Fatalf("lost toggles: %v", ag.Config.EnabledCommands)
	}
}

func TestDocumentRoutesRespectAgent(t *testing.T) {
	s := &Server{store: openTempStore(t), log: logger.Nop()}
	id, err := s.store.AddKnowledgeDoc(context.Background(), "owner", "private", "content")
	if err != nil {
		t.Fatal(err)
	}
	mux := newTestAPI(t, s.registerDocRoutes)
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, fmt.Sprintf("/api/agents/other/docs/%d", id), nil))
		if rec.Code != 404 {
			t.Fatalf("%s other agent: %d %s", method, rec.Code, rec.Body)
		}
	}
	if _, err := s.store.GetKnowledgeDocForAgent(context.Background(), id, "owner"); err != nil {
		t.Fatal("wrong-agent delete removed document", err)
	}
}

type blockingProvider struct {
	started chan struct{}
	stopped chan struct{}
}

func (p *blockingProvider) Model() string { return "blocking" }
func (p *blockingProvider) Name() string  { return "blocking" }
func (p *blockingProvider) ChatCompletion(ctx context.Context, _ llm.ChatRequest, _ llm.StreamFunc) (llm.Message, error) {
	p.started <- struct{}{}
	<-ctx.Done()
	p.stopped <- struct{}{}
	return llm.Message{}, ctx.Err()
}

type providerLookup struct{ p llm.Provider }

func (p providerLookup) GetLLM(string) (llm.Provider, error) { return p.p, nil }

func TestWebSocketCancelAndDisconnect(t *testing.T) {
	s, ar := newAgentServer(t, nil)
	s.log = logger.Nop()
	s.store = openTempStore(t)
	s.cfg.MaxAgentIterations = 2
	s.cfg.WorkspaceDir = filepath.Join(t.TempDir(), "workspace")
	s.engine = agent.New(s.cfg, tools.NewRegistry(nil), s.store, ar)
	p := &blockingProvider{started: make(chan struct{}, 2), stopped: make(chan struct{}, 2)}
	s.engine.SetProviderLookup(providerLookup{p})
	httpServer := httptest.NewServer(http.HandlerFunc(s.handleWS))
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	write := func(msg wsMessage) {
		t.Helper()
		data, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
			t.Fatal(err)
		}
	}
	wait := func(ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	write(wsMessage{ID: "first", Agent: "demo", Message: "wait"})
	wait(p.started)
	write(wsMessage{Kind: "cancel", ID: "first"})
	wait(p.stopped)
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var event wsEvent
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatal(err)
	}
	if event.Kind != "cancelled" || event.ID != "first" {
		t.Fatalf("event = %+v", event)
	}
	write(wsMessage{ID: "second", Agent: "demo", Message: "wait again"})
	wait(p.started)
	conn.CloseNow()
	wait(p.stopped)
}
