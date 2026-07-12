# Running waffle continuously

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
