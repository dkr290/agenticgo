# TODO

Project-wide work items for agenticgo (the per-agent LLM config feature is done;
this lists what's still needed to make the project fully functional).

## 1. Functional gaps — things that claim to work but don't fully

- ~~**Agents cannot read knowledge-base document content.**~~ **DONE**: `search_docs`
  now returns `id — title` lines and a new agent-scoped `read_doc` tool returns the full
  content (`tools.NewReadDoc` → `store.GetKnowledgeDocForAgent`, which filters by the
  calling agent so one agent can never read another's docs by guessing IDs). System prompt
  updated to tell agents to search then read.

- ~~**Add posibility of installation of additional command line packages**~~ **DONE**:
  took the "more secure" branch — commands are baked into the deployment image and
  declared via `AGENTICGO_EXTRA_EXEC_COMMANDS` (comma-separated bare names, e.g.
  `kubectl,git,jq`), not installed at runtime. They are never on the exec allow-list
  by default: each agent enables the ones it may run via `config.json`
  `enabled_commands` (Agents → Extra Dangerous Exec Commands tab, or
  `PUT/DELETE /api/agents/{k}/extra-commands/{name}`), exactly like skills/MCP tools.
  `Engine.callExec` re-checks the allow-list per run and executes enabled commands
  via a per-run exec tool jailed to the agent's own workspace. The commands are
  listed read-only on the new Extra Dangerous Exec Commands sidebar page (with a
  DANGEROUS warning), and surfaced in the system prompt once enabled.
  (The runtime-install-into-/data variant was deliberately not built.)

- ~~**Fix the chat conversations**~~ **DONE**: conversations are now first-class in the UI.
  All chat-header controls are labeled uniformly (**Agent** / **Provider** / **Conversation**);
  the Conversation field stays free-text (type any name — no dropdown). The standalone
  Load-history button is gone: the sidebar **Chat History** section lists all conversations
  grouped by agent (menu-style rows with an icon, name + message count, 10 most recent per
  agent, DOM-built so names are XSS-safe); clicking one opens the chat with that
  agent+conversation and auto-loads its history, and a hover ✕ deletes it
  (`DELETE /api/sessions/{agent}/{session}` → `store.DeleteSession`, 404 when unknown).
  The Overview sessions table links into the same open-conversation flow. The sidebar list
  refreshes on page load, after each completed chat turn (WS `done`), and after a delete.
  Covered by `internal/store/sessions_test.go`.


- ~~**Move the initial agent context-file templates out of `agents.go` into embedded template files.**~~ **DONE**:
  all seven context files (`AGENTS.md`, `SOUL.md`, `IDENTITY.md`, `USER.md`,
  `USER_PREDEFINED.md`, `CAPABILITIES.md`, `HEARTBEAT.md`) now live as template files in
  `internal/agenttemplates/templates/`, embedded into the binary via `go:embed`
  (`internal/agenttemplates`). `Registry.Create` renders them through
  `agenttemplates.Render(name, Data{Name, Role})` — `SOUL.md`/`IDENTITY.md` substitute
  `{{.Name}}`/`{{.Role}}`, the rest are verbatim. The templates are the **initial content
  only**: after creation the files live on disk under `data/agents/<key>/` and are edited
  through the GUI as before. `Registry.Create`'s `soul` override still wins over the
  `SOUL.md` template. `defaultSoul()`/`defaultAgents()` and the inline strings are gone
  from `internal/agents/agents.go`. Covered by `internal/agenttemplates/agenttemplates_test.go`
  and `internal/agents/agents_test.go`.




- **Per-agent workspaces exist but are not used by tools.**
  `agents/<key>/workspace/` is created (`internal/agents/agents.go:105`) but fs/exec tools are
  registered against the global `cfg.WorkspaceDir` (`cmd/agenticgo/main.go:74`). Wire per-agent
  workspaces into the tool loop: construct fs/exec tools scoped to the agent's `WorkspaceDir`
  per run. This is the "per-agent tool jail" the README promises. This requires the per-run
  registry refactor below.

- **Per-run tool registry instead of the `callTool` switch.**
  `Engine.callTool` hardcodes a 3-case switch (`internal/agent/agent.go:304`) and per-agent
  memory tools are constructed per call, while built-ins live in one shared global registry.
  This does not scale: per-agent workspaces (above), per-agent tool allow-lists (below) and
  MCP tools (§3) all need per-run composition. Refactor: build a per-run `tools.Registry`
  (global built-ins + agent-scoped fs/exec + memory/docs tools + MCP tools), expose it via
  a single `Specs()`/`Call()` and drop the switch entirely. MCP tool-lifecycle note from the
  k8s-mcp-server analysis: failure of one source must not break the rest (soft-fail per tool).

- **Per-agent tool allow-list.**
  `cfg.ToolAllowList` is global-only (`internal/config/config.go:41`). With per-agent
  workspaces and MCP tools, agents need individual gating (the k8s-mcp-server "toolsets"
  idea, but per agent): add an allow-list field to the agent's `config.json` (nil = inherit
  global), and apply it in the per-run registry. Also consider a `read_only`-style mode per
  agent (no `write_file`/`exec`) — k8s-mcp-server's `--read-only` is the same concept.

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
  requests through `providers.Store.GetLLM("")`. After this lands, the `engine.llm` env-built
  fallback becomes redundant — fold its removal into this item (keep `llm.NewOpenAI` only as
  the seed for the providers store).

- **`AGENTICGO_KNOWLEDGE_FILE` / `cfg.KnowledgeFile` is dead config.**
  No longer referenced (`internal/config/config.go:33`); self-evolution writes to SQLite.
  Remove it or document it.

## 2. HUMA REST API (new — `docs` endpoint)

- **Add `POST /api/chat` (non-streaming REST chat) first.**
  Today chat is WebSocket-only (`/ws`), so the HUMA layer would have no chat endpoint to
  document. Add a thin REST endpoint that wraps `Engine.Run` (+ optional `Evolve`) and
  returns the final reply; keep `/ws` for streaming. This is what makes the API usable for
  curl/scripts/CI.

- **Add HUMA (github.com/danielgtaylor/huma/v2) and build a `/docs` API layer** covering the
  existing endpoints so there's a machine-readable API alongside the GUI — do NOT reimplement
  the GUI or remove the WebSocket chat.
  - Scaffold huma routers (API + docs) and register current routes: agents CRUD + config,
    context files, skills (+ upload), providers (+ test), sessions/messages, memory
    (knowledge/observations), knowledge-base docs, tools list, MCP + cron scaffolds, evolve,
    and the new `/api/chat`.
  - Decide the mount/versioning scheme (e.g. `/openapi.json`, `/api/v1/...`) — keep it
    harmonized with the existing chi router and the WS endpoint.
  - While redesigning routes, normalize the odd docs search path
    `/agents/{k}/docs/search/query` → `/agents/{k}/docs/search?q=...`.
  - Add examples/manual for the chat req/resp shapes if feasible.
  - This is the single "easy API" the project needs for non-UI clients. Priority after §1.

## 3. Scaffolded features (UI + API shape only — nothing executes)

- **MCP client (Phase 3) — make GUI-registered servers actually usable by agents.**
  Today `POST /api/mcp-servers` only writes to an in-memory map
  (`internal/scaffold/scaffold.go:60`); nothing connects, discovers, or registers tools, so
  the agent cannot use any registered server. The full chain:
  - **Persist config** (JSON file like providers, or new SQLite tables) so servers survive
    restarts. The in-memory scaffold store loses everything.
  - **Connect** with `github.com/modelcontextprotocol/go-sdk/mcp`: stdio transport (spawn
    `command`+`args`) and HTTP/SSE transport (`url`). Connect (or reconnect) when a server is
    added via the GUI/API, not only at startup.
  - **Discover tools**: call `tools/list`; each MCP tool already carries name, description
    and a JSON Schema — pass it through ~1:1 into `llm.ToolSpec` (no reflection/generics
    needed for MCP tools; that go-harness pattern is only for hand-written Go tools).
  - **Adapter**: wrap each discovered tool as a `tools.Tool` whose `Call(ctx, args)` forwards
    to the MCP client's `CallTool` and returns the text content; errors come back as strings
    the model can read.
  - **Merge into the per-run registry** (see §1) namespaced as `mcp_<server>_<tool>` and
    gated by the same allow-list (global + per-agent).
  - **Soft-fail**: an unreachable/dead server must not break chat — its tools return error
    strings; tool-list refresh on reconnect (tools can change between runs).
  - First dogfood target: `containers/kubernetes-mcp-server` via stdio (it is a plain MCP
    server; its own `--toolsets`/`--read-only` flags go into the server `args`, not into
    agenticgo).

- **Optional: generics-based tool registration for hand-written Go tools.**
  Borrow the `RegisterTool[T, R]` + `invopop/jsonschema` pattern from
  `go-harness-coding-agent/tools/registry.go` to generate `Parameters()` JSON Schemas from
  structs instead of hand-writing them. Keep agenticgo's `Call(ctx, json.RawMessage)`
  signature (harness's `map[string]any` dispatch has no context — do not regress
  cancellation). Pure ergonomics; do after the per-run registry refactor in §1.

- **Cron scheduler.** Jobs stored in-memory only (`internal/scaffold/scaffold.go:101`);
  `Enabled` is serialized but unused. Add a scheduler (e.g. robfig/cron or a simple ticker),
  persist jobs, and wire execution into `Engine.Run` per agent/session/prompt. Recurring agents
  should feed `record_observation` — the memory design anticipates this.

## 4. API / UI hardening gaps

- **Knowledge delete endpoint + UI button**: the Memory page is read-only today
  (no DELETE for knowledge entries). (Skill delete is done: `DELETE /api/skills/{key}`.)
- ~~**Skill enable/disable per agent**~~ **DONE**: skills now live in a global library
  (`data/skills/`, upload via `POST /api/skills/upload`) and are enabled per agent via
  `config.json` `enabled_skills` (`PUT/DELETE /api/agents/{k}/skills/{skill}`, Agents →
  Skills tab checkboxes). Legacy per-agent `skills/` dirs are migrated + kept enabled on
  first startup.
- ~~**Agent-scoped session listing**~~ **DONE**: `GET /api/sessions?agent=X` filters by
  agent (`store.ListSessions(ctx, agent)`, empty = all).
- **Observation creation in the UI**: `POST /api/agents/{k}/observations` exists; the Memory
  page has no control that calls it.
- **WebSocket hardening**: the SPA has no client-side reconnect/backoff when the socket drops,
  and no way to abort a running chat turn (a stuck tool loop burns tokens until
  `MaxAgentIterations`). Add reconnect in the UI and a cancel message that propagates
  `ctx` cancellation into `Engine.Run`.

## 5. Real-deployment items

- **Dockerfile** (multi-stage, static binary — the stack is already cgo-free via
  `modernc.org/sqlite`) and **Kubernetes manifests** (Deployment + PVC for the data dir +
  Service + optional Ingress). This is Phase 5 of the README roadmap.
- **Test coverage**: only `internal/store/memory_smoke_test.go` exists. Add tests for the agent
  loop, server handlers, providers, skills, tools, and llm request marshaling. If the generics
  registrar (§3) is adopted, cover schema generation + dispatch too (see
  `go-harness-coding-agent/tools/registry_test.go` for the table-driven pattern).

## 6. Housekeeping

- **Vision flag is per-provider, not per-model**: a provider marked `vision` assumes its
  model sees images. If you switch that provider's model to a non-vision one, untick the
  box or override per agent (Config → Vision). There is no reliable API to auto-detect
  vision support, so it stays manual.
- _Where things live (answers for on-boarding):_
  - Agent definitions + context files: `data/agents/<key>/*.md` (`AgentsDir`)
  - Per-agent LLM config + enabled skills: `data/agents/<key>/config.json`
  - Global skills library (upload once, enable per agent): `data/skills/<skill>/SKILL.md`
  - Legacy per-agent skills (auto-migrated into the library): `data/agents/<key>/skills/`
  - Per-agent workspace jail: `data/agents/<key>/workspace/` (currently unused — see §1)
  - Provider configs: `data/providers.json`
  - Messages / knowledge / observations / knowledge-base docs: `data/agenticgo.db` (SQLite)
  - Global fallback workspace: `data/workspace/`
- **Observation pruning only runs when TTL/keep-latest env vars are set**
  (`internal/agent/agent.go:155`); document or always prune.
- **FTS knowledge_docs backfill** in `migrate` is missing for old DBs
  (`internal/store/store.go:95`); harmless today but asymmetric with `knowledge_fts`.
