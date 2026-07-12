# Sandbox SQLite queue (issue #29)

Host ↔ container tool IPC uses a pair of SQLite files on a shared mount:

- `inbound.db` — host writes exec requests; runner reads
- `outbound.db` — runner writes results; host reads

Journal mode is `TRUNCATE` with `synchronous=FULL` and `busy_timeout=5000`.

## Running the stress harness

```sh
# Concurrent queue load (no Docker required)
go test -tags=sandbox_stress ./internal/sandbox -run Stress -count=1

# Integrity after cancel
go test -tags=sandbox_stress ./internal/sandbox -run Integrity -count=1
```

On **macOS Docker Desktop** (or any host with a working Docker daemon), also run:

```sh
# Queue + bind-mount visibility when docker is available (skips cleanly if not)
go test -tags=sandbox_docker ./internal/sandbox -run BindMount -count=1

# Doctor includes:
#   - sandbox queue round-trip (host FS Client+Runner ping)
#   - sandbox docker round-trip (daemon + container + bind-mount write/read)
#   - mcp <name> authority (execution=host|sandbox, authority=host|sandbox|restricted, groups, env allowlist)
# when [sandbox] mode=docker (or any group uses docker); MCP rows always report
waffle doctor

# Optional: longer stress under VirtioFS bind mount
# (create a queue dir under a bind-mounted volume and run the stress tests there)
WAFFLE_SANDBOX_STRESS=1 go test -tags=sandbox_stress ./internal/sandbox -run Stress -count=1 -timeout 10m
```

## Support statement

| Failure mode | Host local FS | Docker Desktop VirtioFS (historical risk) |
|---|---|---|
| Lock exclusion / SQLITE_BUSY storms | Mitigated by busy_timeout + single writer per file | Re-run stress harness on your Desktop version; if busy storms appear, prefer a volume on the Linux VM filesystem |
| Stale page cache / missed heartbeats | Not observed in in-tree tests | Probe with doctor queue round-trip + stress; raise busy_timeout only after measuring |
| fsync not honored → corruption after kill | Integrity test after cancel | Same integrity test over a Desktop bind mount |

**Outcome (a):** Supported for Linux hosts and for Docker Desktop when doctor queue + stress pass. The IPC design (TRUNCATE, one writer per file, busy_timeout) is intentional for cross-mount use. If stress fails on a specific Desktop build, file a follow-up with Desktop + VirtioFS versions and the failing mode.

Doctor always exercises the queue on the host filesystem when docker mode is configured. When the daemon is available it also probes container start and host↔container bind-mount write/read (the same mount class as `inbound.db`/`outbound.db`). Full multi-minute concurrent stress remains opt-in via the build tags above.
