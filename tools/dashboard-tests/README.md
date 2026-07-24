# Waffle Desk browser checks

This package drives the production embedded Desk handler through a deterministic
Go fixture. The fixture listens only on an ephemeral `127.0.0.1` port, prints
that URL once on stdout, and does not contact providers, Git hosts, containers,
systemd, or any other network service.

Prerequisites:

- Node 26.1.0
- pnpm 11.9.0
- system Google Chrome
- the repository-pinned Go toolchain

Run from the repository root:

```sh
mise run dashboard-check
```

The suite uses Chrome rather than Playwright's bundled Chromium. It verifies the
five embedded sections; the Host, origin, CSRF, response-header, and redaction
boundaries; Today streaming, cancellation, and SSE recovery; task handoff;
guarded workspace and memory lifecycles; reviewed skill activation; keyboard
navigation and dialog focus return; reduced motion; document overflow at 1470,
768, 375, and 320 CSS pixels; and an explicit 200-percent zoom gate.

Failure screenshots and Playwright result files are local artifacts and are
ignored. Traces are disabled because provider-enrollment coverage must never
capture a credential-bearing request body.
