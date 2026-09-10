# agenticgo

A simplified, self-hosted **AI agent gateway** written in Go — inspired by
[GoClaw](https://github.com/nextlevelbuilder/goclaw) / OpenClaw but deliberately minimal.

Create **multiple agents**, each with its own **context files** (SOUL.md, AGENTS.md,
IDENTITY.md) and **skills** (SKILL.md), then chat with any of them through a single
Web UI. No multi-tenancy, no messaging channels, no Postgres, no vector DB. Runs as a
single static binary, in Docker, or on Kubernetes.

> Clean-room reimplementation of the *ideas* (agents, context files, skills, tool-use
> loop, memory, self-evolution). No GoClaw/OpenClaw code is copied.

## What it does

- **Multiple agents** — each agent is a directory with context files that define its
  persona and behavior. Pick which agent to chat with from the sidebar.
- **Context files** — edit `AGENTS.md`, `SOUL.md`, `IDENTITY.md`, `USER.md`,
  `USER_PREDEFINED.md`, `CAPABILITIES.md`, and `HEARTBEAT.md` right in the UI; they're
  composed into the system prompt.
- **Skills** — drop a folder with a `SKILL.md` under an agent's `skills/` dir and it's
  injected into that agent's system prompt, **or upload a ZIP** from the Skills page
  (validated: needs a `SKILL.md` with a `name`; extracted path-traversal-safe).
- **Knowledge Base** — upload reference documents (text/markdown) per agent from the
  Knowledge Base page. They're **full-text indexed over content** (SQLite FTS5) so the
  agent recalls them via the `search_docs` tool — not bulk-injected into prompts.
- **Sidebar Web UI** — dark, GoClaw-inspired SPA (no build step): Overview, Chat,
  Agents (with a per-agent Files/Skills/Knowledge/Config editor), Skills (with ZIP upload),
  Built-in Tools, MCP Servers, Cron, Memory, Knowledge Base, and Providers.
- **Agent loop** — the model can call tools, observe results, and iterate until it
  answers (capped to prevent runaway loops).
- **Providers** — manage multiple OpenAI-compatible endpoints (Ollama, LM Studio, vLLM,
  OpenAI) from the UI; pick a default or override per chat. Each agent can also pin its
  own provider, model, temperature, and max-tokens in `config.json`. Configs persist to
  `data/providers.json`.
- **Built-in tools (allow-listed):**
  - `read_file` — read a file in the workspace
  - `write_file` — write a file in the workspace
  - `list_files` — list a directory in the workspace
  - `exec` — run **allow-listed commands only**, jailed to the workspace
- **Security by default** — filesystem tools jailed to a workspace dir; `exec` only runs
  allow-listed commands; tools can be globally allow-listed.
- **Memory** — conversations persisted to SQLite (pure Go, no cgo), scoped per agent.
  Two memory kinds, designed so a recurring agent (e.g. a k8s cron watcher) doesn't
  hallucinate from stale data:
  - **Curated knowledge** — durable learnings, full-text searchable (SQLite FTS5) via
    the `memory_search` tool; only selectively recalled, not bulk-injected.
  - **Observations** — timestamped, time-bound findings (cluster state, etc.). Only the
    *latest* is injected into prompts; old ones are pruned by retention
    (`AGENTICGO_OBSERVATION_TTL_DAYS` / `AGENTICGO_OBSERVATION_KEEP`).
- **Self-evolution (simplified)** — an "Evolve" pass extracts durable learnings from a
  session into the agent's knowledge store, re-injected into its system prompt later.
- **Scaffolding** — MCP Servers and Cron pages/endpoints store configuration but do not
  connect/execute yet (Phase 3).

## Agent layout on disk

```
data/agents/
  researcher/
    AGENTS.md            # operating instructions: how to approach tasks, use tools
    SOUL.md              # persona: tone, values, behavioral guidelines
    IDENTITY.md          # name, role, short description
    USER.md              # notes about the user this agent serves
    USER_PREDEFINED.md   # canned user context injected into every session
    CAPABILITIES.md      # what the agent can and cannot do
    HEARTBEAT.md         # empty by default (reserved)
    config.json          # per-agent LLM settings (optional; provider/model/temperature/max_tokens, nil = inherit)
    skills/
      web-research/
        SKILL.md         # front-matter (name, description) + instructions
        helper.py        # optional supporting files (informational)
    workspace/           # per-agent file jail for tools
```

### SKILL.md format

```markdown
---
name: Web Research
description: Search and synthesize web sources
---
# Web Research
- Break the question into sub-queries.
- Prefer primary sources.
- Cite what you find.
```

## What it deliberately leaves out (vs GoClaw)

Multi-tenancy / RBAC · messaging channels · agent teams & delegation · PostgreSQL +
pgvector knowledge graph · 5-layer security stack · 20+ provider adapters. Those are the
hard 80% — this project keeps the useful 20%.

## Roadmap

- [x] Phase 1 — server skeleton, embedded chat UI, streaming LLM replies
- [x] Phase 2 — agent tool-use loop + built-in allow-listed tools
- [x] Phase 2b — **multi-agent + context files + skills**
- [x] Phase 2c — **sidebar SPA** (Chat/Agents/Skills/Tools/Providers) + provider CRUD +
      MCP/cron API scaffolding
- [ ] Phase 3 — **MCP client** (`modelcontextprotocol/go-sdk`) to attach external tools
      at runtime, gated by the same allow-list; cron scheduler executing stored jobs
- [x] Phase 4 — SQLite memory + simplified self-evolution
- [ ] Phase 5 — Dockerfile + Kubernetes manifests (Deployment + PVC + Service)

## Quick start

Prereqs: Go 1.26+. A running OpenAI-compatible endpoint (e.g. Ollama).

```bash
# Build
go build -o agenticgo ./cmd/agenticgo

# Run (defaults target Ollama at http://localhost:11434/v1, model qwen2.5)
./agenticgo

# Open the UI
open http://localhost:8080
```

A `default` agent is seeded on first run. Create more from the **+ New** button.

Health check: `curl http://localhost:8080/healthz`

## Configuration (env vars)

| Variable | Default | Description |
|---|---|---|
| `AGENTICGO_ADDR` | `:8080` | HTTP listen address |
| `AGENTICGO_LLM_BASE_URL` | `http://localhost:11434/v1` | OpenAI-compatible base URL |
| `AGENTICGO_LLM_API_KEY` | `ollama` | API key (Bearer), if needed |
| `AGENTICGO_LLM_MODEL` | `qwen2.5` | Model name |
| `AGENTICGO_DATA_DIR` | `data` | Where SQLite + agents + workspace live |
| `AGENTICGO_AGENTS_DIR` | `data/agents` | Root dir containing one folder per agent |
| `AGENTICGO_WORKSPACE_DIR` | `data/workspace` | Fallback jail root for tools |
| `AGENTICGO_EXEC_ALLOWLIST` | `ls,cat,grep,find,echo,pwd,head,tail,wc,mkdir,touch,cp,mv,date` | Commands `exec` may run |
| `AGENTICGO_TOOL_ALLOWLIST` | *(empty = all)* | Restrict which tools are exposed |
| `AGENTICGO_MAX_ITERATIONS` | `12` | Max agent loop iterations |
| `AGENTICGO_OBSERVATION_TTL_DAYS` | `14` | Prune observations older than this |
| `AGENTICGO_OBSERVATION_KEEP` | `200` | Max observations kept per agent |
| `AGENTICGO_SYSTEM_PROMPT` | built-in | Base system prompt (prepended to every agent) |

## API

### Chat
- `GET /healthz` — liveness probe.
- `GET /` — the SPA.
- `GET /ws` — WebSocket. Client sends
  `{ "agent": "researcher", "session": "default", "message": "...", "provider": "ollama-local" }`
  (`provider` optional); server streams events
  `{ kind, text, tool_name, tool_args, tool_result, error }` where
  `kind` ∈ `text | tool_call | tool_result | done | error`.

### Agents
- `GET /api/agents` — list agents.
- `POST /api/agents` — create. Body:
  `{ "key": "researcher", "name": "...", "description": "...", "soul": "...",
  "config": { "provider": "...", "model": "...", "temperature": 0.2, "max_tokens": 2048 } }`
  (all `config` fields optional, nil = inherit).
- `GET /api/agents/{key}` — get one agent + its context files + config.
- `PUT /api/agents/{key}/config` — replace the agent's LLM config. Body:
  `{ "provider": "...", "model": "...", "temperature": 0.9, "max_tokens": 4096 }`; omit
  fields to reset them to inherit, send `{}` to clear everything.
- `DELETE /api/agents/{key}` — delete an agent.

### Context files
- `GET /api/agents/{key}/files/{name}` — read one of the known context files
  (`AGENTS.md`, `SOUL.md`, `IDENTITY.md`, `USER.md`, `USER_PREDEFINED.md`,
  `CAPABILITIES.md`, `HEARTBEAT.md`).
- `PUT /api/agents/{key}/files/{name}` — write. Body: `{ "content": "..." }`.

### Skills
- `GET /api/skills` — list skills across all agents.
- `GET /api/agents/{key}/skills` — list parsed skills for one agent.
- `POST /api/agents/{key}/skills/upload` — install a skill from a ZIP (multipart
  field `file`). The ZIP must contain a `SKILL.md` with a `name` in front-matter.

### Knowledge base
- `GET /api/agents/{key}/docs` — list an agent's knowledge-base documents.
- `POST /api/agents/{key}/docs` — add a document (multipart `file`, or JSON
  `{ "title": "...", "content": "..." }`).
- `GET /api/agents/{key}/docs/{id}` — get one document (with content).
- `DELETE /api/agents/{key}/docs/{id}` — delete a document.
- `GET /api/agents/{key}/docs/search/query?q=...` — full-text search over content.

### Conversations & memory
- `GET /api/sessions` — list agent+session pairs.
- `GET /api/sessions/{agent}/{session}/messages` — stored messages.
- `GET /api/agents/{key}/knowledge` — accumulated per-agent knowledge.
- `GET /api/agents/{key}/knowledge/search?q=...` — FTS5 keyword search over knowledge.
- `GET /api/agents/{key}/observations` — recent observations (newest first).
- `POST /api/agents/{key}/observations` — record one. Body: `{ "content": "..." }`.

### Built-in tools
- `GET /api/tools` — list registered built-in tools.

### Providers
- `GET /api/providers` — list provider configs.
- `POST /api/providers` — create/update. Body:
  `{ "name": "ollama-local", "display_name": "...", "base_url": "http://localhost:11434/v1", "api_key": "...", "model": "qwen2.5", "default": true }`.
- `GET /api/providers/{name}` — get one.
- `DELETE /api/providers/{name}` — delete.
- `POST /api/providers/{name}/test` — test the connection (lists models).

### Scaffolding (stored, not yet functional)
- `GET|POST /api/mcp-servers`, `DELETE /api/mcp-servers/{id}` — MCP server configs.
- `GET|POST /api/cron`, `DELETE /api/cron/{id}` — cron job configs.

### Self-evolution
- `POST /api/evolve` — body `{ "agent": "researcher", "session": "default" }`, runs a
  self-evolution pass that stores learnings for that agent.

## Project layout

```
agenticgo/
├── cmd/agenticgo/        # entry point (manual DI, no framework)
├── internal/
│   ├── config/           # env-based config
│   ├── llm/              # provider interface + OpenAI-compatible client
│   ├── providers/        # named OpenAI-compatible provider configs (JSON on disk)
│   ├── agents/           # file-based agent loader (context files) + registry
│   ├── skills/           # SKILL.md loader + prompt composition
│   ├── tools/            # tool registry + filesystem + exec tools
│   ├── agent/            # per-agent tool-use loop + self-evolution
│   ├── store/            # SQLite (conversations + knowledge, per-agent)
│   ├── scaffold/         # in-memory MCP-server + cron-job scaffolding
│   └── server/           # HTTP + WebSocket + embedded SPA
│       └── web/          # sidebar SPA (embedded via go:embed)
├── go.mod                # module github.com/dkr290/agenticgo
└── README.md
```

## License

MIT (yours to choose — update as you like).
