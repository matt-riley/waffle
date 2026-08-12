# Deploying and running Waffle

## Managed host installation

The protected **Deploy Waffle** operation has no provider, model, database,
or application-secret inputs. It installs a verified Waffle binary, systemd
unit, state directories, management command on `PATH`, age identity, and the
embedded SQLite store location. A successful first deployment reports
**Installed** and the next command:

```sh
sudo waffle provider add
```

Installed is a complete, manageable installation. It does not require an API
key, and `waffle.service` may remain inactive. Waffle has no Workweave Router,
Postgres, or Pub/Sub emulator runtime; it connects directly to the provider
connections enrolled on the host.

Run the bare command for guided enrollment. The complete operator path is:

```sh
sudo waffle provider add
sudo waffle provider models openrouter --search claude
sudo waffle provider models openrouter --refresh
sudo waffle provider model add openrouter anthropic/claude-sonnet-4.6 --default
```

Presets are openai, anthropic, openrouter, and openai-compatible. Only
openai-compatible requires a base URL. OpenAI and Anthropic use their standard
API base URLs, while openrouter uses its standard endpoint and account-filtered
model catalogue. In explicit automation, `--base-url` can override any preset;
the exact `openrouter.ai` host and all of its subdomains, including regional
hosts such as `eu.openrouter.ai`, remain account-filtered through `/models/user`.
Only override hosts outside that domain use the generic `/models` endpoint.

Bare guided add reads a hidden credential and uses it to authenticate model
discovery. Auth-free compatible endpoints may leave it empty. The model picker
supports search, paging, exact upstream IDs, and selection of default, utility,
and additional favourite aliases. In the guided picker, known exact IDs work
directly; unknown numeric or navigation-like IDs use `id:<upstream-id>` to avoid
row-number or navigation ambiguity. Derived aliases are checked against
existing aliases; collisions ask for an explicit replacement. If discovery
fails, guided add offers manual `ALIAS=UPSTREAM` entry. It does the same when
discovery returns no models, while declining that fallback leaves the
connection unchanged.

Selecting a default alias probes the upstream, commits the configuration and
encrypted credential, starts Waffle, verifies `/healthz`, and reports
**Ready**. A failed probe, restart, or health check restores the previous
configuration, secret store, and service state. Cache-write failure after a
successful commit is only a warning and does not undo the enrolled provider.

### Readiness is proven for one exact configuration

Ready means the health probe passed for the exact `config.toml` on disk. Waffle
records that as a generation hash beside the provider lock, and reports
**Installed** whenever the file no longer matches — including after an edit that
has nothing to do with providers.

`waffle serve` therefore re-proves readiness at startup: when a default model is
configured and the recorded generation is missing or stale, it runs the same
health probe and, if it passes, records the current generation. It logs
`re-proved provider readiness for the current config` when it does. An edited
config that still comes up healthy is Ready again after a restart, with no
provider mutation required.

Readiness is never assumed. A failing probe, an unresolvable default model, or a
concurrent provider transaction all leave the recorded generation untouched and
the state Installed, and startup continues either way.

### Managed chat socket

In Ready state, the managed host activates the coupled units in this order:

```sh
systemctl enable --now waffle-chat.socket
systemctl is-active --quiet waffle-chat.socket
systemctl is-active --quiet waffle.service
curl --fail --silent --show-error http://127.0.0.1:8422/healthz
systemctl enable waffle.service
```

`waffle-chat.socket` creates `/run/waffle/chat.sock` as `root:sudo` mode
`0660` and passes its `waffle-chat` descriptor to `waffle.service`. It is a
local Unix socket, not a TCP or tailnet listener. Installed state stops and
disables `waffle.service` first and `waffle-chat.socket` second; rollout and
rollback manage the binary, wrapper, and both units as one compatible set.

After rollout, the ordinary operator smoke test is:

```sh
printf '/status\n/exit\n' | waffle chat --plain
```

Run it without `sudo`. The managed wrapper selects `/run/waffle/chat.sock`
before any privileged configuration or identity access. See
[Chat](chat.md) for the TUI, connection precedence, commands, security
boundary, and troubleshooting.

For an enrolled connection, `provider models` reads an owner-only catalogue
cache under the Waffle home. Directories are mode `0700`, records are mode
`0600`, and a fresh record is reused for 24 hours. The cache is distinct from
selected favourite aliases. `--search` filters the displayed IDs and names,
`--refresh` requests a fresh upstream catalogue, and `--json` emits structured
cache status and model descriptors. If refresh fails and an older valid record
exists, Waffle returns that stale cache with a warning; with no usable record,
the upstream error is returned.

