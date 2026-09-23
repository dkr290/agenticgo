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

- ~~**Agent memory-write tool + knowledge delete + core-tools visibility.**~~ **DONE**:
  agents can now persist durable learnings themselves via a new always-on `memory_save`
  tool (`tools.NewMemorySave` → `store.AddKnowledge`), complementing the background
  `Evolve` pass — the AGENTS.md template's "remember this → save in this turn" instruction
  is now backed by a real tool. `store.AddKnowledge` dedupes exact-match content per agent
  (`ErrKnowledgeDuplicate`, treated as a no-op) and caps entries at 500 bytes. Knowledge
  entries now carry stable IDs (`store.KnowledgeEntry{ID,Content,CreatedAt}`; `Knowledge()`
  returns entries), and wrong/outdated memories can be deleted from the Memory tab
  (`DELETE /api/agents/{k}/knowledge/{id}` → `store.DeleteKnowledge`, agent-scoped, 404 when
  unknown). Fixed a latent bug: the FTS5 "special delete" triggers (`knowledge_ad`,
  `kdocs_ad`) errored in this build — they now delete the FTS row by `rowid` (recreated
  idempotently in `migrate`, which also fixes the pre-existing broken document delete).
  The always-on memory/knowledge tools (`memory_search`, `memory_save`, `record_observation`,
  `search_docs`, `read_doc`) are listed read-only as **Core agent tools** on the Built-in
  Tools page via `GET /api/tools/core` (single source of truth: `tools.CoreTools()`); they
  cannot be enabled/disabled/removed. The AGENTS.md template was trimmed to the features
  agenticgo actually has (single-user chat; cron is scaffolded) — no group-chat/NO_REPLY/
  scheduling instructions. Covered by `internal/store/knowledge_test.go`,
  `internal/server/knowledge_test.go`, `internal/tools/memory_test.go`, and
  `internal/agent/agent_test.go`.




- ~~**Per-agent workspaces exist but are not used by tools.**~~ **DONE**: built-in tools
  (`read_file`, `write_file`, `list_files`, `exec`) are now constructed per-run and jailed
  to each agent's own workspace (`AgentsDir/<key>/workspace/`) in `Engine.callTool`, the
  same way extra dangerous exec commands already worked. Falls back to the global workspace
  if the agent workspace resolution fails. (See PR #1)

- ~~**Per-run tool registry instead of the `callTool` switch.**~~ **DONE**:
  `Engine.runRegistry` now builds a per-run `tools.Registry` for every run — the
  always-on core memory/docs tools, the workspace-jailed fs/exec tools (exec's
  allow-list = global `AGENTICGO_EXEC_ALLOWLIST` + the agent's enabled extra
  commands, named in its description so the model can see them), and the agent's
  enabled MCP tools via a `tools.Tool` adapter (soft-fail: call errors return as
  strings the model can read). `Run` uses the registry's `Specs()` for the LLM
  request and `Call()` for dispatch, so specs and execution share one source of
  truth; the three-block `callTool`/`callExec`/`mcpToolSpecs` switch is gone, and
  per-agent tools are built once per run instead of per call. Gating is enforced
  by construction: a tool absent from the registry is invisible to the model and
  rejected by `Registry.Call`.

- ~~**Per-agent tool allow-list.**~~ **DONE** (scoped to built-ins; skills, MCP
  tools and extra exec commands were already per-agent): `config.json`
  `enabled_builtin_tools` narrows which built-in tools (`read_file`, `write_file`,
  `list_files`, `exec`) an agent gets — nil = inherit the global
  `AGENTICGO_TOOL_ALLOWLIST` (the common case), a list = exactly those, `[]` = no
  built-ins at all. Narrowing can only remove tools, never exceed the global
  ceiling, and the core memory/docs tools are always on. Wire-up: applied in
  `runRegistry`; API `GET/PUT/DELETE /api/agents/{k}/builtin-tools[/{name}]`
  (DELETE without a name resets to inherit); Agents → Built-in Tools tab with
  per-tool checkboxes, global-ceiling indication and a reset button. This also
  subsumes the per-agent `read_only` idea — `["read_file", "list_files"]` *is*
  read-only mode.

- ~~**Conversation history loses the tool trace.**~~ **DONE**: `messages` table now persists
  tool-call metadata — added `tool_calls` (JSON), `tool_call_id`, and `name` columns via
  idempotent migration. `Engine.Run` persists assistant turns with tool calls and tool result
  messages (`role=tool`). `Engine.Evolve` formats tool-call turns in the transcript. The UI
  `loadConversationMessages` renders tool-call and tool-result messages when loading history,
  matching the real-time WebSocket rendering pattern. `Store.Close()` method added (was
  missing).

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

- ~~**MCP client (Phase 3)**~~ **DONE** (see the README roadmap): `internal/mcp`
  persists servers to `data/mcp_servers.json`, connects manually (stdio + HTTP via
  `modelcontextprotocol/go-sdk`), discovers tools on Connect, namespaces them
  `mcp_<server>_<tool>`, and merges them into the per-run registry via a
  `tools.Tool` adapter gated by the agent's `enabled_tools`. Soft-fail holds: a
  dead/unreachable server's calls return error strings the model can read, and a
  configured-but-disconnected server is lazily reconnected on first call. First
  dogfood target remains `containers/kubernetes-mcp-server` via stdio.

- **Optional: generics-based tool registration for hand-written Go tools.**
  Borrow the `RegisterTool[T, R]` + `invopop/jsonschema` pattern from
  `go-harness-coding-agent/tools/registry.go` to generate `Parameters()` JSON Schemas from
  structs instead of hand-writing them. Keep agenticgo's `Call(ctx, json.RawMessage)`
  signature (harness's `map[string]any` dispatch has no context — do not regress
  cancellation). Pure ergonomics; do after the per-run registry refactor in §1.

- **Cron scheduler — currently a non-functional placeholder.** The Cron UI/API only stores job
  definitions in memory (`internal/scaffold/scaffold.go`); **nothing executes them** (no
  scheduler/ticker exists), `Enabled` is serialized but unused, and jobs are **lost on restart**
  (not persisted). Today only a human can create jobs via the UI — there is intentionally **no
  `cron` agent tool**, which is why the AGENTS.md template omits any scheduling instructions.
  To make it real: add a scheduler (e.g. robfig/cron or a simple ticker), persist jobs
  (SQLite/JSON), and wire execution into `Engine.Run` per agent/session/prompt. Recurring agents
  should feed `record_observation` — the memory design anticipates this. **Open decision:** whether
  agents may also self-create/manage jobs via a `cron` tool (some systems allow it), and if so how
  it is gated; if added, re-add a Scheduling section to the AGENTS.md template.

## 4. API / UI hardening gaps

- ~~**Knowledge delete endpoint + UI button**~~ **DONE**: `DELETE /api/agents/{k}/knowledge/{id}`
  + a per-row ✕ on the Memory page (see §1 memory-write item above). (Skill delete is done:
  `DELETE /api/skills/{key}`.)
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
