// Package cron runs agents on a schedule. Jobs are persisted to a JSON file
// (data/cron.json, consistent with providers.json / mcp_servers.json) and
// executed via robfig/cron: on each tick of an enabled job the scheduler runs
// the agent's Engine.Run with the job's prompt, on its own per-job session so
// each run is a distinct resumable conversation.
//
// A typical use is a recurring monitor (e.g. a k8s watcher): the job's prompt
// asks the agent to inspect state, the agent records findings via the
// record_observation tool, and the next run sees those observations in its
// system prompt to diff against.
package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/dkr290/agenticgo/internal/logger"
)

// Runner executes one agent turn. Implemented by *agent.Engine.Run.
type Runner func(ctx context.Context, agentKey, session, message string) error

// Job is a scheduled agent run. ID and CreatedAt are server-assigned; the
// omitempty lets a POST body omit them (they're set by Add) while still
// serializing on responses.
type Job struct {
	ID        string    `json:"id,omitempty"`
	Name      string    `json:"name"`
	Schedule  string    `json:"schedule"` // cron expression, e.g. "*/5 * * * *"
	Agent     string    `json:"agent,omitempty"`
	Prompt    string    `json:"prompt"`
	Enabled   bool      `json:"enabled,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// JobWithStatus is a Job annotated with its live schedule state, so the UI can
// show whether a job is actually armed and when it fires next.
type JobWithStatus struct {
	Job
	Scheduled bool       `json:"scheduled"` // armed in the live scheduler (enabled + valid)
	NextRun   *time.Time `json:"next_run,omitempty"`
}

// Scheduler is a JSON-persisted cron-job registry plus a live runner.
type Scheduler struct {
	lifecycleMu sync.Mutex // prevents Start from racing a Stop that is waiting for jobs
	path        string
	run         Runner
	parser      cron.Parser

	mu      sync.Mutex
	jobs    map[string]*Job
	entries map[string]cron.EntryID // job ID -> live scheduler entry
	cron    *cron.Cron
	log     logger.Logger
	running bool
	ctx     context.Context
	cancel  context.CancelFunc
	active  map[string]bool
	wg      sync.WaitGroup
}

var idRE = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// Open loads (or initializes) the cron job registry at path. run executes one
// agent turn per tick. The scheduler is not started until Start is called.
func Open(path string, run Runner) (*Scheduler, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create cron dir: %w", err)
	}
	s := &Scheduler{
		path:    path,
		run:     run,
		parser:  cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor),
		jobs:    map[string]*Job{},
		entries: map[string]cron.EntryID{},
		log:     logger.Nop(),
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("read cron jobs: %w", err)
	}
	var list []*Job
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parse cron jobs: %w", err)
	}
	for _, j := range list {
		if j != nil && j.ID != "" {
			s.jobs[j.ID] = j
		}
	}
	return s, nil
}

// SetLogger wires verbose logging into the scheduler (nil keeps a no-op).
func (s *Scheduler) SetLogger(l logger.Logger) {
	if l == nil {
		l = logger.Nop()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = l
}

// Start arms the live scheduler: every enabled job with a valid schedule is
// registered. Called once the engine and store are wired (main).
func (s *Scheduler) Start() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return
	}
	s.cron = cron.New()
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.active = make(map[string]bool)
	s.entries = make(map[string]cron.EntryID)
	s.running = true
	// Snapshot the jobs first: scheduleLocked mutates s.entries (a map), and
	// ranging over s.jobs while mutating another map is fine, but arming before
	// s.running is set would be skipped by scheduleLocked's guard — so set
	// running first, then arm.
	for _, j := range s.jobs {
		s.scheduleLocked(j)
	}
	s.cron.Start()
}

// Stop halts the live scheduler. It does not persist (jobs are already saved
// on every mutation).
func (s *Scheduler) Stop() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	stopped := s.cron.Stop()
	s.running = false
	s.cancel()
	s.mu.Unlock()
	<-stopped.Done()
	s.wg.Wait()
}

// List returns all jobs, annotated with live schedule state, sorted by name.
func (s *Scheduler) List() []JobWithStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]JobWithStatus, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, s.statusLocked(j))
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Name < out[k].Name })
	return out
}

// Add validates and registers a new job, persists it, and (re)arms it.
func (s *Scheduler) Add(j Job) (*Job, error) {
	name := strings.TrimSpace(j.Name)
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if _, err := s.parser.Parse(strings.TrimSpace(j.Schedule)); err != nil {
		return nil, fmt.Errorf("invalid schedule %q: %w", j.Schedule, err)
	}
	if strings.TrimSpace(j.Prompt) == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	agent := strings.TrimSpace(j.Agent)
	if agent == "" {
		agent = "default"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	cp := Job{
		ID:        newID(name),
		Name:      name,
		Schedule:  strings.TrimSpace(j.Schedule),
		Agent:     agent,
		Prompt:    j.Prompt,
		Enabled:   j.Enabled,
		CreatedAt: time.Now(),
	}
	s.jobs[cp.ID] = &cp
	if err := s.saveLocked(); err != nil {
		delete(s.jobs, cp.ID)
		return nil, err
	}
	s.scheduleLocked(&cp)
	out := cp
	return &out, nil
}

// Delete removes a job (disarming it first). Returns an error when not found.
func (s *Scheduler) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return fmt.Errorf("cron job %q not found", id)
	}
	s.unscheduleLocked(j)
	delete(s.jobs, id)
	return s.saveLocked()
}

// --- internal ---

// statusLocked builds the live view of a job (armed + next run). Caller holds mu.
func (s *Scheduler) statusLocked(j *Job) JobWithStatus {
	out := JobWithStatus{Job: *j}
	if s.running && j.Enabled {
		if eid, ok := s.entries[j.ID]; ok {
			entry := s.cron.Entry(eid)
			if entry.Valid() {
				out.Scheduled = true
				if !entry.Next.IsZero() {
					next := entry.Next
					out.NextRun = &next
				}
			}
		}
	}
	return out
}

// scheduleLocked (re)arms a job in the live scheduler. No-op when the job is
// disabled or the scheduler isn't running. Caller holds mu.
func (s *Scheduler) scheduleLocked(j *Job) {
	s.unscheduleLocked(j)
	if !s.running || !j.Enabled {
		return
	}
	jobID, agentKey, prompt := j.ID, j.Agent, j.Prompt
	eid, err := s.cron.AddFunc(j.Schedule, func() { s.fire(jobID, agentKey, prompt) })
	if err != nil {
		s.log.Error("cron schedule failed", "job", jobID, "schedule", j.Schedule, "error", err)
		return
	}
	s.entries[j.ID] = eid
}

// unscheduleLocked disarms a job. Caller holds mu.
func (s *Scheduler) unscheduleLocked(j *Job) {
	if s.running {
		if eid, ok := s.entries[j.ID]; ok {
			s.cron.Remove(eid)
		}
	}
	delete(s.entries, j.ID)
}

// fire executes one scheduled run. The agent gets its own session per job so
// runs accumulate as a resumable conversation.
func (s *Scheduler) fire(jobID, agentKey, prompt string) {
	s.mu.Lock()
	if !s.running || s.active[jobID] {
		s.mu.Unlock()
		return
	}
	s.active[jobID] = true
	s.wg.Add(1)
	ctx, cancel := context.WithTimeout(s.ctx, 15*time.Minute)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.active, jobID)
		s.mu.Unlock()
		s.wg.Done()
	}()
	defer cancel()
	s.log.Info("cron run start", "job", jobID, "agent", agentKey)
	if err := s.run(ctx, agentKey, "cron-"+jobID, prompt); err != nil {
		s.log.Error("cron run failed", "job", jobID, "agent", agentKey, "error", err)
		return
	}
	s.log.Info("cron run done", "job", jobID, "agent", agentKey)
}

// saveLocked persists jobs to JSON atomically (tmp + rename). Caller holds mu.
func (s *Scheduler) saveLocked() error {
	list := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		list = append(list, j)
	}
	sort.Slice(list, func(i, k int) bool { return list[i].Name < list[k].Name })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cron jobs: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write cron jobs: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("persist cron jobs: %w", err)
	}
	return nil
}

// newID derives a stable, filename/URL-safe job ID from the job name plus a
// timestamp suffix for uniqueness.
func newID(name string) string {
	base := idRE.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "job"
	}
	if len(base) > 40 {
		base = base[:40]
	}
	return fmt.Sprintf("%s-%d", base, time.Now().UnixNano())
}
