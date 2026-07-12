# Phase 3 Lifecycle and Operations Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Complete the remaining open Phase 3 issues from #72: #36 workspace lifecycle, #47 data lifecycle, #50 daemon health/deployment, and #40 GitHub App credentials, while verifying the already-landed #46 and #52 work.

**Architecture:** Extend the existing SQLite-backed stores and `serve` owner loop. Workspace and retention sweeps receive injected clocks and notification interfaces for deterministic tests; only `serve` starts them. `/healthz` joins the existing loopback status listener and shares its `http.Server`. Git credentials remain behind `broker.GitCredentialFunc`, with a GitHub App implementation in a focused package and PAT fallback in the command wiring.

**Tech Stack:** Go 1.25, SQLite migrations, `net/http`, `crypto/rsa`, `github.com/BurntSushi/toml`, Docker CLI, Go unit tests and `httptest`.

## Global Constraints

- Preserve loopback-only admin access and single-owner behavior under `waffle serve`.
- Auto-close must never force-delete dirty or unpushed work.
- Retention defaults to forever; deletion does not affect provider logs, delivered messages, or old backups.
- GitHub App private keys are secret references; minted tokens are repo-scoped and cached only until shortly before expiry.
- Every production behavior is introduced with a failing test first and verified with focused plus full Go tests.

### Task 1: Verify existing #46 and #52 implementations

**Files:** `internal/sandbox/*`, `internal/workspace/runtime*`, `internal/schedule/*`, `internal/config/*`, `README.md`

- [ ] Run focused tests for Docker resource args, config validation, scheduler retries/stalls, and `go test ./...`; record any regression before new work.
- [ ] Compare implementation and tests against every acceptance criterion in issues #46 and #52; add only missing coverage or behavior.

### Task 2: Add shared lifecycle configuration and migrations

**Files:** Create `internal/store/migrations/0011_lifecycle.sql`; modify `internal/config/config.go`, `internal/config/config_test.go`; test `internal/store/store_test.go`.

- [ ] Add `workspaces.last_active` and `jobs` retry fields already required by current schema as needed, plus defaults compatible with existing rows.
- [ ] Add `[workspace] idle_timeout`, `[workspace] close_ttl`, and `[store] retain`, parsing positive durations or `0` and documenting defaults.
- [ ] Add tests for migration columns, config defaults, valid durations, zero disablement, and invalid durations.

### Task 3: Implement workspace activity and serve-owned lifecycle sweeps

**Files:** Modify `internal/workspace/workspace.go`, `internal/workspace/workspace_test.go`, `cmd/waffle/serve_cmd.go`; create `internal/workspace/reaper.go` and tests.

- [ ] Add `Touch` and update it around workspace execution/resume/open activity.
- [ ] Add a `Reaper` with injected `now`, `Idle`, `Close`, and notification dependencies; idle only stale open workspaces, close only clean TTL-expired workspaces, and notify dirty/unpushed ones.
- [ ] Start exactly one reaper from `serve`; do not start it from CLI-only commands.
- [ ] Test stale/recent behavior, clean close, dirty notification, disabled timers, and auto-idled resume.

### Task 4: Implement session deletion, forget, retention, and vacuum

**Files:** Modify `internal/session/session.go`, `internal/memory/memory.go`, `cmd/waffle/session_cmd.go`, `cmd/waffle/main.go`, `cmd/waffle/serve_cmd.go`; create `internal/session/lifecycle_test.go`; modify `README.md`.

- [ ] Add transactional session deletion with cascade, summary removal through the session row, FTS integrity verification, and incremental vacuum.
- [ ] Add search-hit deletion and a `forget` flow that reports matching turns and matching `MEMORY.md` lines, requiring explicit confirmation for each selected source.
- [ ] Add retention sweep with injected clock, single-owner serve wiring, and concurrency-safe transactions.
- [ ] Add destructive command confirmation and clear unknown-session errors.
- [ ] Document live-store-only deletion and provider/channel/backup non-coverage.

### Task 5: Share the admin listener and add `/healthz`

**Files:** Modify `internal/observability/http.go`, `internal/observability/observability.go`, `cmd/waffle/serve_cmd.go`; create health tests; create `docs/deploy.md`.

- [ ] Add health state for database reachability, scheduler last tick, and adapter last-success/staleness.
- [ ] Serve `/healthz` and `/status` from one listener and one HTTP server; return JSON and 503 when stale/unhealthy.
- [ ] Assert loopback binding and shared listener behavior in tests.
- [ ] Add tested systemd and launchd examples, restart/log/hardening directives, `WAFFLE_AGE_IDENTITY` sourcing, curl shape, and liveness-vs-introspection documentation.

### Task 6: Implement GitHub App credentials

**Files:** Create `internal/gitcred/app.go`, `internal/gitcred/app_test.go`; modify `internal/config/config.go`, `internal/broker/broker.go`, `cmd/waffle/ws_cmd.go`, related tests/docs.

- [ ] Add `[github.app]` config with secret reference, app and installation IDs, permissions, and optional base URL.
- [ ] Build and sign RS256 JWTs with bounded `iat`/`exp`; request exactly one repo and least-privilege permissions from a fake API server.
- [ ] Cache per installation/repo until five minutes before expiry and remint after expiry; fail clearly on API/network errors.
- [ ] Keep PAT fallback when App config is absent; distinguish backend and repo scope in audit rows.
- [ ] Ensure repo scope validation happens before minting and document least-privilege app registration.

### Task 7: Full verification and completion audit

**Files:** No new production files.

- [ ] Run `gofmt`, focused tests for each issue, `go test ./...`, `go vet ./...`, and `go test -race ./...`.
- [ ] Re-read all six Phase 3 issue acceptance lists and map each criterion to current code, tests, docs, or runtime evidence.
- [ ] Confirm issue #46 and #52 remain complete and no unrelated worktree changes were overwritten.
