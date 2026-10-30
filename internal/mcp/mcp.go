// Package mcp implements the Model Context Protocol client side of agenticgo:
// it keeps a JSON-persisted registry of external MCP servers (stdio commands
// or HTTP endpoints), connects to them on demand, discovers their tools, and
// invokes them.
//
// Discovered tools are exposed to agents under namespaced names
// ("mcp_<server>_<tool>") and gated per agent via AgentConfig.EnabledTools —
// the "MCP Tools" tab in the UI. Nothing is inherited: an agent gets only
// the tools explicitly ticked for it.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dkr290/agenticgo/internal/logger"
)

// DiscoveredPrefix prefixes every namespaced MCP tool name.
const DiscoveredPrefix = "mcp_"

// ServerConfig is a persisted MCP server definition (data/mcp_servers.json).
type ServerConfig struct {
	ID        string    `json:"id,omitempty"`
	Name      string    `json:"name"` // unique key; becomes part of tool names
	Transport string    `json:"transport"`
	Command   string    `json:"command,omitempty"`
	Args      []string  `json:"args,omitempty"`
	URL       string    `json:"url,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	// Enabled records the operator's intent that this server "should be
	// connected". Set by Connect, cleared ONLY by Disconnect (GUI or API).
	// After a pod restart the manager auto-reconnects every Enabled server
	// (bounded retry); an Enabled server whose connection drops is likewise
	// redialed. Disabled servers are never dialed automatically.
	Enabled bool `json:"enabled,omitempty"`
}

// ToolInfo is one discovered tool on a connected server.
type ToolInfo struct {
	Name        string         `json:"name"`        // original name from the server
	Description string         `json:"description"` // from the server
	Server      string         `json:"server"`      // owning server config Name
	Schema      map[string]any `json:"-"`           // input schema captured at discovery
}

// DiscoveredName returns the agenticgo-global tool name (mcp_<server>_<tool>).
func (t ToolInfo) DiscoveredName() string { return ToolName(t.Server, t.Name) }

// ToolName builds the namespaced tool name for a server + tool.
func ToolName(serverName, toolName string) string {
	return DiscoveredPrefix + serverName + "_" + toolName
}

// ServerStatus is the runtime state of a server for API/UI display.
type ServerStatus struct {
	ServerConfig
	Connected bool   `json:"connected"`
	Error     string `json:"error,omitempty"`
	ToolCount int    `json:"tool_count"`
}

// connection is one live client session to a server.
type connection struct {
	session *sdk.ClientSession
	tools   []ToolInfo
	err     string // last connect/call error (retained for status display)
}

// Manager is a thread-safe registry of MCP servers: CRUD persisted to JSON,
// plus on-demand connections and a discovered-tool catalog. Connections are
// established manually (the Connect API / UI button) or lazily re-established
// when a tool call arrives for a configured-but-disconnected server. Servers
// marked Enabled are additionally auto-reconnected by a bounded-retry
// background loop (Start) so they survive pod restarts and dropped sessions.
type Manager struct {
	path string
	log  logger.Logger

	// dial builds a transport for a server config. Overridable in tests
	// (e.g. an in-memory transport to a local server); nil uses the real
	// stdio/HTTP transports.
	dial func(ctx context.Context, srv *ServerConfig) (sdk.Transport, error)

	// reconnectAttempts bounds how many times the auto-reconnect loop dials an
	// Enabled server before giving up (0 = use the default). reconnectInterval
	// is the base delay between attempts (exponential backoff, capped).
	reconnectAttempts int
	reconnectInterval time.Duration

	mu      sync.RWMutex
	servers map[string]*ServerConfig
	conns   map[string]*connection // keyed by server ID

	// retry tracks the auto-reconnect loop's per-server failure count and the
	// cancel func for an in-flight dial, so Disconnect/Delete stop it.
	retry map[string]*retryState

	// bgCtx is the long-lived context passed to Start (cancelled on shutdown).
	// Update uses it to re-arm the reconnect loop for a still-Enabled server
	// whose config changed. Nil until Start runs.
	bgCtx context.Context
}

// retryState is the auto-reconnect bookkeeping for one server.
type retryState struct {
	failures int
	cancel   context.CancelFunc
	ctx      context.Context // the loop's lctx; nil-Err means still live
}

// Open loads (or initializes) the MCP server registry at path.
func Open(path string) (*Manager, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create mcp dir: %w", err)
	}
	m := &Manager{
		path:    path,
		log:     logger.Nop(),
		servers: map[string]*ServerConfig{},
		conns:   map[string]*connection{},
		retry:   map[string]*retryState{},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, fmt.Errorf("read mcp servers: %w", err)
	}
	var list []*ServerConfig
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parse mcp servers: %w", err)
	}
	for _, s := range list {
		if s != nil && s.ID != "" {
			m.servers[s.ID] = s
		}
	}
	return m, nil
}

// SetLogger wires verbose logging into the manager (nil keeps a no-op logger).
func (m *Manager) SetLogger(l logger.Logger) {
	if l == nil {
		l = logger.Nop()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.log = l
}

// SetDial overrides transport construction (tests only): nil restores the
// real stdio/HTTP transports.
func (m *Manager) SetDial(dial func(ctx context.Context, srv *ServerConfig) (sdk.Transport, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dial = dial
}

// Default reconnect policy for Enabled servers, overridable via SetReconnectPolicy.
const (
	defaultReconnectAttempts = 10
	defaultReconnectInterval = 5 * time.Second
	maxReconnectBackoff      = 60 * time.Second
)

// SetReconnectPolicy tunes the auto-reconnect loop for Enabled servers:
// attempts bounds the dials before giving up (<=0 uses the default), interval
// is the base backoff delay (<=0 uses the default).
func (m *Manager) SetReconnectPolicy(attempts int, interval time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reconnectAttempts = attempts
	m.reconnectInterval = interval
}

// reconnectConfig resolves the effective retry policy.
func (m *Manager) reconnectConfig() (attempts int, interval time.Duration) {
	attempts = m.reconnectAttempts
	if attempts <= 0 {
		attempts = defaultReconnectAttempts
	}
	interval = m.reconnectInterval
	if interval <= 0 {
		interval = defaultReconnectInterval
	}
	return attempts, interval
}

func (m *Manager) saveLocked() error {
	list := make([]*ServerConfig, 0, len(m.servers))
	for _, s := range m.servers {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal mcp servers: %w", err)
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write mcp servers: %w", err)
	}
	if err := os.Rename(tmp, m.path); err != nil {
		return fmt.Errorf("persist mcp servers: %w", err)
	}
	return nil
}

func validName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func validateServer(s *ServerConfig) error {
	if !validName(s.Name) {
		return fmt.Errorf("name must be 1-64 chars of a-z, 0-9, -, _ (it becomes part of tool names)")
	}
	switch s.Transport {
	case "stdio":
		if strings.TrimSpace(s.Command) == "" {
			return fmt.Errorf("command is required for stdio transport")
		}
	case "http":
		if strings.TrimSpace(s.URL) == "" {
			return fmt.Errorf("url is required for http transport")
		}
	default:
		return fmt.Errorf("transport must be \"stdio\" or \"http\"")
	}
	return nil
}

// Add registers a new MCP server definition and persists it.
func (m *Manager) Add(srv ServerConfig) (*ServerConfig, error) {
	if err := validateServer(&srv); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.servers {
		if s.Name == srv.Name {
			return nil, fmt.Errorf("server %q already exists", srv.Name)
		}
	}
	cp := srv
	cp.ID = fmt.Sprintf("mcp-%d", time.Now().UnixNano())
	cp.CreatedAt = time.Now()
	m.servers[cp.ID] = &cp
	if err := m.saveLocked(); err != nil {
		delete(m.servers, cp.ID)
		return nil, err
	}
	out := cp
	return &out, nil
}

// Update replaces an existing server definition and drops its old connection
// and discovery cache. The stay-connected intent (Enabled) is preserved; a
// still-Enabled server is reconnected automatically with the new settings.
func (m *Manager) Update(id string, srv ServerConfig) (*ServerConfig, error) {
	if err := validateServer(&srv); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.servers[id]
	if !ok {
		return nil, fmt.Errorf("mcp server %q not found", id)
	}
	for _, s := range m.servers {
		if s.ID != id && s.Name == srv.Name {
			return nil, fmt.Errorf("server %q already exists", srv.Name)
		}
	}
	srv.ID = id
	srv.CreatedAt = cur.CreatedAt
	srv.Enabled = cur.Enabled // config edits never change the stay-connected intent
	m.servers[id] = &srv
	if err := m.saveLocked(); err != nil {
		m.servers[id] = cur
		return nil, err
	}
	m.stopRetryLocked(id)
	if conn, ok := m.conns[id]; ok {
		if conn.session != nil {
			_ = conn.session.Close()
		}
		delete(m.conns, id)
	}
	// A still-enabled server whose config changed should reconnect with the new
	// settings without waiting for a pod restart or a manual Connect.
	bg := m.bgCtx
	stillEnabled := srv.Enabled
	out := srv
	if stillEnabled && bg != nil && bg.Err() == nil {
		// Unlock before arming the loop (retryLoop takes m.mu itself).
		m.mu.Unlock()
		m.retryLoop(bg, id)
		m.mu.Lock()
	}
	return &out, nil
}

// Delete removes a server, closing any live connection.
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	srv, ok := m.servers[id]
	if !ok {
		return fmt.Errorf("mcp server %q not found", id)
	}
	delete(m.servers, id)
	if err := m.saveLocked(); err != nil {
		m.servers[id] = srv
		return err
	}
	m.stopRetryLocked(id)
	conn, connected := m.conns[id]
	if connected {
		if conn.session != nil {
			_ = conn.session.Close()
		}
		delete(m.conns, id)
	}
	return nil
}

// ListServers returns every configured server with its runtime status.
func (m *Manager) ListServers() []ServerStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ServerStatus, 0, len(m.servers))
	for _, s := range m.servers {
		out = append(out, m.statusLocked(s.ID))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// statusLocked builds the status for one server. Caller holds at least a read lock.
func (m *Manager) statusLocked(id string) ServerStatus {
	s := m.servers[id]
	st := ServerStatus{ServerConfig: *s}
	if conn, ok := m.conns[id]; ok {
		st.Connected = conn.session != nil && conn.err == ""
		st.Error = conn.err
		st.ToolCount = len(conn.tools)
	}
	return st
}

// connectLocked dials a server and discovers its tools. Caller holds m.mu.
func (m *Manager) connectLocked(ctx context.Context, id string) (*connection, error) {
	srv, ok := m.servers[id]
	if !ok {
		return nil, fmt.Errorf("mcp server %q not found", id)
	}
	if conn, ok := m.conns[id]; ok && conn.session != nil {
		_ = conn.session.Close()
		delete(m.conns, id)
	}

	client := sdk.NewClient(&sdk.Implementation{Name: "agenticgo", Version: "0.1.0"}, nil)
	var transport sdk.Transport
	if m.dial != nil {
		t, err := m.dial(ctx, srv)
		if err != nil {
			m.conns[id] = &connection{err: err.Error()}
			return nil, fmt.Errorf("dial %q: %w", srv.Name, err)
		}
		transport = t
	} else {
		switch srv.Transport {
		case "stdio":
			// The SDK session owns and closes this process. The request context
			// bounds the handshake, not the lifetime of a connected server.
			transport = &sdk.CommandTransport{Command: exec.Command(srv.Command, srv.Args...)}
		default: // http
			transport = &sdk.StreamableClientTransport{Endpoint: srv.URL, MaxRetries: 0}
		}
	}

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		m.conns[id] = &connection{err: err.Error()}
		return nil, fmt.Errorf("connect %q: %w", srv.Name, err)
	}

	var tools []ToolInfo
	for t, err := range session.Tools(ctx, nil) {
		if err != nil {
			_ = session.Close()
			m.conns[id] = &connection{err: err.Error()}
			return nil, fmt.Errorf("list tools on %q: %w", srv.Name, err)
		}
		schema, _ := t.InputSchema.(map[string]any)
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		tools = append(tools, ToolInfo{Name: t.Name, Description: t.Description, Server: srv.Name, Schema: schema})
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })

	conn := &connection{session: session, tools: tools}
	m.conns[id] = conn
	m.log.Info("mcp server connected", "server", srv.Name, "tools", len(tools))
	return conn, nil
}

// Connect establishes (or re-establishes) the connection to a server and
// refreshes its discovered tool list. On success it marks the server Enabled
// (should-stay-connected) and persists that intent so it is auto-reconnected
// after a pod restart; on failure the prior Enabled value is left unchanged.
func (m *Manager) Connect(ctx context.Context, id string) (*ServerStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[id]; !ok {
		return nil, fmt.Errorf("mcp server %q not found", id)
	}
	if _, err := m.connectLocked(ctx, id); err != nil {
		st := m.statusLocked(id)
		return &st, err
	}
	// Success: clear any retry bookkeeping and record the stay-connected intent.
	delete(m.retry, id)
	if !m.servers[id].Enabled {
		m.servers[id].Enabled = true
		if err := m.saveLocked(); err != nil {
			return nil, fmt.Errorf("persist enabled: %w", err)
		}
	}
	st := m.statusLocked(id)
	return &st, nil
}

// Disconnect closes the connection to a server (keeping its definition),
// clears the stay-connected intent (so it is NOT auto-reconnected), and stops
// any in-flight retry loop. This is the only place Enabled is cleared.
func (m *Manager) Disconnect(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[id]; !ok {
		return fmt.Errorf("mcp server %q not found", id)
	}
	m.stopRetryLocked(id)
	if conn, ok := m.conns[id]; ok {
		if conn.session != nil {
			_ = conn.session.Close()
		}
		delete(m.conns, id)
	}
	if m.servers[id].Enabled {
		m.servers[id].Enabled = false
		if err := m.saveLocked(); err != nil {
			return fmt.Errorf("persist disabled: %w", err)
		}
	}
	return nil
}

// stopRetryLocked cancels an in-flight retry dial and clears bookkeeping.
// Caller holds m.mu.
func (m *Manager) stopRetryLocked(id string) {
	if rs, ok := m.retry[id]; ok {
		if rs.cancel != nil {
			rs.cancel()
		}
		delete(m.retry, id)
	}
}

// Start launches a bounded-retry auto-reconnect for every server marked
// Enabled (should-stay-connected). It runs in the background so a slow or
// unreachable server never blocks boot. Each Enabled server is dialed until it
// connects, Disconnect is pressed, the retry budget is exhausted, or ctx is
// cancelled (shutdown). Disabled servers are never touched.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	m.bgCtx = ctx
	var enabled []string
	for id, srv := range m.servers {
		if srv.Enabled {
			enabled = append(enabled, id)
		}
	}
	m.mu.Unlock()
	for _, id := range enabled {
		m.retryLoop(ctx, id)
	}
}

// retryLoop dials one Enabled server with exponential backoff until it
// connects, is disconnected/deleted, the attempt budget runs out, or ctx is
// cancelled. Bookkeeping lives in m.retry so Disconnect/Delete can stop it.
func (m *Manager) retryLoop(ctx context.Context, id string) {
	maxAttempts, base := m.reconnectConfig()

	m.mu.Lock()
	// A live (not-yet-cancelled) loop is already running for this server; don't
	// double-dial. A loop whose context was already cancelled (e.g. by
	// stopRetryLocked in Update/Disconnect) is tearing down and must not block
	// a fresh arm — replace its bookkeeping.
	if rs, ok := m.retry[id]; ok && rs.cancel != nil && rs.ctx.Err() == nil {
		m.mu.Unlock()
		return
	}
	lctx, cancel := context.WithCancel(ctx)
	self := &retryState{cancel: cancel, ctx: lctx}
	m.retry[id] = self
	m.mu.Unlock()

	go func() {
		defer func() {
			m.mu.Lock()
			// Remove bookkeeping only if it still belongs to THIS loop — a new
			// loop may have been armed (e.g. by Update) after this one stopped.
			if rs, ok := m.retry[id]; ok && rs == self {
				delete(m.retry, id)
			}
			m.mu.Unlock()
		}()
		backoff := base
		for failures := 0; ; {
			// Stop if the server went away or is no longer meant to be connected.
			m.mu.RLock()
			srv, ok := m.servers[id]
			enabled := ok && srv.Enabled
			m.mu.RUnlock()
			if !enabled {
				return
			}

			m.mu.Lock()
			// If a newer loop has taken over (Update/Disconnect re-armed), or our
			// context is already cancelled, exit WITHOUT touching m.conns — a stale
			// connectLocked would close the newer loop's healthy session and then
			// overwrite its state with a cancelled-context error.
			if m.retry[id] != self || lctx.Err() != nil {
				m.mu.Unlock()
				return
			}
			_, err := m.connectLocked(lctx, id)
			name := ""
			if s, ok := m.servers[id]; ok {
				name = s.Name
			}
			if err == nil {
				m.mu.Unlock()
				m.log.Info("mcp server auto-reconnected", "server", name)
				return
			}
			failures++
			// Record failures on OUR entry only if it's still the current one.
			if rs := m.retry[id]; rs == self {
				rs.failures = failures
			}
			m.mu.Unlock()

			if failures >= maxAttempts {
				m.log.Error("mcp auto-reconnect gave up", "server", name, "attempts", failures, "error", err)
				return
			}
			m.log.Debug("mcp auto-reconnect failed, retrying", "server", name, "attempt", failures, "of", maxAttempts, "in", backoff, "error", err)

			select {
			case <-lctx.Done():
				return
			case <-time.After(backoff):
			}
			// Exponential backoff, capped.
			backoff *= 2
			if backoff > maxReconnectBackoff {
				backoff = maxReconnectBackoff
			}
		}
	}()
}

// Tools returns the discovered tool catalog across all connected servers,
// sorted by namespaced name. Servers that were never connected contribute
// nothing (their tools are unknown until first Connect).
func (m *Manager) Tools() []ToolInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []ToolInfo
	for _, conn := range m.conns {
		if conn.session == nil || conn.err != "" {
			continue
		}
		out = append(out, conn.tools...)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].DiscoveredName() < out[j].DiscoveredName()
	})
	return out
}

// Lookup returns one discovered tool by its namespaced name.
func (m *Manager) Lookup(discoveredName string) (ToolInfo, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, tool, ok := m.resolveLocked(discoveredName)
	if !ok {
		return ToolInfo{}, false
	}
	if conn, has := m.conns[id]; has {
		for _, t := range conn.tools {
			if t.Name == tool.Name {
				return t, true
			}
		}
	}
	return tool, false
}

// resolveLocked maps a namespaced tool name back to (serverID, tool). MCP
// tool names may contain underscores, so resolution matches against known
// server names (prefix "mcp_<serverName>_") rather than splitting on "_".
// When several server names are prefixes of each other (e.g. "kube" and
// "kube_system"), the longest match wins so the tool maps to the most
// specific server.
func (m *Manager) resolveLocked(discoveredName string) (string, ToolInfo, bool) {
	bestLen := -1
	var bestID string
	var bestTool string
	for id, srv := range m.servers {
		prefix := ToolName(srv.Name, "")
		if !strings.HasPrefix(discoveredName, prefix) {
			continue
		}
		toolName := strings.TrimPrefix(discoveredName, prefix)
		if toolName == "" {
			continue
		}
		if len(prefix) > bestLen {
			bestLen = len(prefix)
			bestID = id
			bestTool = toolName
		}
	}
	if bestLen < 0 {
		return "", ToolInfo{}, false
	}
	return bestID, ToolInfo{Name: bestTool, Server: m.servers[bestID].Name}, true
}

// CallTool invokes a namespaced tool ("mcp_<server>_<tool>"). If the owning
// server is configured but not connected, a lazy reconnect is attempted.
func (m *Manager) CallTool(ctx context.Context, discoveredName string, args json.RawMessage) (string, error) {
	m.mu.Lock()
	id, tool, ok := m.resolveLocked(discoveredName)
	if !ok {
		m.mu.Unlock()
		return "", fmt.Errorf("unknown mcp tool %q (is the server connected and the tool enabled?)", discoveredName)
	}
	conn, hasConn := m.conns[id]
	if !hasConn || conn.session == nil || conn.err != "" {
		m.log.Debug("mcp lazy reconnect", "tool", discoveredName)
		var err error
		conn, err = m.connectLocked(ctx, id)
		if err != nil {
			m.mu.Unlock()
			return "", err
		}
	}
	session := conn.session
	serverName := m.servers[id].Name
	m.mu.Unlock()

	var arguments any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &arguments); err != nil {
			return "", fmt.Errorf("decode tool args: %w", err)
		}
	}

	result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: tool.Name, Arguments: arguments})
	if err != nil {
		// Session may be dead (server restarted): mark it so the next call
		// triggers a reconnect instead of failing forever.
		m.mu.Lock()
		if c, ok := m.conns[id]; ok && c.session == session {
			c.err = err.Error()
		}
		m.mu.Unlock()
		return "", fmt.Errorf("call %s on %s: %w", tool.Name, serverName, err)
	}

	var b strings.Builder
	for _, c := range result.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		} else if data, err := json.Marshal(c); err == nil {
			// Non-text content (images, resources): emit its JSON so nothing
			// is silently dropped.
			b.Write(data)
		}
		b.WriteString("\n")
	}
	out := strings.TrimSpace(b.String())
	if result.IsError {
		return "", fmt.Errorf("%s", out)
	}
	if out == "" {
		out = "(no output)"
	}
	return out, nil
}

// CloseAll closes every live connection and stops every retry loop (shutdown).
func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.retry {
		m.stopRetryLocked(id)
	}
	for id, conn := range m.conns {
		if conn.session != nil {
			_ = conn.session.Close()
		}
		delete(m.conns, id)
	}
}

// IsMCPToolName reports whether a tool name is in the MCP namespace.
func IsMCPToolName(name string) bool { return strings.HasPrefix(name, DiscoveredPrefix) }