Cache records are derived, non-authoritative discovery data. Each contains the
connection name, type, base URL, opaque scope, and model descriptors, but
contains no API credential. The cache is disposable: removing it only forces
discovery again, and provider removal invalidates its record. Selected
favourite aliases remain authoritative provider configuration.

`provider model add` takes the upstream ID literally and must not receive the
`id:` prefix. It derives an alias when `--alias` is omitted, probes the exact
model before committing, and lets `--default` and `--utility` assign its roles
in the same transaction.

For explicit automation, supply the connection name, preset, and at least one
`--model ALIAS=UPSTREAM`; no catalogue selection is inferred. Pipe the key on
standard input without placing it in the process arguments:

```sh
credential-command | sudo waffle provider add \
  --name anthropic \
  --type anthropic \
  --model claude=claude-model-id \
  --default claude \
  --api-key-stdin
```

Alternatively, point at a root-owned regular file whose exact mode is `0600`:

```sh
sudo waffle provider add \
  --name openai \
  --type openai \
  --model gpt=gpt-model-id \
  --api-key-file /root/provider-api-key
```

The file option rejects symlinks, non-regular files, wider permissions, and
oversized values. There is deliberately no `--api-key VALUE` option. Auth-free
local OpenAI-compatible endpoints can leave the hidden API-key prompt empty.

Connections and aliases are independent, so one installation can use models
from several providers:

```sh
sudo waffle provider add \
  --name local \
  --type openai \
  --base-url http://127.0.0.1:11434/v1 \
  --model local=qwen3:32b

sudo waffle provider list
sudo waffle provider test local
```

`provider list --json` exposes the stable `installed` or `ready` lifecycle
state without credentials. On an Installed system, adding a provider without
selecting a default keeps it Installed; activate a validated alias later with:

```sh
sudo waffle provider model activate local
```

Model and connection removal are explicit:

```sh
sudo waffle provider model remove old-alias --replace-with local
sudo waffle provider remove unused-connection
```

For model removal, `--replace-with` reassigns default, utility, and
agent-profile references before deleting the old alias. Without it, default
and utility references are cleared when no agent profile blocks the operation;
removing the active default can move a Ready installation back to Installed.
Agent-profile references remain blocking without a replacement. Provider
removal remains blocked while any model alias references the connection, so
remove or reassign those aliases first.

Waffle does not automatically fail over, load balance, or choose a provider by
cost: every alias maps to one named connection and one upstream model.

## Running Waffle continuously

`waffle serve` is a host process. Run it as the owner account so its state
directory and secret store are available, and keep the unauthenticated admin
listener on loopback. The `/healthz` endpoint is liveness only; `/status` is
the local run-introspection surface owned by the same listener. The endpoint
is intentionally not a remote monitoring API.

### Waffle Desk

Waffle Desk is the local browser companion included in the Waffle binary.
See the [Waffle Desk rollout guide](waffle-desk.md) for access boundaries,
release checks, and rollback.

Enable it in the service configuration:

```toml
[dashboard]
enabled = true
```

Start the ordinary host process, then open the Desk locally:

```sh
waffle serve
open http://127.0.0.1:8422/desk/
```

The Desk stays on the loopback listener. To reach a managed host, forward that
loopback port explicitly from your local machine:

```sh
ssh -N -L 8422:127.0.0.1:8422 user@host
open http://127.0.0.1:8422/desk/
```

For access from a phone or any other tailnet device without port forwarding,
enable `[dashboard.tailnet]` and publish the same loopback listener to the
tailnet only. The bind address does not change:

```sh
sudo tailscale serve --bg --https=443 http://127.0.0.1:8422
```

