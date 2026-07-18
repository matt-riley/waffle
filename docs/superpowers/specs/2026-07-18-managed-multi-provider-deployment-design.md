# Managed Multi-Provider Deployment Design

## Summary

Waffle deployment becomes one operator intent rather than a sequence of infrastructure, router, database, and secret-rotation procedures. A first deployment installs Waffle on the existing supported Hetzner and Tailscale foundation without requiring a model provider. The operator then enrolls one or more provider connections directly on the host through the Waffle CLI.

Workweave Router and its Postgres, Pub/Sub emulator, Compose topology, client keys, provider environment, readiness checks, and host runtime are removed. Waffle talks directly to its configured providers and can use models from several providers in the same installation.

## Goals

- Make **Deploy Waffle** a single protected operation that provisions or reuses the host, installs a verified artifact, and leaves a manageable installation.
- Keep database accounts, generated identities, service users, and other internal operational details outside the normal operator experience.
- Remove Workweave Router and every Router-owned runtime dependency.
- Support multiple named provider connections and model aliases in one Waffle process.
- Allow different agent profiles, the default model, and the utility model to use different providers.
- Allow provider enrollment on the deployed host without sending provider credentials through deployment environment variables or workflow inputs.
- Preserve immutable artifact verification, tailnet-only exposure, rollback, and legacy Waffle configuration compatibility.

## Non-goals

- Automatic provider failover, cost-based selection, load balancing, or model arbitration.
- A universal adapter for APIs that are neither natively supported nor OpenAI-compatible.
- Replacing the existing Hetzner or Tailscale foundation in this change.
- Moving Waffle-specific provisioning or host policy into `matt-riley-ci`.
- Making Terraform run for every application artifact rollout.

## Operator experience

### First deployment

The operator runs the protected **Deploy Waffle** workflow without artifact IDs, digests, database credentials, provider credentials, Router configuration, or secret-rotation scopes. The workflow reconciles the Waffle infrastructure, resolves the latest successful immutable Waffle artifact, installs the binary and systemd unit, generates the Waffle age identity if and only if the installation has never had one, and verifies the installation contract.

The workflow succeeds in the **Installed** state. It reports the installed Waffle version and the exact next command:

```sh
sudo waffle provider add
```

The deployed host exposes a correctly configured `waffle` management command on `PATH`; the operator does not need to know the release directory, source the service identity, or set `WAFFLE_HOME` manually. System-install mutations require `sudo`, while read-only provider commands remain available without unnecessary privilege where filesystem permissions allow it.

### Provider enrollment

`sudo waffle provider add` is an interactive command. It establishes the required privilege before reading secret input, then prompts for a connection name, provider type, optional base URL, API key through a hidden prompt when required, one or more model aliases, and whether a model becomes the default or utility model.

The command also supports automation through granular flags and `--api-key-stdin` or `--api-key-file`. API keys are never accepted as command-line argument values and never appear in process listings, logs, workflow summaries, or configuration files.

Provider enrollment stages configuration and encrypted-secret changes, probes the selected upstream model, and commits only after validation succeeds. The first validated default model starts Waffle and moves the installation to **Ready**.

The provider command group includes:

- `waffle provider add`
- `waffle provider list`
- `waffle provider test <connection>`
- `waffle provider remove <connection>`

Provider removal fails while a model alias or agent profile still references the connection. Model-alias management may be exposed through the provider enrollment flow and focused model commands, but model selection remains deterministic.

## Waffle configuration model

Waffle gains named provider connections and a model catalog:

```toml
[providers.anthropic]
type = "anthropic"
api_key = "secret://provider/anthropic/api-key"

[providers.openai]
type = "openai"
api_key = "secret://provider/openai/api-key"

[providers.local]
type = "openai"
base_url = "http://127.0.0.1:11434/v1"

[models.claude]
provider = "anthropic"
model = "claude-sonnet-4-6"

[models.gpt]
provider = "openai"
model = "gpt-5.4"

[models.local]
provider = "local"
model = "qwen3:32b"

[agent]
default_model = "claude"
utility_model = "local"
```

