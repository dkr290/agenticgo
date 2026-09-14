// Package server exposes the HTTP API, WebSocket chat endpoint, and the
// embedded chat UI. It serves agent CRUD, context-file, and skills APIs, and
// streams agent-scoped chat over WebSocket.
package server

import (
	"context"
	"embed"
	"encoding/base64"
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
		r.Put("/agents/{key}/config", s.handleUpdateAgentConfig)
		r.Get("/agents/{key}/files/{name}", s.handleReadFile)
		r.Put("/agents/{key}/files/{name}", s.handleWriteFile)
		r.Get("/agents/{key}/skills", s.handleListAgentSkills)
		r.Put("/agents/{key}/skills/{skill}", s.handleEnableSkill)
		r.Delete("/agents/{key}/skills/{skill}", s.handleDisableSkill)
		r.Get("/agents/{key}/images", s.handleListImages)
		r.Post("/agents/{key}/images", s.handleUploadImage)
		r.Delete("/agents/{key}/images/{name}", s.handleDeleteImage)
		r.Get("/agents/{key}/vision", s.handleVisionStatus)
		r.Get("/agents/{key}/knowledge", s.handleListKnowledge)
		r.Get("/agents/{key}/knowledge/search", s.handleSearchKnowledge)
		r.Get("/agents/{key}/observations", s.handleListObservations)
		r.Post("/agents/{key}/observations", s.handleAddObservation)
		r.Get("/agents/{key}/docs", s.handleListDocs)
		r.Post("/agents/{key}/docs", s.handleAddDoc)
		r.Get("/agents/{key}/docs/{id}", s.handleGetDoc)
		r.Delete("/agents/{key}/docs/{id}", s.handleDeleteDoc)
		r.Get("/agents/{key}/docs/search/query", s.handleSearchDocs)

		// Global skills library: upload once, enable per agent.
		r.Get("/skills", s.handleListLibrarySkills)
		r.Post("/skills/upload", s.handleUploadSkill)
		r.Delete("/skills/{key}", s.handleDeleteSkill)
		r.Post("/evolve", s.handleEvolve)

		// Conversations.
		r.Get("/sessions", s.handleListSessions)
		r.Get("/sessions/{agent}/{session}/messages", s.handleSessionMessages)

		// Capabilities.
		r.Get("/tools", s.handleListTools)

		// Providers (OpenAI-compatible endpoints).
		r.Get("/providers", s.handleListProviders)
		r.Post("/providers", s.handleUpsertProvider)
		r.Post("/providers/test", s.handleTestProvider) // ad-hoc test of a posted config
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
	Key         string              `json:"key"`
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Soul        string              `json:"soul"`
	Config      *agents.AgentConfig `json:"config,omitempty"` // optional LLM settings
}

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	var req createAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var cfg agents.AgentConfig
	if req.Config != nil {
		cfg = *req.Config
	}
	ag, err := s.agents.Create(req.Key, req.Name, req.Description, req.Soul, cfg)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, ag)
}

// handleUpdateAgentConfig replaces an agent's per-agent LLM config.
func (s *Server) handleUpdateAgentConfig(w http.ResponseWriter, r *http.Request) {
	var cfg agents.AgentConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ag, err := s.agents.UpdateConfig(chi.URLParam(r, "key"), cfg)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, ag)
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

// skillWithState is a library skill plus whether the agent in context has it
// enabled.
type skillWithState struct {
	skills.Skill
	Enabled bool `json:"enabled"`
}