Requests admitted that way must carry an allowlisted Tailscale login, and may
reach only `/desk/` and `/api/v1/desk/*` — `/status` and `/healthz` stay
loopback-only. The full boundary, the exposure trade-offs, and
`tailscale serve reset` as the withdrawal step are documented in
[Waffle Desk tailnet access](waffle-desk.md#tailnet-access). Consider a tailnet
grant restricting `tcp:443` on the host to your own devices as defence in depth.

On a host where Waffle's runtime config is already installed, `[dashboard]` and
`[dashboard.tailnet]` have to be edited in the live config file, not in a
first-install seed. Preserve the file's owner and `0600` mode: managed rollouts
validate both and will fail a later deploy if they drift.

Do not expose the Desk through a public bind, hostname, reverse proxy, or
Tailscale Funnel.
It remains behind the existing loopback/admin security boundary and reuses the
configured provider, workspace, session, and memory services owned by
`waffle serve`; it is not a public administration surface or a separate
deployment. Desk can archive Waffle-owned notes, but it does not delete
provider logs, delivered messages, backups, or provider-side data. Its
schedule, workspace, and memory workflows and limits are documented in the
[usage guide](usage-guide.md#waffle-desk-operations).

## Headless secrets

The login keychain is often unavailable to a service context. Set
`WAFFLE_AGE_IDENTITY` to the age identity that decrypts `~/.waffle/secrets.age`
and source it from a root-readable, owner-only credentials file or your
service manager's credential facility. Do not put the identity in
`config.toml`, a unit command line, or a world-readable environment file.

## systemd user service (Linux)

Save as `~/.config/systemd/user/waffle.service`:

```ini
[Unit]
Description=waffle personal agent
After=network-online.target

[Service]
Type=simple
ExecStart=%h/bin/waffle serve
Restart=on-failure
RestartSec=5s
EnvironmentFile=%h/.config/waffle/identity.env
ProtectSystem=strict
ProtectHome=read-only
NoNewPrivileges=true
# Lets host Bash create a child cgroup with pids.max (#275).
Delegate=yes
ReadWritePaths=%h/.waffle %h/.local/state/waffle
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=default.target
```

Create `~/.config/waffle/identity.env` with mode `0600`:

```sh
WAFFLE_AGE_IDENTITY=AGE-SECRET-KEY-...
```

Then validate and start it:

```sh
systemd-analyze verify ~/.config/systemd/user/waffle.service
systemctl --user daemon-reload
systemctl --user enable --now waffle.service
curl --fail http://127.0.0.1:8422/healthz
```

If the binary or state directory lives elsewhere, update `ExecStart` and the
matching `ReadWritePaths`. `ProtectHome=read-only` permits reading the binary
and identity file while the explicit paths permit only waffle state writes.

## Linux release artifact contract

Pushes to `main` build one deployable Linux release artifact after the
deterministic quality gate succeeds. The gate is the same repository suite
used by CI: deterministic evals plus build, vet, test, lint, and security
workflow checks. Only after those jobs pass does the `build linux artifact`
job run.

That job publishes exactly one GitHub Actions artifact named
`waffle-linux-amd64`. The archive file is `waffle-linux-amd64.tar.gz` and it
contains:

- `waffle` — the statically linked `linux/amd64` binary built from
  `./cmd/waffle`
- `waffle.sha256` — the SHA-256 digest for that binary
- `build-metadata.json` — normalized build metadata for the deploy request

`build-metadata.json` records the source commit and workflow provenance the
infra rollout consumes:

- `source_repo`
- `source_sha`
- `source_ref`
- `workflow_run_id`
- `version`
- `goos`
- `goarch`

The artifact builder normalizes timestamps and tar ownership so repeated builds
from the same commit produce the same payload shape.

## Existing-server rollout ownership

Application pushes do not create or replace Hetzner infrastructure directly.
After the artifact upload completes, Infra's zero-input **Operate Waffle**
workflow (and its scheduled discovery job) can resolve the latest successful
run, artifact name, and uploaded digest, then roll that already-built artifact
out to the existing server. This default path keeps cross-repository GitHub App
credentials in Infra.

Immediate push-triggered handoff is optional. If the Waffle repository has an
`APP_ID` variable and matching `PRIVATE_KEY` Actions secret, CI sends the same
immutable provenance through the released shared workflow. Without that
explicit opt-in, the handoff job is skipped and artifact publication remains
successful; no setup credential is required in Waffle.

This split is intentional:

- Waffle owns the application artifact and its provenance.
- Infra owns the server update procedure and any host-specific rollout logic.
- Release Please stays independent from binary deployment, so version-tag
  publication is not the trigger for shipping the Linux binary.

The handoff contains only immutable artifact provenance: artifact name,
Actions run ID, and digest. Provider credentials, model choices, database
credentials, and host topology never pass through the application workflow.
Routine artifact rollout preserves the lifecycle state: an Installed system
receives the verified binary without forced activation, while a Ready system
restarts and must return to health or rolls back to the previous binary.

## No Terraform per push

Every successful application push must not imply a Terraform apply. The
per-push path is artifact build, artifact upload, and infra-owned rollout of
the existing server. Provisioning or topology changes remain explicit
infrastructure work, outside the normal Waffle application release path.

## launchd user agent (macOS)

Save as `~/Library/LaunchAgents/com.matt-riley.waffle.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.matt-riley.waffle</string>
  <key>ProgramArguments</key><array>
    <string>/Users/OWNER/bin/waffle</string><string>serve</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>EnvironmentVariables</key><dict>
    <key>WAFFLE_AGE_IDENTITY</key><string>AGE-SECRET-KEY-...</string>
  </dict>
  <key>StandardOutPath</key><string>/Users/OWNER/.waffle/serve.log</string>
  <key>StandardErrorPath</key><string>/Users/OWNER/.waffle/serve.err.log</string>
</dict></plist>
```

Replace `OWNER` and preferably inject the identity from a protected launchd
credential/bootstrap mechanism rather than committing it to the plist. Check
and load it with:

```sh
plutil -lint ~/Library/LaunchAgents/com.matt-riley.waffle.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.matt-riley.waffle.plist
curl --fail http://127.0.0.1:8422/healthz
```

## Health and recovery

`/healthz` returns JSON with database reachability, scheduler tick freshness,
and adapter success/staleness. It returns HTTP 200 when live and HTTP 503 when
a required subsystem is stale or unavailable. `/status` reports active and
recent runs, token totals, and retry state; it is not a substitute for the
liveness probe. Backups remain separate: deleting live sessions or applying
retention does not remove provider logs, already-delivered channel messages,
or data in existing backups.

## GitHub App setup

Register an App with the permissions for the tools you use and no broader
permissions, install it only on repositories waffle may operate on, and store
its PEM private key in the secret store:

```toml
[github.app]
app_id = 12345
installation_id = 67890
private_key = "secret://github/app-key"
```

The broker requests an installation token for exactly the workspace repository,
caches it until five minutes before expiry, and records `backend=github-app`
plus the repo scope in its audit row. If `[github.app]` is absent, waffle uses
the scoped `GITHUB_TOKEN`/`secret://github/token` fallback. A session bound to
repository A cannot mint a credential for repository B.

Host-side tools (`github_pr` plus the issue #252 read/comment surface) mint one
installation token per call, each carrying only the permission that tool
needs, and never pass those tokens into a workspace container (containers only
ever see the `contents:write` git credential). The permission sets are:
`contents:write` for git push; `pull_requests:write` for `github_pr` and
`github_comment` on pull requests; `pull_requests:read` for `github_pr_get`,
`github_pr_diff`, and `github_pr_comments`; `issues:write` for `github_comment`
on issues; `issues:read` for `github_issue_get`; `checks:read` for
`github_checks`. Operators upgrading an existing app must re-grant the newly
needed permissions in the GitHub App settings before the corresponding tools
start working.

## Workspace egress

`[workspace] egress` controls how repository workspace containers reach the
network. Defaults are deny-by-default:

| Value | Docker network | Host broker (`waffle-host`) | Wider internet |
| --- | --- | --- | --- |
| `none` (default) | user-defined bridge `waffle-ws` | reachable via `--add-host waffle-host:host-gateway` | **Route lockdown** (runner drops the default route, keeps only `waffle-host`) blocks raw internet. HTTP(S)_PROXY points at the broker; the repo's git host is allowlisted for clone/fetch; other hosts get 403. |
| `allowlist` | same `waffle-ws` bridge | same host-gateway path | Same route lockdown + HTTP(S) only through the broker for hosts in `[workspace] allowlist` |
| `full` | default Docker `bridge` | same host-gateway path when the broker URL is set | unrestricted |

Waffle creates the `waffle-ws` network on demand (`docker network create waffle-ws`)
before starting a none/allowlist workspace. Operators do not need to pre-create
it; concurrent creates are ignored when the network already exists.

**Why not Docker `--network none` or `--internal`?** Mode `none` has no route to
the host gateway, so the credential broker is unreachable. Docker `--internal`
also blocks host-gateway on Docker Desktop. Instead, `waffle-ws` is a normal
bridge (so host-gateway works) and the **linux runner** applies netlock at
start (`CAP_NET_ADMIN` + drop default route, host `/32` only). That yields the
same operational outcome: broker reachable, raw external probes fail.

**Clone under `none`:** before `git clone`, Open allows the repository host on
the broker egress allowlist so proxy-aware git can fetch that host only.
