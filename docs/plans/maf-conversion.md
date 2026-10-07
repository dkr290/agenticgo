# Plan: Convert agenticgo to microsoft/agent-framework-go (MAF)

## Goal

Replace the hand-rolled LLM/tool-call plumbing (`internal/llm`, `internal/tools`, the tool
loop inside `internal/agent`) with `github.com/microsoft/agent-framework-go`, while keeping
everything that makes agenticgo itself: agents registry + context files, skills, SQLite
memory/FTS5/retention, cron, MCP manager, Huma REST + WS + SPA, bubblewrap jail, encrypted
providers.json. Also gain a first-class Azure Foundry provider path.

## Decision (from discussion)

- Auth for Foundry: **API key**.
- Scope: **full conversion** of the loop to MAF (not the minimal "Foundry-as-OpenAI-endpoint"
  option, which remains the fallback if the conversion stalls).

## Key API facts (verified against MAF v0.1.0)

- `tool.FuncTool` interface: `Name/Description` + `Schema() any` + `ReturnSchema() any` +
  `Call(ctx, args string) (any, error)`. Our `tools.Tool` (`Name/Description/Parameters/Call`
  with `json.RawMessage`) adapts 1:1 via one small wrapper — **tools do not need to be
  rewritten as functool typed funcs**. `mcptool` package exists for MCP.
