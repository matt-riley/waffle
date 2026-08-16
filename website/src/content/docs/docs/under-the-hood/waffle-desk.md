---
title: "Waffle Desk"
description: "The loopback cockpit: enabling it, its access boundary, tailnet access, and rollback."
sidebar:
  order: 4
---

*In plain terms: [Meet Waffle](/docs/meet/what-waffle-is/).*

Waffle Desk is the loopback-only personal cockpit embedded in the normal
`waffle` binary. It is not a separate web service or listener: it shares the
numeric-IP loopback address configured by `gateway.status_listen` with
`/healthz` and `/status`. It does not add a public administration listener.

For the surrounding operator workflows, see the [deployment guide](deploy.md#waffle-desk),
[Desk operations guide](usage-guide.md#waffle-desk-operations), and
[`waffle chat` browser companion](chat.md#waffle-desk-browser-companion).

## Enable and open Desk

Desk is disabled by default, and a disabled Desk cannot enable itself. The loop
is closed from the CLI: `waffle setup` offers to enable it as its last step and
prints the loopback URL on completion. Re-running setup on a configured install
is safe — every completed step is skipped.

To enable it by hand instead:

```toml
[dashboard]
enabled = true
```

Start the normal service, then open the local address:

```sh
waffle serve
# http://127.0.0.1:8422/desk/
```

### Setup checklist

Capabilities opens with a **Setup** panel listing the same prerequisites
`waffle setup` walks — secret-store identity, provider connection, default and
utility model, `[agent.profile.main]`, and Desk itself — each reported as
configured, missing, or misconfigured. A prerequisite Desk can satisfy offers
the control inline; one it cannot states the exact command to run instead.
Today shows a banner pointing at the checklist while anything is outstanding,
so an install that cannot open a conversation says why.

The checklist projects the configuration the running process loaded, matching
the posture view and profile editor beside it. Desk mutations are
restart-deferred, so a step satisfied from the browser flips once Waffle
restarts.

Creating the secret-store identity is the one action the checklist performs
itself. The key is written to the OS keyring and never returned to the browser;
back it up afterwards with `waffle secret export-identity`. On a headless host
with no usable keyring, use `waffle secret init --print` and supply the value as
`WAFFLE_AGE_IDENTITY` instead.

Keep the listener on loopback. Do not expose Desk with a public bind, public
reverse proxy, public hostname, or Tailscale Funnel. A tailnet-only
`tailscale serve` rule is permitted, and only when `[dashboard.tailnet]` is
configured — see [tailnet access](#tailnet-access).

For a managed host, forward the existing loopback listener:

```sh
ssh -N -L 8422:127.0.0.1:8422 user@host
```

Use the equivalent local forwarding command with Tailscale SSH, then open
`http://127.0.0.1:8422/desk/` on the local machine.

## Access boundary

Desk admits a request through exactly one of two profiles, selected by `Host`.
Anything else is rejected.

The **loopback profile** is the default and only enabled boundary. It requires
the numeric loopback (or `localhost`) `Host` for the configured listener, no
cross-site `Sec-Fetch-Site`, and an `Origin` that, when present, is the same
`http` origin. It has no login layer: reads rely on the loopback listener
itself.

The **tailnet profile** exists only when `[dashboard.tailnet]` is configured. It
authenticates the caller's Tailscale identity and is described below.

Mutations on both profiles additionally require the process-scoped
`X-Waffle-Desk-Token` and an `Idempotency-Key`.

The process token is a CSRF control delivered to the same-origin Desk. It is not
a bearer credential and must not be treated as authentication for a public
listener, reverse proxy, or remote client. That remains true on the tailnet
profile: authentication there is the Tailscale identity, never the token.

## Tailnet access

This path lets a phone or any other tailnet device open Desk without SSH port
forwarding. It does not move the bind address. `gateway.status_listen` stays
loopback-only, so the only process that can reach the socket over the network is
`tailscaled` on the same host — which is precisely what makes the identity
headers it injects trustworthy, because it strips any inbound copy of them
first.

Configure Waffle:

```toml
[dashboard]
enabled = true

[dashboard.tailnet]
enabled = true
serve_host = "waffle.example-tailnet.ts.net"
allowed_logins = ["user@github"]
```

Then publish the existing loopback listener to the tailnet only. `--bg` persists
across reboots and tailscaled restarts:

```sh
sudo tailscale serve --bg --https=443 http://127.0.0.1:8422
```

Open `https://waffle.example-tailnet.ts.net/desk/`. The certificate is a
publicly trusted Let's Encrypt certificate for the tailnet DNS name, so no
certificate needs installing on the phone. MagicDNS resolves the name while the
Tailscale app is connected.

A request is admitted through this profile only when every one of these holds:

- The `Host` is exactly the configured `serve_host`, with or without `:443`.
- The path is under `/desk/` or `/api/v1/desk/`. `/status` and `/healthz` are
  never reachable through this profile, including via path traversal.
- No `Tailscale-Funnel-Request` header is present.
- `X-Forwarded-Proto` is `https`.
- `Sec-Fetch-Site`, when present, is `same-origin` or `none`. This is stricter
  than the loopback rule on purpose: sibling MagicDNS names in the same tailnet
  are same-site.
- `Origin`, when present, is `https://<serve_host>`.
- `Tailscale-User-Login` is non-empty and listed in `allowed_logins`.

`allowed_logins` holds login names as tailscaled reports them. SSO logins are
not email addresses — a GitHub-authenticated tailnet reports `user@github`.
Verify the exact value once: a rejected login is logged as
`desk tailnet login rejected` with the login it received, so an allowlist
mismatch names itself instead of being a silent 403.

Consequences worth deciding deliberately:

- The allowlist is per **user**, not per device. Every device logged in as an
  allowed login can open Desk. Restricting to specific devices requires a
  tailnet grant on `tcp:443`, because the identity headers do not distinguish
  one of your devices from another.
- Tagged devices — CI runners, tagged servers — send no login and always fail
  closed.
- Node sharing populates the identity headers for the share recipient, so the
  allowlist is what keeps a shared node from granting Desk access.
- Enabling HTTPS certificates publishes the host's MagicDNS name to public
  Certificate Transparency logs.
- A local process on the serve host can still forge these headers over loopback.
  That is unchanged: any such process can already reach Desk directly on the
  loopback `Host` and read the process token out of the shell. Restrict who can
  execute code on the host, and keep the listener on loopback.

To withdraw tailnet access, remove the serve rule and the config:

```sh
sudo tailscale serve reset
```

Revoking a lost device is a Tailscale admin console action. There is no bearer
credential on the device to rotate and nothing to change in Waffle.

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
mise run dashboard-check
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
