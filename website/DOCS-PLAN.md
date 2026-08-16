# Waffle documentation site — plan

Status: **in progress**. Decisions in §10 are settled. Phase 1a (infra) and
Phase 1b (foundation) are built; Phase 2 onward is still ahead. This document
stays the brief the remaining phases work from.

Design lineage: `design-exploration/BRAND_BRIEF.md`,
`design-exploration/A-sunlit-kitten/CONCEPT.md`, `design-exploration/DECISION.md`.
Character rules: `assets/brand/waffle/canon/character-canon.md`.
Operator vocabulary: `CONTEXT.md`.
Deployment control plane: `matt-riley/infra` (Cloudflare Pages + Terraform).

## 1. Who this is for

**Primary reader: non-technical.** Someone who has heard about Waffle, likes
the cat, and wants to know what she is and how to have one. They do not know
what a daemon is, have never heard of a sandbox broker, and will bounce off a
wall of `[agent.profile.main]`.

**Secondary reader: technical.** Someone deploying Waffle on a real host,
writing policy rules, or wiring a provider. They need exact keys, exact paths,
and no hand-holding.

The site serves the primary reader by default and lets the secondary reader
descend on demand. Descending is a **structural** device, not a tone shift:
every plain-language page ends with one link into its technical counterpart.

## 2. Principles

1. **Clarity outranks charm.** The voice from `BRAND_BRIEF.md` (warm,
   affectionate, a bit mischievous) lives in page intros, section names, and
   callout labels. It never lives inside an instruction. A step is a step.
2. **One idea per page.** Non-technical readers scroll; they don't hunt.
3. **No orphan jargon.** First use of an operator term links to the glossary.
   `CONTEXT.md` terms (First Deployment, Managed Setup, Installed, Ready,
   Provider Connection, Model Alias) are used precisely in technical pages and
   introduced gently — never assumed — in plain-language pages.
4. **The cat is a guide, not a mascot assistant.** She appears at section
   entrances and in callouts. She does not float, follow the cursor, or speak
   in the first person about features.
5. **Every fact has a source in the repo.** Where possible the site derives from
   the repo rather than restating it, and a test fails when they diverge (§7).
6. **No marketing gravity.** No "Get started free", no trust-logo wall, no
   fabricated metrics. This stays a personal project that happens to be
   documented well.

## 3. Information architecture

Three tiers in one sidebar. URLs live under `/docs/`; the existing hand-built
marketing homepage at `/` is untouched.

### Tier 1 — Meet Waffle (plain language, no jargon)

| Page | Answers |
| --- | --- |
| `what-waffle-is` | What she is, what she is not, why she exists. One screen. |
| `what-she-can-do` | Conversation, memory, doing things with tools, jobs that run on a schedule — in human terms. |
| `bringing-her-home` | The happy path end to end: install, `waffle setup`, connect a model provider, first conversation. Screenshots at every step. |
| `talking-to-her` | The three front doors: terminal chat, Telegram, Waffle Desk. When you'd want each. |
| `teaching-her` | Memory, notes, and skills explained without the word "corpus". |
| `keeping-her-safe` | What she can and cannot reach, why the sandbox exists, what you are trusting and what you are not. The most important page on the site. |
| `when-somethings-wrong` | Friendly triage: `waffle doctor`, `waffle status`, where the logs are, how to back out. |
| `glossary` | Every operator term in one place, plain definition first. |

### Tier 2 — Under the hood (technical)

Architecture and trust model · Agent profiles and groups · Policy rules ·
Sandbox, broker, and network lock · Providers and Model Aliases · Memory and
storage · Skills, plugins, and learning · MCP and code intelligence ·
Waffle Desk · Managed host deployment · Self-development (doctor / upgrade /
rollback).

### Tier 3 — Reference

Configuration keys (derived from `config.example.toml`) · CLI command reference
(one entry per `waffle` subcommand) · Release and compatibility notes.

### The descent

