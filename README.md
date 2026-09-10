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
- **Context files** — edit `SOUL.md` (persona), `AGENTS.md` (operating instructions),
  and `IDENTITY.md` (name/role) right in the UI; they're composed into the system prompt.
- **Skills** — drop a folder with a `SKILL.md` under an agent's `skills/` dir and it's
  injected into that agent's system prompt (progressive disclosure).
- **Chat Web UI** — embedded HTML/JS page (no build step), streaming replies over
  WebSocket, with live tool-call visibility.
- **Agent loop** — the model can call tools, observe results, and iterate until it
  answers (capped to prevent runaway loops).
- **OpenAI-compatible LLM** — works with Ollama, LM Studio, vLLM, or OpenAI.
- **Built-in tools (allow-listed):**
  - `read_file` — read a file in the workspace
  - `write_file` — write a file in the workspace
  - `list_files` — list a directory in the workspace
  - `exec` — run **allow-listed commands only**, jailed to the workspace
- **Security by default** — filesystem tools jailed to a workspace dir; `exec` only runs
  allow-listed commands; tools can be globally allow-listed.
- **Memory** — conversations persisted to SQLite (pure Go, no cgo), scoped per agent.
- **Self-evolution (simplified)** — an "Evolve" pass extracts durable learnings from a
  session into the agent's knowledge store, re-injected into its system prompt later.

## Agent layout on disk

```
data/agents/
  researcher/
    SOUL.md            # persona: tone, values, behavioral guidelines
    AGENTS.md          # operating instructions: how to approach tasks, use tools
    IDENTITY.md        # name, role, short description
    skills/
      web-research/
        SKILL.md       # front-matter (name, description) + instructions
        helper.py      # optional supporting files (informational)
    workspace/         # per-agent file jail for tools
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
- [x] Phase 2b — **multi-agent + context files (SOUL/AGENTS/IDENTITY) + skills**
- [ ] Phase 3 — **MCP client** (`modelcontextprotocol/go-sdk`) to attach external tools
      at runtime, gated by the same allow-list
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
| `AGENTICGO_SYSTEM_PROMPT` | built-in | Base system prompt (prepended to every agent) |

## API

### Chat
- `GET /healthz` — liveness probe.
- `GET /` — chat UI.
- `GET /ws` — WebSocket. Client sends
  `{ "agent": "researcher", "session": "default", "message": "..." }`; server streams
  events `{ kind, text, tool_name, tool_args, tool_result, error }` where
  `kind` ∈ `text | tool_call | tool_result | done | error`.

### Agents
- `GET /api/agents` — list agents.
- `POST /api/agents` — create. Body:
  `{ "key": "researcher", "name": "...", "description": "...", "soul": "..." }`.
- `GET /api/agents/{key}` — get one agent + its context files.
- `DELETE /api/agents/{key}` — delete an agent.

### Context files
- `GET /api/agents/{key}/files/{name}` — read `SOUL.md` / `AGENTS.md` / `IDENTITY.md`.
- `PUT /api/agents/{key}/files/{name}` — write. Body: `{ "content": "..." }`.

### Skills
- `GET /api/agents/{key}/skills` — list parsed skills for an agent.

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
│   ├── agents/           # file-based agent loader (SOUL/AGENTS/IDENTITY) + registry
│   ├── skills/           # SKILL.md loader + prompt composition
│   ├── tools/            # tool registry + filesystem + exec tools
│   ├── agent/            # per-agent tool-use loop + self-evolution
│   ├── store/            # SQLite (conversations + knowledge, per-agent)
│   └── server/           # HTTP + WebSocket + embedded UI
│       └── web/          # chat UI (embedded via go:embed)
├── go.mod                # module github.com/dkr290/agenticgo
└── README.md
```

## License

MIT (yours to choose — update as you like).
