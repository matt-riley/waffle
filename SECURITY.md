# Security Policy

## Supported versions

Only the default branch is supported with security updates.

| Version | Supported          |
| ------- | ------------------ |
| `main`  | :white_check_mark: |
| other   | :x:                |

## Reporting a vulnerability

Please report security issues **privately**. Do not open a public GitHub issue
for vulnerabilities.

Preferred channel: <https://github.com/matt-riley/waffle/security/advisories/new>
— GitHub private vulnerability reporting (*Security → Report a vulnerability*
on this repository). That is the `security@` path for this project; it is
private to the maintainer until an advisory is published.

Include enough detail to reproduce the issue: affected version or commit,
environment, and steps.

## What to expect

| Stage                  | Target                                                     |
| ---------------------- | ---------------------------------------------------------- |
| Acknowledgement        | within 3 working days                                       |
| Initial assessment     | within 7 days, including whether the report is accepted     |
| Fix on `main`          | within 30 days for high severity, 90 days otherwise         |
| Coordinated disclosure | on release of the fix, or 90 days after the report          |

Waffle is a single-owner agent, so the practical blast radius of most issues is
one operator's own hardware. Reports that cross a trust boundary — sandbox
escape, secret exfiltration through the broker, network policy bypass, or
policy-engine evasion — are treated as high severity.

We will credit reporters in the published advisory unless you ask us not to.

## Out of scope

- Findings that require the owner's own shell on the host, which already has
  full access to `$WAFFLE_HOME` by design.
- Configurations that deliberately disable a documented control, for example
  `[sandbox]` egress set to `full`.