Every Tier 1 page ends with a single **Nerd corner** link to its Tier 2 page.
Every Tier 2 page opens with a one-line "in plain terms" link back up. That
pair is the whole mechanism for "more technical documentation available if
required" — no tabs, no toggles, no duplicated page trees.

## 4. Art direction

The docs inherit **Concept A · Sunlit Kitten** and extend it for long-form
reading. Existing tokens in `website/src/styles/global.css` carry over unchanged.

### Reading surface

- Ground stays paper `#FBF7F0`; long-form body stays Source Serif 4 at
  ~1.06rem with a 66–70ch measure. Nunito for headings, sidebar, and UI chrome.
- Code blocks are a **warm card on paper**, not a dark slab dropped into a cream
  page: `paper-warm #F5EDE0` ground, `outline #613619` at low alpha for the
  frame, warm-toned syntax colours.
- Sidebar is quiet type on paper — no boxes, no zebra. Active item marked by a
  ginger rule, reusing `.ginger-rule`.

### Contrast rules (non-negotiable, measured against paper `#FBF7F0`)

| Colour | Ratio | Allowed for |
| --- | --- | --- |
| `ink #1A1612` | 17:1 | Body text, headings |
| `ink-muted #5C4A3A` | 7.9:1 | Secondary text |
| `label #8D481F` | 6.4:1 | Links, labels, small text |
| `ginger #E99A42` | **2.2:1** | Rules, borders, fills, icons — **never text** |

This is the "ginger is accent only" rule from the brief, restated as an
accessibility constraint so it survives contact with a docs site.

### Dark mode ("evening") — agreed, and accessible

Docs get read at night; the marketing page does not need it but the docs do.
Ground `#17130F`, raised surface `#211B15`, text `#F2E9DC`, muted `#C4B2A0`. On
that ground `ginger #E99A42` reaches 8.2:1 and `ginger-light #F5C579` reaches
11.6:1, so on dark the accent *may* carry text — the paper-mode ban on ginger
text does not apply in reverse. Cat PNGs are transparent and sit correctly on
both grounds.

"Accessible" here means specific, checkable things, all of which are Phase 1
acceptance criteria rather than aspirations:

- Every text/background pair in both themes meets WCAG AA (4.5:1 body, 3:1
  large text and UI borders). The tables above are the source values.
- The theme toggle is a real `<button>` with an accessible name and pressed
  state, is keyboard reachable, and never traps focus.
- Default follows `prefers-color-scheme`; an explicit choice persists and wins.
- No flash of the wrong theme — the choice is applied by a tiny inline script in
  `<head>` before first paint, matching the pattern the site already uses for
  the motion guard.
- Focus rings stay visible in both themes (the current `:focus-visible` ginger
  ring needs a lighter variant on the evening ground).
- Code blocks get a light and a dark syntax theme, both AA-checked — a single
  theme tinted two ways is what usually fails here.
- Cat art and screenshots are checked on both grounds; anything that only reads
  on paper gets a dark-mode variant or is dropped.

### How the cat appears

Sparingly, and always from the approved canon — no new cat art is invented.

- **Docs landing:** a splash page using `poses/sitting-airplane-ears.png` and
  two doors: "I just want to use her" → Tier 1, "I want to know how she works"
  → Tier 2.
- **Section entrances:** one canon expression head at ~28px beside each sidebar
  group heading.
- **Callouts:** four, each bound to one canon expression, so the reader learns
  the vocabulary by face:
  - `pleased` → **Waffle's tip** — a nicety, safe to skip.
  - `startled` → **Careful** — destructive, security-relevant, or hard to undo.
  - `focused` → **Nerd corner** — technical detail, collapsed by default.
  - `sleepy` → **Later** — optional, or not needed on a first pass.
- **Budget:** at most two cat images per page, and never two in a row. This is
  the guard against mascot fatigue, which is the single most likely way this
  art direction goes wrong.
- **404 and empty states:** `startled`.

### Motion

