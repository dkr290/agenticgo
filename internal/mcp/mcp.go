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
	ID        string    `json:"id"`
	Name      string    `json:"name"` // unique key; becomes part of tool names
	Transport string    `json:"transport"`
	Command   string    `json:"command,omitempty"`
	Args      []string  `json:"args,omitempty"`
	URL       string    `json:"url,omitempty"`
	CreatedAt time.Time `json:"created_at"`
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
// when a tool call arrives for a configured-but-disconnected server.
type Manager struct {
	path string
	log  logger.Logger

	// dial builds a transport for a server config. Overridable in tests
	// (e.g. an in-memory transport to a local server); nil uses the real
	// stdio/HTTP transports.
	dial func(ctx context.Context, srv *ServerConfig) (sdk.Transport, error)

	mu      sync.RWMutex
	servers map[string]*ServerConfig
	conns   map[string]*connection // keyed by server ID
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

// Update replaces an existing server definition. If the server is connected
// and its name changed, the connection is dropped (its tools were namespaced
// under the old name); new settings apply on the next connect.
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
	conn, connected := m.conns[id]
	if connected && srv.Name != cur.Name {
		_ = conn.session.Close()
		delete(m.conns, id)
	}
	srv.ID = id
	srv.CreatedAt = cur.CreatedAt
	m.servers[id] = &srv
	if err := m.saveLocked(); err != nil {
		return nil, err
	}
	out := srv
	return &out, nil
}

// Delete removes a server, closing any live connection.
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[id]; !ok {
		return fmt.Errorf("mcp server %q not found", id)
	}
	conn, connected := m.conns[id]
	if connected {
		_ = conn.session.Close()
		delete(m.conns, id)
	}
	delete(m.servers, id)
	return m.saveLocked()
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
			transport = &sdk.CommandTransport{Command: exec.CommandContext(ctx, srv.Command, srv.Args...)}
		default: // http
			transport = &sdk.StreamableClientTransport{Endpoint: srv.URL, MaxRetries: 0}
		}
	}

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		m.conns[id] = &connection{err: err.Error()}
		return nil, fmt.Errorf("connect %q: %w", srv.Name, err)
	}

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		_ = session.Close()
		m.conns[id] = &connection{err: err.Error()}
		return nil, fmt.Errorf("list tools on %q: %w", srv.Name, err)
	}
	tools := make([]ToolInfo, 0, len(res.Tools))
	for _, t := range res.Tools {
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
// refreshes its discovered tool list.
func (m *Manager) Connect(ctx context.Context, id string) (*ServerStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.connectLocked(ctx, id); err != nil {
		st := m.statusLocked(id)
		return &st, err
	}
	st := m.statusLocked(id)
	return &st, nil
}

// Disconnect closes the connection to a server (keeping its definition).
func (m *Manager) Disconnect(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[id]; !ok {
		return fmt.Errorf("mcp server %q not found", id)
	}
	if conn, ok := m.conns[id]; ok {
		if conn.session != nil {
			_ = conn.session.Close()
		}
		delete(m.conns, id)
	}
	return nil
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

// CloseAll closes every live connection (server shutdown).
func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, conn := range m.conns {
		if conn.session != nil {
			_ = conn.session.Close()
		}
		delete(m.conns, id)
	}
}

// IsMCPToolName reports whether a tool name is in the MCP namespace.
func IsMCPToolName(name string) bool { return strings.HasPrefix(name, DiscoveredPrefix) }
