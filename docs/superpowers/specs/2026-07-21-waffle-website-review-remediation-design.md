# Waffle website review remediation design

## Goal

Resolve the six findings from the project-skill review of `website/` without changing the approved Concept A — Sunlit Kitten visual direction.

## Approved behavior

- Motion remains a one-shot GSAP entrance plus scroll reveals, but content fails open if the motion module does not initialize.
- Navigation labels describe their destination: `About`, `What she does`, and `Why Waffle`; `Notes` is not used for the namesake section.
- Every page exposes an immediate keyboard-only `Skip to content` link targeting `#main-content`.
- Canonical and social metadata use `PUBLIC_SITE_URL` when configured and retain a local fallback for development builds.
- Header, footer, and brand links provide at least a 44 by 44 CSS-pixel hit area without changing the visible type treatment.
- Unknown routes render a branded Waffle 404 with a direct path home.

## Architecture

`Layout.astro` owns shared metadata, the skip link, and the motion fail-open guard. `motion.ts` marks successful initialization. Existing section components own truthful IDs and labels. `global.css` owns focus-only skip-link presentation and hit areas. `404.astro` reuses the shared layout and approved Waffle artwork. Node's built-in test runner inspects source and generated output without introducing a test dependency.

## Failure handling

The inline motion guard removes the motion class and any animation-owned inline hiding if initialization has not completed within three seconds. Reduced-motion users continue to bypass GSAP initialization. Production builds should set `PUBLIC_SITE_URL`; local builds use Astro's local URL so development remains frictionless.

## Verification

- A source-level regression suite proves each requested contract.
- `npm test` builds the static site and inspects generated metadata and routes.
- Browser QA covers desktop and mobile rendering, keyboard order, in-page navigation, console health, 404 identity, overflow, and successful reveal completion.
