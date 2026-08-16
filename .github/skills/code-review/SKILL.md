---
name: code-review
description: How to review changes in the Waffle repository — where the real risk lives, which conventions look like defects but are deliberate, and what to check before reporting a finding.
---

Waffle is a personal AI agent: one Go binary serving exactly one owner on that
owner's own hardware, plus an Astro website under `website/`. Sandbox, network,
and tool policy are deny-by-default throughout.

`.github/copilot-instructions.md` carries the architecture and commands. This
file is about reviewing.

## Where the real risk is

Weight attention here. A missed defect in these areas is a security or data
problem, not a style nit.

1. **Trust boundaries.** Agent groups define the ceiling; agent profiles select
   within it. A profile must never widen its group's tool or sandbox policy, and
   repo policy can only tighten further. Flag any change that lets a narrower
   context gain capability.
2. **Credential paths.** Sandboxes never receive raw provider or Git
   credentials — they get scoped, expiring broker tokens while real secrets stay
   host-side. Flag anything that moves a secret across that boundary, or that
   logs or renders one without redaction.
3. **Fail-closed behaviour.** Every credential, network, and verification path
   prefers a hard error over a degraded default. A new fallback, a swallowed
   error, or a "if we can't check, allow it" branch is a defect here even when
   it reads as robustness.
4. **Migrations.** Ordered, embedded, contiguous version numbers, and every one
   must stay safe to apply to a database already in the wild. Flag renumbering,
   gaps, and anything destructive without a stated rollback.
5. **SQLite access patterns.** The store opens WAL with a **single connection**.
   Long transactions, foreground maintenance, and per-row commits on request
   paths are correctness problems, not performance opinions.
6. **The status listener stays loopback-only** in every configuration.

## Deliberate choices — do not report these

Each of these has been reviewed before and is intentional.

- **`website/src/styles/docs.css` omits Tailwind.** Docs pages are Starlight;
  the homepage is Tailwind. Two resets in one page is the failure mode.
- **Brand tokens are duplicated** between `global.css` and `docs.css`, and a
  test enforces that they stay identical. Shared values, separate machinery.
- **`docs.css` copies Starlight's selector lists verbatim**, including
  `::backdrop` entries. Overrides must land on exactly the selectors carrying
  the values they replace.
- **The screenshot capture script duplicates a small fixture bootstrap** instead
  of importing it from the Desk spec, whose top-level test registrations would
  otherwise execute.
- **`config.example.toml` is strict on purpose.** Absence of a permissive
  fallback is the feature.

## Before reporting a finding

- **Check whether the pattern is copied from a dependency.** Several files
  deliberately mirror upstream structure so overrides line up. Look at the
  vendored source before calling duplication a bug.
- **Prefer a failure scenario over a rule.** "This is inconsistent" is weak;
  "with input X this returns Y and the caller assumes Z" is actionable.
- **Do not ask for assertions that pin formatting.** Tests here match intent —
  a formatter run must never fail a test whose subject has not changed.
- **Check the test file before saying something is untested.** Tests sit beside
  their implementation as `*_test.go`. The website's tests live in
  `website/tests/`.

## Conventions

Errors wrap with `%w`. `nolint` suppressions carry a documented reason. Package
names are short and lowercase, and a new package gets a `// Package x ...`
header explaining its boundary. Tests are table-driven and named
`TestBehavior`, with coverage for failure, cancellation, persistence, and
concurrency paths. Commits are Conventional Commits.

Generated files are committed: `internal/dashboard/ui/*_templ.go` must be
regenerated and committed with any `.templ` change, or CI fails.