Docs pages ship no GSAP. The animation rig
(`assets/brand/waffle/rigs/standing-v2/motions/`) stays on the marketing
homepage and, at most, the docs splash. Docs body content gets nothing beyond
CSS fades, and honours `prefers-reduced-motion` as the site already does.

### Screenshots

Terminal chat and Desk screenshots get one consistent treatment (warm frame,
rounded corners, soft shadow, fixed capture width) and live in
`website/src/assets/screenshots/`. Desk shots are **captured by the existing
Playwright rig** in `tools/dashboard-tests` rather than taken by hand, so they
regenerate instead of going stale.

## 5. Technical approach

**Agreed: Astro Starlight, heavily themed, mounted at `/docs/`.**

Why: it brings sidebar, table of contents, prev/next, Pagefind search,
Expressive Code code blocks, heading anchors, and a properly keyboard- and
screen-reader-accessible shell — all of which we would otherwise hand-roll and
get subtly wrong. Its visual identity is almost entirely CSS custom properties,
so "Sunlit Kitten" is a token map plus a small number of component overrides
(header, footer, page title, callouts).

What stays bespoke: `/` and `/404` keep their current hand-built components and
GSAP motion. Starlight only owns routes it generates from its content
collection; nesting that collection one level deep yields `/docs/...` URLs while
leaving the homepage alone. (Exact routing mechanics get verified in Phase 1 —
if nesting proves awkward, the fallback is Starlight at the root with the
marketing page kept as an explicit page route, which Astro resolves in favour of
`src/pages/`.)

Rejected alternative: hand-rolled content collections plus a custom sidebar and
standalone Pagefind. More control, roughly two to three times the work, and it
puts the accessibility burden on us. Worth revisiting only if Starlight's
chrome fights the art direction harder than expected — which we will know by
the end of Phase 1, deliberately, before most content exists.

Dependencies added: `@astrojs/starlight` (which pulls Expressive Code and
Pagefind). No React, Vue, or Svelte. Tailwind 4 stays.

## 6. What happens to `docs/*.md`

Today `docs/` mixes two audiences: operator guides (`chat.md`, `deploy.md`,
`usage-guide.md`, `waffle-desk.md`, `code-intelligence.md`, `sandbox-queue.md`)
and contributor material (`plan.md`, `research.md`, the audits and issue notes).

Agreed: **the site becomes canonical for operator-facing material; contributor
material stays in the repo.** As each operator guide is absorbed into Tier 2,
the in-repo file is replaced by a short pointer so existing README links and
bookmarks keep working. `plan.md`, `research.md`, and the audit notes are linked
from the site but never mirrored.

The alternative — writing fresh site prose and leaving `docs/` intact — is
rejected because two live copies of deployment instructions will diverge, and
the wrong one will be the one someone follows.

## 7. Keeping the site honest

Documentation rots quietly. Three mechanical guards, each a CI failure:

1. **CLI coverage test.** A Go test in `cmd/waffle` asserts every subcommand in
   the dispatch switch has a reference page, and every reference page maps to a
   real subcommand. A new command lands undocumented → CI red.
2. **Config coverage test.** The configuration reference derives from
   `config.example.toml`; a test asserts every `[section]` in the contract has
   prose on the site.
3. **Link check.** Internal links and anchors verified on every build.

Plus the existing `website/tests/site.test.mjs` pattern, extended to cover docs
nav and the Tier 1 → Tier 2 descent links.

## 8. Build, CI, and hosting

### Waffle-repo CI (gap to close first)

`website/` has tests (`npm test`) and **no CI job at all** — nothing in
`.github/workflows/ci.yml` touches it. Phase 1 adds a `website` job: install,
build, `node --test`, link check. Without it the site can go red and merge.

### Hosting: Cloudflare Pages, driven from `matt-riley/infra`

