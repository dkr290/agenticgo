package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/functool"
)

// MemoryStore is the subset of the store the memory tools need.
// Implemented by *store.Store.
type MemoryStore interface {
	SearchKnowledge(ctx context.Context, agent, query string, limit int) ([]string, error)
	AddKnowledge(ctx context.Context, agent, content string) error
	AddObservation(ctx context.Context, agent, content string) error
}

// DocSearcher finds documents by keyword and returns "id — title" lines.
// The engine adapts store.SearchDocs to this, keeping tools decoupled.
type DocSearcher func(ctx context.Context, query string, limit int) ([]string, error)

// DocReader fetches one document's content by ID, scoped to the agent.
// Implemented by the engine over store.GetKnowledgeDocForAgent.
type DocReader func(ctx context.Context, id int64) (title, content string, err error)

// ImageLister lists the images available for an agent.
type ImageLister func(ctx context.Context) ([]string, error)

// ImageFetcher reads one agent image and returns its base64 data-URL.
// The engine uses this to inject the image into the LLM turn.
type ImageFetcher func(ctx context.Context, name string) (dataURL string, err error)

// ImageSink collects image data-URLs for injection into the next LLM turn.
type ImageSink interface {
	// AddImage records an image data-URL to be injected before the next LLM call.
	AddImage(dataURL string)
	// DrainImages returns all collected image data-URLs and clears the buffer.
	DrainImages() []string
}

// --- memory_search ---

type memorySearchArgs struct {
	Query string `json:"query"          jsonschema:"Keywords to search for."`
	Limit int    `json:"limit,omitempty" jsonschema:"Max results (default 8)."`
}

// NewMemorySearch creates the memory_search tool scoped to an agent. It lets
// the agent query its own curated knowledge by keyword, instead of relying on
// all of it being injected into the prompt. Selective retrieval reduces noise
// (and hallucination from irrelevant context).
func NewMemorySearch(store MemoryStore, agentKey string) tool.FuncTool {
	return functool.MustNew(functool.Config{
		Name: "memory_search",
		Description: "Search your long-term curated knowledge for relevant facts by keyword. " +
			"Use this to recall prior learnings instead of guessing.",
	}, func(ctx context.Context, args memorySearchArgs) (string, error) {
		if args.Limit <= 0 {
			args.Limit = 8
		}
		hits, err := store.SearchKnowledge(ctx, agentKey, args.Query, args.Limit)
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
	})
}

// --- memory_save ---

type memorySaveArgs struct {
	Content string `json:"content" jsonschema:"One concise durable fact, preference, or lesson to remember."`
}

// NewMemorySave creates the memory_save tool scoped to an agent. It lets the
// agent persist a durable fact, preference, or lesson to its curated
// long-term knowledge. This is the agent-initiated write path (complementing
// the background self-evolution pass): when the user says "remember this",
// the agent saves it in the same turn. Stored knowledge is full-text
// searchable via memory_search and selectively recalled, not bulk-injected.
func NewMemorySave(store MemoryStore, agentKey string) tool.FuncTool {
	return functool.MustNew(functool.Config{
		Name: "memory_save",
		Description: "Save a durable fact, user preference, or lesson to your long-term curated " +
			"knowledge. Use when the user asks you to remember something, or when you learn " +
			"something worth keeping across sessions. Not for time-bound state — use " +
			"record_observation for that. Saved knowledge is searchable via memory_search.",
	}, func(ctx context.Context, args memorySaveArgs) (string, error) {
		if strings.TrimSpace(args.Content) == "" {
			return "", fmt.Errorf("content must not be empty")
		}
		if err := store.AddKnowledge(ctx, agentKey, args.Content); err != nil {
			return "", err
		}
		return "saved to long-term memory", nil
	})
}

// --- record_observation ---

type recordObservationArgs struct {
	Content string `json:"content" jsonschema:"The observation, e.g. 'pod payments-api has 3 restarts'."`
}

// NewRecordObservation creates the record_observation tool scoped to an
// agent. It lets a recurring agent (e.g. a cron job) record a timestamped
// observation. Observations are pruned by retention and only the latest is
// injected into prompts, so stale state does not mislead the model.
func NewRecordObservation(store MemoryStore, agentKey string) tool.FuncTool {
	return functool.MustNew(functool.Config{
		Name: "record_observation",
		Description: "Record a timestamped observation about the current state of something " +
			"(e.g. a k8s cluster health check). Observations are time-bound and pruned " +
			"automatically; record durable patterns as knowledge instead.",
	}, func(ctx context.Context, args recordObservationArgs) (string, error) {
		if strings.TrimSpace(args.Content) == "" {
			return "", fmt.Errorf("content must not be empty")
		}
		if err := store.AddObservation(ctx, agentKey, args.Content); err != nil {
			return "", err
		}
		return "observation recorded", nil
	})
}

// --- search_docs ---

type searchDocsArgs struct {
	Query string `json:"query"          jsonschema:"Keywords to search for."`
	Limit int    `json:"limit,omitempty" jsonschema:"Max results (default 5)."`
}

