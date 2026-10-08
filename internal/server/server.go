// Package server exposes the HTTP API, WebSocket chat endpoint, and the
// embedded chat UI. All REST routes are registered as Huma operations (see
// routes.go), so the OpenAPI spec (/openapi.json) and the generated docs UI
// (/docs) are always in sync with the served API. The WebSocket chat endpoint
// and the embedded SPA stay on the plain mux next to the Huma API.
package server

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/dkr290/agenticgo/internal/agent"
	"github.com/dkr290/agenticgo/internal/agents"
	"github.com/dkr290/agenticgo/internal/config"
	"github.com/dkr290/agenticgo/internal/cron"
	"github.com/dkr290/agenticgo/internal/logger"
	"github.com/dkr290/agenticgo/internal/mcp"
	"github.com/dkr290/agenticgo/internal/providers"
	"github.com/dkr290/agenticgo/internal/skills"
	"github.com/dkr290/agenticgo/internal/store"
	"github.com/dkr290/agenticgo/internal/tools"
)

//go:embed all:web
var webFS embed.FS

// Server is the HTTP server for agenticgo.
type Server struct {
	cfg        *config.Config
	engine     *agent.Engine
	agents     *agents.Registry
	tools      []tools.BuiltinToolInfo // static catalog for the Built-in Tools page
	store      *store.Store
	providers  *providers.Store
	cron       *cron.Scheduler
	mcp        *mcp.Manager
	http       *http.Server
	log        logger.Logger
	wsMu       sync.Mutex
	wsActive   map[*websocket.Conn]context.CancelFunc
	wsStopping bool
	wsWG       sync.WaitGroup
}