Deployment is owned by the infra repo's control plane, not by this repo. The
site follows the checked-in `provider: cloudflare` + `deploy_kind: pages-static`
contract (`docs/operator/add-cloudflare-pages-app.md`), the same path as
`snurble`. Four changes, all in `matt-riley/infra`:

**1. Catalog entry — `catalog/apps/waffle-site.yaml`.** The filename must *not*
be `waffle.yaml`: that is already the Hetzner host entry (`provider: hetzner`,
`deploy_kind: binary-systemd`), the deploy workflow resolves apps by filename,
and its discover step filters to `provider: cloudflare`, so the host entry stays
untouched.

```yaml
app_id: waffle-site
source_repo: matt-riley/waffle
provider: cloudflare
deploy_kind: pages-static
environment: prod
terraform_stack: stacks/prod/cloudflare/app-waffle-site
zone_stack: stacks/prod/cloudflare/zone-mattriley-tools
zone_name: mattriley.tools
project_name: waffle-site
pages_project_name: waffle-site
production_branch: main
custom_domain: waffle.mattriley.tools
domain:
  apex: waffle.mattriley.tools
  aliases: []
build:
  package_manager: npm
  node_version: "26"
  working_directory: website
  output_directory: dist
  env:
    PUBLIC_SITE_URL: https://waffle.mattriley.tools
```

Field notes, each checked against the workflow rather than assumed:

- Install, build, and `wrangler pages deploy` all run in
  `source/<working_directory>`, so `output_directory` is relative to `website/`
  and is just `dist`.
- `npm`, because `website/package-lock.json` is the lockfile and the workflow
  runs `npm ci`. The repo's pnpm pin is for brand tooling, not the site.
- `node_version: "26"` matches the mise pin (26.1.0); drop to `"24"` if
  `setup-node` cannot resolve 26.
- **`build.env.PUBLIC_SITE_URL` is load-bearing.** `astro.config.mjs` already
  reads it, and without it every canonical and OG URL builds relative. This is
  the one field whose absence degrades silently rather than failing the build.

**2. Terraform root — `stacks/prod/cloudflare/app-waffle-site/`.** Copy the
`app-snurble/` file set (`backend.tf` with its own R2 state key, `main.tf`
calling `modules/cloudflare/pages-static-site`, `providers.tf`, `variables.tf`,
`outputs.tf`, `terraform.tfvars`).

**3. One DNS record.** Append to `additional_pages_records` in
`stacks/prod/cloudflare/zone-mattriley-tools/terraform.tfvars`:

```hcl
{ name = "waffle", pages_project_name = "waffle-site" },
```

The existing zone root already handles subdomains under `mattriley.tools` — the
`snurble` precedent — so **no new zone root is needed**. `waffle.mattriley.tools`
is currently unused; the Hetzner host is reached over the tailnet, not this zone.

**4. State-recovery mappings.** `.github/workflows/recover-terraform-state.yml`
hardcodes per-app imports (Pages domain + DNS record). A new app that skips this
is silently absent from R2 state recovery — add both mappings in the same PR.

### Sequencing trap

Terraform plan/apply trigger only on `bootstrap/**`, `modules/**`, and
`stacks/**`. A catalog-only PR **will not run Terraform**, so the catalog entry
and the stack root must land in the same infra PR or the Pages project is never
created and the first deploy fails.

### Deploy trigger and latency

The Cloudflare deploy workflow runs on `workflow_dispatch`,
`repository_dispatch` (`deploy-app`), and a daily 08:00 UTC schedule. It
compares the source repo's production-branch SHA against the latest production
Pages deployment and no-ops when they match — so merges to waffle `main` go live
within a day on their own. Recommendation: accept the daily reconcile at first
and add a `repository_dispatch` ping from waffle CI in Phase 4 only if the
latency is annoying in practice. That keeps the initial cross-repo surface to
one PR.

### URLs

Marketing homepage at `https://waffle.mattriley.tools/`, docs at
`https://waffle.mattriley.tools/docs/`. One Pages project, one origin, one
deploy — which is also why docs live at `/docs/` rather than on a `docs.`
subdomain.

