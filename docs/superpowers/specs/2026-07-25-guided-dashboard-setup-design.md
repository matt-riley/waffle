# Guided Dashboard Setup and Capability Onboarding Design

**Status:** Approved product direction; awaiting written-spec review

## Summary

Waffle Desk replaces its overloaded **Capabilities** page with a persistent
**Setup** area for configuring and managing models, skills, GitHub access,
Telegram, and later integrations. Setup is the primary interactive path. The
CLI exposes equivalent operations for automation, recovery, and operators who
prefer a terminal.

The current page exposes backend-shaped forms. A model catalogue is displayed
as non-selectable text, adding a model requires retyping three related values,
skill installation exposes deny-by-default source fields without explaining
their policy, and GitHub and Telegram have no guided setup at all. The new
experience presents installation state first, gives each task one focused
flow, supplies safe defaults, and keeps advanced overrides available per value.

Setup never turns optional integrations into prerequisites. A validated default
model is required for Waffle's Ready lifecycle; GitHub, Telegram, and skills
remain optional and can be configured, changed, or left disabled independently.

This design supersedes the Capabilities navigation, model-management, and
skill-installation interaction decisions in
`2026-07-23-waffle-desk-dashboard-design.md`. It preserves that design's
security boundaries, Today conversation controls, and visual system. It also
preserves the authenticated catalogue contract from
`2026-07-19-authenticated-model-catalog-design.md` while making its results
selectable.

## Goals

- Make adding a model from an enrolled provider a selectable, end-to-end task.
- Make provider enrollment usable without knowing base URLs, model IDs, or
  Waffle alias rules in advance.
- Make skill installation usable from a public GitHub URL or local archive
  without requiring operators to pre-edit allowlists or find a commit hash.
- Retain a host-approved local-folder import path for advanced installations.
- Let an operator add and verify GitHub credentials without editing raw TOML.
- Let an operator create, verify, enable, and pair a Telegram bot from Desk.
- Show readiness, optionality, scope, and the next useful action consistently.
- Preserve transactional configuration, encrypted secret storage, exact source
  review, loopback-only Desk access, and repository-scoped Git credentials.
- Keep the CLI and Desk on shared application operations rather than creating
  two configuration systems.
- Provide specific, sanitized, actionable errors at the control that caused
  them.

## Non-goals

- A public administration endpoint, remote authentication layer, or multi-user
  setup.
- Making GitHub, Telegram, or skills mandatory for Waffle readiness.
- Automatic model selection by price, popularity, latency, or capability.
- Automatically activating every installed skill.
- Silently replacing existing model aliases, providers, credentials, or skills.
- Accepting unpinned executable content without a review boundary.
- Reusing workspace Git credentials for arbitrary private skill repositories.
- Arbitrary raw configuration, environment, filesystem, or secret-store editing
  in the browser.
- Supporting messaging adapters other than Telegram in this implementation.

## Product Structure

### Persistent Setup destination

The existing `section=capabilities` route remains valid for bookmarks and
released clients, but its navigation label and page title become **Setup**.
There is no disappearing first-run wizard and no second settings destination.
The page remains useful after initial setup for health checks, additions, and
changes.

Setup opens with a compact status list:

| Area | States | Primary action |
| --- | --- | --- |
| Models | Not configured, Ready, Needs attention | Add provider or Add model |
| GitHub | Not configured, PAT configured, App configured, Needs attention | Connect or Verify |
| Telegram | Off, Configured, Ready, Needs pairing, Needs attention | Connect, Pair, or Repair |
| Skills | None installed, Installed inactive, Active, Needs attention | Add skill or Review |

Each row contains one plain-language summary, an explicit optional/required
label, and one primary action. It does not expose raw paths, secret references,
provider URLs, command lines, or internal configuration keys.

Selecting an action opens a focused single-column task within Setup. A task has
a clear title, short explanation, Back action, inline validation, primary
submit action, and canonical success state. Only the affected task is disabled
while it is pending.

### Readiness semantics

- Models show **Required** until a probed default alias exists.
- GitHub, Telegram, and Skills always show **Optional**.
- An optional integration failure is visible but never changes Waffle's
  Installed/Ready lifecycle.
- Setup distinguishes **configured** from **verified**. Presence of a secret or
  config record alone does not imply successful access.
- Status is derived from server state on every load and after every mutation;
  browser state is never authoritative.

## Model Setup

### Existing provider

