# Authenticated Model Catalogue Design

**Status:** Approved for implementation

## Summary

Waffle will discover model catalogues from authenticated provider connections so
an operator does not need to find and type upstream model identifiers during
host setup. The complete catalogue is cached locally for browsing, while only
models explicitly selected as favourites become durable Waffle model aliases.

The common setup path becomes: choose a provider preset, enter its API key, and
select a default model. OpenAI, Anthropic, and OpenRouter presets supply their
standard API URLs. A custom URL is required only for an OpenAI-compatible
provider.

Discovery is an aid, not a runtime dependency. Existing explicit
`--model ALIAS=UPSTREAM` enrollment remains supported, selected models still
receive Waffle's existing completion probe, and providers without a usable
model-list endpoint retain a manual enrollment path.

## Goals

- Discover the models available through an authenticated provider connection.
- Make bare `sudo waffle provider add` usable without knowing provider URLs or
  model identifiers in advance.
- Keep a complete, searchable local catalogue without copying every provider
  model into Waffle configuration.
- Let operators select a small set of favourite models for deterministic Waffle
  aliases, including default and utility roles.
- Avoid repeatedly calling provider model-list endpoints while making newly
  published models visible within a reasonable period.
- Preserve provider-neutral deployment: credentials and models remain
  application-owned data configured on the host.

## Non-goals

- Automatically enabling every discovered model.
- Dynamically choosing models by price, popularity, latency, or capability.
- Automatically replacing configured aliases when a provider catalogue changes.
- Treating catalogue presence as proof that a model supports Waffle. Selected
  models still pass the existing provider probe before configuration commits.
- Inferring Waffle token limits from provider-specific context-window metadata.
- Sending provider credentials or model choices through Infra or shared CI.

## Provider presets

The interactive provider prompt and `--type` flag accept four user-facing
choices:

| Choice | Runtime type | Default base URL |
| --- | --- | --- |
| `openai` | `openai` | `https://api.openai.com/v1` |
| `anthropic` | `anthropic` | `https://api.anthropic.com` |
| `openrouter` | `openai` | `https://openrouter.ai/api/v1` |
| `openai-compatible` | `openai` | none; the operator must supply one |

Presets are CLI conveniences. OpenRouter and OpenAI-compatible connections keep
using Waffle's OpenAI-compatible runtime adapter and existing persisted
provider schema. Later catalogue refreshes classify a connection as OpenRouter
only when its normalized effective URL has the exact `openrouter.ai` host or an
`openrouter.ai` subdomain such as `eu.openrouter.ai`; every other OpenAI runtime
connection uses the generic `/models` endpoint. This URL rule survives process
restart and cache deletion without adding a provider-specific configuration
field. When no connection name is supplied interactively, it defaults to the
selected preset name and remains subject to the existing collision and slug
validation rules.

An explicit `--base-url` overrides a preset URL. The CLI validates that custom
URLs are absolute HTTP or HTTPS URLs and continues to apply Waffle's existing
provider configuration validation.

## Interactive enrollment

Bare enrollment uses this order:

1. Collect the provider preset, connection name, and optional custom base URL.
2. Run privileged-host preflight before requesting or reading a credential.
3. Read the API key through the existing hidden-input path.
4. Attempt one authenticated catalogue refresh with the newly entered
   credential. New enrollment never reuses a previous cache because catalogue
   contents may be account-specific.
5. Let the operator select a default model, an optional utility model, and
   additional favourites, requiring at least one favourite overall.
6. Construct the existing complete `providerconfig.AddRequest`.
7. Probe every selected model and transactionally commit configuration,
   encrypted credentials, service state, and readiness exactly as today.
8. After provider enrollment succeeds, read back its newly committed catalogue
   scope and persist the fetched catalogue. Scope lookup or cache persistence
   failure warns but does not turn the committed mutation into a failure.

Example interaction:

```text
$ sudo waffle provider add
Provider (openai|anthropic|openrouter|openai-compatible): openrouter
Connection name [openrouter]:
API key (input hidden):

Discovered 327 available models.
Default model (search term, exact ID, or - for none): claude sonnet
  1. Claude Sonnet 4.6               anthropic/claude-sonnet-4.6
  2. Claude 3.7 Sonnet               anthropic/claude-3.7-sonnet
Select: 1
Utility model (search term, exact ID, or - to use default): gemini flash
Add another favourite? [y/N]: n

provider openrouter validated and saved
Waffle is Ready with default model anthropic-claude-sonnet-4-6
```

Catalogues containing at most 50 models are shown directly. Larger catalogues
start with a search prompt. Search is a case-insensitive substring match over
the upstream identifier and display name. Results are displayed 20 at a time;
the operator can move between pages or enter another search. An exact upstream
identifier is accepted even if it is not present in the current cache, but it
must still pass the normal completion probe.

The default and utility selections automatically become favourites. Choosing
`-` for utility retains Waffle's existing behavior of using the default model
for utility work. Choosing `-` for the default leaves the installation in the
Installed state. If neither role selects a model, Waffle requires one unassigned
favourite so the connection remains testable. Additional favourites do not
change either role.

