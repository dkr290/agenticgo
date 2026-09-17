# AGENTS.md

Context for AI coding agents working on **agenticgo**.

## What this project is

`agenticgo` is a **simplified, self-hosted AI agent gateway in Go**, inspired by
GoClaw (nextlevelbuilder/goclaw) / OpenClaw but intentionally minimal. It is a
**clean-room reimplementation of ideas** — do NOT copy GoClaw code (CC BY-NC).

**Module path:** `github.com/dkr290/agenticgo`

## Scope (what it does)

- **Multiple agents**: each agent is a directory under `AgentsDir` with context
  files `AGENTS.md` (operating instructions), `SOUL.md` (persona), `IDENTITY.md`
  (name/role), `USER.md`, `USER_PREDEFINED.md`, `CAPABILITIES.md`, `HEARTBEAT.md`,
  a `config.json` (per-agent LLM settings: provider/model/temperature/max_tokens/vision,
  nil = inherit, plus `enabled_skills`, `enabled_tools` and `enabled_commands`), a per-agent `images/`
  dir (reference pictures/screenshots for vision models), and a per-agent
  `workspace/` (tool jail).
  Files that don't exist yet are still listed (empty) in the UI so they can be created.
- **Skills (shared library + per-agent enable)**: a skill is a folder with
  `SKILL.md` (optional YAML-ish front-matter with `name`/`description` + markdown
  instructions). Skills live in **one global library** (`data/skills/`,
  `Registry.SkillsLibraryDir`) — uploaded once from the UI as a ZIP
  (`skills.InstallZip` validates the `SKILL.md`/`name`, derives a slug, and extracts
  path-traversal-safe). They are **never inherited automatically**: each agent
  enables the ones it wants via `config.json` `enabled_skills` (Agents → Skills tab
  in the UI, or `PUT/DELETE /api/agents/{k}/skills/{skill}`); only enabled skills are
  injected into that agent's system prompt.
