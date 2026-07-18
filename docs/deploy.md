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

Run the bare command for guided enrollment. It prompts for a connection name,
provider type, optional base URL, one or more `ALIAS=UPSTREAM` models, default
and utility aliases, and a hidden API key. Use `-` at optional prompts to leave
the value unset. Selecting a default alias probes the upstream, commits the
configuration and encrypted credential, starts Waffle, verifies `/healthz`,
and reports **Ready**. A failed probe, restart, or health check restores the
previous configuration, secret store, and service state.

For granular automation, pipe the key on standard input without placing it in
the process arguments:

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
After the artifact upload completes, Waffle sends an infra deploy request with
the GitHub Actions run id, artifact name, and uploaded artifact digest. The
infra side downloads that already-built artifact and rolls it out onto the
existing server.

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

Register an App with `Contents: Read and write` and no broader permissions,
install it only on repositories waffle may operate on, and store its PEM
private key in the secret store:

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
