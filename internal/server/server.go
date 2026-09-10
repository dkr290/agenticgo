// Package server exposes the HTTP API, WebSocket chat endpoint, and the
// embedded chat UI. It serves agent CRUD, context-file, and skills APIs, and
// streams agent-scoped chat over WebSocket.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/dkr290/agenticgo/internal/agent"
	"github.com/dkr290/agenticgo/internal/agents"
	"github.com/dkr290/agenticgo/internal/config"
	"github.com/dkr290/agenticgo/internal/skills"
)

//go:embed all:web
var webFS embed.FS

// Server is the HTTP server for agenticgo.
type Server struct {
	cfg    *config.Config
	engine *agent.Engine
	agents *agents.Registry
	http   *http.Server
}

// New builds the server.
func New(cfg *config.Config, eng *agent.Engine, ar *agents.Registry) *Server {
	s := &Server{cfg: cfg, engine: eng, agents: ar}

	r := chi.NewRouter()
	r.Get("/healthz", s.handleHealth)
	r.Get("/ws", s.handleWS)

	// API.
	r.Route("/api", func(r chi.Router) {
		r.Get("/agents", s.handleListAgents)
		r.Post("/agents", s.handleCreateAgent)
		r.Get("/agents/{key}", s.handleGetAgent)
		r.Delete("/agents/{key}", s.handleDeleteAgent)
		r.Get("/agents/{key}/files/{name}", s.handleReadFile)
		r.Put("/agents/{key}/files/{name}", s.handleWriteFile)
		r.Get("/agents/{key}/skills", s.handleListSkills)
		r.Post("/evolve", s.handleEvolve)
	})

	// Serve the embedded chat UI at /.
	static, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("embed web: %v", err)
	}
	r.Handle("/*", http.FileServer(http.FS(static)))

	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// Start listens and serves until the context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		log.Printf("agenticgo listening on %s", s.cfg.Addr)
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.http.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- Agent CRUD ---

func (s *Server) handleListAgents(w http.ResponseWriter, _ *http.Request) {
	list, err := s.agents.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if list == nil {
		list = []agents.Agent{}
	}
	writeJSON(w, http.StatusOK, list)
}

type createAgentRequest struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Soul        string `json:"soul"`
}

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	var req createAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ag, err := s.agents.Create(req.Key, req.Name, req.Description, req.Soul)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, ag)
}

func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	ag, err := s.agents.Get(chi.URLParam(r, "key"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, ag)
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	if err := s.agents.Delete(chi.URLParam(r, "key")); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- Context files ---

func (s *Server) handleReadFile(w http.ResponseWriter, r *http.Request) {
	content, err := s.agents.ReadFile(chi.URLParam(r, "key"), chi.URLParam(r, "name"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": chi.URLParam(r, "name"), "content": content})
}

func (s *Server) handleWriteFile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.agents.WriteFile(chi.URLParam(r, "key"), chi.URLParam(r, "name"), body.Content); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// --- Skills ---

func (s *Server) handleListSkills(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	sd, err := s.agents.SkillsDir(key)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	sks, err := skills.Load(sd)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if sks == nil {
		sks = []skills.Skill{}
	}
	writeJSON(w, http.StatusOK, sks)
}

// --- Evolve ---

func (s *Server) handleEvolve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Agent   string `json:"agent"`
		Session string `json:"session"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if body.Agent == "" {
		body.Agent = "default"
	}
	if body.Session == "" {
		body.Session = "default"
	}
	if err := s.engine.Evolve(r.Context(), body.Agent, body.Session); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "evolved"})
}

// --- WebSocket chat ---

type wsMessage struct {
	Agent   string `json:"agent"`
	Session string `json:"session"`
	Message string `json:"message"`
}

type wsEvent struct {
	Kind       string `json:"kind"` // text | tool_call | tool_result | done | error
	Text       string `json:"text,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	ToolArgs   string `json:"tool_args,omitempty"`
	ToolResult string `json:"tool_result,omitempty"`
	Error      string `json:"error,omitempty"`
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		log.Printf("websocket accept: %v", err)
		return
	}
	defer conn.Close(websocket.StatusInternalError, "closing")

	conn.SetReadLimit(1 << 20) // 1 MiB

	for {
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return // client disconnected or read error
		}

		var msg wsMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			s.sendEvent(r.Context(), conn, wsEvent{Kind: "error", Error: "invalid message: " + err.Error()})
			continue
		}
		if msg.Agent == "" {
			msg.Agent = "default"
		}
		if msg.Session == "" {
			msg.Session = "default"
		}

		ctx, cancel := context.WithCancel(r.Context())
		emit := func(ev agent.Event) {
			out := wsEvent{
				Kind:       ev.Kind,
				Text:       ev.Text,
				ToolName:   ev.ToolName,
				ToolArgs:   ev.ToolArgs,
				ToolResult: ev.ToolResult,
			}
			if ev.Err != nil {
				out.Error = ev.Err.Error()
			}
			_ = s.sendEvent(ctx, conn, out)
		}

		_, runErr := s.engine.Run(ctx, msg.Agent, msg.Session, msg.Message, emit)
		cancel()
		if runErr != nil {
			s.sendEvent(r.Context(), conn, wsEvent{Kind: "error", Error: runErr.Error()})
		}
	}
}

func (s *Server) sendEvent(ctx context.Context, conn *websocket.Conn, ev wsEvent) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, data)
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