- **Knowledge base**: per-agent reference documents (`knowledge_docs` table) uploaded
  from the UI. **Full-text indexed over content** (FTS5, `knowledge_docs_fts`) and
  recalled via tools — not bulk-injected: `search_docs` returns `id — title` matches,
  `read_doc` fetches a document's content by id. Both are scoped to the calling agent
  (`GetKnowledgeDocForAgent` filters by agent, so one agent cannot read another's docs).
  Titles are listed in the system prompt so the agent knows to search.
- **Sidebar Web UI** (embedded HTML/JS SPA, no build step) + **WebSocket** API.
  Pages: Overview, Chat, Agents (per-agent Files/Skills/MCP Tools/Knowledge/
  Images/Config tabs), Skills, Built-in Tools, MCP Servers, Cron (scaffold),
  Providers.
- **Agent loop**: LLM may call tools, observe results, iterate (capped).
- **MCP tools**: external MCP servers (stdio or HTTP, e.g.
  kubernetes-mcp-server) are registered in `data/mcp_servers.json` and connected
  **manually** (Connect button / `POST /api/mcp-servers/{id}/connect`) — nothing
  spawns at startup. On connect, `internal/mcp` discovers the server's tools and
  caches them namespaced as `mcp_<server>_<tool>`. Agents then enable exactly the
  tools they may use via `config.json` `enabled_tools` (Agents → MCP Tools tab,
  or `PUT/DELETE /api/agents/{k}/custom-tools/{name}`); only enabled tools are
  exposed to the LLM, and `Engine.callTool` re-checks the allow-list server-side
  before delegating to `mcp.Manager.CallTool` (which lazily reconnects a
  configured-but-disconnected server). Tool names resolve back by longest-prefix
  matching against known server names, so underscores in tool names are safe.
  Built-in tools stay gated by the global `AGENTICGO_TOOL_ALLOWLIST`; MCP tools
  are purely additive per agent.
- **LLM providers**: OpenAI-compatible endpoints only (Ollama / LM Studio / vLLM /
  OpenAI), behind a `llm.Provider` interface. Named provider configs are managed
  from the UI, persisted to `data/providers.json`, and seeded from env on first
  run. A chat request may override the provider (`wsMessage.Provider`); each
  agent's `config.json` may pin a provider/model/temperature/max_tokens too
  (per-request override > agent config > default);
  `agent.ProviderLookup` (`providers.Store.GetLLM`) resolves names.
  **API keys are encrypted at rest** (`internal/crypto`, AES-256-GCM, `enc:v1:`
  envelope in the JSON) using a key from `AGENTICGO_SECRET_KEY` (base64, 32 bytes)
  or a generated `data/secret.key` (0600). Keys are decrypted transparently on
  load, so callers and the UI still work with plaintext; legacy plaintext keys in
  `providers.json` are migrated on first boot.
- **Logging**: `internal/logger` defines a small `Logger` interface (Info/Error/
  Debug/Warn with slog-style key/value pairs) backed by stdlib `log/slog`
  (human-readable text to stderr). Set `AGENTICGO_DEBUG=true` to enable debug
  level. Wired into the providers/LLM path for now (`providers.Store.SetLogger`,
  `llm.OpenAIProvider.SetLogger`); other packages keep `logger.Nop()` until
  adopted. Secrets are redacted (`****` + last 4) before logging.
- **Built-in tools**: `read_file`, `write_file`, `list_files`, `exec`. The `exec`
  tool's safe commands come from `AGENTICGO_EXEC_ALLOWLIST`; additional dangerous
  commands (`AGENTICGO_EXTRA_EXEC_COMMANDS`) are enabled per agent.
- **Vision / images**: a provider can be flagged `vision` (its model understands
  images — set manually, there's no reliable API to detect it). An agent's
  `config.json` `vision` tri-state overrides it (nil = inherit) for when the agent
  pins a different model. When the effective provider is vision-capable, the UI
  enables the per-agent **Images** tab (`data/agents/<key>/images/`) and the chat
  attach button; attached images go over the WS as names, are read into base64
  data-URLs, and sent as OpenAI `image_url` content parts. Non-vision models have
  attach blocked up front and any images dropped in `Engine.Run`, so they never
  error. Resolution: `Engine.EffectiveVision` (agent override > provider flag).
- **Security**: filesystem tools jailed to a workspace; `exec` runs only allow-listed
  commands (`AGENTICGO_EXEC_ALLOWLIST`); tools gated by a global allow-list.
  Extra, dangerous commands (e.g. `kubectl`, `git`) are declared via
  `AGENTICGO_EXTRA_EXEC_COMMANDS` but are **never** runnable until an agent enables
  them via `config.json` `enabled_commands` (Agents → Extra Dangerous Exec Commands
  tab); `Engine.callExec` re-checks per run and runs them jailed to the agent's own
  workspace. Context-file names are validated
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
  `search_docs` + `read_doc` (knowledge-base documents).
- **Self-evolution (simplified)**: `Engine.Evolve` extracts learnings from a session
  into the agent's knowledge store; re-injected into its system prompt.
- **Scaffolding**: `internal/scaffold` holds the in-memory cron-job registry backing
  the UI/API shape only — nothing executes yet. (MCP servers graduated from
  scaffolding to a real client in `internal/mcp`.)

## Architecture map (how it fits together)

- `cmd/agenticgo/main.go` — wiring: config → agents.Registry (seeds a `default`
  agent) → store → llm.Provider → providers.Store (seeded from env) →
  mcp.Manager + scaffold.Store → tools.Registry → agent.Engine → server.
- `internal/agents` — file-based agent CRUD + context files. `Registry` owns the
  `AgentsDir`. `Agent.SystemPrompt()` composes context files.
- `internal/skills` — `Load(skillsDir)` → `[]Skill`; `Prompt(skills, enabled)` →
  system-prompt section.
- `internal/mcp` — MCP client: JSON-persisted server registry
  (`data/mcp_servers.json`), manual connect/disconnect, tool discovery cache,
  namespaced `CallTool` with lazy reconnect (`github.com/modelcontextprotocol/
  go-sdk/mcp`, stdio `CommandTransport` + HTTP `StreamableClientTransport`).
- `internal/providers` — named OpenAI-compatible provider configs in
  `data/providers.json`; `GetLLM(name)` resolves names (empty = default).
- `internal/scaffold` — in-memory cron-job registry (not persisted, not
  executed — API/UI shape only).
- `internal/agent` — `Engine` resolves an agent, builds the system prompt
  (base + context files + skills + per-agent knowledge) and runs the tool loop,
  persisting turns under `(agent, session)`. `SetProviderLookup` enables
  per-request provider overrides; `SetMCPManager` enables per-agent MCP tools,
  gated by `AgentConfig.EnabledTools` in `callTool`.
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
  mcp_servers.json             # MCP server definitions (MCP tools)
  workspace/                   # fallback tool jail
  skills/                      # GLOBAL skills library (upload once; enabled per agent)
    <skill>/SKILL.md
  agents/
    <key>/
      SOUL.md AGENTS.md IDENTITY.md
      USER.md USER_PREDEFINED.md CAPABILITIES.md HEARTBEAT.md
      config.json                 # per-agent LLM settings + enabled_skills
                                  #   + enabled_tools (MCP tools)
                                  #   + enabled_commands (extra dangerous exec cmds)
      images/                  # reference images for vision models (Images tab)
      workspace/               # per-agent tool jail
```

## Explicit non-goals (do not add without being asked)

- Multi-tenancy, RBAC, per-user anything
- Messaging channels (Telegram/Discord/Slack/WhatsApp/etc.)
- Agent teams / delegation / task boards
- PostgreSQL, pgvector, knowledge graph
- Multiple provider adapters up front (keep the interface, add only if asked)

## Planned next phases (from the README roadmap)

1. ~~**Phase 3 — MCP client**~~ **(done)**: `internal/mcp` integrates
   `github.com/modelcontextprotocol/go-sdk/mcp` (stdio + HTTP). Tools are
   discovered on manual Connect, namespaced `mcp_<server>_<tool>`, and enabled
   per agent via `config.json` `enabled_tools` (Agents → MCP Tools tab) —
   separate from the built-in `AGENTICGO_TOOL_ALLOWLIST`, which still gates the
   system tools.
2. **Phase 5 — Docker + k8s**: multi-stage Dockerfile producing a static binary;
   manifests for Deployment + PVC (for the data dir) + Service (+ optional Ingress).

Possible follow-ons the owner may ask for (not yet built): per-agent workspaces wired
into the tool loop (tools currently use the global `WorkspaceDir`).

## Tech choices (keep these)

- **Go 1.26+**. Stdlib-first.
- LLM client: `github.com/openai/openai-go/v3` (official SDK, Chat Completions
  API with base-URL override — works with Ollama / LM Studio / LocalAI / vLLM /
  OpenAI). The old hand-rolled SSE client is kept, bug-fixed, as reference only
  in `internal/llm/openai.go.bak` (not compiled).
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
- `internal/crypto/crypto.go` — AES-256-GCM encryption of secrets at rest (`AGENTICGO_SECRET_KEY` or `data/secret.key`)
- `internal/logger/logger.go` — minimal `Logger` interface + slog backend (debug via `AGENTICGO_DEBUG`)
- `internal/scaffold/scaffold.go` — in-memory cron-job scaffolding
- `internal/mcp/mcp.go` — MCP client manager (server registry, discovery, `CallTool`)
- `internal/llm/openai.go` — OpenAI-compatible provider on the official SDK
  (streaming + tool calls); `openai.go.bak` = pre-SDK reference copy (not compiled)
- `internal/tools/{tools,fs,exec,memory}.go` — registry, filesystem jail,
  allow-listed exec, memory tools (`memory_search`, `record_observation`)
- `internal/store/store.go` — SQLite schema + queries (per-agent; FTS5 knowledge +
  observations)
- `internal/server/server.go` — chi routes, `/ws` streaming, embedded SPA
- `internal/server/web/index.html` — the whole UI (single file, no build step)
