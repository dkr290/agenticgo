# agenticgo

A simplified, self-hosted **AI agent gateway** written in Go — inspired by
[GoClaw](https://github.com/nextlevelbuilder/goclaw) / OpenClaw but deliberately minimal.

Create **multiple agents**, each with its own **context files** (SOUL.md, AGENTS.md,
IDENTITY.md) and **skills** (SKILL.md), then chat with any of them through a single
Web UI. No multi-tenancy, no messaging channels, no Postgres, no vector DB. Runs as a
single static Go binary or a Docker container. Kubernetes manifests are on the roadmap.

> Clean-room reimplementation of the *ideas* (agents, context files, skills, tool-use
> loop, memory, self-evolution). No GoClaw/OpenClaw code is copied.

## What it does

- **Multiple agents** — each agent is a directory with context files that define its
  persona and behavior. Pick which agent to chat with from the sidebar.
- **Context files** — edit `AGENTS.md`, `SOUL.md`, `IDENTITY.md`, `USER.md`,
  `USER_PREDEFINED.md`, `CAPABILITIES.md`, and `HEARTBEAT.md` right in the UI; they're
  composed into the system prompt.
- **Skills** — upload a ZIP from the Skills page into the shared `data/skills/`
  library, then explicitly enable it for each agent under Agents → Skills.
  Each skill contains a `SKILL.md`; enabled instructions enter the system prompt.
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
  `data/providers.json`; **API keys are encrypted at rest** (AES-256-GCM, `enc:v1:`
  envelope) with a key from `AGENTICGO_SECRET_KEY` or a generated `data/secret.key`.
- **Built-in tools (allow-listed):**
  - `read_file` — read a file in the workspace
  - `write_file` — write a file in the workspace
  - `list_files` — list a directory in the workspace
  - `exec` — run **allow-listed commands only**; standard commands use a Linux
    bubblewrap sandbox, while explicitly enabled dangerous extras use process/container privileges
- **Filesystem isolation** — filesystem tools use Go's `os.Root` to enforce the
  workspace boundary, including symlinks. Standard exec commands receive the workspace,
  read-only system binaries/libraries, a private temporary directory, and a clean environment.
  They have no host network or access to application data outside the workspace.
- **Memory** — conversations persisted to SQLite (pure Go, no cgo), scoped per agent.
  Two memory kinds, designed so a recurring agent (e.g. a k8s cron watcher) doesn't
  hallucinate from stale data:
  - **Curated knowledge** — durable learnings, full-text searchable (SQLite FTS5) via
    the `memory_search` tool; a configurable recent subset is also injected into the prompt
    (`AGENTICGO_KNOWLEDGE_INJECT`, default 25; 0 disables injection). The agent
    also saves durable facts itself via the `memory_save` tool, and entries can be
    deleted from the Memory page. Self-educated knowledge can optionally be bounded by
    retention (`AGENTICGO_KNOWLEDGE_TTL_DAYS` / `AGENTICGO_KNOWLEDGE_KEEP`, both `0` =
    off by default; e.g. `365` / `1000`) so a long-lived agent can't grow it without
    limit. GUI-uploaded knowledge-base documents are a separate store and are never
    auto-pruned.
  - **Observations** — timestamped, time-bound findings (cluster state, etc.). Old ones
    are pruned by retention (`AGENTICGO_OBSERVATION_TTL_DAYS` / `AGENTICGO_OBSERVATION_KEEP`,
    default 14 days / 200 entries). Each limit is independent: 0 disables that limit.
    How many recent ones are injected into the prompt each run is configurable — per agent
    (`config.json` `observation_inject`, Agents → Config) over the global
    `AGENTICGO_OBSERVATION_INJECT` default — so a monitor can diff the last few snapshots.
- **Self-evolution (simplified)** — an "Evolve" pass extracts durable learnings from a
  session into the agent's knowledge store, re-injected into its system prompt later.
- **Cron** — jobs are persisted to `data/cron.json` and executed by a real scheduler
  (`robfig/cron`): each enabled job runs its agent on its own cron session. The Cron page
  shows live status (armed / next run). Overlapping ticks of the same job are skipped;
  shutdown cancels and waits for active jobs. Chat turns sharing an agent/session are serialized.

## Agent layout on disk

```
data/
  skills/                  # shared library; uploaded once, enabled per agent
    web-research/
      SKILL.md
  agents/researcher/
    AGENTS.md            # operating instructions: how to approach tasks, use tools
    SOUL.md              # persona: tone, values, behavioral guidelines
    IDENTITY.md          # name, role, short description
    USER.md              # notes about the user this agent serves
    USER_PREDEFINED.md   # canned user context injected into every session
    CAPABILITIES.md      # what the agent can and cannot do
    HEARTBEAT.md         # checklist for recurring checks; included in every run's prompt
    config.json          # LLM settings + enabled_skills/tools/commands/builtin_tools
    images/              # reference images for vision-capable models
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


## Roadmap

- [x] Phase 1 — server skeleton, embedded chat UI, streaming LLM replies
- [x] Phase 2 — agent tool-use loop + built-in allow-listed tools
- [x] Phase 2b — **multi-agent + context files + skills**
- [x] Phase 2c — **sidebar SPA** (Chat/Agents/Skills/Tools/Providers) + provider CRUD +
      MCP/cron API scaffolding
- [x] Phase 2d — **Huma API** (`danielgtaylor/huma/v2`): every REST route is a typed
      Huma operation, so the OpenAPI 3.1 spec (`/openapi.json`, `/openapi.yaml`) and
      the generated docs UI (`/docs`) are always in sync with the served API
- [x] Phase 3 — **MCP client** (`modelcontextprotocol/go-sdk`) to attach external tools
      at runtime, gated by the same allow-list; **cron scheduler** executing stored
      jobs (`internal/cron`, persisted to `data/cron.json`)
- [x] Phase 4 — SQLite memory + simplified self-evolution
- [x] Phase 5a — multi-stage Dockerfile with non-root runtime and persistent data
- [ ] Phase 5b — Kubernetes manifests (Deployment + PVC + Service)

## Quick start

Prereqs: Go 1.26.5+. A running OpenAI-compatible endpoint (e.g. Ollama).
Standard `exec` commands additionally require Linux, bubblewrap (`bwrap`), and
permission to create user namespaces. On Debian/Ubuntu install it with
`apt-get install bubblewrap`. If the sandbox cannot start, the tool returns an
error; it never falls back to unsandboxed execution.

```bash
# Build
go build -o agenticgo ./cmd/agenticgo

# Run
./agenticgo

# Open the UI
open http://localhost:8080

# API docs (generated from the Huma-registered routes)
open http://localhost:8080/docs
```

Nothing is seeded on first run — configure a provider (Providers page), then create agents from the **+ New** button.

Health check: `curl http://localhost:8080/healthz`

## Docker deployment

Build the image and start an instance with persistent data:

```bash
docker build -t agenticgo:local .
docker volume create agenticgo-data
docker run -d --name agenticgo \
  --restart unless-stopped \
  --stop-timeout 30 \
  -p 127.0.0.1:8080:8080 \
  -v agenticgo-data:/data \
  agenticgo:local

docker logs agenticgo
curl --fail http://127.0.0.1:8080/healthz
```

Open `http://localhost:8080`, configure a provider, then create an agent. Provider
URLs are resolved **inside the container**: `localhost` refers to the container,
so use an address or Docker network name reachable from it for Ollama or other
services. On Linux, `--add-host=host.docker.internal:host-gateway` can make a
host-bound service reachable if that service listens on a reachable interface.

The Dockerfile compiles a static binary (`CGO_ENABLED=0`) in a Go build stage.
The Debian runtime includes bubblewrap, standard command-line utilities, CA
certificates, timezone data, and curl for the Docker health check. The Go binary
runs directly as PID 1 and handles `SIGTERM` for graceful shutdown. It runs as
**UID/GID 10001**, listens on 8080, and stores all application data under `/data`.
The default timezone is UTC; set `TZ` to change the default cron timezone.

If you want Docker to reap orphaned descendants of exec/MCP commands, add
`--init` to `docker run`; the image itself starts the Go binary directly.

Use a named volume as above, or make a bind-mounted data directory writable by
UID/GID 10001. Persist the **whole data directory**, including `secret.key`, so
encrypted provider credentials remain usable after container recreation. If
using `AGENTICGO_SECRET_KEY` instead, supply the same key to every replacement
container. Runtime data and local Git files are excluded from the build context
by `.dockerignore`.

If your build network uses a private TLS certificate authority, provide a trusted
CA bundle as an optional BuildKit secret. It is used by Go during the build and
is not copied into the runtime image:

```bash
docker build --secret id=build_ca_bundle,src=/path/to/trusted-ca-bundle.pem \
  -t agenticgo:local .
```

For runtime provider/MCP HTTPS endpoints using a private CA, mount a trusted
bundle and set `SSL_CERT_FILE` to its container path.

Pass application settings with `-e NAME=value` or `--env-file /path/to/settings.env`.
Use **one agenticgo process per data directory**: the JSON registries and SQLite
deployment are designed for a single instance. Connect MCP servers manually after
startup before relying on cron jobs that use them.

### Standard exec sandbox in Docker

Installing bubblewrap supplies the sandbox executable. The host kernel and the
container runtime's seccomp/AppArmor policy must also permit creating the user,
mount, PID, and network namespaces it uses. Verify this on the deployment host:

```bash
docker exec agenticgo bwrap \
  --die-with-parent --new-session --unshare-all --cap-drop ALL --clearenv \
  --ro-bind /usr /usr --ro-bind /bin /bin \
  --ro-bind /lib /lib --ro-bind /lib64 /lib64 \
  --tmpfs /tmp --dev /dev --bind /data/workspace /workspace \
  --chdir /workspace --setenv PATH /usr/bin:/bin -- /usr/bin/pwd
```

Expected output is `/workspace` (omit the `/lib64` bind on architectures where
that directory is absent). A namespace/permission error means the runtime policy
must be configured for bubblewrap. Standard exec then returns a sandbox error,
without falling back to unsandboxed execution. Filesystem tools and external MCP
tools use their own execution paths; explicitly enabled dangerous exec commands
use the container process's privileges as described below.

### Extra commands in a container

Install commands such as `kubectl` or `git` in your deployment image, declare them
in `AGENTICGO_EXTRA_EXEC_COMMANDS`, and enable them for the agent in its Extra
Dangerous Exec Commands tab. They are resolved through the container's `PATH`
and run in the agent workspace with the container process's environment,
network, and mounted credentials. They do not require bubblewrap. Mounting a
kubeconfig or supplying a Kubernetes service account makes those credentials
available to an enabled `kubectl` command. A command installed in the image but
not declared and enabled is still unavailable to the agent.

For standard commands, container runtimes must permit bubblewrap's user/mount
namespaces. The application reports a sandbox error when the runtime blocks them.

For example, build a derived image containing `git` and `jq`:

```dockerfile
FROM agenticgo:local
USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends git jq \
    && rm -rf /var/lib/apt/lists/*
USER 10001:10001
```

Run that image with `-e AGENTICGO_EXTRA_EXEC_COMMANDS=git,jq`, then enable the
commands for the chosen agent. You can install a pinned `kubectl` binary into
`/usr/local/bin` in the same way. A mounted kubeconfig must be readable by UID
10001; pass its path with `KUBECONFIG`. Installation and global declaration alone
do not enable a dangerous command for any agent.

### Deployment status

The automated tests cover the agent loop, persistence, isolation, MCP lifecycle,
cron cancellation, and key UI flows. Production suitability still depends on
validation against your actual providers, MCP servers, and container runtime.
The application has no built-in authentication: expose the UI/API through a
trusted network or an authenticated reverse proxy. The example above publishes
the port only on loopback. Exercise data backup/restore and a sustained hourly
monitoring run before depending on it for unattended operations.

Run the disposable container smoke test against your locally built image:

```bash
python3 scripts/docker-smoke.py agenticgo:local
# Also require standard exec to work under this host's namespace policy:
python3 scripts/docker-smoke.py agenticgo:local --require-sandbox
```

The script requires a local Docker daemon and Python 3. It uses a local mock LLM
and temporary Docker resources to test health, non-root execution, file tools,
extra-command gating, encrypted keys, graceful shutdown, and persistent data
across container recreation. It reports sandbox availability separately and
cleans up its containers and volume afterward.

### Development checks

```bash
gofmt -w .
go vet ./...
go build ./...
go test -race ./...
node --test internal/server/web_test.cjs
```

## Configuration (env vars)

| Variable | Default | Description |
|---|---|---|
| `AGENTICGO_ADDR` | `:8080` | HTTP listen address |
| `AGENTICGO_DATA_DIR` | `data` | Where SQLite + agents + workspace live |
| `AGENTICGO_AGENTS_DIR` | `data/agents` | Root dir containing one folder per agent |
| `AGENTICGO_WORKSPACE_DIR` | `data/workspace` | Fallback jail root for tools |
| `AGENTICGO_EXEC_ALLOWLIST` | `ls,cat,grep,find,echo,pwd,head,tail,wc,mkdir,touch,cp,mv,date` | Commands `exec` may run |
| `AGENTICGO_EXTRA_EXEC_COMMANDS` | *(empty)* | Extra, dangerous commands baked into the deployment image (e.g. `kubectl,git,jq`). Not runnable by default — each agent must enable them via `config.json` `enabled_commands` (Agents → Extra Dangerous Exec Commands tab). |
| `AGENTICGO_TOOL_ALLOWLIST` | *(empty = all)* | Restrict which tools are exposed |
| `AGENTICGO_MAX_ITERATIONS` | `12` | Max agent loop iterations |
| `AGENTICGO_OBSERVATION_TTL_DAYS` | `14` | Prune observations older than this many days. `0` disables age-based pruning. |
| `AGENTICGO_OBSERVATION_KEEP` | `200` | Cap observations per agent at cleanup time. `0` disables the count limit. |
| `AGENTICGO_OBSERVATION_INJECT` | `1` | Default number of recent observations injected into an agent's prompt (1 = only the latest; 0 = none). Overridable per agent via `config.json` `observation_inject`. |
| `AGENTICGO_KNOWLEDGE_INJECT` | `25` | How many recent curated-knowledge entries are injected into an agent's prompt (app-wide). `0` disables injection while keeping memory searchable. |
| `AGENTICGO_KNOWLEDGE_TTL_DAYS` | `0` (off) | **Opt-in age-based retention** for self-educated knowledge. `0` = never expire by age (the default — nothing is deleted). Example: `365` deletes entries older than ~1 year on the next run. Affects only the agent's `knowledge` (self-educated learnings) — never the GUI-uploaded knowledge-base documents. |
| `AGENTICGO_KNOWLEDGE_KEEP` | `0` (off) | **Opt-in count-based retention.** `0` = no cap (the default). Example: `1000` keeps only the newest 1000 entries per agent at cleanup time. Set either or both retention limits; both apply age-then-count. Leave both `0` to keep everything forever. |
| `AGENTICGO_SYSTEM_PROMPT` | built-in | Base system prompt (prepended to every agent) |
| `AGENTICGO_RETENTION_SWEEP_MINUTES` | `60` | Background retention interval for all agents. `0` disables the sweeper; per-run pruning still applies. |
| `AGENTICGO_DEBUG` | `false` | Enable debug logs. |
| `AGENTICGO_SECRET_KEY` | *(none)* | Base64-encoded 32-byte master key used to encrypt provider API keys at rest (`enc:v1:` values in `data/providers.json`). When unset, a random key is generated once and stored in `data/secret.key` (0600). Supply it via env (e.g. from a secrets manager) so the key never touches disk. Generate one with `openssl rand -base64 32`. **Warning:** changing (or losing) the key makes previously encrypted API keys undecryptable. |

## Learning, cron runs, memory, and history

### What "self-learning" means

An agent builds experience by saving information and recalling it in future
runs. This is persistent memory, not training or changing the underlying model's
weights. An hourly Kubernetes monitor can learn across runs, provided it uses
the memory tools:

1. A cron job invokes the agent with its configured prompt. The agent receives
   its context files, enabled skills/tools, recent conversation messages, and
   the configured number of observations and knowledge entries.
2. The agent uses its enabled MCP tools to inspect live Kubernetes state. Connect
   the MCP server and enable the required tools for that agent first.
3. It calls `record_observation` to save current findings and `memory_save` when
   it has a new, verified, reusable lesson. These core memory tools are always
   available; they do not need separate MCP or built-in-tool enablement.
4. Messages and tool results are stored in the job's conversation. The next run
   can use saved observations and knowledge alongside fresh tool results.

**A completed run does not guarantee a new knowledge entry.** Instructions guide
the model to make the appropriate tool calls; saving requires the call to succeed.
A healthy check with no new durable lesson should still save its observation,
without manufacturing a lesson merely to "learn something" each hour.

The separate **Evolve** step reads recent conversation messages and asks the LLM
to extract durable learnings. It is triggered by the Chat page's Evolve button,
`POST /api/evolve`, or REST chat with `"evolve": true`. **Cron does not automatically
call Evolve after each run.** It can save lessons during the normal loop through
`memory_save`.

### What is stored, and what the agent sees

| Store | Written by | Retention | Available to a future run |
|---|---|---|---|
| Observations | `record_observation` or the observations API | Default: 14 days and newest 200 entries per agent | Newest 1 injected by default; configurable |
| Curated knowledge | `memory_save` or Evolve | Default: no automatic deletion | Newest 25 injected by default; other entries searchable with `memory_search` |
| Conversation history | Engine saves messages and tool results | No automatic deletion or retention env var | Latest 40 message rows loaded for a normal run, omitting incomplete tool exchanges |
| Uploaded knowledge-base documents | Knowledge Base UI/API | No automatic deletion | Titles in the prompt; content recalled through `search_docs` and `read_doc` |

Observations and knowledge are **per agent**, shared across that agent's chat
sessions and cron jobs. Each cron job has its own persistent conversation named
`cron-<job-id>` and reuses it on every execution. If several jobs share an agent,
include the cluster/namespace or other scope in observations so comparisons use
the correct baseline.

The 40-row history window counts **messages, not cron runs**. One run can produce
many rows through tool calls. Evolve reads up to 60 rows and needs at least four;
the history UI retrieves up to 200 rows. These read limits do not delete older
rows. A conversation's messages remain until the conversation is deleted.
Pruning observations or knowledge does not erase copies already stored in
conversation messages or tool results.

### Retention versus prompt injection

Retention determines what remains stored; injection determines how much of it
is automatically included in a prompt. The settings are global, with limits
applied separately to each agent:

| Environment variable | Default | Exact effect of `0` |
|---|---:|---|
| `AGENTICGO_OBSERVATION_TTL_DAYS` | `14` | Disable observation age-based deletion |
| `AGENTICGO_OBSERVATION_KEEP` | `200` | Disable the observation count limit |
| `AGENTICGO_OBSERVATION_INJECT` | `1` | Inject no observations; stored entries remain |
| `AGENTICGO_KNOWLEDGE_TTL_DAYS` | `0` | Disable knowledge age-based deletion |
| `AGENTICGO_KNOWLEDGE_KEEP` | `0` | Disable the knowledge count limit |
| `AGENTICGO_KNOWLEDGE_INJECT` | `25` | Inject no knowledge; entries remain searchable |
| `AGENTICGO_RETENTION_SWEEP_MINUTES` | `60` | Disable background cleanup; per-run cleanup still applies |

**TTL and keep-count limits are independent.** Cleanup deletes entries older than
the TTL, then keeps only the newest N remaining entries when a count limit is
enabled. Both retention limits at `0` disable automatic pruning for that store.
Setting only TTL to `0` still allows the keep-count limit to remove older entries.

With one observation per hour, the default count of 200 represents approximately
**8 days and 8 hours**, so it takes effect before the 14-day TTL. Counts refer to
saved entries; multiple entries per run fill the limit faster.

Cleanup runs at the start of an agent run and on the background sweep (every 60
minutes by default). Expired or excess entries are removed at cleanup time, not
immediately on every save. No background sweeper starts if all retention limits
are disabled. Uploaded reference documents are never affected by these limits.

An agent's **Config → Observations injected** (`config.json` `observation_inject`)
overrides `AGENTICGO_OBSERVATION_INJECT`; leaving it empty inherits the global
value. Knowledge injection and the retention limits are global settings.
Environment changes require restarting agenticgo. Use literal `0` to disable a
limit; leaving an environment variable unset or empty uses its default.

### Example: hourly Kubernetes monitor

For about two weeks of observations at one saved snapshot per hour, three recent
snapshots in each prompt, and indefinitely retained durable lessons:

```env
AGENTICGO_OBSERVATION_TTL_DAYS=14
AGENTICGO_OBSERVATION_KEEP=336
AGENTICGO_OBSERVATION_INJECT=3

AGENTICGO_KNOWLEDGE_TTL_DAYS=0
AGENTICGO_KNOWLEDGE_KEEP=0
AGENTICGO_KNOWLEDGE_INJECT=25

AGENTICGO_RETENTION_SWEEP_MINUTES=60
```

Create a job for the monitoring agent on the **Cron** page, set its schedule to
`0 * * * *` (hourly, using the server's timezone by default), and use a prompt such as:

```text
Run the recurring Kubernetes health check for cluster production, namespace payments.
Use the enabled MCP tools to inspect current state and follow the monitoring
checklist in HEARTBEAT.md already included in your prompt. Save one consolidated
observation, compare against available observations for this scope, and save a
durable lesson only when supported by new evidence. Summarize changes, unresolved
issues, and any checks that failed.
```

`AGENTICGO_MAX_ITERATIONS` (default 12) caps LLM/tool-loop rounds **within one
invocation**. It does not control the hourly schedule, the number of learning
entries, or retention. Overlapping ticks of a job are skipped, and a cron run
has a 15-minute timeout.

### What to put in the agent's AGENTS.md and HEARTBEAT.md

Edit these through **Agents → select the agent → Files** (on disk:
`data/agents/<key>/AGENTS.md` and `HEARTBEAT.md`). The repository's root `AGENTS.md`
is for developers; it is not the monitoring agent's instructions.

- **AGENTS.md:** general operating and memory rules for every task: recall relevant
  knowledge, save verified reusable learnings, distinguish temporary observations,
  and confirm successful saves.
- **HEARTBEAT.md:** the checklist for recurring monitoring tasks: inspect, compare,
  save one snapshot, consider durable lessons, and report findings.
- **Cron job:** when to run, the agent to use, and the specific task/scope to check.

Both files are included in the system prompt on **every normal agent run**,
including interactive chat. HEARTBEAT.md has no special timer or execution hook.
Its monitoring checklist should explicitly apply only when the current task asks
for a check. The agent already receives the text; it does not need to fetch the
file with `read_file`.

For an existing agent, merge this section into its **AGENTS.md**:

```markdown
## Memory and learning

- Use memory_search to recall related knowledge before relying on past findings
  or saving a lesson that may already be known.
- Use memory_save for new, verified, reusable facts, preferences, and lessons.
  Keep each entry concise (at most 500 bytes); confirm it is saved only after
  the tool succeeds. Do not infer a lasting pattern from one snapshot.
- Use record_observation for current, time-bound state such as health checks.
  Observations are timestamped and subject to configured retention.
- For recurring monitoring tasks, follow HEARTBEAT.md already in the prompt.
  Save the check's observation even when nothing changed. Save a knowledge
  entry only when there is a genuinely new durable lesson.
- Re-check live state before acting on historical observations or lessons.
```

For a Kubernetes watcher, put this checklist in **HEARTBEAT.md**:

```markdown
# Recurring Kubernetes checks

Apply this checklist when the current task requests a monitoring check.

1. Inspect the requested cluster and namespaces using the enabled MCP tools.
   Check workload health, restarts, recent warning events, and relevant resource
   pressure where the available tools support those checks.
2. Compare current findings with available observations for the same scope.
   If no baseline is available, say so. Do not invent previous findings.
3. Call record_observation once with a concise, consolidated snapshot: scope,
   checks performed, important findings, changes, and unresolved issues.
   Include failed/unavailable checks; do not report them as healthy.
4. If the evidence establishes a new durable lesson or stable configuration
   fact, search existing knowledge and use memory_save when it is new.
   Temporary pod status and restart counts belong in the observation.
5. Summarize changes, ongoing actionable issues, and failed checks. If nothing
   changed and no issue needs attention, keep the report brief.
```

New agents receive general memory guidance and a conditional recurring-check
checklist from the embedded templates in `internal/agenttemplates/templates/`.
Templates seed files **only when an agent is created**. To adopt updated guidance
for an existing agent, edit its Files tab; template changes do not overwrite its
saved instructions. Edited context files are loaded on the next run.

## API

### Chat
- `GET /healthz` — liveness probe.
- `GET /` — the SPA.
- `GET /ws` — WebSocket. Client sends
  `{ "agent": "researcher", "session": "default", "message": "...", "provider": "ollama-local" }`
  (`provider` optional); server streams events
  `{ kind, text, tool_name, tool_args, tool_result, error }` where
   `kind` ∈ `text | tool_call | tool_result | done | cancelled | error`.
   An optional request `id` is echoed in events. Send `{ "kind": "cancel", "id": "..." }`
   to stop the active turn. Disconnecting also cancels it. The UI has a Stop button
   and reconnects with backoff.
- `POST /api/chat` — non-streaming REST chat with `{ "agent": "researcher",
  "session": "default", "message": "hello", "provider": "optional-name", "evolve": false }`.
  Returns the final reply and tool-call summary.

### Agents
- `GET /api/agents` — list agents.
- `POST /api/agents` — create. Body:
  `{ "key": "researcher", "name": "...", "description": "...", "soul": "...",
  "config": { "provider": "...", "model": "...", "temperature": 0.2, "max_tokens": 2048 } }`
  (all `config` fields optional, nil = inherit).
- `GET /api/agents/{key}` — get one agent + its context files + config.
- `PUT /api/agents/{key}/config` — replace the agent's entire config, including capability settings. Body:
  `{ "provider": "...", "model": "...", "temperature": 0.9, "max_tokens": 4096 }`; omit
  fields to reset them to inherit, send `{}` to clear everything.
- `PUT /api/agents/{key}/llm-config` — replace only provider/model/temperature/max_tokens/
  vision/observation_inject settings, preserving tool permissions and enabled skills.
  This is the endpoint used by the Config tab. Configuration updates are serialized
  and atomically persisted; malformed configuration files produce an error.
- `DELETE /api/agents/{key}` — delete an agent.

### Context files
- `GET /api/agents/{key}/files/{name}` — read one of the known context files
  (`AGENTS.md`, `SOUL.md`, `IDENTITY.md`, `USER.md`, `USER_PREDEFINED.md`,
  `CAPABILITIES.md`, `HEARTBEAT.md`).
- `PUT /api/agents/{key}/files/{name}` — write. Body: `{ "content": "..." }`.

### Skills
- `GET /api/skills` — list the shared skills library.
- `GET /api/agents/{key}/skills` — list library skills with this agent's enabled state.
- `PUT|DELETE /api/agents/{key}/skills/{skill}` — enable/disable a skill for an agent.
- `POST /api/skills/upload` — install a skill from a ZIP (multipart
  field `file`). The ZIP must contain a `SKILL.md` with a `name` in front-matter.

### Knowledge base
- `GET /api/agents/{key}/docs` — list an agent's knowledge-base documents.
- `POST /api/agents/{key}/docs` — add a document (multipart `file`, or JSON
  `{ "title": "...", "content": "..." }`).
- `GET /api/agents/{key}/docs/{id}` — get one document (with content), scoped to the agent.
- `DELETE /api/agents/{key}/docs/{id}` — delete a document belonging to the agent.
- `GET /api/agents/{key}/docs/search/query?q=...` — full-text search over content.

### Conversations & memory
- `GET /api/sessions` — list agent+session pairs.
- `GET /api/sessions/{agent}/{session}/messages` — stored messages.
- `GET /api/agents/{key}/knowledge` — accumulated per-agent knowledge (entries with `id`/`content`/`created_at`).
- `GET /api/agents/{key}/knowledge/search?q=...` — FTS5 keyword search over knowledge.
- `DELETE /api/agents/{key}/knowledge/{id}` — delete a knowledge entry (Memory page).
- `GET /api/agents/{key}/observations` — recent observations (newest first).
- `POST /api/agents/{key}/observations` — record one. Body: `{ "content": "..." }`.

### Built-in tools
- `GET /api/tools` — list registered built-in tools.
- `GET /api/tools/core` — list the always-on core agent tools (memory/knowledge; read-only).

### Extra dangerous exec commands
- `GET /api/extra-commands` — list commands declared via `AGENTICGO_EXTRA_EXEC_COMMANDS`.
- `GET /api/agents/{key}/extra-commands` — list them with this agent's enabled state.
- `PUT /api/agents/{key}/extra-commands/{name}` — enable one for the agent.
- `DELETE /api/agents/{key}/extra-commands/{name}` — disable it.

### Providers
- `GET /api/providers` — list provider configs.
- `POST /api/providers` — create/update. Body:
  `{ "name": "ollama-local", "display_name": "...", "base_url": "http://localhost:11434/v1", "api_key": "...", "model": "qwen2.5", "default": true }`.
- `GET /api/providers/{name}` — get one.
- `DELETE /api/providers/{name}` — delete.
- `POST /api/providers/{name}/test` — test the connection (lists models).

### MCP servers
- `GET|POST /api/mcp-servers`, `DELETE /api/mcp-servers/{id}` — MCP server configs.
- `POST /api/mcp-servers/{id}/connect|disconnect` — connect/discover tools, or disconnect.

### Cron
- `GET /api/cron` — list jobs (with live `scheduled`/`next_run` status).
- `POST /api/cron` — add a job. Body: `{ "name": "...", "schedule": "*/5 * * * *", "agent": "k8s", "prompt": "...", "enabled": true }`.
- `DELETE /api/cron/{id}` — delete a job.

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
│   ├── mcp/              # MCP client: server registry, discovery, CallTool
│   ├── cron/             # cron scheduler: JSON-persisted jobs + robfig/cron runner
│   └── server/           # HTTP + WebSocket + embedded SPA
│       └── web/          # sidebar SPA (embedded via go:embed)
├── go.mod                # module github.com/dkr290/agenticgo
└── README.md
```

## License

MIT (yours to choose — update as you like).