// New builds the server.
func New(cfg *config.Config, eng *agent.Engine, ar *agents.Registry, st *store.Store, ps *providers.Store, cs *cron.Scheduler, mm *mcp.Manager) *Server {
	s := &Server{cfg: cfg, engine: eng, agents: ar, tools: tools.BuiltinTools(cfg.ToolAllowList), store: st, providers: ps, cron: cs, mcp: mm, log: logger.Nop()}

	mux := http.NewServeMux()

	// Huma API: every REST route is a registered operation, so the OpenAPI
	// spec and the docs UI are generated from the actual handlers.
	humaCfg := huma.DefaultConfig("agenticgo", "1.0.0")
	humaCfg.Info.Description = "A simplified, self-hosted AI agent gateway: multiple agents with context files, skills, knowledge/docs memory, MCP tools, and OpenAI-compatible providers."
	humaCfg.Info.Contact = &huma.Contact{Name: "agenticgo"}
	api := humago.New(mux, humaCfg)
	s.registerRoutes(api)

	// Non-REST surface: WebSocket chat and the embedded chat UI.
	mux.HandleFunc("GET /ws", s.handleWS)

	static, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("embed web: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(static)))

	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// SetLogger wires structured logging into the server. 5xx handler errors are
// always logged (huma itself never logs handler errors — it only writes them
// into the response body, which makes failures invisible on the server side);
// request logging and 4xx errors are logged at debug level only
// (AGENTICGO_DEBUG=true).
func (s *Server) SetLogger(l logger.Logger) {
	if l == nil {
		return
	}
	s.log = l
	s.http.Handler = s.logRequests(s.http.Handler)
}

// Start listens and serves until the context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	s.http.BaseContext = func(net.Listener) context.Context { return ctx }
	errCh := make(chan error, 1)
	go func() {
		log.Printf("agenticgo listening on %s (API docs at /docs)", s.cfg.Addr)
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		s.wsMu.Lock()
		s.wsStopping = true
		for conn, cancel := range s.wsActive {
			cancel()
			conn.CloseNow()
		}
		s.wsMu.Unlock()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := s.http.Shutdown(shutdownCtx)
		s.wsWG.Wait()
		return err
	case err := <-errCh:
		return err
	}
}

// --- Shared business helpers (used by the Huma handlers in routes.go) ---

// loadLibrary loads all skills from the global library dir.
func (s *Server) loadLibrary() ([]skills.Skill, error) {
	dir, err := s.agents.SkillsLibraryDir()
	if err != nil {
		return nil, err
	}
	return skills.Load(dir)
}

// setSkillEnabled enables or disables a library skill for an agent. The skill
// must exist in the library. Errors are huma errors with the right status.
func (s *Server) setSkillEnabled(key, skillKey string, on bool) error {
	// The skill must exist in the library.
	lib, err := s.loadLibrary()
	if err != nil {
		return huma.Error500InternalServerError(err.Error())
	}
	found := false
	for _, sk := range lib {
		if sk.Key == skillKey {
			found = true
			break
		}
	}
	if !found {
		return huma.Error404NotFound(fmt.Sprintf("skill %q not found in library", skillKey))
	}

	if _, err := s.agents.MutateConfig(key, func(cfg *agents.AgentConfig) error {
		cfg.EnabledSkills = toggleName(cfg.EnabledSkills, skillKey, on)
		return nil
	}); err != nil {
		return huma.Error500InternalServerError(err.Error())
	}
	return nil
}

// setCustomToolEnabled enables or disables a discovered MCP tool for an agent.
// Errors are huma errors with the right status.
func (s *Server) setCustomToolEnabled(key, name string, on bool) error {
	if !mcp.IsMCPToolName(name) {
		return huma.Error400BadRequest(fmt.Sprintf("invalid MCP tool name %q", name))
	}
	// Enabling requires the tool to be discovered (currently known from a
	// connected server); disabling is always allowed.
	if on {
		if _, ok := s.mcp.Lookup(name); !ok {
			return huma.Error404NotFound(fmt.Sprintf("tool %q not discovered — connect its MCP server first", name))
		}
	}

	if _, err := s.agents.MutateConfig(key, func(cfg *agents.AgentConfig) error {
		cfg.EnabledTools = toggleName(cfg.EnabledTools, name, on)
		return nil
	}); err != nil {
		return huma.Error500InternalServerError(err.Error())
	}
	return nil
}

// setExtraCommandEnabled enables or disables an env-declared extra (dangerous)
// exec command for an agent. Errors are huma errors with the right status.
func (s *Server) setExtraCommandEnabled(key, name string, on bool) error {
	// The command must be declared via AGENTICGO_EXTRA_EXEC_COMMANDS.
	if !slices.Contains(s.cfg.ExtraExecCommands, name) {
		return huma.Error404NotFound(fmt.Sprintf("command %q is not declared in AGENTICGO_EXTRA_EXEC_COMMANDS", name))
	}

	if _, err := s.agents.MutateConfig(key, func(cfg *agents.AgentConfig) error {
		cfg.EnabledCommands = toggleName(cfg.EnabledCommands, name, on)
		return nil
	}); err != nil {
		return huma.Error500InternalServerError(err.Error())
	}
	return nil
}

// listAgentBuiltinTools lists the built-in tools annotated with the agent's
// state (Agents → Built-in Tools tab).
func (s *Server) listAgentBuiltinTools(key string) ([]builtinToolWithState, error) {
	ag, err := s.agents.Get(key)
	if err != nil {
		return nil, huma.Error404NotFound(err.Error())
	}
	ceiling := map[string]bool{}
	for _, n := range s.cfg.ToolAllowList {
		ceiling[n] = true
	}
	allowed := func(n string) bool { return len(ceiling) == 0 || ceiling[n] }

	narrowed := map[string]bool{}
	if ag.Config.EnabledBuiltinTools != nil {
		for _, n := range *ag.Config.EnabledBuiltinTools {
			narrowed[n] = true
		}
	}
	out := make([]builtinToolWithState, 0, len(builtinTools))
	for _, n := range builtinTools {
		a := allowed(n)
		enabled := a && (ag.Config.EnabledBuiltinTools == nil || narrowed[n])
		out = append(out, builtinToolWithState{Name: n, Allowed: a, Enabled: enabled})
	}
	return out, nil
}

// setBuiltinToolEnabled narrows the agent's built-in tool set. The global
// allow-list is the ceiling: a tool not permitted there can never be enabled
// per agent. Errors are huma errors with the right status.
func (s *Server) setBuiltinToolEnabled(key, name string, on bool) error {
	if !slices.Contains(builtinTools, name) {
		return huma.Error400BadRequest(fmt.Sprintf("unknown built-in tool %q", name))
	}
	// The global allow-list is the ceiling: a tool not permitted there can
	// never be enabled per agent.
	if on && len(s.cfg.ToolAllowList) > 0 && !slices.Contains(s.cfg.ToolAllowList, name) {
		return huma.Error409Conflict(fmt.Sprintf("tool %q is not on the global AGENTICGO_TOOL_ALLOWLIST", name))
	}

	// Materialize the current effective set (inherit = the global ceiling,
	// or all built-ins when no ceiling is configured), then apply the change.
	if _, err := s.agents.MutateConfig(key, func(cfg *agents.AgentConfig) error {
		var names []string
		if cfg.EnabledBuiltinTools != nil {
			names = *cfg.EnabledBuiltinTools
		} else {
			for _, n := range builtinTools {
				if len(s.cfg.ToolAllowList) == 0 || slices.Contains(s.cfg.ToolAllowList, n) {
					names = append(names, n)
				}
			}
		}
		names = toggleName(names, name, on)
		cfg.EnabledBuiltinTools = &names
		return nil
	}); err != nil {
		return huma.Error404NotFound(err.Error())
	}
	return nil
}

func toggleName(names []string, name string, on bool) []string {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	if on {
		set[name] = true
	} else {
		delete(set, name)
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// imageDataURL reads one of the agent's images and returns it as a base64
// data-URL suitable for an OpenAI image_url content part.
func (s *Server) imageDataURL(agentKey, name string) (string, error) {
	data, mimeType, err := s.agents.ReadImage(agentKey, name)
	if err != nil {
		return "", err
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// --- Multipart helpers ---

// firstMultipartFile reads the first file of the named field from a parsed
// multipart form, capped at max bytes.
func firstMultipartFile(form *multipart.Form, field string, maxSize int64) ([]byte, error) {
	data, _, err := firstMultipartFileNamed(form, field, maxSize)
	return data, err
}

// firstMultipartFileNamed is firstMultipartFile but also returns the uploaded
// filename.
func firstMultipartFileNamed(form *multipart.Form, field string, maxSize int64) ([]byte, string, error) {
	if form == nil || form.File == nil {
		return nil, "", fmt.Errorf("missing multipart file field %q", field)
	}
	headers := form.File[field]
	if len(headers) == 0 {
		return nil, "", fmt.Errorf("missing file: multipart field %q is required", field)
	}
	f, err := headers[0].Open()
	if err != nil {
		return nil, "", fmt.Errorf("open upload: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSize))
	if err != nil {
		return nil, "", fmt.Errorf("read upload: %w", err)
	}
	return data, headers[0].Filename, nil
}

// parseMultipartFileBody re-parses a raw request body as multipart/form-data
// (used by the add-doc endpoint which accepts both JSON and multipart) and
// returns the uploaded file's title (filename without extension) and content.
// contentType is the request's Content-Type header (already parsed by huma
// into the input struct).
func parseMultipartFileBody(contentType string, raw []byte, field string, maxSize int64) (string, []byte, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", nil, fmt.Errorf("parse content type: %w", err)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return "", nil, fmt.Errorf("missing multipart boundary")
	}
	r := multipart.NewReader(bytes.NewReader(raw), boundary)
	form, err := r.ReadForm(maxSize)
	if err != nil {
		return "", nil, fmt.Errorf("parse upload: %w", err)
	}
	defer form.RemoveAll()
	data, filename, err := firstMultipartFileNamed(form, field, maxSize)
	if err != nil {
		return "", nil, err
	}
	title := strings.TrimSuffix(filename, filepath.Ext(filename))
	return title, data, nil
}

// --- WebSocket chat ---

type wsMessage struct {
	Kind     string `json:"kind,omitempty"` // "cancel" stops the active turn
	ID       string `json:"id,omitempty"`
	Agent    string `json:"agent"`
	Session  string `json:"session"`
	Message  string `json:"message"`
	Provider string `json:"provider"` // optional provider override
	// Images are names of the agent's stored reference images to attach to this
	// turn (only sent when the effective model is vision-capable).
	Images []string `json:"images,omitempty"`
}

type wsEvent struct {
	ID         string `json:"id,omitempty"`
	Kind       string `json:"kind"` // text | tool_call | tool_result | done | cancelled | error
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
	socketCtx, closeSocket := context.WithCancel(r.Context())
	s.wsMu.Lock()
	if s.wsStopping {
		s.wsMu.Unlock()
		closeSocket()
		return
	}
	if s.wsActive == nil {
		s.wsActive = make(map[*websocket.Conn]context.CancelFunc)
	}
	s.wsActive[conn] = closeSocket
	s.wsWG.Add(1)
	s.wsMu.Unlock()
	defer func() {
		s.wsMu.Lock()
		delete(s.wsActive, conn)
		s.wsMu.Unlock()
		s.wsWG.Done()
	}()
	var runMu sync.Mutex
	var runCancel context.CancelFunc
	var runID string
	var runs sync.WaitGroup
	defer func() { closeSocket(); runs.Wait() }()

	conn.SetReadLimit(1 << 20) // 1 MiB

	for {
		_, data, err := conn.Read(socketCtx)
		if err != nil {
			return // client disconnected or read error
		}

		var msg wsMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			if err := s.sendEvent(socketCtx, conn, wsEvent{Kind: "error", Error: "invalid message: " + err.Error()}); err != nil {
				return
			}
			continue
		}
		runMu.Lock()
		if msg.Kind == "cancel" {
			if runCancel != nil && (msg.ID == "" || msg.ID == runID) {
				runCancel()
			}
			runMu.Unlock()
			continue
		}
		if runCancel != nil {
			runMu.Unlock()
			if err := s.sendEvent(socketCtx, conn, wsEvent{ID: msg.ID, Kind: "error", Error: "a chat turn is already running"}); err != nil {
				return
			}
			continue
		}
		if msg.Agent == "" {
			msg.Agent = "default"
		}
		if msg.Session == "" {
			msg.Session = "default"
		}

		ctx, cancel := context.WithCancel(socketCtx)
		runCancel, runID = cancel, msg.ID
		runMu.Unlock()
		runs.Add(1)
		go func(msg wsMessage) {
			defer runs.Done()
			defer cancel()
			emit := func(ev agent.Event) {
				// Send a single terminal event after releasing the active run.
				if ev.Kind == "done" || ev.Kind == "error" {
					return
				}
				out := wsEvent{
					ID:         msg.ID,
					Kind:       ev.Kind,
					Text:       ev.Text,
					ToolName:   ev.ToolName,
					ToolArgs:   ev.ToolArgs,
					ToolResult: ev.ToolResult,
				}
				if ev.Err != nil {
					out.Error = ev.Err.Error()
				}
				if err := s.sendEvent(ctx, conn, out); err != nil {
					if ctx.Err() == nil {
						closeSocket()
					}
					cancel()
				}
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
			terminal := wsEvent{ID: msg.ID, Kind: "done"}
			if errors.Is(runErr, context.Canceled) {
				terminal.Kind = "cancelled"
			} else if runErr != nil {
				s.log.Error("ws chat run failed", "agent", msg.Agent, "session", msg.Session, "error", runErr)
				terminal.Kind, terminal.Error = "error", runErr.Error()
			}
			runMu.Lock()
			runCancel, runID = nil, ""
			if err := s.sendEvent(socketCtx, conn, terminal); err != nil {
				closeSocket()
			}
			runMu.Unlock()
		}(msg)
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

// logErr logs an API handler error: 5xx at error level (always — these are
// server-side failures), 4xx at debug level (client mistakes; noisy). It
// returns err unchanged so handlers can `return nil, s.logErr(err)`.
func (s *Server) logErr(err error) error {
	var se huma.StatusError
	if errors.As(err, &se) {
		if se.GetStatus() >= 500 {
			s.log.Error("api handler error", "status", se.GetStatus(), "error", se.Error())
		} else {
			s.log.Debug("api request rejected", "status", se.GetStatus(), "error", se.Error())
		}
	} else {
		s.log.Error("api handler error", "error", err)
	}
	return err
}

// statusRecorder captures the response status code for request logging. It
// must pass through the optional net/http interfaces the underlying writer
// implements — notably http.Hijacker, or the /ws WebSocket upgrade breaks
// ("ResponseWriter does not implement http.Hijacker").
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Hijack implements http.Hijacker by delegating to the wrapped writer.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("wrapped ResponseWriter does not implement http.Hijacker")
	}
	return h.Hijack()
}

// Flush implements http.Flusher by delegating to the wrapped writer.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the wrapped ResponseWriter (http.ResponseController support).
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// logRequests logs one line per request at debug level (method, path, status,
// duration). Silent unless AGENTICGO_DEBUG=true. WebSocket upgrades are passed
// through unwrapped — a hijacked long-lived connection has no meaningful
// status/duration to log, and wrapping must not interfere with the upgrade.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Debug("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