## 9. Phases

| Phase | Contents | Rough size |
| --- | --- | --- |
| 0 | This plan; decisions settled (§10) | done |
| 1a | **Infra PR** (`matt-riley/infra`): catalog entry, `app-waffle-site` stack root, zone record, state-recovery mappings. Lands first so the Pages project exists before anything is pushed at it. | done — matt-riley/infra#60 |
| 1b | **Foundation** (this repo): Starlight installed and themed to Sunlit Kitten, `/docs/` routing, accessible dark mode with its acceptance criteria, the four callout components, splash landing, search, `website` CI job. Ships with three real pages so the theme is proven against actual content, not lorem. | done |
| 2 | **Tier 1 complete.** The remaining six plain-language pages, glossary, screenshot capture recipe. | 2–3 PRs |
| 3 | **Tier 2 + reference.** Migrate operator guides out of `docs/`, add config and CLI references with their drift tests. | 3–4 PRs |
| 4 | **Polish.** Per-page OG images using canon art, search tuning, Lighthouse/a11y budget in CI, `docs/` pointer cleanup, optional `repository_dispatch` deploy ping. | 1 PR |

Phase 1b is deliberately front-loaded with theming risk: if Starlight cannot be
made to look like Waffle, we find out while there are three pages to port, not
thirty.

## 10. Settled decisions

1. **Hosting.** Cloudflare Pages via the `matt-riley/infra` control plane,
   following the `snurble` pattern. Domain `waffle.mattriley.tools`, docs at
   `/docs/` on the same origin. Wiring in §8.
2. **`docs/*.md`.** Absorb the operator guides into the site; leave pointers
   behind. Contributor material stays in-repo. (§6)
3. **Framework.** Starlight. (§5)
4. **Dark mode.** Yes, with the accessibility criteria in §4 as Phase 1
   acceptance, not aspiration.
5. **Tier 1 scope.** All eight pages — no trimmed first cut.

Nothing is left blocking. Phase 1a can start against `matt-riley/infra`.

## 10a. Deviations from this plan

Recorded as they happen, so the plan and the tree do not quietly diverge.

- **Homepage nav entry moved from Phase 2 to Phase 1b.** Shipping a docs site
  with no link to it from the homepage made no sense for a single saved commit
  of effort. `SiteHeader`, `SiteFooter`, and the nav test moved together.
- **Docs pages carry no Tailwind.** The plan said "themed Starlight"; the
  implementation makes that concrete by giving `/docs/` its own stylesheet with
  no Tailwind import, because two CSS resets in one page is how themed docs
  break. Token *values* are shared with `global.css` and a test enforces that
  they stay identical.
- **Syntax themes are `github-light` / `github-dark`, not warm-tinted ones.**
  Warmth comes from the code card (background, border, radius) rather than from
  recolouring syntax tokens, because both stock themes ship contrast-checked
  colours and a hand-tinted theme is the usual way dark-mode code fails AA.

## 11. Risks

- **Mascot fatigue.** The most likely failure. Mitigated by the two-image page
  budget and by keeping the cat at entrances rather than in the prose.
- **Ginger overload.** Mitigated by the measured contrast table in §4 — ginger
  is mechanically barred from text.
- **Starlight upgrades moving the theme.** Mitigated by keeping overrides few
  and token-driven, and by a visual check in the website CI job.
- **Duplication drift** between site and `docs/`. Mitigated by §6 and §7.
- **Voice creep into instructions.** Warmth in the intro, precision in the
  steps. Anything else and a non-technical reader cannot tell whether a line is
  a joke or a command they must type.
- **Scope creep into marketing.** The homepage is finished and stays finished;
  this work adds `/docs/`, not a redesign.
- **Cross-repo sequencing.** The site cannot deploy until the infra PR is
  merged and applied, and a catalog-only infra PR does not run Terraform at all
  (§8). Phase 1a exists to get that wrong early and cheaply.
