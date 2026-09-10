// Package server exposes the HTTP API, WebSocket chat endpoint, and the
// embedded chat UI. It serves agent CRUD, context-file, and skills APIs, and
// streams agent-scoped chat over WebSocket.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/dkr290/agenticgo/internal/agent"
	"github.com/dkr290/agenticgo/internal/agents"
	"github.com/dkr290/agenticgo/internal/config"
	"github.com/dkr290/agenticgo/internal/providers"
	"github.com/dkr290/agenticgo/internal/scaffold"
	"github.com/dkr290/agenticgo/internal/skills"
	"github.com/dkr290/agenticgo/internal/store"
	"github.com/dkr290/agenticgo/internal/tools"
)

//go:embed all:web
var webFS embed.FS

// Server is the HTTP server for agenticgo.
type Server struct {
	cfg       *config.Config
	engine    *agent.Engine
	agents    *agents.Registry
	tools     *tools.Registry
	store     *store.Store
	providers *providers.Store
	scaffold  *scaffold.Store
	http      *http.Server
}

// New builds the server.
func New(cfg *config.Config, eng *agent.Engine, ar *agents.Registry, tr *tools.Registry, st *store.Store, ps *providers.Store, sc *scaffold.Store) *Server {
	s := &Server{cfg: cfg, engine: eng, agents: ar, tools: tr, store: st, providers: ps, scaffold: sc}

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
		r.Get("/agents/{key}/knowledge", s.handleListKnowledge)
		r.Get("/agents/{key}/knowledge/search", s.handleSearchKnowledge)
		r.Get("/agents/{key}/observations", s.handleListObservations)
		r.Post("/agents/{key}/observations", s.handleAddObservation)
		r.Get("/agents/{key}/docs", s.handleListDocs)
		r.Post("/agents/{key}/docs", s.handleAddDoc)
		r.Get("/agents/{key}/docs/{id}", s.handleGetDoc)
		r.Delete("/agents/{key}/docs/{id}", s.handleDeleteDoc)
		r.Get("/agents/{key}/docs/search/query", s.handleSearchDocs)
		r.Get("/skills", s.handleListAllSkills)
		r.Post("/agents/{key}/skills/upload", s.handleUploadSkill)
		r.Post("/evolve", s.handleEvolve)

		// Conversations.
		r.Get("/sessions", s.handleListSessions)
		r.Get("/sessions/{agent}/{session}/messages", s.handleSessionMessages)

		// Capabilities.
		r.Get("/tools", s.handleListTools)

		// Providers (OpenAI-compatible endpoints).
		r.Get("/providers", s.handleListProviders)
		r.Post("/providers", s.handleUpsertProvider)
		r.Get("/providers/{name}", s.handleGetProvider)
		r.Delete("/providers/{name}", s.handleDeleteProvider)
		r.Post("/providers/{name}/test", s.handleTestProvider)

		// Scaffolding (not yet functional; UI + API shape only).
		r.Get("/mcp-servers", s.handleListMCPServers)
		r.Post("/mcp-servers", s.handleAddMCPServer)
		r.Delete("/mcp-servers/{id}", s.handleDeleteMCPServer)
		r.Get("/cron", s.handleListCronJobs)
		r.Post("/cron", s.handleAddCronJob)
		r.Delete("/cron/{id}", s.handleDeleteCronJob)
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

// handleListAllSkills lists skills across all agents (for the Skills page).
func (s *Server) handleListAllSkills(w http.ResponseWriter, _ *http.Request) {
	agentList, err := s.agents.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := []skills.Skill{}
	for _, ag := range agentList {
		sd, err := s.agents.SkillsDir(ag.Key)
		if err != nil {
			continue
		}
		sks, err := skills.Load(sd)
		if err != nil {
			continue
		}
		for _, sk := range sks {
			sk.Agent = ag.Key
			out = append(out, sk)
		}
	}
	writeJSON(w, http.StatusOK, out)
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

// --- Knowledge ---

func (s *Server) handleListKnowledge(w http.ResponseWriter, r *http.Request) {
	k, err := s.store.Knowledge(r.Context(), chi.URLParam(r, "key"), 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if k == nil {
		k = []string{}
	}
	writeJSON(w, http.StatusOK, k)
}

func (s *Server) handleSearchKnowledge(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	hits, err := s.store.SearchKnowledge(r.Context(), chi.URLParam(r, "key"), q, 20)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if hits == nil {
		hits = []string{}
	}
	writeJSON(w, http.StatusOK, hits)
}

// --- Observations ---

func (s *Server) handleListObservations(w http.ResponseWriter, r *http.Request) {
	obs, err := s.store.ListObservations(r.Context(), chi.URLParam(r, "key"), 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, obs)
}

func (s *Server) handleAddObservation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.AddObservation(r.Context(), chi.URLParam(r, "key"), body.Content); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "recorded"})
}

// --- Knowledge-base documents ---

