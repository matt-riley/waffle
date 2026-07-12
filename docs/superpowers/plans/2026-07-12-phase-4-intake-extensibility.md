# Phase 4 Intake & Extensibility Implementation Plan

**Goal:** Complete Phase 4 from meta-issue #72: #51 issue intake, #53 repo policy, #54 lifecycle hooks, #41 extension-surface decision.

**Architecture:** Compose existing serve owner loop, workspace containers, agent groups, and schedule delivery. Issue intake is a serve-owned poller with SQLite claims. Repo policy and hooks are tighten-only / sandbox-only.

## Delivered

- [x] #41 — tier map + decision checklist in `docs/plan.md` (embedded runtime deferred)
- [x] #53 — `internal/repopolicy` with WAFFLE.md/AGENT.md, tighten-only tools/egress/idle, untrusted prompt marker
- [x] #54 — `internal/hooks` + workspace Open/Close/RunHook wiring; fatal vs best-effort semantics
- [x] #51 — `internal/intake` GitHub tracker, claims, concurrency, reconcile, delivery; serve wiring on `GroupIssue`
- [x] Config: `[[intake.github]]`, `[workspace.hooks]`; migration `0012_issue_claims.sql`
- [x] Tests for policy, hooks, intake, agent issue tier, config validation

## Config sketch

```toml
[[intake.github]]
repo = "owner/name"
label = "agent-ok"
max_concurrency = 1
deliver = "telegram:123"
poll_interval = "1m"

[workspace.hooks]
after_create = "go mod download"
before_run = "git fetch --all"
after_run = "git status"
before_remove = "./scripts/export.sh"
timeout = "5m"
```

Repo root `WAFFLE.md` (or `AGENT.md`) may declare the same hooks and a tighten-only tool filter.
