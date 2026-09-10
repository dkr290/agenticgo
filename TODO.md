# TODO

Project-wide work items for agenticgo (the per-agent LLM config feature is done;
this lists what's still needed to make the project fully functional).

## 1. Functional gaps — things that claim to work but don't fully

- **Agents cannot read knowledge-base document content.**
  `search_docs` returns titles only (`internal/store/store.go:403`, `internal/tools/memory.go:168`),
  and the system prompt advertises content that no tool delivers (`internal/agent/agent.go:92`).
  Add a `get_doc`/`read_doc` tool (or return content snippets from `search_docs`) so uploaded
  documents are actually usable by agents.

- **Per-agent workspaces exist but are not used by tools.**
  `agents/<key>/workspace/` is created (`internal/agents/agents.go:105`) but fs/exec tools are
  registered against the global `cfg.WorkspaceDir` (`cmd/agenticgo/main.go:74`). Wire per-agent
  workspaces into the tool loop (construct fs/exec tools scoped to the agent's `WorkspaceDir`).
  This is the "per-agent tool jail" the README promises.

- **Conversation history loses the tool trace.**
  `messages` only persists role+content (`internal/store/store.go:44`); intermediate
  assistant tool-call turns and `tool` results are kept in memory only (`internal/agent/agent.go:231`).
  On the next turn the model can't see what tools were called. Persist tool calls/results
  (add `tool_calls`, `tool_call_id`, `name` columns) so multi-turn sessions stay correct and
  the UI "Load history" shows tool activity.

- **Evolve ignores provider resolution and agent config.**
  `Evolve` hardcodes `e.cfg.LLMModel` + `e.llm` (`internal/agent/agent.go:335`). Use the same
  provider resolution as `Run` (per-request + agent config) and pass temperature/max_tokens.

- **Chat bypasses the providers store's "default" marker.**
  Empty provider name falls back to the env-built `e.llm` (`internal/agent/agent.go:277`), so
  marking a provider as default in the UI has no effect until you pick one. Route empty-name
  requests through `providers.Store.GetLLM("")`.

- **Streaming tool-call accumulation drops sparse tool indexes.**
  `internal/llm/openai.go:243` iterates `len(toolArgs)` over a map; tool calls that stream
  an ID/name but zero argument bytes are lost, and a gap between indexes mis-reports them.
  Track a slice (or maxIndex) instead.

- **`AGENTICGO_KNOWLEDGE_FILE` / `cfg.KnowledgeFile` is dead config.**
  No longer referenced (`internal/config/config.go:33`); self-evolution writes to SQLite.
  Remove it or document it.

## 2. HUMA REST API (new — `docs` endpoint)

- **Add HUMA (github.com/danielgtaylor/huma/v2) and build a `/docs` API layer** covering the
  existing endpoints so there's a machine-readable API alongside the GUI — do NOT reimplement
  the GUI or remove the WebSocket chat.
  - Scaffold huma routers (API + docs) and register current routes: agents CRUD + config,
    context files, skills (+ upload), providers (+ test), sessions/messages, memory
    (knowledge/observations), knowledge-base docs, tools list, MCP + cron scaffolds, evolve.
  - Decide the mount/versioning scheme (e.g. `/openapi.json`, `/api/v1/...`) — keep it
    harmonized with the existing chi router and the WS endpoint.
  - Add examples/manual for the chat req/resp shapes if feasible.
  - This is the single "easy API" the project needs for non-UI clients. Priority after §1.

## 3. Scaffolded features (UI + API shape only — nothing executes)

- **MCP client (Phase 3).** In-memory only now (`internal/scaffold/scaffold.go:60`); not
  persisted, no SDK. Integrate `github.com/modelcontextprotocol/go-sdk/mcp`, allow stdio and
  HTTP/SSE servers, merge tools into `tools.Registry` as `mcp_<server>_<tool>`, gate by the
  allow-list. Persist config (JSON or new SQLite tables) so servers survive restarts.

- **Cron scheduler.** Jobs stored in-memory only (`internal/scaffold/scaffold.go:101`);
  `Enabled` is serialized but unused. Add a scheduler (e.g. robfig/cron or a simple ticker),
  persist jobs, and wire execution into `Engine.Run` per agent/session/prompt. Recurring agents
  should feed `record_observation` — the memory design anticipates this.

## 4. API / UI hardening gaps

- **Skill delete & knowledge delete endpoints** (and UI buttons): Skills tab and Memory page are
  read-only today (no DELETE for skills/knowledge).
- **Agent-scoped session listing**: `GET /api/sessions` returns everything; filter by agent.
- **Observation creation in the UI**: `POST /api/agents/{k}/observations` exists; the Memory
  page has no control that calls it.

## 5. Real-deployment items

- **Dockerfile** (multi-stage, static binary — the stack is already cgo-free via
  `modernc.org/sqlite`) and **Kubernetes manifests** (Deployment + PVC for the data dir +
  Service + optional Ingress). This is Phase 5 of the README roadmap.
- **Test coverage**: only `internal/store/memory_smoke_test.go` exists. Add tests for the agent
  loop, server handlers, providers, skills, tools, and llm request marshaling.

## 6. Housekeeping

- *Where things live (answers for on-boarding):*
  - Agent definitions + context files: `data/agents/<key>/*.md` (`AgentsDir`)
  - Per-agent LLM config: `data/agents/<key>/config.json`
  - Per-agent skills: `data/agents/<key>/skills/<skill>/SKILL.md`
  - Per-agent workspace jail: `data/agents/<key>/workspace/` (currently unused — see §1)
  - Provider configs: `data/providers.json`
  - Messages / knowledge / observations / knowledge-base docs: `data/agenticgo.db` (SQLite)
  - Global fallback workspace: `data/workspace/`
- **Observation pruning only runs when TTL/keep-latest env vars are set**
  (`internal/agent/agent.go:155`); document or always prune.
- **FTS knowledge_docs backfill** in `migrate` is missing for old DBs
  (`internal/store/store.go:95`); harmless today but asymmetric with `knowledge_fts`.