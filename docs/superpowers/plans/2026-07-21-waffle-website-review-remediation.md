# Waffle Website Review Remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the six validated website review findings while preserving the approved Sunlit Kitten design.

**Architecture:** Shared document concerns stay in `Layout.astro`; page-specific semantics stay in page/components; interaction sizing stays in `global.css`; a new Astro page owns the branded 404. A dependency-free Node test suite validates source contracts and generated output.

**Tech Stack:** Astro 7, Tailwind CSS 4, GSAP 3, Node.js built-in test runner.

## Global Constraints

- Keep Concept A — Sunlit Kitten, Nunito, Source Serif 4, and the existing cream/ginger palette.
- Use only approved Waffle PNG assets.
- Keep motion restrained and honor `prefers-reduced-motion`.
- Do not add runtime or test dependencies.
- Do not hard-code an unverified production hostname.

---

### Task 1: Add the regression harness

**Files:**
- Create: `website/tests/site.test.mjs`
- Modify: `website/package.json`

**Interfaces:**
- Consumes: website source files and generated `dist/` files.
- Produces: `npm test`, which builds and then runs `node --test tests/*.test.mjs`.

- [x] **Step 1: Write failing tests** for motion fail-open, truthful navigation, skip-link targeting, metadata, hit areas, and the 404 route.
- [x] **Step 2: Run `node --test tests/site.test.mjs`** and confirm failures describe the six missing contracts.
- [x] **Step 3: Add the package test script** only after the tests exist.

### Task 2: Fix shared layout and motion safety

**Files:**
- Modify: `website/src/layouts/Layout.astro`
- Modify: `website/src/scripts/motion.ts`
- Modify: `website/src/styles/global.css`
- Modify: `website/astro.config.mjs`

**Interfaces:**
- Consumes: optional `PUBLIC_SITE_URL` and the existing animation data attributes.
- Produces: canonical/social metadata, `#main-content` bypass navigation, and the `js-motion-ready` initialization contract.

- [x] **Step 1: Run focused tests** and confirm this task's assertions fail.
- [x] **Step 2: Implement the three-second fail-open guard and ready marker.**
- [x] **Step 3: Add canonical/Open Graph/Twitter metadata using the optional configured site origin.**
- [x] **Step 4: Add focus-only skip-link and 44px brand/navigation targets.**
- [x] **Step 5: Run focused tests** and confirm this task's assertions pass.

### Task 3: Repair page semantics and add the branded 404

**Files:**
- Modify: `website/src/pages/index.astro`
- Modify: `website/src/components/SiteHeader.astro`
- Modify: `website/src/components/SiteFooter.astro`
- Modify: `website/src/components/WhatSheGetsInto.astro`
- Modify: `website/src/components/WhyWaffle.astro`
- Create: `website/src/pages/404.astro`
- Modify: `website/README.md`

**Interfaces:**
- Consumes: shared `Layout` and approved `sitting-airplane-ears.png` artwork.
- Produces: truthful anchors, a main-content target on both routes, a branded 404, and documented production metadata configuration.

- [x] **Step 1: Run focused tests** and confirm this task's assertions fail.
- [x] **Step 2: Rename navigation labels and anchors without changing section copy.**
- [x] **Step 3: Add the branded 404 page with a single `Back home` action.**
- [x] **Step 4: Document `PUBLIC_SITE_URL` for production builds.**
- [x] **Step 5: Run `npm test`** and confirm the full suite passes.

### Task 4: Rendered verification

**Files:**
- Verify only; no planned production edits.

**Interfaces:**
- Consumes: local Astro preview.
- Produces: desktop/mobile screenshots and browser assertions.

- [x] **Step 1: Run `npm run build`** and confirm both `/` and `/404.html` are generated.
- [x] **Step 2: Verify desktop and 375px mobile homepages** for overflow, console health, motion completion, and correct anchor navigation.
- [x] **Step 3: Verify keyboard focus begins on `Skip to content`** and activating it targets `main-content`.
- [x] **Step 4: Verify the branded 404** has the expected title, copy, image, and home link.
- [x] **Step 5: Review the diff** for unrelated edits and report remaining browser/device risk.