A model alias resolves to exactly one provider connection and one upstream model identifier. Agent profiles reference model aliases. Waffle constructs and reuses provider clients by connection rather than assigning one global provider client to every agent.

Provider types initially comprise the native Anthropic adapter and the OpenAI-compatible adapter. OpenAI, OpenRouter, Ollama, and other compatible endpoints use named OpenAI-compatible connections with their own base URLs. Future native adapters extend Waffle without requiring changes in Infra or shared CI.

The existing singular `[provider]` configuration remains readable. Waffle normalizes it in memory to a named `default` connection and model alias. The first explicit provider-management write migrates it to the new representation without changing the selected provider or model.

## Installation states

**Installed** means the verified binary, systemd unit, state directories, SQLite store location, generated age identity, configuration lock, and management command are present. A provider is not required and the service may remain inactive.

**Ready** means the installation has a validated default model, the Waffle service is active, and its health checks pass. A routine artifact rollout preserves the current state: Installed systems receive the new artifact without forced activation, while Ready systems restart and must return to health.

The deployment workflow must not claim Ready when only the installation contract passed. Provider commands must not claim success when only configuration files were written.

## Cross-repository ownership

### `waffle`

Waffle owns provider and model schemas, legacy normalization, provider-client construction, model-alias resolution, provider-management commands, provider probing, encrypted credential storage, Installed and Ready status semantics, and the immutable Linux artifact.

After all release gates pass, Waffle may invoke the released shared deployment-request workflow with the artifact run ID, name, and digest. The caller must use a released `matt-riley-ci` major or an immutable commit, not an incompatible historical `v1` contract.

### `matt-riley-ci`

Shared CI owns only the generic authenticated handoff. It validates the all-or-none artifact tuple, creates a short-lived GitHub App token, and dispatches source and artifact provenance to Infra. It does not receive provider credentials, understand provider connections, select models, provision Hetzner, join Tailscale, mutate the host, or decide Waffle health.

The artifact-aware deployment-request contract must be covered by an executed contract test and released on the repository's current major line before Waffle depends on that major tag.

### `infra`

Infra owns the supported Hetzner host, Tailscale access, artifact provenance checks, service account and filesystem setup, age-identity retention, systemd installation, host mutation locking, activation, health verification, and binary rollback.

Infra treats Waffle configuration as application-owned data. It does not hard-code Anthropic, OpenAI, OpenRouter, model IDs, provider secret names, or provider-specific environment variables. It never creates or manages provider accounts or API keys.

The protected manual deployment operation reconciles Terraform when explicitly requested, waits for cloud-init and tailnet reachability, resolves a verified Waffle artifact, and invokes the same host installation primitives used by routine artifact deployments. Routine application deployments remain Terraform-free.

## Router removal

The following are removed from Infra:

- `router_ref` catalog metadata
- the Router Compose file
- Router and Postgres environment validation
- Router checkout, build, archive, image transfer, and container startup
- Postgres, migration, Pub/Sub emulator, and Router containers and volumes
- `WEAVE_ROUTER_ENV` and `WAFFLE_ROUTER_API_KEY`
- Router provider and client rotation scopes
- Router readiness and health ordering
- `/opt/weave-router` runtime management

Docker remains installed because Waffle's own sandbox mode uses it.

During production migration, the existing Router runtime is stopped before direct provider enrollment. Router containers, images, checkout, and data remain recoverable only until a direct provider is validated and Waffle reaches Ready. After that verification, the obsolete runtime is removed and the removal is reported explicitly.

## Secrets and security

The Waffle age identity is the only new persistent internal credential required by the deployment. Infra generates it exactly once, stores it in the service-manager credential location with restrictive ownership and mode, and never emits it through Terraform state, workflow outputs, logs, command arguments, or GitHub inputs.

An existing encrypted store without its matching identity fails closed. Deployment and provider management never regenerate an identity merely because it cannot be read.

