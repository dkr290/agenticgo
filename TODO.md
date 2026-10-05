# TODO

## Remaining roadmap

- **Kubernetes packaging:** manifests for Deployment, data PVC, Service, and
  optional Ingress. Run one instance per data directory and account for the
  container runtime's bubblewrap/user-namespace policy when using standard exec.
- **Optional OpenAI-compatible gateway API:** `/v1/chat/completions` and `/v1/models`
  need decisions about model-to-agent mapping, sessions, and streaming.
- **Optional observation entry UI:** the POST API exists; the Memory page displays
  observations recorded by agents but has no manual-entry form.
- **Optional typed tool registration:** generate tool schemas from Go types if
  maintaining hand-written schemas becomes cumbersome.
- **Expand integration coverage:** real provider compatibility, HTTP MCP reconnects,
  multipart/ZIP uploads, and browser rendering. Current regressions exercise Go
  runtime paths and SPA events; use `go test -race ./...` and
  `node --test internal/server/web_test.cjs`.

## Project-review fixes

- Config-tab saves preserve skills/tools/commands and built-in restrictions.
- Configuration mutations serialize read-modify-write and use atomic file replacement.
  Invalid agent configs fail explicitly rather than silently widening permissions.
- Tool history restores call metadata and discards incomplete exchanges.
- MCP stdio servers survive the Connect request; failed/missing connections are
  handled safely, and tool discovery consumes all pages.
- Observation TTL=0 disables age expiry independently of the keep count.
- Knowledge injection=0 keeps memory searchable without adding it to the prompt.
- Filesystem tools enforce symlink-safe containment via `os.Root`; standard exec
  uses bubblewrap, blocks `find` subprocess actions, and bounds captured output.
- Enabled dangerous extras remain directly executable inside the deployment container.
- Fetched images respect effective vision capability; initial attachment state refreshes.
- Document reads/deletes enforce agent ownership.
- Legacy migrations add agent columns before indexes and backfill both FTS tables.
- WS Stop/disconnect cancels runs; reconnection restores the UI and uses backoff.
- UI escapes attribute quotes, displays Huma error details, and correctly handles
  provider saves and conversation selection.
- `list_files` accepts an omitted/empty root path and rejects malformed arguments.
- Malformed master-key files are never silently replaced.
- Provider JSON writes are atomic, with in-memory rollback on persistence failure.
- Cron skips overlapping job runs and cancels/waits during shutdown.

## Implemented features

Multiple agents and per-agent workspaces; embedded initial context-file templates;
shared skills library with explicit per-agent enablement; scoped knowledge/docs
recall and durable memory tools; configurable retention and background sweeping;
MCP discovery and per-agent tool gates; persistent executable cron jobs; encrypted
provider keys; per-agent LLM/vision settings; conversation history with tool traces;
Huma-generated REST/OpenAPI docs and synchronous `/api/chat`.
The multi-stage Dockerfile builds a static binary and runs it as UID/GID 10001
with tini, health checks, bubblewrap, and persistent `/data`; README covers image
extensions for dangerous commands and runtime namespace requirements.

## Retention defaults

| Store | TTL days | Keep latest | Meaning |
|---|---:|---:|---|
| Observations | 14 | 200 | Age and count pruning enabled |
| Curated knowledge | 0 | 0 | No automatic pruning |

Each zero disables only its own limit. Both zero disables pruning for that store.
Uploaded knowledge-base documents are never retention-pruned.
