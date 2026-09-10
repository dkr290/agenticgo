# AGENTS.md

Context for AI coding agents working on **agenticgo**.

## What this project is

`agenticgo` is a **simplified, self-hosted AI agent gateway in Go**, inspired by
GoClaw (nextlevelbuilder/goclaw) / OpenClaw but intentionally minimal. It is a
**clean-room reimplementation of ideas** — do NOT copy GoClaw code (CC BY-NC).
The owner is building this to learn Go.

**Module path:** `github.com/dkr290/agenticgo`

## Scope (what it does)

- **Multiple agents**: each agent is a directory under `AgentsDir` with context
  files `AGENTS.md` (operating instructions), `SOUL.md` (persona), `IDENTITY.md`
  (name/role), `USER.md`, `USER_PREDEFINED.md`, `CAPABILITIES.md`, `HEARTBEAT.md`,
  a `config.json` (per-agent LLM settings: provider/model/temperature/max_tokens,
  nil = inherit), a `skills/` dir, and a per-agent `workspace/` (tool jail). Files
  that don't exist yet are still listed (empty) in the UI so they can be created.
- **Skills**: a skill is a folder with `SKILL.md` (optional YAML-ish front-matter
  with `name`/`description` + markdown instructions). Loaded per agent and injected
  into the system prompt. Skills can be installed from the UI as a ZIP upload
  (`skills.InstallZip` validates the `SKILL.md`/`name`, derives a slug, and extracts
  path-traversal-safe).
- **Knowledge base**: per-agent reference documents (`knowledge_docs` table) uploaded
  from the UI. **Full-text indexed over content** (FTS5, `knowledge_docs_fts`) and
  recalled via the `search_docs` tool — not bulk-injected. Titles are listed in the
  system prompt so the agent knows to search.
- **Sidebar Web UI** (embedded HTML/JS SPA, no build step) + **WebSocket** API.
  Pages: Overview, Chat, Agents (per-agent Files/Skills/Knowledge/Config tabs), Skills,
  Built-in Tools, MCP Servers (scaffold), Cron (scaffold), Providers.
- **Agent loop**: LLM may call tools, observe results, iterate (capped).
- **LLM providers**: OpenAI-compatible endpoints only (Ollama / LM Studio / vLLM /
  OpenAI), behind a `llm.Provider` interface. Named provider configs are managed
  from the UI, persisted to `data/providers.json`, and seeded from env on first
  run. A chat request may override the provider (`wsMessage.Provider`); each
  agent's `config.json` may pin a provider/model/temperature/max_tokens too
  (per-request override > agent config > default);
  `agent.ProviderLookup` (`providers.Store.GetLLM`) resolves names.
- **Built-in tools**: `read_file`, `write_file`, `list_files`, `exec`.
- **Security**: filesystem tools jailed to a workspace; `exec` runs only allow-listed
  commands; tools gated by a global allow-list. Context-file names are validated
  against an allow-list (`validContextFile`) to prevent path traversal; agent keys are
  validated (`ValidKey`) since they're used as directory names.
- **Memory**: SQLite (`modernc.org/sqlite`, no cgo). Two kinds, scoped **per agent**:
  - `knowledge` — curated durable learnings. Full-text searchable via an FTS5
    virtual table (`knowledge_fts`, kept in sync by triggers) and the
    `memory_search` tool; selectively recalled, not bulk-injected.
  - `observations` — high-churn, timestamped findings from recurring agents (e.g. a
    k8s cron watcher). Retention-pruned (`ObservationTTLDays` / `ObservationKeepLatest`)
    and only the *latest* is injected into prompts, so stale state doesn't mislead the
    model. Recorded via the `record_observation` tool.
  Both memory tools are built per-run in `Engine.callTool` (scoped to the agent), not
  registered in the shared `tools.Registry`. The same per-run pattern is used for
  `search_docs` (knowledge-base documents).
- **Self-evolution (simplified)**: `Engine.Evolve` extracts learnings from a session
  into the agent's knowledge store; re-injected into its system prompt.
- **Scaffolding**: `internal/scaffold` holds in-memory MCP-server and cron-job
  registries backing the UI/API shape only — nothing connects or executes yet.

## Architecture map (how it fits together)

- `cmd/agenticgo/main.go` — wiring: config → agents.Registry (seeds a `default`
  agent) → store → llm.Provider → providers.Store (seeded from env) →
  scaffold.Store → tools.Registry → agent.Engine → server.
- `internal/agents` — file-based agent CRUD + context files. `Registry` owns the
  `AgentsDir`. `Agent.SystemPrompt()` composes context files.
- `internal/skills` — `Load(skillsDir)` → `[]Skill`; `Prompt(skills, enabled)` →
  system-prompt section.
- `internal/providers` — named OpenAI-compatible provider configs in
  `data/providers.json`; `GetLLM(name)` resolves names (empty = default).
