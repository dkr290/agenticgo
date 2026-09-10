// Package scaffold holds in-memory scaffolding registries for features that
// are designed but not yet implemented: MCP servers (Phase 3) and cron jobs.
// They back the UI pages and API endpoints so the shape of the data and the
// workflows are settled before the real functionality lands.
package scaffold

import (
	"fmt"
	"sync"
	"time"
)

// MCPServer is a configured (but not yet connected) MCP server.
type MCPServer struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Transport string    `json:"transport"` // "stdio" | "http"
	Command   string    `json:"command,omitempty"`
	Args      []string  `json:"args,omitempty"`
	URL       string    `json:"url,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// CronJob is a scheduled (but not yet executed) agent run.
type CronJob struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Schedule  string    `json:"schedule"` // cron expression, e.g. "*/5 * * * *"
	Agent     string    `json:"agent"`
	Prompt    string    `json:"prompt"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

// Store is a thread-safe in-memory store for scaffolding resources.
// State is intentionally not persisted yet.
type Store struct {
	mu      sync.RWMutex
	mcp     map[string]*MCPServer
	cron    map[string]*CronJob
	counter int
}

// New creates an empty scaffold store.
func New() *Store {
	return &Store{
		mcp:  map[string]*MCPServer{},
		cron: map[string]*CronJob{},
	}
}

func (s *Store) nextID(prefix string) string {
	s.counter++
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano()/1e6, s.counter)
}

// --- MCP servers ---

// AddMCPServer registers an MCP server definition.
func (s *Store) AddMCPServer(srv MCPServer) (*MCPServer, error) {
	if srv.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if srv.Transport != "stdio" && srv.Transport != "http" {
		return nil, fmt.Errorf("transport must be \"stdio\" or \"http\"")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := srv
	cp.ID = s.nextID("mcp")
	cp.CreatedAt = time.Now()
	s.mcp[cp.ID] = &cp
	out := cp
	return &out, nil
}

// ListMCPServers returns all registered MCP servers.
func (s *Store) ListMCPServers() []MCPServer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]MCPServer, 0, len(s.mcp))
	for _, srv := range s.mcp {
		out = append(out, *srv)
	}
	return out
}

// DeleteMCPServer removes an MCP server by ID.
func (s *Store) DeleteMCPServer(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.mcp[id]; !ok {
		return fmt.Errorf("mcp server %q not found", id)
	}
	delete(s.mcp, id)
	return nil
}

// --- Cron jobs ---

// AddCronJob registers a cron job definition.
func (s *Store) AddCronJob(job CronJob) (*CronJob, error) {
	if job.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if job.Schedule == "" {
		return nil, fmt.Errorf("schedule is required")
	}
	if job.Agent == "" {
		job.Agent = "default"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := job
	cp.ID = s.nextID("cron")
	cp.CreatedAt = time.Now()
	s.cron[cp.ID] = &cp
	out := cp
	return &out, nil
}

// ListCronJobs returns all registered cron jobs.
func (s *Store) ListCronJobs() []CronJob {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]CronJob, 0, len(s.cron))
	for _, j := range s.cron {
		out = append(out, *j)
	}
	return out
}

// DeleteCronJob removes a cron job by ID.
func (s *Store) DeleteCronJob(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cron[id]; !ok {
		return fmt.Errorf("cron job %q not found", id)
	}
	delete(s.cron, id)
	return nil
}
