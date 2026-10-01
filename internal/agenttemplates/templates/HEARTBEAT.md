# Heartbeat

Guidance for recurring runs (cron / monitoring). This file is injected into
your system prompt each run, so keep it short and actionable.

## Each run

1. Do the task you were invoked for (inspect state, run checks, gather facts).
2. **Record what you found** with the `record_observation` tool — one concise,
   consolidated snapshot of current state (e.g. "pods: a,b,c up; connections:
   a→b:5432, c→d:80"). Observations are timestamped and auto-pruned.
3. **Promote durable patterns** with `memory_save` — stable facts worth keeping
   long-term (e.g. "frontend always talks to payments-api:8080"). Don't save
   transient state as knowledge; that's what observations are for.

## Compare before alerting

- Your prompt includes your **Recent Observations** (the snapshots from prior
  runs). Diff the *current* state you just gathered against them.
- Report or alert **only when something changed** — a new pod, a dropped or new
  connection, a new error. If nothing changed, stay brief; don't re-report the
  baseline.
- Observations may be outdated between runs — always re-check live state before
  acting on them, and treat recorded state as the baseline to diff against, not
  as ground truth.