Each provider API key is stored at `provider/<connection-name>/api-key` in Waffle's encrypted secret store. Connection names are validated before they become path components. Credentials for auth-free local endpoints are omitted.

Provider-management changes acquire a configuration lock. The command prepares staged configuration and encrypted-store copies, validates them, and probes the provider before live mutation. Commit ordering ensures an unused secret may exist briefly before its configuration reference, never the reverse. Restart or health failure restores both previous files and the previous service state.

## Failure handling and rollback

Deployment preflight validates infrastructure credentials, artifact provenance, archive contents, checksums, host reachability, and the installation bundle before host mutation.

First deployment is idempotent. It creates missing Waffle-owned resources, preserves valid existing resources, and refuses contradictory states such as a secret store without its identity. It never regenerates internal credentials on an ordinary rerun.

Ready-system artifact rollout preserves exact source and digest validation, lexical release-target validation, atomic current-symlink switching, health verification, and restoration of the previous binary on failure. Installed systems update the artifact without falsely running provider health checks.

Provider addition and removal are transactional across configuration, encrypted secrets, and service state. Alias collisions, missing model references, invalid credentials, provider errors, filesystem errors, restart errors, and failed health checks leave the previous working state intact.

## Testing and acceptance

### Waffle

- Parse and validate multiple named provider connections and model aliases.
- Normalize legacy singular provider configuration without behavior drift.
- Resolve default, utility, and agent-profile models through different provider connections.
- Reuse clients within a connection while isolating credentials and base URLs between connections.
- Exercise native Anthropic and OpenAI-compatible adapters through local deterministic HTTP servers.
- Test interactive enrollment and every non-interactive secret-input mode.
- Prove API keys never enter argv, logs, config files, or command output.
- Prove staged validation, locking, permissions, Installed-to-Ready transition, restart, and rollback behavior.
- Reject removal of referenced providers and invalid connection-name path components.
- Preserve all existing repository gates: format, vet, lint, race tests, deterministic evals, build, and artifact reproducibility.

### Shared CI

- Execute contract tests for typed artifact inputs, all-or-none validation, positive run IDs, lowercase SHA-256 digests, payload forwarding, minimal permissions, and pinned external actions.
- Release the artifact-capable contract on the current major line before updating Waffle's caller.

### Infra

- Assert no catalog, workflow, script, config, service, or runtime reference to Workweave Router, Postgres, Router client keys, or port 8080 remains.
- Test first installation, repeat installation, Installed artifact update, Ready artifact update, missing-identity failure, permissions, host lock contention, activation failure, health failure, and binary rollback.
- Test that Terraform runs only for explicit full reconciliation and not for routine application artifacts.
- Preserve tailnet-only exposure and Waffle Docker sandbox prerequisites.
- Run Ruby workflow contracts, shell syntax, ShellCheck, actionlint, Terraform format/validate/test, and repository diff checks.

### Live acceptance

Completion requires a real protected deployment against the Waffle host:

1. Deploy from no active Waffle provider to Installed.
2. Confirm the management command works without release-path or environment knowledge.
3. Add at least two provider connections from different providers.
4. Add model aliases for both and assign different default, utility, or profile roles.
5. Exercise both providers through Waffle.
6. Confirm Waffle reaches Ready and remains tailnet-only.
7. Perform a routine artifact update and verify health.
8. Force a controlled failed update and verify binary rollback.
9. Remove the obsolete Router runtime after Ready is proven.

## Delivery order

1. Synchronize each clean repository with its fetched `origin/main`; preserve Waffle's existing identity changes and unrelated untracked files.
2. Complete and release the generic artifact handoff in `matt-riley-ci`.
3. Implement Waffle's multi-provider configuration, runtime resolution, provider CLI, state semantics, and CI caller through test-first cycles.
4. Remove Router and implement convergent Installed/Ready deployment behavior in Infra through test-first shell and workflow contracts.
5. Run all local gates in each repository.
6. Publish in dependency order: shared CI, Waffle, then Infra where required by the live workflow graph.
7. Execute the live acceptance sequence and report any external gate without overstating completion.
