# Contributing to waffle

Thanks for taking a look. Waffle is a personal AI agent that serves exactly one
owner and runs on that owner's own hardware, and it is in developer preview:
config keys, flags, and SQLite migrations can still change. Bug reports are
welcome if you actually ran it, as are focused pull requests — but this is a
personal agent, not a feature-intake queue. If you are planning something
large, open an issue before writing it.

Please report security vulnerabilities privately rather than through an issue —
see [SECURITY.md](SECURITY.md).

## Getting set up

`mise` owns the pinned toolchain (Go, Node, pnpm), so you do not need to install
those yourself:

```sh
mise install     # installs every pinned tool
mise tasks       # lists everything you can run
```

`go.mod` declares the Go *language* version separately from the toolchain `mise`
installs. The two are deliberately different numbers; don't sync them.

To run the agent locally:

```sh
go run ./cmd/waffle setup    # secret identity, provider enrollment, profile
go run ./cmd/waffle chat
```

## Everyday commands

| Command | What it does |
| --- | --- |
| `mise run build` | Version-stamped `bin/waffle` |
| `mise run test` | The full gate: templ regeneration, Desk client tests, `go test -race ./...`, and the zero-network `waffle eval` |
| `mise run fmt` | `gofmt` check |
| `mise run vet` | `go vet ./...` |
| `mise run lint` | `golangci-lint run ./...` |
| `mise run dashboard-generate` | Regenerate Waffle Desk templ components |
| `mise run dashboard-check` | The full browser gate (Playwright/Chrome) |

While iterating, run one package rather than the suite:

```sh
go test ./internal/agent -run TestName
```

`internal/workspace` alone is about half of the race suite's runtime, so expect
`mise run test` to take a while.

## Tests

New functionality needs tests. Non-trivial logic should leave behind one
runnable check; a genuinely trivial one-liner does not need one.

- Table-driven, named `TestBehavior`, colocated with the code as `*_test.go`.
- Add regression coverage for the paths that actually break in production:
  failure, cancellation, persistence, and concurrency.
- Some suites are opt-in behind build tags — see
  `website/src/content/docs/docs/under-the-hood/sandbox.md` for the sandbox
  stress and Docker tags. Live provider evals need `WAFFLE_EVAL_LIVE=1` and skip
  otherwise, so the default suite stays zero-network.

## Code style

- Format with `gofmt`. `goimports`, `misspell`, and `unconvert` run under
  golangci-lint on top of the standard set.
- Wrap errors with context using `%w`.
- Document any `nolint` suppression — `nolintlint` requires it.
- Short lowercase package names, `PascalCase` exported, `camelCase` unexported.
- When adding a package, write a `// Package x ...` doc comment explaining the
  boundary and the decision behind it, and reference the driving issue or the
  relevant `docs/plan.md` section. The rationale lives in the package headers.

Keep changes proportionate to the problem. The shortest diff that solves it,
once you understand it, is the one to send: prefer reusing an existing helper
over a new abstraction, and prefer deleting code over adding it. Fix causes
rather than symptoms — if a shared function is wrong, grep its callers instead
of patching only the one the issue names.

## Things that bite

**Generated files are committed.** `internal/dashboard/ui/*_templ.go` is
generated from `.templ` sources. Run `mise run dashboard-generate` and commit
the result with any `.templ` change, or CI fails on the dirty-tree check.

**Migrations are ordered and permanent.** Schema changes are embedded SQL files
in `internal/store/migrations/`. Version numbers must stay contiguous, and every
migration must remain safe to apply to an existing database.

**Policy is deny-by-default, and stays that way.** Sandbox, network, tool, and
secret policy all fail closed by design. If your change touches the gateway,
broker, MCP, sandbox, policy, or workspace code, preserve that: a hard error is
the correct outcome, never a permissive fallback. Repo policy can tighten a
group's permissions, never widen them.

**Never commit secrets.** `config.toml`, generated databases, and identities are
all ignored for a reason. `config.toml` holds `secret://` references only, never
secret values. Use `config.example.toml` as the configuration contract — it is
strict, so don't add tolerant fallbacks for unknown or invalid keys.

## Commits and pull requests

Commits follow [Conventional Commits](https://www.conventionalcommits.org) with
focused, imperative subjects. release-please consumes them to drive versioning
and `CHANGELOG.md`, so the type matters:

```
feat: add workspace cleanup
fix: handle cancelled runs
fix(#68): reject stale broker tokens
docs: clarify deployment
build(deps): bump sqlite
```

A pull request should:

- Explain what behavior changed, and link the issue it closes.
- List the verification commands you actually ran.
- Call out configuration or migration effects explicitly. Migrations, secret
  store changes, and sandbox or network policy changes are
  compatibility-sensitive — describe rollback and any operational change they
  require.
- Include screenshots only when user-visible output changed.

CI runs the race suite in shards along with lint, vet, the browser gate, the
website build, and the zero-network eval. Please get it green rather than
leaving it for review.

## Conventions for AI agents

`CLAUDE.md` and `AGENTS.md` carry the same conventions in agent-facing form and
should stay consistent with each other. `CONTEXT.md` is the operations
vocabulary — First Deployment, Managed Setup, Installed vs Ready, Provider
Connection, Model Alias — and operator-facing text should use those terms.
