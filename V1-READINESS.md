# Waffle v1 readiness

This document is the canonical v1 contract. Feature work is frozen against
it: nothing merges that is not required by a blocker below or by the
release gate. The audit that produced this contract reviewed the tree at
`v0.31.0` (2026-08-21).

## Locked decisions

1. **Platforms.** Published artifacts target Linux amd64 and Linux arm64.
   macOS is source-install and best-effort (`waffle chat` direct mode);
   there are no macOS binaries. Docker sandbox/workspaces on macOS require
   an explicitly built Linux runner (`[sandbox] runner_binary`).
2. **Git credentials.** The GitHub App is the documented v1 path for
   workspace git credentials. The PAT fallback continues to work for
   existing installations; new installations must opt in explicitly, and
   `waffle doctor` warns whenever the PAT backend is active.
3. **Restore and identity.** `waffle restore` never installs a bundled
   identity into the OS keychain. Recovery on a fresh machine is:
   restore the backup, then `waffle secret import-identity` explicitly.
4. **Feature freeze.** No Desk or agent feature PRs merge until v1.0.0
   ships. Blocker fixes and gate work only.
5. **Docker verification.** Docker-gated tests run in CI on every PR
   (Linux runners have Docker) and locally once Docker Desktop is
   available. A Docker-gated skip is never accepted as release evidence.
6. **Release shape.** `v1.0.0` is tagged when every blocker below is closed
   and the gate is green. There is no date commitment.

## Support matrix

| Capability | Linux amd64 | Linux arm64 | macOS |
| --- | --- | --- | --- |
| `waffle chat` (direct) | supported | supported | best effort |
| `waffle serve` (managed host) | supported | supported | best effort |
| Docker sandbox / workspaces | supported | supported | Linux runner required |
| Published artifact | yes | yes | no (build from source) |

## Security contract

Guaranteed: deny-by-default sandbox, network, tool, and secret policy;
secrets only in the host store with `secret://` references in config;
broker-scoped session tokens; repo-scoped git credentials; loopback-only
admin listener.

Non-guarantees stated plainly: the owner's own shell has full access to
`$WAFFLE_HOME`; `full` egress is unrestricted by choice; a workspace git
credential is necessarily present in the workspace's git process; deletion
never reaches provider logs, delivered messages, or existing backups.

## Compatibility policy

- `waffle setup` -> `chat`, and `serve` + pairing, stay accurate as
  documented in README.md.
- `waffle upgrade` / `waffle rollback` remain the install contract.
- Breaking config or schema changes are called out in CHANGELOG.md; after
  v1 they require a major version.
- Deny-by-default posture never loosens in a patch or minor release.

## Release gate

1. `mise run v1-check` — fmt, vet, lint, generated-code cleanliness, Desk
   client tests, `go test -race ./...`, zero-network evals, Desk browser
   gate, website build/tests, brand validation, Linux artifact
   reproducibility, govulncheck.
2. Docker-gated suites on a real daemon: `sandbox_docker`,
   `sandbox_stress`, workspace egress/netlock integration.
3. Manual smoke matrix on the release candidate: fresh setup, provider
   enrollment, terminal chat, Telegram pairing and delivery, Desk flows,
   scheduled jobs, workspace open/edit/push/idle/close, backup ->
   restore with identity import, upgrade -> rollback, `waffle doctor`
   after each transition.
4. Evidence protocol: every blocker closes with the exact commit, command,
   and result recorded here or in the closing PR.

## Blockers

| # | Blocker | Status |
| --- | --- | --- |
| 1 | Workspace egress lockdown is reversible in-container (`CAP_NET_ADMIN` retained; IPv4-only route deletion) | **closed** — setup phase applies IPv4+IPv6 lockdown then re-execs with capabilities dropped; adversarial restore-route test plus positive control run in the new `docker integration` CI job |
| 2 | `waffle doctor` accepts unusable Docker setups (busybox probe, no runner boot, no arch check) | open |
| 3 | Toolchain security advisories; release builds not on the dev toolchain | **closed** — go 1.26.7 pinned, workflows use mise, govulncheck in gate |
| 4 | Durable PAT handed to workspaces by default | open — decision 2 defines the target |
| 5 | Restore is not atomic across live-file replacement; manifest records `dev` | open |
| 6 | Release gate flaky (unix socket path length, wall-clock deadlines) | **closed** — short-path fallback, load-tolerant budgets |
| 7 | Platform/artifact contract undocumented | **closed** — this document |
| 8 | Stale acceptance evidence and superseded plans | open — archive sweep |

v1.0.0 ships when every row is closed with recorded evidence.