- `openaiprovider.NewChatCompletionsAgent(openai.Client, AgentConfig)` — works with any
  OpenAI-compatible base URL (Ollama/LM Studio/vLLM/Foundry's OpenAI-compatible endpoint).
  `AgentConfig.ToolAutoCall *toolautocall.Config` controls the automatic function-calling
  loop (iteration cap lives here).
- `foundryprovider.NewAgent(endpoint, azcore.TokenCredential, ModelDeployment|ServerAgent,
  AgentConfig)` — Foundry-native path, **but it only accepts a TokenCredential**.
- `agent.HistoryProvider` interface (`Provide`/`Store` hooks) — our SQLite `store.Store`
  implements this, so history persistence, tool-call metadata, and window-truncation repair
  stay ours. `agent.Config.HistoryProvider` wires it in.
- `agent.MessageInjector` — can replace the `imageSink` drain-and-inject mechanism for
  `fetch_agent_image` (enqueue the image message between provider calls).
- `agent.Middleware` (`MiddlewareFunc`) — where per-run dispatch gating can be re-asserted
  if needed, and where WS event emission hooks in.
- `message.Message` is a content-parts model (text, images, tool calls as typed parts) —
  richer than our flat `llm.Message`; most conversion effort is translating at this boundary.
- Cancellation is plain `context.Context` through `Agent.Run`; `ResponseStream` is an
  `iter.Seq2[*ResponseUpdate, error]`.

## Foundry = the same openaiprovider (confirmed)

There is no separate Foundry SDK path in this conversion. Following MAF's own OpenAI
chat-completion example, everything goes through
`openaiprovider.NewChatCompletionsAgent(openai.NewClient(option.WithBaseURL(...),
option.WithAPIKey(...)), AgentConfig{Model: ...})`. One provider constructor covers all
agenticgo provider types:

- Ollama / LM Studio / vLLM / OpenAI — base_url + key, as today.
- **Azure Foundry** — the Foundry project's OpenAI-compatible endpoint + API key, with the
  deployment name as the model. No `foundryprovider`, no `azidentity`, no TokenCredential
  (that package only accepts TokenCredential and targets server-side agents / Foundry
  memory, which agenticgo deliberately keeps local — out of scope entirely).

`providers.Provider.Type` gains `"azure-foundry"` purely as a UI/validation hint (endpoint
shape, deployment naming); the runtime path is identical to any other OpenAI-compatible
provider.

## Work items (in dependency order)

### Phase 1 — Provider & message bridge (foundation, ~1.5d)

1. **New `internal/maf` package** — all MAF-facing code isolated here:
   - `bridge.go`: translation between `store.Message`/`llm.Message` and `message.Message`
     (text, tool calls w/ IDs, tool results, image data-URL parts). Handles the
     window-truncation/incomplete-exchange repair currently in `replayHistory` (move that
     logic to operate before translation; keep its tests).
   - `history.go`: `HistoryProvider` implementation over `store.Store` (Provide = load last
     N + repair; Store = append with tool-call metadata, matching current schema so no DB
     migration).
   - `tools.go`: `funcToolAdapter` wrapping our `tools.Tool` as MAF `tool.FuncTool`
     (schema passthrough, `json.RawMessage`→string, string result). Registry.Call stays the
     dispatch gate by construction: only enabled tools are wrapped and handed to the agent.
2. **`internal/providers`**: `Provider.Type` gains `"azure-foundry"` (UI label + validation);
   resolution constructs the right MAF-backed client. API keys stay encrypted at rest
   unchanged. TestConnection for Foundry = chat-completions models endpoint or a 1-token
   completion fallback.

### Phase 2 — Engine rewrite (~2d, the risky core)

3. **`internal/agent/agent.go` — `Run` rewritten:**
   - Keep: `lockConversation`, provider/vision resolution precedence, retention pruning,
     `runRegistry` composition (it now produces `[]tool.Tool` via adapters instead of a
     `tools.Registry`, same gating logic), event emission contract (`text`/`tool_call`/
     `tool_result`/`error`/`done` + `cancelled`).
   - Replace the manual iteration loop with: build per-run MAF agent
     (`openaiprovider.NewChatCompletionsAgent` with per-request client, `Instructions` =
     composed system prompt, `ToolAutoCall` cap = `MaxAgentIterations`, `HistoryProvider` =
     ours, `MessageInjector` for the image sink), then consume the `ResponseStream`, mapping
     updates to `Event`s. Tool-call/tool-result events are emitted from a thin
     middleware/adapter wrapper around each tool's `Call`.
   - **Do not use MAF `Session`s** for history ownership: history stays in SQLite via our
     HistoryProvider; MAF sessions would add a second source of truth. (Verify at spike time
     that HistoryProvider works without `WithSession`; if MAF requires a session, use an
     ephemeral per-run session with our provider authoritative.)
   - Cancel: `kind:cancel` cancels the run context; MAF stream observes ctx. Terminal
     `cancelled` event preserved.
   - Vision: images become content parts on the outgoing user message; non-vision drop +
     `fetch_agent_image` guard preserved; sink → `MessageInjector.EnqueueMessages`.
   - `Evolve`: single non-streaming MAF run with `Stream(false)`, same extraction prompt.
4. **Delete**: `internal/llm/openai.go` (and `openai.go.bak`), `internal/tools/tools.go`
   registry type (keep the `Tool` interface — it's the adapter target), the manual loop.
   `llm.Message`/`ToolSpec`/`Delta` shrink to what the bridge needs, or move into
   `internal/maf` and disappear from the rest of the app.

### Phase 3 — Integration & cleanup (~1d)

5. **`internal/server`**: no API shape changes; WS handler keeps emitting the same events.
   Adjust construction wiring in `cmd/agenticgo/main.go`.
6. **UI**: Providers page gains the `azure-foundry` type option (endpoint + key +
   deployment). No other UI change.
7. **Tests**: port existing loop/registry/replay tests to the bridge; add adapter tests
   (tool gating enforced at dispatch — a disabled tool is never wrapped); providers tests
   for the new type; keep `node --test` SPA tests green (no event-shape changes).
8. **Docs**: AGENTS.md architecture map, tech choices (MAF added; openai-go still the
   underlying client), data layout unchanged.

## What stays 100% untouched

agents registry/templates, skills, store schema (FTS5, retention), cron, MCP manager
(we adapt its tools, not replace with mcptool initially), crypto, logger, Huma routes,
SPA pages, Docker/bubblewrap security model.

## Risks & mitigations

- **Preview API churn (v0.1.0)**: isolate behind `internal/maf`; version-pin; the rest of
  the app only sees our own types.
- **HistoryProvider/session semantics mismatch**: spike first (Phase 1, day 1) with a
  100-line throwaway main that runs a tool-calling chat against Ollama with our
  HistoryProvider — proves the seam before the engine is touched.
- **Streaming event granularity**: MAF updates may batch differently than our per-delta
  events; the WS contract tolerates any chunking of `text`, so risk is limited to
  cosmetic timing.
- **Tool-call repair edge cases**: keep `replayHistory` logic + tests verbatim, just
  relocated — this is the subtlest correctness invariant and must not be re-derived.

## Estimate

~4–5 days total: 1.5 bridge/providers, 2 engine, 1 tests/docs — plus a 2–3 hour spike
before committing (validates HistoryProvider + ToolAutoCall + streaming against a real
model). Rollback story is clean: the conversion touches no stored data, so reverting is a
code revert only.

## Out of scope (explicitly not adopted now)

MAF workflows/multi-agent, MAF skills package (ours stays), `foundryprovider` (including
Foundry MemoryProvider, server-side Foundry agents, TokenCredential/Entra-ID auth),
mcptool replacing our MCP manager, OTel middleware.
