# Provider doctor probe design

## Goal

Make `waffle doctor` verify that its configured LLM provider is reachable and
authenticated, so `waffle upgrade` refuses to install a build with unusable
provider configuration.

## Design

`selfdev.Doctor` will use the config it already loads to run one provider
probe. It resolves the configured provider API key through the secret store
using the same provider-specific environment fallback as normal startup. A
missing key is reported as a passing `skipped` check; an explicitly configured
secret reference that cannot be resolved is a failing check.

For a resolved key, doctor constructs the existing Anthropic or
OpenAI-compatible adapter and calls `Complete` with one short user prompt and
`MaxTokens: 1`. The request runs under a five-second child context. A completed
authenticated response is a passing `provider reachable` check. Invalid
provider names, endpoint errors, authentication failures, and timeout errors
are failing checks and therefore make the aggregate doctor result false.

The check sends no tools, stores no conversation data, and makes no retry. Its
one-token limit bounds cost while exercising the exact provider path used by
the agent.

## Tests

Tests will configure the OpenAI-compatible adapter against `httptest` servers
to cover successful authentication, rejected authentication, and a deadline.
They will also cover missing-key skip behavior and unresolved key-reference
failure. The aggregate doctor result must be false when the provider check
fails.