// handleListAgentSkills lists the global library skills annotated with the
// agent's enabled state (Agents → Skills tab).
func (s *Server) handleListAgentSkills(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	ag, err := s.agents.Get(key)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	lib, err := s.loadLibrary()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	enabled := map[string]bool{}
	for _, k := range ag.Config.EnabledSkills {
		enabled[k] = true
	}
	out := make([]skillWithState, 0, len(lib))
	for _, sk := range lib {
		out = append(out, skillWithState{Skill: sk, Enabled: enabled[sk.Key]})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleEnableSkill enables a library skill for an agent.
func (s *Server) handleEnableSkill(w http.ResponseWriter, r *http.Request) {
	s.setSkillEnabled(w, r, true)
}

// handleDisableSkill disables a library skill for an agent.
func (s *Server) handleDisableSkill(w http.ResponseWriter, r *http.Request) {
	s.setSkillEnabled(w, r, false)
}

func (s *Server) setSkillEnabled(w http.ResponseWriter, r *http.Request, on bool) {
	key := chi.URLParam(r, "key")
	skillKey := chi.URLParam(r, "skill")

	// The skill must exist in the library.
	lib, err := s.loadLibrary()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	found := false
	for _, sk := range lib {
		if sk.Key == skillKey {
			found = true
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, fmt.Errorf("skill %q not found in library", skillKey))
		return
	}

	ag, err := s.agents.Get(key)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	set := map[string]bool{}
	for _, k := range ag.Config.EnabledSkills {
		set[k] = true
	}
	if on {
		set[skillKey] = true
	} else {
		delete(set, skillKey)
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if _, err := s.agents.SetEnabledSkills(key, keys); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": key, "skill": skillKey, "enabled": on})
}

// handleListLibrarySkills lists every skill in the global library (Skills
// page). No agent inheritance — enabling happens per agent.
func (s *Server) handleListLibrarySkills(w http.ResponseWriter, _ *http.Request) {
	lib, err := s.loadLibrary()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if lib == nil {
		lib = []skills.Skill{}
	}
	writeJSON(w, http.StatusOK, lib)
}

// handleUploadSkill installs a skill from an uploaded ZIP (multipart field
// "file") into the global skills library. It is not enabled for any agent
// automatically — enable it per agent afterwards.
func (s *Server) handleUploadSkill(w http.ResponseWriter, r *http.Request) {
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
	lib, err := s.agents.SkillsLibraryDir()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	sk, err := skills.InstallZip(lib, data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, sk)
}

// handleDeleteSkill removes a skill from the global library.
func (s *Server) handleDeleteSkill(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	lib, err := s.agents.SkillsLibraryDir()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := skills.Delete(lib, key); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": key})
}

// loadLibrary loads all skills from the global library dir.
func (s *Server) loadLibrary() ([]skills.Skill, error) {
	dir, err := s.agents.SkillsLibraryDir()
	if err != nil {
		return nil, err
	}
	return skills.Load(dir)
}

// --- Agent images (vision) ---

// handleListImages lists an agent's stored reference images.
func (s *Server) handleListImages(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	imgs, err := s.agents.ListImages(key)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, imgs)
}

// handleUploadImage stores one image (multipart field "file") for the agent.
func (s *Server) handleUploadImage(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if err := r.ParseMultipartForm(10 << 20); err != nil { // 10 MB form
		writeError(w, http.StatusBadRequest, fmt.Errorf("parse upload: %w", err))
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("missing file: %w", err))
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 9<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("read upload: %w", err))
		return
	}
	img, err := s.agents.SaveImage(key, hdr.Filename, data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, img)
}

// handleDeleteImage removes one of the agent's images.
func (s *Server) handleDeleteImage(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	name := chi.URLParam(r, "name")
	if err := s.agents.DeleteImage(key, name); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": name})
}

// handleVisionStatus reports whether the agent may attach images, given the
// optional ?provider= override the chat UI passes for the selected provider.
func (s *Server) handleVisionStatus(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	ag, err := s.agents.Get(key)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	provider := r.URL.Query().Get("provider")
	writeJSON(w, http.StatusOK, map[string]bool{"vision": s.engine.EffectiveVision(ag, provider)})
}

// imageDataURL reads one of the agent's images and returns it as a base64
// data-URL suitable for an OpenAI image_url content part.
func (s *Server) imageDataURL(agentKey, name string) (string, error) {
	data, mime, err := s.agents.ReadImage(agentKey, name)
	if err != nil {
		return "", err
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
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

// handleTestProvider tests a provider connection. On POST /api/providers/test
// (or with a JSON body on the named route) the posted provider config is
// tested as-is, so the UI can test the form's current values before saving.
// With an empty body it falls back to testing the saved provider named in the
// path.
func (s *Server) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	var adhoc *providers.Provider
	if r.Body != nil && r.ContentLength != 0 {
		var p providers.Provider
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("decode provider: %w", err))
			return
		}
		adhoc = &p
	}
	models, err := s.providers.TestConnection(r.Context(), chi.URLParam(r, "name"), adhoc)
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
	// Images are names of the agent's stored reference images to attach to this
	// turn (only sent when the effective model is vision-capable).
	Images []string `json:"images,omitempty"`
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

		// Resolve any referenced agent images into base64 data-URLs. Unknown or
		// unreadable images are skipped (the engine drops them entirely if the
		// model isn't vision-capable).
		var images []string
		for _, name := range msg.Images {
			if dataURL, err := s.imageDataURL(msg.Agent, name); err == nil {
				images = append(images, dataURL)
			}
		}

		_, runErr := s.engine.Run(ctx, msg.Agent, msg.Session, msg.Message, msg.Provider, images, emit)
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