Local aliases are generated from upstream identifiers by lowercasing, replacing
each run of characters outside `[a-z0-9]` with `-`, trimming surrounding
hyphens, and limiting the result to the existing 64-character alias maximum.
The generated alias is displayed. If it is empty, truncated into a collision,
or collides with an existing alias, Waffle asks for an explicit valid alias
instead of silently renaming or overwriting anything.

If discovery fails, interactive enrollment explains the sanitized failure and
offers the existing manual `ALIAS=UPSTREAM` entry. It does not abort merely
because catalogue discovery is unsupported.

## Catalogue and favourite commands

The full catalogue remains separate from configured model aliases:

```text
waffle provider models <connection> [--search QUERY] [--refresh] [--json]
waffle provider model add <connection> <upstream-id> [--alias ALIAS]
                          [--default] [--utility]
```

`provider models` loads a fresh cache without making a network request. A
missing or expired cache causes one authenticated refresh. `--refresh` forces
that attempt regardless of cache age. Human output shows display name,
upstream identifier, and available capability metadata. JSON output includes
the connection, fetch time, age, stale state, sanitized refresh warning when
present, and the model descriptors.

`provider model add` defaults the alias using the same deterministic conversion
as interactive enrollment. It probes the selected upstream model and commits
the new alias transactionally. `--default` activates the alias as the default;
`--utility` selects it for utility work; both flags may be used together. The
existing model activation and removal commands remain valid.

Existing deterministic automation is unchanged:

```text
credential-command | sudo waffle provider add \
  --name openrouter \
  --type openrouter \
  --model sonnet=anthropic/claude-sonnet-4.6 \
  --default sonnet \
  --api-key-stdin
```

Supplying any explicit `--model` bypasses catalogue discovery. Non-interactive
enrollment through `--api-key-stdin` or `--api-key-file` without at least one
explicit model remains an error because Waffle cannot safely prompt after
consuming a piped credential. Guided discovery is the bare, hidden-input
`provider add` flow; partial flag-based automation does not silently enter the
guided path.

## Catalogue adapters

Catalogue discovery is an optional capability beside `llm.Provider`; it is not
added to the core completion interface. A small provider-neutral descriptor
contains:

- exact upstream model identifier;
- optional display name;
- optional provider owner;
- optional context window; and
- optional advertised capabilities such as text output and tool calling.

Provider-specific clients translate their responses into these descriptors:

- **OpenAI:** authenticated `GET /models`, using `data[].id` and other stable
  fields when available.
- **OpenRouter preset:** authenticated `GET /models/user` so the catalogue
  respects account provider preferences, privacy settings, guardrails, and
  regional routing. A 404 from that extension falls back to `GET /models`.
- **Generic OpenAI-compatible:** authenticated `GET /models`. Because chat
  compatibility does not guarantee discovery compatibility, unsupported
  responses lead to cache/manual fallback rather than provider rejection.
- **Anthropic:** authenticated, paginated `GET /v1/models`, following pagination
  until completion or the fixed 100-page safety bound.

Discovery uses the same effective endpoint and authentication material as the
connection runtime. Credentials exist only in memory and are never placed in
catalogue records, cache keys, errors, logs, or command arguments.

Every provider enrollment also generates an opaque random catalogue-scope ID
and stores it inside the encrypted secret store beside that connection's API
key. It is not derived from the key and cannot be used to recover or compare
credentials. Existing connections receive a scope ID under the provider lock
the first time catalogue access is requested. Removing a provider deletes both
its credential and scope ID.

## Cache design

Catalogue caches live below Waffle's resolved application state directory in
`cache/model-catalogs/`. The directory mode is `0700`; regular cache files use
`0600`. Waffle rejects symlinks and non-regular cache targets, writes a temporary
file in the same directory, synchronizes it, and atomically renames it into
place.

Each versioned JSON record contains:

- schema version;
- connection name;
- effective provider runtime type;
- normalized base URL;
- opaque catalogue-scope ID;
- UTC fetch timestamp; and
- sorted model descriptors.

A cache record is applicable only when its connection name, runtime type,
normalized base URL, and opaque catalogue-scope ID match the current effective
connection. Credential material and credential fingerprints are deliberately
excluded. Successful provider removal attempts to invalidate that connection's
cache, while the changed scope ID guarantees that an invalidation failure still
cannot make a previous account's cache applicable after re-enrollment. New
provider enrollment never reads an old cache. A successful discovery writes a
new record only after the provider transaction commits.

The default time-to-live is 24 hours and is a code-level operational constant,
not a new required setting. A fresh cache is returned immediately. An expired
cache triggers one refresh for the command. A per-connection advisory file lock
prevents simultaneous processes from refreshing the same catalogue; after
acquiring the lock, a process rechecks the cache before making a request.
Atomic writes keep concurrent readers safe.

When refresh fails:

- a stale, valid cache is returned with its age and a visible warning;
- JSON marks `stale: true` and includes a sanitized refresh warning;
- a command that successfully returns stale catalogue data exits successfully;
  callers needing freshness inspect `stale`; and