- `internal/scaffold` — in-memory MCP-server and cron-job registries (not
  persisted, not executed — API/UI shape only).
- `internal/agent` — `Engine` resolves an agent, builds the system prompt
  (base + context files + skills + per-agent knowledge) and runs the tool loop,
  persisting turns under `(agent, session)`. `SetProviderLookup` enables
  per-request provider overrides.
- `internal/server` — chi routes for agent CRUD / context files / skills /
  providers / tools / sessions / MCP / cron / evolve, plus `/ws` streaming chat.
  The agent key (and optional provider name) flows through WS messages and API
  paths.
- `internal/store` — SQLite. `messages`, `knowledge`, and `observations` tables all
  have an `agent` column; `knowledge_fts` is an FTS5 virtual table kept in sync by
  triggers; `migrate` is idempotent for older DBs.

## Data layout on disk

```
data/
  agenticgo.db                 # SQLite (messages + knowledge, per-agent)
  providers.json               # named OpenAI-compatible provider configs
  workspace/                   # fallback tool jail
  agents/
    <key>/
      SOUL.md AGENTS.md IDENTITY.md
      USER.md USER_PREDEFINED.md CAPABILITIES.md HEARTBEAT.md
      config.json                 # per-agent LLM settings (optional, nil = inherit)
      skills/<skill>/SKILL.md
      workspace/               # per-agent tool jail
```

## Explicit non-goals (do not add without being asked)

- Multi-tenancy, RBAC, per-user anything
- Messaging channels (Telegram/Discord/Slack/WhatsApp/etc.)
- Agent teams / delegation / task boards
- PostgreSQL, pgvector, knowledge graph
- Multiple provider adapters up front (keep the interface, add only if asked)

## Planned next phases (from the README roadmap)

1. **Phase 3 — MCP client**: integrate `github.com/modelcontextprotocol/go-sdk/mcp`
   to attach external tools at runtime. MCP tools must be merged into the existing
   `tools.Registry`, namespaced (e.g. `mcp_<server>_<tool>`), and gated by the same
   allow-list. Config lists MCP servers (stdio and/or HTTP/SSE).
2. **Phase 5 — Docker + k8s**: multi-stage Dockerfile producing a static binary;
   manifests for Deployment + PVC (for the data dir) + Service (+ optional Ingress).

Possible follow-ons the owner may ask for (not yet built): per-agent workspaces wired
into the tool loop (tools currently use the global `WorkspaceDir`), skill enable/disable
per agent.

## Tech choices (keep these)

- **Go 1.26+**. Stdlib-first.
- Router: `github.com/go-chi/chi/v5`.
- WebSocket: `github.com/coder/websocket`.
- SQLite: `modernc.org/sqlite` (no cgo — static binary / k8s friendly).
- Config: **env vars only** (12-factor). See `internal/config`.
- DI: **manual wiring in `cmd/agenticgo/main.go`** — no FX/DI framework yet.
- UI: plain HTML/JS embedded with `go:embed` in `internal/server/web/` (must live
  inside the `server` package for the embed pattern to resolve).

## Conventions

- Standard Go layout: `cmd/` entry points, `internal/` app code.
- One package = one responsibility. Small interfaces. Accept interfaces, return structs.
- Always check and wrap errors with `fmt.Errorf("...: %w", err)`.
- Use `context.Context` for cancellation/timeouts.
- Keep functions small. Table-driven tests when adding tests.
- Run `gofmt -w .`, `go vet ./...`, and `go build ./...` before considering a change done.

## Build / run / test

```bash
go build ./...                              # compile
go vet ./...                                # vet
go build -o agenticgo ./cmd/agenticgo       # binary
AGENTICGO_ADDR=:18099 ./agenticgo           # run
curl localhost:18099/healthz                # health
curl localhost:18099/api/agents             # list agents
```

## Key files

- `cmd/agenticgo/main.go` — wiring
- `internal/agent/agent.go` — `Engine` tool loop + `Evolve` (self-evolution)
- `internal/agents/agents.go` — agent registry + context files
- `internal/skills/skills.go` — SKILL.md loader + prompt composition
- `internal/providers/providers.go` — named provider configs (JSON store)
- `internal/scaffold/scaffold.go` — in-memory MCP-server + cron-job scaffolding
- `internal/llm/openai.go` — OpenAI-compatible streaming client (SSE + tool calls)
- `internal/tools/{tools,fs,exec,memory}.go` — registry, filesystem jail,
  allow-listed exec, memory tools (`memory_search`, `record_observation`)
- `internal/store/store.go` — SQLite schema + queries (per-agent; FTS5 knowledge +
  observations)
- `internal/server/server.go` — chi routes, `/ws` streaming, embedded SPA
- `internal/server/web/index.html` — the whole UI (single file, no build step)