**Add model** first shows enrolled provider connections as selectable choices.
If there is one connection, it is preselected. The operator may refresh its
authenticated catalogue and search by display name, owner, or exact model ID.

Catalogue results are interactive rows, not static articles. Selecting a row:

1. preserves the exact upstream model ID;
2. generates a valid Waffle alias using the existing deterministic alias rule;
3. shows the alias as an editable advanced value;
4. offers **Use for new conversations** and **Use for utility work** checkboxes;
5. shows a compact confirmation summary; and
6. submits one add-model request.

The selected model passes the existing completion probe before configuration
commits. A probe, collision, validation, restart, or health failure leaves the
previous working state unchanged. Success returns the canonical model list and
role assignments, then offers **Use in this conversation** when a current Desk
session exists.

Large catalogues render a bounded result page and do not insert hundreds of
cards into the document. Search remains client-side when the bounded catalogue
is already loaded, with paging or virtualization for rendering.

### New provider

**Add provider** uses presets for OpenAI, Anthropic, OpenRouter, and
OpenAI-compatible:

- preset;
- connection name, prefilled from the preset;
- credential, when required;
- base URL only for OpenAI-compatible, with per-value override for presets;
- authenticated catalogue discovery;
- default model selection;
- optional utility model and additional favourites; and
- final transactional validation.

Credentials are accepted only in the mutation request, cleared from the DOM
after success or failure, never returned, and never written to browser storage.
Manual upstream model entry remains available when discovery is unsupported.

## Skill Setup

### Source chooser

**Add skill** presents only source methods the server reports as available:

1. **GitHub** — the normal path for a public repository or skill-folder URL.
2. **Upload archive** — a bounded `.zip` containing one or more skills.
3. **Approved host folder** — an advanced path shown only when the host has
   configured labeled import roots.

An unavailable method is explained in policy-neutral language rather than
rendered as a dead form. The server returns safe capability flags and labels,
not raw filesystem paths or secret configuration.

### GitHub discovery and pinning

The operator pastes a credential-free HTTPS GitHub repository URL or a GitHub
folder URL. Waffle:

1. validates the allowed host;
2. resolves the selected/default ref to an exact lowercase 40-character commit;
3. downloads a bounded archive without following unapproved redirects;
4. discovers a root skill or direct skill directories below the selected path;
5. lets the operator choose when more than one skill is present; and
6. stages only the chosen skill at the resolved commit.

The exact commit is displayed in the review but is not something the operator
must discover or type. An advanced commit override accepts only a complete
commit. Source provenance becomes
`git:<canonical-repository>@<commit>#<skill-path>`.

V1 GitHub discovery supports public repositories. A private repository produces
a specific explanation and does not reuse provider credentials or
repository-scoped workspace credentials implicitly.

### Archive upload

Desk accepts one `.zip` through a bounded multipart request. The service streams
it into a private staging area, rejects absolute paths, traversal, symlinks,
special files, duplicate paths, excessive compression, excessive files, or
excessive expanded bytes, then applies the same skill discovery and audit as a
GitHub source. Raw upload content is deleted when staging fails, expires, or
installation completes.

The existing review bounds remain the baseline: at most 64 reviewed files and
1 MiB of reviewed content. The HTTP body and compressed archive receive
separate conservative limits so compression cannot bypass the expanded bound.

### Approved host folder

Hosts may continue to configure import roots. Setup exposes a stable, sanitized
label for each valid root and lists eligible child skill directories through a
server-side chooser. Operators do not type raw server paths. Symlink and
time-of-check/time-of-use protections remain in force.

### Review, install, and activation

Every source converges on the same readable review:

- name and description;
- source and resolved commit when applicable;
- complete file manifest with sizes and hashes;
- bounded text previews;
- audit result and flags;
- content digest; and
- stage expiry.

**Install inactive** is the only install action. After successful installation,
Setup shows the canonical library record and a separate **Activate** action.
Activation remains Waffle-wide and restart-aware. Attaching an active skill to
one conversation remains a separate Today control.

Name collisions never overwrite an installed skill. Updates and replacement
remain separate future operations.

## GitHub Setup

The GitHub row reports one of:

- Not configured;
- Fine-grained PAT configured;
- GitHub App configured;
- Configured, verification required; or
- Needs attention.

**Connect GitHub** offers:

### Fine-grained personal access token

This is the shortest path. Setup explains that the token should be limited to
repositories Waffle may operate on and needs repository contents read/write.
The token is submitted once, validated against GitHub, and committed as
`github/token` only after validation succeeds. It is never echoed or returned.

