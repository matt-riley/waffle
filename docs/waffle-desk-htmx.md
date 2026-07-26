# Waffle Desk htmx provenance

Desk ships htmx as an embedded, same-origin asset. It is not loaded from a CDN
at runtime.

- Upstream package: `htmx.org`
- Pinned version: `2.0.7`
- Source: `https://cdn.jsdelivr.net/npm/htmx.org@2.0.7/dist/htmx.min.js`
- Embedded SHA-256: `6cf37d968150607c38666e3b73d66bd3522ef44b247cd15f17b7539cf8b032ab`

The embedded runtime is configured by the external Waffle request bridge to
disable eval and response script tags. The bridge injects the current Desk
token and stable intent-bound idempotency key, serializes marked form requests
as JSON, and clears a key only after a successful response. The server keeps
the existing JSON media type as the default and uses `text/html` only for an
explicit HTML or htmx request.
