package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// MemoryStore is the subset of the store the memory tools need.
// Implemented by *store.Store.
type MemoryStore interface {
	SearchKnowledge(ctx context.Context, agent, query string, limit int) ([]string, error)
	AddObservation(ctx context.Context, agent, content string) error
}

// DocSearcher finds documents by keyword and returns "id — title" lines.
// The engine adapts store.SearchDocs to this, keeping tools decoupled.
type DocSearcher func(ctx context.Context, query string, limit int) ([]string, error)

// memorySearchTool lets the agent query its own curated knowledge by keyword,
// instead of relying on all of it being injected into the prompt. Selective
// retrieval reduces noise (and hallucination from irrelevant context).
type memorySearchTool struct {
	store MemoryStore
	agent string
}

// NewMemorySearch creates the memory_search tool scoped to an agent.
func NewMemorySearch(store MemoryStore, agentKey string) Tool {
	return &memorySearchTool{store: store, agent: agentKey}
}

func (t *memorySearchTool) Name() string { return "memory_search" }
func (t *memorySearchTool) Description() string {
	return "Search your long-term curated knowledge for relevant facts by keyword. " +
		"Use this to recall prior learnings instead of guessing."
}
func (t *memorySearchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "Keywords to search for."},
			"limit": map[string]any{"type": "integer", "description": "Max results (default 8)."},
		},
		"required": []string{"query"},
	}
}

func (t *memorySearchTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if in.Limit <= 0 {
		in.Limit = 8
	}
	hits, err := t.store.SearchKnowledge(ctx, t.agent, in.Query, in.Limit)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "No relevant knowledge found.", nil
	}
	var b strings.Builder
	b.WriteString("Relevant knowledge (may be from past sessions — verify it is still current):\n")
	for _, h := range hits {
		b.WriteString("- ")
		b.WriteString(strings.TrimSpace(h))
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String()), nil
}

// recordObservationTool lets a recurring agent (e.g. a cron job) record a
// timestamped observation. Observations are pruned by retention and only the
// latest is injected into prompts, so stale state does not mislead the model.
type recordObservationTool struct {
	store MemoryStore
	agent string
}

// NewRecordObservation creates the record_observation tool scoped to an agent.
func NewRecordObservation(store MemoryStore, agentKey string) Tool {
	return &recordObservationTool{store: store, agent: agentKey}
}

func (t *recordObservationTool) Name() string { return "record_observation" }
func (t *recordObservationTool) Description() string {
	return "Record a timestamped observation about the current state of something " +
		"(e.g. a k8s cluster health check). Observations are time-bound and pruned " +
		"automatically; record durable patterns as knowledge instead."
}
func (t *recordObservationTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"content": map[string]any{"type": "string", "description": "The observation, e.g. 'pod payments-api has 3 restarts'."},
		},
		"required": []string{"content"},
	}
}

func (t *recordObservationTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if strings.TrimSpace(in.Content) == "" {
		return "", fmt.Errorf("content must not be empty")
	}
	if err := t.store.AddObservation(ctx, t.agent, in.Content); err != nil {
		return "", err
	}
	return "observation recorded", nil
}

// searchDocsTool searches the agent's knowledge-base documents (uploaded
// reference material). Returns "id — title" lines so the agent can follow up
// with read_doc to fetch the actual content.
type searchDocsTool struct {
	search DocSearcher
}

// NewSearchDocs creates the search_docs tool from a search function.
func NewSearchDocs(search DocSearcher) Tool {
	return &searchDocsTool{search: search}
}

func (t *searchDocsTool) Name() string { return "search_docs" }
func (t *searchDocsTool) Description() string {
	return "Search your knowledge base (documents uploaded for you) by keyword. " +
		"Returns matching documents as 'id — title'. Call read_doc with a document's " +
		"id to read its full content before relying on it."
}
func (t *searchDocsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "Keywords to search for."},
			"limit": map[string]any{"type": "integer", "description": "Max results (default 5)."},
		},
		"required": []string{"query"},
	}
}

func (t *searchDocsTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if in.Limit <= 0 {
		in.Limit = 5
	}
	lines, err := t.search(ctx, in.Query, in.Limit)
	if err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "No documents matched.", nil
	}
	var b strings.Builder
	b.WriteString("Matching documents (use read_doc with the id to read content):\n")
	for _, l := range lines {
		b.WriteString("- ")
		b.WriteString(l)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String()), nil
}

// DocReader fetches one document's content by ID, scoped to the agent.
// Implemented by the engine over store.GetKnowledgeDocForAgent.
type DocReader func(ctx context.Context, id int64) (title, content string, err error)

// readDocTool lets the agent read the full content of a knowledge-base
// document it found via search_docs. Scoped to the agent: it cannot read
// other agents' documents.
type readDocTool struct {
	read DocReader
}

// NewReadDoc creates the read_doc tool from a reader function.
func NewReadDoc(read DocReader) Tool {
	return &readDocTool{read: read}
}

func (t *readDocTool) Name() string { return "read_doc" }
func (t *readDocTool) Description() string {
	return "Read the full content of a knowledge-base document by its numeric id " +
		"(from search_docs results). Always read a document before quoting or acting on it."
}
func (t *readDocTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{"type": "integer", "description": "The document id from search_docs."},
		},
		"required": []string{"id"},
	}
}

func (t *readDocTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if in.ID <= 0 {
		return "", fmt.Errorf("id must be a positive number (from search_docs)")
	}
	title, content, err := t.read(ctx, in.ID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("# %s\n\n%s", title, strings.TrimSpace(content)), nil
}
