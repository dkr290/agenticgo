# HEARTBEAT.md - Recurring Checks

This file is included in every run's system prompt. Apply this checklist when
the current task asks for a recurring check or monitoring; ordinary conversation
uses the general memory guidance in AGENTS.md. The cron job supplies the task and
schedule — this file does not start runs by itself.

## Monitoring checklist

1. **Inspect live state** using the enabled tools for the requested scope. Record
   what was checked and any failed or unavailable checks; an unsuccessful check
   does not establish that the system is healthy.
2. **Compare with available observations** for the same scope. The prompt may
   include the latest observation or several recent ones, depending on settings.
   If none are available, establish a baseline without inventing prior state.
3. **Save one consolidated snapshot** with `record_observation` after the check.
   Include the scope, important findings, changes, and unresolved issues. The
   store timestamps the entry and applies the configured retention policy.
4. **Save useful lessons selectively** with `memory_save` when current evidence
   establishes a new, reusable fact or lesson. Search existing knowledge first.
   Temporary status, restart counts, and a single observed connection belong in
   the snapshot; a verified ownership or deployment convention can be knowledge.
   A run with no new durable lesson needs no new knowledge entry.
5. **Report the result** briefly, emphasizing changes, ongoing actionable issues,
   and failed checks. If nothing changed and no issue needs attention, say so
   concisely. Confirm memory was saved only after its tool call succeeds.