When a current repository workspace exists, validation also checks that exact
repository. Without one, Setup reports **Credential verified; repository access
will be checked when a workspace opens** rather than claiming repository access.

### GitHub App

This is labeled **Recommended for least privilege**. The task collects App ID,
Installation ID, PEM private key, and an advanced base URL override. The service
parses the key, mints an installation token, and verifies the installation
before transactionally committing:

```toml
[github.app]
app_id = <id>
installation_id = <id>
private_key = "secret://github/app-key"
```

The private key is never returned, logged, placed in TOML, or retained by the
browser. The existing exact-workspace repository scope check remains the
credential broker's authorization boundary.

The CLI gains a guided GitHub setup operation that calls the same domain
service. Existing `waffle secret set github/token` and raw configuration remain
compatible recovery paths, not the recommended onboarding instructions.

## Telegram Setup

**Connect Telegram**:

1. explains how to create a bot with BotFather;
2. accepts the bot token through a password input;
3. calls Telegram `getMe` before committing;
4. shows the verified bot name and username;
5. transactionally stores `telegram/bot-token`, writes the secret reference,
   enables the adapter, and schedules the normal restart;
6. waits for the new process and confirms adapter health; and
7. presents a link/username and asks the owner to send the bot a private
   message.

Pending private-chat pairings appear in the same task with sender name,
Telegram identity details already safe for the existing `pair ls` output, and
the six-character code. **Approve as me** calls the existing entity pairing
operation. The UI never offers pairing approval for a group-originated request,
and the existing group-chat mention and stranger posture remains unchanged.

If a token validates but restart or health reconciliation fails, the
transaction restores the previous configuration, secret, and service state.
Telegram stays optional on fresh and existing installations.

The CLI gains a guided Telegram setup operation and continues supporting
`waffle secret set telegram/bot-token`, explicit config, and `waffle pair` for
recovery and automation.

## Shared Architecture

### Application services

The dashboard remains an adapter over typed application services:

```text
cmd/waffle
  constructs provider, setup, pairing, restart, and health dependencies
internal/dashboard
  HTTP validation, sanitized view models, request tokens, idempotency
internal/dashboard/ui
  templ Setup views and small focused JavaScript modules
internal/configtxn
  shared crash-safe lock, journal, commit, rollback, and recovery
internal/providerconfig
  provider/model catalogue, probes, transactional provider mutations
internal/skillinstall
  source discovery, bounded staging, review, install provenance
internal/setupconfig
  GitHub and Telegram validation plus config/secret mutation plans
internal/entity
  pending pairing list and approval
```

The crash-safe transaction coordinator currently embedded in provider
management moves into `internal/configtxn`; `internal/providerconfig` adopts it
without changing provider behavior. Provider, GitHub, and Telegram mutations
share one host lock and recovery journal so two concurrent setup operations
cannot overwrite each other. A PAT mutation may contain only a secret-store
change, but it still uses the same lock, journal, validation, and rollback
contract. Domain services return typed results and typed errors; they do not
know about HTTP or HTML.

The CLI and dashboard call these same services. CLI prompts and Desk forms
collect input, but validation, probing, transaction order, rollback, and status
derivation have one implementation.

### Sanitized setup snapshot

`GET /api/v1/desk/setup` returns:

- installation readiness;
- each area status and optional/required classification;
- sanitized provider/model/library records;
- available skill source capabilities and safe labels;
- verified Telegram bot identity when configured;
- pending private pairing summaries; and
- supported actions.

It never returns credentials, secret references, raw paths, unfiltered
configuration, environment names, command lines, PEM material, or upstream
error bodies.

### Mutations

Focused endpoints remain below `/api/v1/desk`:

| Area | Operations |
| --- | --- |
| Models | catalogue refresh, add model, set roles, enroll provider |
| Skills | discover source, stage selection, install, activate |
| GitHub | configure PAT, configure App, verify |
| Telegram | configure, verify, list private pairings, approve |

All mutations require the process request token and idempotency key. Credential
requests use strict body limits. Uploaded archives use a separate streaming
multipart endpoint with compressed and expanded limits. Provider, GitHub, and
Telegram changes schedule restart only after a committed transaction.

## Error Handling

Errors appear inline in the active task and preserve entered non-secret values.
Secrets are cleared. A page-level status is reserved for loss of the whole
Setup snapshot, not individual form failures.

The API maps known domain errors to stable codes and safe guidance, including:

- `provider_not_found`;
- `catalogue_refresh_failed`;
- `model_probe_failed`;
- `model_alias_conflict`;
- `skill_source_unavailable`;
- `skill_git_host_not_allowed`;
- `skill_commit_resolution_failed`;
- `skill_not_found_in_source`;
- `skill_archive_unsafe`;
- `skill_review_failed`;
- `skill_already_installed`;
- `github_credential_invalid`;
- `github_repository_denied`;
- `telegram_token_invalid`;
- `telegram_restart_failed`; and
- `pairing_not_found`.

Messages never include credentials, raw provider bodies, filesystem paths,
environment values, Git command output, or internal error chains. Unknown
errors retain a generic message and a correlation-safe operation identifier in
the audit trail.

## Accessibility and Interaction

- Setup tasks use real links, buttons, radios, checkboxes, and labeled inputs.
- Catalogue and skill choices are keyboard selectable and announce result
  counts, selection, pending state, errors, and success.
- Focus moves to the task heading on open, the first invalid control on
  validation failure, and the success heading after completion.
- Status never depends on color alone.
- Busy controls retain their label and add an accessible pending description.
- Back never discards a secret silently; secrets are cleared when leaving a
  task.
- The current warm cream, near-black, ginger, border, shadow, type, motion, and
  reduced-motion system remains unchanged.
- Mobile uses the same focused single-column tasks; it does not compress the
  former three-column page.

## Testing and Verification

### Domain tests

- Provider catalogue selection generates deterministic valid aliases, handles
  collisions, probes selected models, applies optional roles, and rolls back on
  every mutation failure point.
- Skill GitHub discovery resolves an exact commit, supports a selected
  subdirectory and multi-skill repository, enforces host policy, and records
  provenance.
- Archive staging rejects traversal, absolute paths, symlinks, special files,
  duplicate paths, compression abuse, excessive counts, and excessive bytes.
- All skill sources converge on identical audit, digest, inactive install, and
  activation behavior.
- PAT and GitHub App validation covers success, bad credentials, denied
  repositories, key parsing, token minting, rollback, and redaction.
- Telegram covers `getMe`, configuration, restart/health rollback, private
  pairing listing, approval, and group-origin denial.
- Concurrent provider/GitHub/Telegram mutations share the lock and cannot lose
  configuration or secret updates.

### HTTP and client tests

- Setup snapshots expose only allowlisted fields.
- Every known error maps to its stable safe code and secret-bearing errors are
  redacted.
- Credential inputs are cleared after both success and failure.
- Model rows are selectable and populate one confirmation task.
- Skill source methods match server capabilities; unusable methods do not
  render as working forms.
- Review and activation remain separate explicit actions.
- Keyboard, focus, live-region, narrow-screen, reduced-motion, request-token,
  idempotency, body-limit, Host, Origin, fetch-metadata, CSP, and no-store
  behavior receive regression coverage.

### Acceptance

The release gate includes:

```sh
mise run dashboard-check
mise run fmt
mise run vet
mise run lint
mise run test
mise run build
git diff --check
```

Current Safari is the authoritative live visual target for this goal. Manual
acceptance uses Safari through the forwarded managed Desk and proves:

1. select and add a real model from the current provider catalogue;
2. use the new model in a Desk conversation;
3. discover, review, install inactive, activate, and attach a real skill;
4. configure or verify repository-scoped GitHub access and exercise it in a
   workspace;
5. configure Telegram, verify the bot identity, receive a private pairing, and
   approve it in Desk;
6. confirm all credential fields remain blank after submission and reload;
7. confirm optional unset integrations do not degrade Waffle health; and
8. confirm the equivalent CLI reads the same resulting state.

Managed-host completion additionally requires current service health, restart
behavior, logs without credential leakage, and successful rollback evidence for
one injected transactional failure. Local fixtures or screenshots alone do not
prove the managed setup goal complete.

## Documentation

- `docs/waffle-desk.md` describes Setup as the primary interactive path.
- `docs/chat.md` and `docs/deploy.md` distinguish ordinary user chat from
  privileged setup mutations.
- GitHub documentation explains PAT and App permissions without asking users to
  edit TOML on the normal path.
- Telegram documentation starts with **Connect Telegram** and retains CLI
  recovery commands.
- Skill documentation explains review, inactive installation, activation,
  exact commit provenance, archive limits, and why private repositories are not
  implicitly authorized.
- `config.example.toml` retains advanced policy controls but no longer serves as
  the primary onboarding interface.