// NewSearchDocs creates the search_docs tool from a search function. It
// searches the agent's knowledge-base documents (uploaded reference
// material), returning "id — title" lines so the agent can follow up with
// read_doc to fetch the actual content.
func NewSearchDocs(search DocSearcher) tool.FuncTool {
	return functool.MustNew(functool.Config{
		Name: "search_docs",
		Description: "Search your knowledge base (documents uploaded for you) by keyword. " +
			"Returns matching documents as 'id — title'. Call read_doc with a document's " +
			"id to read its full content before relying on it.",
	}, func(ctx context.Context, args searchDocsArgs) (string, error) {
		if args.Limit <= 0 {
			args.Limit = 5
		}
		lines, err := search(ctx, args.Query, args.Limit)
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
	})
}

// --- read_doc ---

type readDocArgs struct {
	ID int64 `json:"id" jsonschema:"The document id from search_docs."`
}

// NewReadDoc creates the read_doc tool from a reader function. It lets the
// agent read the full content of a knowledge-base document it found via
// search_docs. Scoped to the agent: it cannot read other agents' documents.
func NewReadDoc(read DocReader) tool.FuncTool {
	return functool.MustNew(functool.Config{
		Name: "read_doc",
		Description: "Read the full content of a knowledge-base document by its numeric id " +
			"(from search_docs results). Always read a document before quoting or acting on it.",
	}, func(ctx context.Context, args readDocArgs) (string, error) {
		if args.ID <= 0 {
			return "", fmt.Errorf("id must be a positive number (from search_docs)")
		}
		title, content, err := read(ctx, args.ID)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("# %s\n\n%s", title, strings.TrimSpace(content)), nil
	})
}

// --- list_agent_images ---

// listAgentImagesArgs takes no arguments.
type listAgentImagesArgs struct{}

// NewListAgentImages creates the list_agent_images tool from a lister
// function. It lets the agent discover what reference images exist so it can
// fetch one with fetch_agent_image when it needs visual context.
func NewListAgentImages(list ImageLister) tool.FuncTool {
	return functool.MustNew(functool.Config{
		Name: "list_agent_images",
		Description: "List the reference images available for this agent. " +
			"Use fetch_agent_image with the image name to retrieve an image.",
	}, func(ctx context.Context, _ listAgentImagesArgs) (string, error) {
		names, err := list(ctx)
		if err != nil {
			return "", err
		}
		if len(names) == 0 {
			return "No reference images available.", nil
		}
		var b strings.Builder
		b.WriteString("Available reference images (use fetch_agent_image to retrieve one):\n")
		for _, n := range names {
			b.WriteString("- ")
			b.WriteString(n)
			b.WriteString("\n")
		}
		return strings.TrimSpace(b.String()), nil
	})
}

// --- fetch_agent_image ---

type fetchAgentImageArgs struct {
	Name string `json:"name" jsonschema:"The image filename from list_agent_images."`
}

// NewFetchAgentImage creates the fetch_agent_image tool from a fetcher and an
// ImageSink. It retrieves one agent image by name and injects it into the
// next LLM turn via the sink. The model can actually see the image (not just
// the data-URL string) because the engine attaches it as an image_url content
// part. Returns a confirmation string.
func NewFetchAgentImage(fetch ImageFetcher, sink ImageSink) tool.FuncTool {
	return functool.MustNew(functool.Config{
		Name: "fetch_agent_image",
		Description: "Fetch a reference image by its name (from list_agent_images results) " +
			"and make it available to you in the next turn. Use this when you need " +
			"visual context from a previously stored image.",
	}, func(ctx context.Context, args fetchAgentImageArgs) (string, error) {
		if strings.TrimSpace(args.Name) == "" {
			return "", fmt.Errorf("name must not be empty")
		}
		dataURL, err := fetch(ctx, args.Name)
		if err != nil {
			return "", err
		}
		sink.AddImage(dataURL)
		return fmt.Sprintf("Image %q fetched and will be available in the next turn.", args.Name), nil
	})
}

// CoreTool is the metadata (name + description) of an always-on built-in
// agent capability. These are the per-run memory/knowledge tools the engine
// wires for every agent — not the allow-listed filesystem/exec tools in the
// shared registry. Core tools cannot be enabled, disabled, or removed.
type CoreTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// CoreTools returns the always-on built-in agent tools. It is the single
// source of truth for their names and descriptions: the engine wires these
// per run (scoped to the calling agent) and the server lists them read-only.
// Instances are store/agent-scoped, so metadata is produced by lightweight
// constructors with nil dependencies (Call is never invoked here).
func CoreTools() []CoreTool {
	probes := []tool.Tool{
		NewMemorySearch(nil, ""),
		NewMemorySave(nil, ""),
		NewRecordObservation(nil, ""),
		NewSearchDocs(nil),
		NewReadDoc(nil),
		NewListAgentImages(nil),
		NewFetchAgentImage(nil, nil),
	}
	out := make([]CoreTool, 0, len(probes))
	for _, t := range probes {
		out = append(out, CoreTool{Name: t.Name(), Description: t.Description()})
	}
	return out
}
