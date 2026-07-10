# Run Observability Design

## Goal

Implement issue #55's local run-accounting and status foundation so operators can inspect active and recently completed gateway or cron runs, and #43 has durable token measurements to enforce.

## Architecture

Create an `internal/observability` service backed by a `run_metrics` SQLite table. A run is linked to a session and has its own ID, source (`gateway` or `cron`), phase, outcome, start/end time, and token totals. The service keeps active runs in memory and persists their final state; snapshot construction combines persisted recent completions with active elapsed time using an injected clock.

Agent hooks emit cumulative token usage for a run after each provider response. The service tracks the latest observed cumulative usage for that run and persists only positive deltas, so an identical re-streamed observation cannot double-count tokens. Usage is provider-neutral because it consumes `llm.Usage`.

`serve` owns the service and exposes its JSON snapshot from a loopback-only listener. The retry queue has a stable empty-array field until #52 supplies queue state. `waffle status` fetches the endpoint and renders the active and recent runs in terminal-readable text.

## API

`GET /status` returns:

```json
{
  "active": [{"id":"run_…","session_id":"…","source":"gateway","phase":"agent","elapsed_ms":12,"input_tokens":3,"output_tokens":4}],
  "recent": [{"id":"run_…","session_id":"…","source":"cron","outcome":"ok","runtime_ms":1200,"input_tokens":3,"output_tokens":4}],
  "retry_queue": []
}
```

The listener uses `127.0.0.1:8422` by default, with an explicit `[gateway] status_listen` override. It rejects non-loopback addresses during configuration so an operator cannot accidentally make this unauthenticated surface remote.

## Boundaries

- No dashboard, remote access, authentication, or retry implementation.
- No historical analytics beyond recent persisted completions.
- #50 may add `/healthz` to the same listener later.
- Logs for gateway and cron runs receive `session_id`; `job_id` is added on cron execution.

## Tests

Test migration-backed delta persistence, clock-driven active plus ended runtime totals, empty snapshots, HTTP JSON and loopback validation, CLI rendering of a stubbed snapshot, and captured structured log attributes.
