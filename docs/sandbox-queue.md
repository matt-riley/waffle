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

On **macOS Docker Desktop with VirtioFS selected** (Settings → General →
Virtual Machine Options / file sharing), run the real cross-VM harness:

```sh
# Build a Linux waffle runner, bind-mount the queue, run that runner inside a
# networkless container, and concurrently drive the host-side Client for two
# minutes. A second test docker-kills the runner during writes and checks both
# databases with PRAGMA integrity_check.
WAFFLE_SANDBOX_DOCKER_SECONDS=120 \
  go test -tags=sandbox_docker ./internal/sandbox \
  -run DockerBindMount -count=1 -timeout 10m -v

# PID-limit containment (issue #46). This starts a fork workload only inside
# a disposable container capped at 32 PIDs, observes that it cannot exceed the
# cgroup limit, and always force-removes the container during cleanup.
go test -tags=sandbox_docker ./internal/sandbox \
  -run '^TestDockerPIDLimitContainsForkBomb$' -count=1 -timeout 2m -v

# Doctor includes:
#   - sandbox queue round-trip (host FS Client+Runner ping)
#   - sandbox docker round-trip (daemon + container + bind-mount write/read)
#   - mcp <name> authority (execution=host|sandbox, authority=host|sandbox|restricted, groups, env allowlist)
# when [sandbox] mode=docker (or any group uses docker); MCP rows always report
waffle doctor

# Host-filesystem baseline (not a substitute for the command above).
WAFFLE_SANDBOX_STRESS=1 go test -tags=sandbox_stress ./internal/sandbox -run Stress -count=1 -timeout 10m
```

Record the verbose header (Docker server version, architecture and driver
status) with the result. Expected evidence is:

- `TestDockerBindMountContainerRunnerStress` passes with non-zero completed
  work, equal request/distinct-tool-use/result counts, no busy-timeout error,
  and no observed heartbeat gap over four seconds (the two-second runner
  heartbeat interval ×2);
- `TestDockerBindMountKillMidWriteIntegrity` shows a real `docker kill` and
  both `inbound.db` and `outbound.db` return `ok` from `PRAGMA integrity_check`.
  The test first holds an exclusive outbound lock, waits for the containerized
  tool to write a bind-mounted marker (proving tool completion and a blocked
  result INSERT), verifies no result committed, and only then kills Docker;
- without Docker, both tests explicitly skip with `docker not in PATH` or
  `docker daemon unavailable`. This is an infrastructure gate, not positive
  Docker Desktop evidence.

`TestDockerPIDLimitContainsForkBomb` has the same deterministic Docker gate.
A skip proves only that the host cannot run the acceptance test; a pass records
the maximum process count observed and the limit read back from Docker.

The requested stress duration begins only after the first container heartbeat;
the request workers and heartbeat observer share that one fresh deadline.

## Support statement

| Failure mode | Host local FS | Docker Desktop VirtioFS (historical risk) |
|---|---|---|
| Lock exclusion / SQLITE_BUSY storms | Mitigated by busy_timeout + single writer per file | Re-run stress harness on your Desktop version; if busy storms appear, prefer a volume on the Linux VM filesystem |
| Stale page cache / missed heartbeats | Not observed in in-tree tests | Probe with doctor queue round-trip + stress; raise busy_timeout only after measuring |
| fsync not honored → corruption after kill | Integrity test after cancel | Same integrity test over a Desktop bind mount |

Docker documents that macOS bind mounts may use VirtioFS or gRPC-FUSE and that
VirtioFS is the default/recommended high-performance option, while also
recommending Linux-VM named volumes rather than shared folders for database
performance. The current Docker documentation makes no explicit guarantee
that advisory locks and `fsync` are passed through with local-filesystem
semantics. SQLite documents that rollback-journal correctness assumes OS
locking and `fsync` behave as advertised, and cautions against network/shared
filesystems. These sources motivate testing each Docker Desktop build rather
than inferring correctness from VirtioFS performance claims:

- https://docs.docker.com/desktop/features/networking/#backend-processes
- https://docs.docker.com/desktop/settings-and-maintenance/settings/#file-sharing
- https://www.sqlite.org/lockingv3.html

**Outcome (a):** Supported for Linux hosts and for a specific Docker Desktop
VirtioFS installation only after the real container-runner stress and crash
tests above pass. The IPC design (TRUNCATE, one writer per file, busy_timeout)
is intentional for cross-mount use. If stress fails on a Desktop build, file a
follow-up with Desktop/VirtioFS versions and the failing mode.

The issue close-out/support evidence is recorded at:

- https://github.com/matt-riley/waffle/issues/29#issuecomment-4952731710
- https://github.com/matt-riley/waffle/issues/29#issuecomment-4952819113
- https://github.com/matt-riley/waffle/issues/29#issuecomment-4952865252

Doctor always exercises the queue on the host filesystem when docker mode is configured. When the daemon is available it also probes container start and host↔container bind-mount write/read (the same mount class as `inbound.db`/`outbound.db`). Full multi-minute concurrent stress remains opt-in via the build tags above.
# Gated MCP sandbox proof

The #77 restricted-executor integration test requires a working Docker daemon
and permission to pull `alpine:3.20`:

```bash
WAFFLE_TEST_DOCKER=1 go test ./internal/mcp -run TestDockerSandboxMCPExecution -count=1 -v
```

Expected evidence is a passing `TestDockerSandboxMCPExecution`. A daemon,
network, image-pull, or container-start failure is an infrastructure failure;
it must not be recorded as behavioral acceptance evidence until rerun in a
Docker-capable environment. The deterministic environment-isolation and launch
planning tests remain mandatory even when this gate is skipped.
