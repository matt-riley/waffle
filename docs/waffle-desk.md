# Waffle Desk

Waffle Desk is the loopback-only personal cockpit embedded in the normal
`waffle` binary. It is not a separate web service or listener: it shares the
numeric-IP loopback address configured by `gateway.status_listen` with
`/healthz` and `/status`. It does not add a public administration listener.

For the surrounding operator workflows, see the [deployment guide](deploy.md#waffle-desk),
[Desk operations guide](usage-guide.md#waffle-desk-operations), and
[`waffle chat` browser companion](chat.md#waffle-desk-browser-companion).

## Enable and open Desk

Desk is disabled by default. Enable it deliberately:

```toml
[dashboard]
enabled = true
```

Start the normal service, then open the local address:

```sh
waffle serve
# http://127.0.0.1:8422/desk/
```

Keep the listener on loopback. Do not expose Desk with a public bind, public
reverse proxy, public hostname, or Tailscale Serve rule.

For a managed host, forward the existing loopback listener:

```sh
ssh -N -L 8422:127.0.0.1:8422 user@host
```

Use the equivalent local forwarding command with Tailscale SSH, then open
`http://127.0.0.1:8422/desk/` on the local machine.

## Access boundary

Desk has no login layer and its first release does not provide remote
authentication. Reads rely on the numeric loopback listener plus the strict
`Host`, `Origin`, and `Sec-Fetch-Site` boundary. Mutations additionally require
the process-scoped `X-Waffle-Desk-Token` and an `Idempotency-Key`.

The process token is a CSRF control delivered to the same-origin Desk. It is not
a bearer credential and must not be treated as authentication for a public
listener, reverse proxy, or remote client.

## Scope and safety rules

- A model selected on Today changes only that persisted session.
- Skills attached on Today belong only to that persisted session.
- Default and utility model roles are Waffle-wide.
- Skill-library activation is Waffle-wide.
- A skill source must match the configured local/Git allowlists. Git sources
  require a full commit pin.
- Installation is review, install inactive, then explicit activation.
- Provider credentials are accepted only by the mutation boundary. They are
  not returned by Desk, cached in browser storage, or included in connection
  status.
- Memory attachment adds one bounded, attributed fact to an explicit persisted
  session.
- Forget archives only the selected Waffle-owned note. It does not erase
  provider logs, delivered messages, or backups, and it has no Undo claim.
- Workspace close always uses preview then confirmation. Dirty or unpushed
  evidence blocks close; Desk does not offer force close.

## Release gate

The deterministic browser fixture uses the real embedded shell, assets,
security wrapper, and production route handlers with in-memory domain fakes.
It makes no provider, Git, Docker, systemd, or other network call. The browser
suite requires system Chrome and the pinned Node/pnpm versions.

Run:

```sh
pnpm --dir tools/dashboard-tests install --frozen-lockfile
pnpm --dir tools/dashboard-tests test
mise run fmt
mise run vet
mise run lint
mise run test
mise run build
git diff --check
```

Before a managed rollout, separately verify artifact digest/provenance,
protected Infra approval, provider health, SSH or Tailscale access, and the
actual host service state. An Installed host receives the verified binary but
does not start Waffle merely to expose Desk. A Ready host follows the normal
service restart and loopback health gate:

```sh
systemctl is-active --quiet waffle.service
curl --fail --silent --show-error http://127.0.0.1:8422/healthz
```

Smoke-test Today, Tasks, Workspaces, Memory, and Capabilities through the local
or forwarded URL. Confirm `/healthz` and `/status` remain valid and that
provider and connection output contains no credential material.

## Roll back Desk

Disable only the dashboard:

```toml
[dashboard]
enabled = false
```

Restart the normal service and verify `/desk/` returns 404 while `/healthz`,
`/status`, the chat socket, CLI, provider lifecycle, and persisted data remain
available. Do not delete databases, secrets, skills, providers, workspaces, or
memory to disable Desk.

If the binary release itself is unhealthy, use the existing managed artifact
rollback or `waffle rollback`. When Waffle is already running as a service,
restart that service after `waffle rollback` so the restored binary is actually
loaded, then prove the prior service state and loopback health. Schema
migrations remain forward-only.
