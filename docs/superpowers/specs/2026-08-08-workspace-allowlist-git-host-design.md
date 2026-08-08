# Workspace Allowlist Git Host Design

**Issue:** #239  
**Goal:** Preserve implicit access to a workspace's bound Git host when workspace egress is configured as `allowlist`.

## Context

Workspace cloning uses the broker as an HTTP(S) proxy for `none` and `allowlist` egress. The broker starts with configured allowlist entries, while `newWorkspaceManager` adds the repository host when a workspace opens. The current wiring only installs that host callback for `none` and the empty/default mode, so switching to `allowlist` removes access to the repository host unless the operator duplicates it in the configuration.

The repository host remains session-scoped by the broker's Git credential binding, so adding it to the egress allowlist does not broaden repository credential access.

## Design

Update `newWorkspaceManager` in `cmd/waffle/ws_cmd.go` so the callback that adds the bound repository host is installed for `allowlist`, `none`, and the empty/default mode. Leave `full` unchanged because it does not use the broker allowlist.

Add a table-driven test beside the existing workspace broker wiring tests. It will construct a manager for each egress mode with a broker and assert that the repository-host callback is present for brokered modes and absent for `full`. This directly covers the regression: the allowlist mode must retain the same callback that makes the bound Git host reachable.

No broker API, policy semantics, configuration schema, dependency, or runtime behavior outside this wiring change is required.

## Validation

Run the focused `cmd/waffle` tests, then the repository's standard formatting, vet, lint, and test tasks where the available CI environment supports them. The pull request will link #239 and describe the root cause and verification commands.