- without any valid cache, the caller receives a sanitized discovery error and
  interactive setup offers manual entry.

Discovery requests have a 10-second timeout. Responses are limited to 16 MiB,
catalogues to 10,000 descriptors, descriptor string fields to 4 KiB, and
Anthropic pagination to 100 pages. Oversized, incomplete, or malformed provider
responses are rejected without replacing a good cache.

Provider-supplied identifiers, display names, owners, and capability labels
containing Unicode control characters are rejected before caching or terminal
rendering to prevent escape-sequence and multiline output injection. Bounded
provider error text is escaped before terminal display. A descriptor containing
the active credential is rejected. Catalogue clients do not follow redirects,
so an Authorization or API-key header cannot move to another origin.

## Failure handling and security

- Privileged-host preflight remains before secret input.
- API keys are accepted only through hidden input, standard input, or the
  existing protected key-file mechanism; raw key arguments remain forbidden.
- Provider response bodies and errors pass through Waffle's credential
  redaction and bounded-error handling before display or caching.
- A catalogue refresh cannot mutate provider configuration, model aliases,
  credentials, default selection, utility selection, or service state.
- Only explicit favourite selection enters the transactional mutation path.
- Alias collisions and existing provider/model references retain their current
  deny-by-default behavior.
- Cache failure never damages a previously working provider configuration.
- A cache write or invalidation failure after a successful provider transaction
  is reported as a warning but does not change the command's successful exit;
  host lifecycle reconciliation must still run after the committed mutation.

## Testing

Provider adapter tests use local HTTP servers and prove:

- exact request method, paths, authentication headers, and provider versions;
- OpenRouter `/models/user` behavior and 404 fallback;
- OpenAI-compatible parsing and authentication-free endpoints;
- Anthropic pagination;
- deterministic sorting and metadata normalization;
- cancellation, timeouts, non-success responses, malformed JSON, and response
  bounds; and
- credential redaction in every failure path.

Cache tests use an injected clock and prove:

- fresh-cache hits make no provider request;
- expiry and forced refresh make exactly one request;
- stale fallback and JSON freshness metadata;
- endpoint/type mismatch, corrupt records, and schema-version mismatch;
- permissions, symlink rejection, atomic replacement, and preservation of a
  good cache after a failed refresh; and
- concurrent refresh/read behavior under the race detector.

CLI and manager tests prove:

- provider presets and URL overrides;
- preflight occurs before credential read and discovery;
- small-list browsing, large-list searching, pagination, exact IDs, alias
  generation, and collision prompts;
- default, utility, and additional favourite selection;
- manual fallback when discovery is unavailable;
- `provider models` human and stable JSON output;
- transactional `provider model add` behavior and rollback;
- refresh after process restart and cache deletion still selects OpenRouter's
  `/models/user` endpoint by normalized host;
- provider removal invalidates its cache, including remove-and-re-add with a
  different credential;
- a failed cache invalidation followed by re-enrollment produces a new scope ID
  and cannot reuse the previous account's catalogue;
- post-commit cache write/invalidation failures warn without turning a committed
  provider mutation into a command failure;
- explicit `--model` bypasses discovery; and
- existing provider add/list/test/remove/model commands remain compatible.

Before delivery, run the repository's full `mise run fmt`, `mise run test`,
`mise run vet`, `mise run lint`, and `mise run build` verification set.

## Repository ownership

Catalogue discovery, caching, selection, and provider/model transactions belong
in Waffle. Infra continues to install a provider-empty application and expose
the on-host management command. Shared CI continues to transport only source
and artifact provenance. Neither Infra nor shared CI receives provider
credentials, provider URLs, catalogue data, favourites, or model aliases.

Infra's existing `waffle-admin.sh` wrapper treats provider mutations as
lifecycle-reconciling commands. It must classify the new
`provider model add` subcommand as mutating, with regression coverage alongside
the existing activate/remove cases. This is a command-classification change
only; Infra's deployment inputs, provider-empty seed, secrets boundary, and
automation requirements remain unchanged. No `matt-riley-ci` change is needed.

Waffle's `docs/deploy.md`, root `README.md`, CLI help, and
`config.example.toml` are updated to document preset URLs, authenticated
discovery, the 24-hour cache, favourites, manual fallback, catalogue browsing,
forced refresh, and model addition. Infra's existing provider-neutral docs do
not need provider or catalogue details.

## Compatibility and rollout

The persisted provider and model schema remains compatible. Existing
installations gain catalogue browsing without migration. Existing explicit
enrollment commands behave as before. Cache records are disposable derived
data: operators may delete them safely, and Waffle recreates them on the next
refresh.

The feature can be rolled back by deploying an earlier Waffle binary; configured
providers and favourite model aliases remain readable because the authoritative
configuration format does not change.

## References

- [OpenAI Models API](https://platform.openai.com/docs/api-reference/models/object)
- [OpenRouter Models API](https://openrouter.ai/docs/api/api-reference/models/get-models)
- [OpenRouter user-filtered Models API](https://openrouter.ai/docs/api/api-reference/models/list-models-user)
- [Anthropic List Models API](https://platform.claude.com/docs/en/api/models/list)