func (s *Server) handleListDocs(w http.ResponseWriter, r *http.Request) {
	docs, err := s.store.ListKnowledgeDocs(r.Context(), chi.URLParam(r, "key"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, docs)
}

func (s *Server) handleGetDoc(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid id"))
		return
	}
	doc, err := s.store.GetKnowledgeDoc(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handleDeleteDoc(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid id"))
		return
	}
	if err := s.store.DeleteKnowledgeDoc(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleSearchDocs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	docs, err := s.store.SearchDocs(r.Context(), chi.URLParam(r, "key"), q, 20)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if docs == nil {
		docs = []store.KnowledgeDoc{}
	}
	writeJSON(w, http.StatusOK, docs)
}

// handleAddDoc accepts a document either as multipart file upload
// (field "file") or as JSON { "title": "...", "content": "..." }.
func (s *Server) handleAddDoc(w http.ResponseWriter, r *http.Request) {
	agent := chi.URLParam(r, "key")
	ct := r.Header.Get("Content-Type")

	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(20 << 20); err != nil { // 20 MB
			writeError(w, http.StatusBadRequest, fmt.Errorf("parse upload: %w", err))
			return
		}
		file, hdr, err := r.FormFile("file")
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("missing file: %w", err))
			return
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, 20<<20))
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("read file: %w", err))
			return
		}
		title := strings.TrimSuffix(hdr.Filename, filepath.Ext(hdr.Filename))
		id, err := s.store.AddKnowledgeDoc(r.Context(), agent, title, string(data))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "title": title})
		return
	}

	// JSON body.
	var body struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(body.Title) == "" || strings.TrimSpace(body.Content) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("title and content are required"))
		return
	}
	id, err := s.store.AddKnowledgeDoc(r.Context(), agent, body.Title, body.Content)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "title": body.Title})
}

// --- Skill upload ---

// handleUploadSkill installs a skill from an uploaded ZIP (multipart field
// "file") into the agent's skills directory.
func (s *Server) handleUploadSkill(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if err := r.ParseMultipartForm(25 << 20); err != nil { // 25 MB
		writeError(w, http.StatusBadRequest, fmt.Errorf("parse upload: %w", err))
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("missing file: %w", err))
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 25<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("read upload: %w", err))
		return
	}
	sd, err := s.agents.SkillsDir(key)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	sk, err := skills.InstallZip(sd, data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, sk)
}

// --- Sessions ---

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.store.ListSessions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (s *Server) handleSessionMessages(w http.ResponseWriter, r *http.Request) {
	msgs, err := s.store.Messages(r.Context(), chi.URLParam(r, "agent"), chi.URLParam(r, "session"), 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if msgs == nil {
		msgs = []store.Message{}
	}
	writeJSON(w, http.StatusOK, msgs)
}

// --- Built-in tools ---

type toolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (s *Server) handleListTools(w http.ResponseWriter, _ *http.Request) {
	specs := s.tools.Specs()
	out := make([]toolInfo, 0, len(specs))
	for _, sp := range specs {
		out = append(out, toolInfo{Name: sp.Function.Name, Description: sp.Function.Description})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

// --- Providers ---

func (s *Server) handleListProviders(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.providers.List())
}

func (s *Server) handleGetProvider(w http.ResponseWriter, r *http.Request) {
	p, err := s.providers.Get(chi.URLParam(r, "name"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleUpsertProvider(w http.ResponseWriter, r *http.Request) {
	var p providers.Provider
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.providers.Upsert(p); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	if err := s.providers.Delete(chi.URLParam(r, "name")); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	models, err := s.providers.TestConnection(r.Context(), chi.URLParam(r, "name"))
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if models == nil {
		models = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "models": models})
}

// --- MCP servers (scaffolding) ---

func (s *Server) handleListMCPServers(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.scaffold.ListMCPServers())
}

func (s *Server) handleAddMCPServer(w http.ResponseWriter, r *http.Request) {
	var srv scaffold.MCPServer
	if err := json.NewDecoder(r.Body).Decode(&srv); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	created, err := s.scaffold.AddMCPServer(srv)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleDeleteMCPServer(w http.ResponseWriter, r *http.Request) {
	if err := s.scaffold.DeleteMCPServer(chi.URLParam(r, "id")); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- Cron jobs (scaffolding) ---

func (s *Server) handleListCronJobs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.scaffold.ListCronJobs())
}

func (s *Server) handleAddCronJob(w http.ResponseWriter, r *http.Request) {
	var job scaffold.CronJob
	if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	created, err := s.scaffold.AddCronJob(job)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleDeleteCronJob(w http.ResponseWriter, r *http.Request) {
	if err := s.scaffold.DeleteCronJob(chi.URLParam(r, "id")); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- WebSocket chat ---

type wsMessage struct {
	Agent    string `json:"agent"`
	Session  string `json:"session"`
	Message  string `json:"message"`
	Provider string `json:"provider"` // optional provider override
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

		_, runErr := s.engine.Run(ctx, msg.Agent, msg.Session, msg.Message, msg.Provider, emit)
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
