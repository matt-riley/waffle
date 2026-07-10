# Run Observability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add loopback-only run status, durable provider-neutral token accounting, runtime totals, and a `waffle status` CLI for issue #55.

**Architecture:** An observability service owns active in-memory runs and persisted `run_metrics` completions. Agent hooks report cumulative usage to the service; it writes only deltas. Serve injects the service into gateway and cron execution and serves its snapshot via a local HTTP listener; status renders that JSON.

**Tech Stack:** Go, SQLite migrations, `net/http`, Go standard testing.

## Global Constraints

- Bind the unauthenticated status API only to loopback; default `127.0.0.1:8422`.
- Snapshot JSON always includes `active`, `recent`, and `retry_queue`; retry queue is empty until #52.
- Usage accounting consumes provider-neutral `llm.Usage`; no Anthropic-only fields.
- Duplicate cumulative usage observations add zero token delta.
- Runtime is ended duration plus active elapsed time evaluated at snapshot time.
- No dashboard, remote access, authentication, or retry mechanism.

---

### Task 1: Add durable run metric accounting

**Files:**

- Create: `internal/store/migrations/0008_run_metrics.sql`
- Create: `internal/observability/observability.go`
- Create: `internal/observability/observability_test.go`

- [ ] Write failing tests for duplicate cumulative usage, completed runtime, active elapsed runtime, and an empty snapshot using a fixed clock.
- [ ] Run `go test ./internal/observability -count=1`; expect failure because the package does not exist.
- [ ] Add the migration and service API: start a run, record cumulative `llm.Usage`, finish a run, and produce a snapshot with active/recent/retry fields.
- [ ] Run `go test ./internal/observability -count=1`; expect pass.

### Task 2: Instrument agent, gateway, and scheduler runs

**Files:**

- Modify: `internal/agent/agent.go`
- Modify: `internal/agent/agent_test.go`
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/gateway_test.go`
- Modify: `internal/schedule/schedule.go`
- Modify: `internal/schedule/schedule_test.go`

- [ ] Write failing tests proving an agent hook sees cumulative provider-neutral usage and gateway/cron runs add `session_id`/`job_id` to structured logs.
- [ ] Run focused package tests; expect failure because no usage hook or run context exists.
- [ ] Add `Hooks.OnUsage`, invoke it after every successful provider response with cumulative totals, and start/finish observability runs around gateway and cron execution.
- [ ] Run focused agent, gateway, and schedule tests; expect pass.

### Task 3: Add the local HTTP snapshot server

**Files:**

- Create: `internal/observability/http.go`
- Modify: `internal/observability/observability_test.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `cmd/waffle/serve_cmd.go`

- [ ] Write failing tests for `/status` JSON, empty arrays, and rejection of a non-loopback listen address.
- [ ] Run focused observability/config tests; expect failure because no handler/listener validation exists.
- [ ] Add the handler, loopback validation, default config, and serve lifecycle wiring.
- [ ] Run focused tests; expect pass.

### Task 4: Add `waffle status` and documentation

**Files:**

- Create: `cmd/waffle/status_cmd.go`
- Create: `cmd/waffle/status_cmd_test.go`
- Modify: `cmd/waffle/main.go`
- Modify: `README.md`

- [ ] Write failing tests that a stub JSON snapshot renders active and recent run totals in readable text and reports an unavailable server clearly.
- [ ] Run `go test ./cmd/waffle -run TestStatus -count=1`; expect failure because the command does not exist.
- [ ] Add the command and document the status endpoint/config semantics.
- [ ] Run `go test ./cmd/waffle -run TestStatus -count=1`; expect pass.

### Task 5: Verify and close #55

**Files:**

- Modify: only formatting or docs corrections required by verification.

- [ ] Run `gofmt -w` over changed Go files, `git diff --check`, `go test ./...`, and `go vet ./...`.
- [ ] Reconcile #55 acceptance criteria, including OpenAI-compatible `llm.Usage` handling, then close #55 only with passing evidence.
