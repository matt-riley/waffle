# Waffle Raster Character System Implementation Plan

> **For agentic workers:** Continue with
> `superpowers:subagent-driven-development`. Tasks 1 and 2 in the superseded SVG
> plan are complete; this plan replaces Tasks 3 through 6.

**Goal:** Deliver a Waffle-faithful raster model sheet, transparent master,
expression and pose assets, one idle sprite proof, and a recorded QA decision
without requiring a paid animation product.

**Architecture:** Approved references and the Balanced study feed
high-resolution raster composites. The repository boundary is RGBA PNG assets
plus schema-versioned JSON manifests. Fixed web motion is generated as one
whole strip from an approved seed, then normalized into registered PNG frames
with one shared scale and anchor. A manual cutout rig is deferred.

**Global constraints:** Waffle is an eternal kitten and cat first. Preserve all
canon anchors. Passing file validation never substitutes for owner likeness
approval. Keep private media, rejected SVGs, and editor project files under
ignored `.superpowers/`. Do not add documentation-site code, promotional video,
Remotion/Blender project files, or Hero Waffle in this plan.

---

### Task 3: Raster Pivot Hygiene and Validation

**Files:**

- Modify: `assets/brand/waffle/README.md`
- Modify: `mise.toml`
- Create: `tools/brand-assets/validate-raster.mjs`
- Create: `tools/brand-assets/sanitize-png.mjs`
- Create: `tools/brand-assets/resize-raster.mjs`
- Create: `tools/brand-assets/build-sprite-edit-canvas.mjs`
- Create: `tools/brand-assets/normalize-sprite-strip.mjs`
- Create: `tools/brand-assets/test/validate-raster.test.mjs`
- Create: `tools/brand-assets/test/sprite-raster.test.mjs`
- Local only: `.superpowers/rejected/waffle-svg/`

- [ ] Move the uncommitted rejected model-sheet SVG and its PNG render into the
  ignored rejected-study directory. Confirm neither remains in `git status`.
- [ ] Add RED tests for PNG dimensions, RGBA colour, configurable corner-alpha
  policy, manifest file existence, schema version, unique asset IDs, declared
  width/height parity, file-size budgets, and idle-frame dimensions, anchors,
  order, and durations. Parse PNG chunks and reject `tEXt`, `zTXt`, `iTXt`,
  `eXIf`, and `iCCP` so identifying metadata cannot hide in binary assets.
- [ ] Implement raster and manifest validation using the existing `pngjs`
  dependency. Keep the completed SVG validator as general-purpose tooling, but
  remove Waffle SVGs from `brand-check` and `brand-render` assumptions.
- [ ] Add `sanitize-png.mjs` to decode and re-encode production images through
  PNGJS, preserving RGBA pixels while dropping text, EXIF, and embedded-profile
  chunks. Validation runs after sanitization and still rejects forbidden chunks.
- [ ] Add deterministic PNGJS utilities for smooth bilinear resizing, building
  a square chroma-key sprite edit canvas, splitting a horizontal strip,
  shared-scale bottom-centre normalization, and pixel-for-pixel frame-01
  lockback. Cover them with tests; do not require Pillow for these project
  operations.
- [ ] Update the README source-of-truth contract, directory map, review gates,
  and commands for the raster architecture.
- [ ] Use asset-manifest schema version 1. Each asset entry requires `id`,
  `file`, `role`, `width`, `height`, `alphaPolicy`, and `provenance`. Idle
  manifests require `schemaVersion`, `canvas` as numeric `{width, height}`,
  `anchor` as numeric `{x, y}`, boolean `loop`, and ordered frame objects with
  `file` and positive numeric `durationMs`.
- [ ] Create ignored `.superpowers/privacy-patterns.txt` from the active private
  task context. Run
  `rg -n "share\.icloud\.com|icloudlinks|Shared by" assets tools mise.toml .gitignore`,
  `rg -n "EXIF|GPS" assets`,
  and `rg -n -f .superpowers/privacy-patterns.txt assets tools mise.toml .gitignore`,
  expecting no matches, then run `git diff --check`.
- [ ] Commit as `build: pivot Waffle asset tooling to raster`.

---

### Task 4: Canonical Raster Model Sheet

**Files:**

- Create: `assets/brand/waffle/canon/model-sheet.png`
- Create: `assets/brand/waffle/canon/generation-record.md`
- Local only: `.superpowers/concepts/waffle/raster-model-sheet-*.png`

- [ ] Use the built-in image-generation workflow with the seed, approved
  Balanced study, canon, and approved reference observations. Produce one
  polished front/three-quarter/profile/rear-top model sheet with coherent face,
  proportions, markings, and rendering across views. Do not request vector
  simplification.
- [ ] Keep the warm storybook illustration language: soft fur planes, round
  grey-green eyes, small integrated muzzle, expressive ears, organic broken
  swirls, and long kitten anatomy. Avoid geometric masks, square torsos, and
  column legs.
- [ ] Keep the presentation sheet on an opaque, uniform warm-white background;
  it is not a runtime cutout. Validate RGBA, dimensions, metadata chunks, and
  size budget, then present full-size and 160px-character reviews.
- [ ] Record a privacy-safe prompt summary, tool, date, approved input roles,
  and owner decision in `generation-record.md`. Do not record private filenames,
  URLs, metadata, or claims that the artwork was hand-drawn.
- [ ] Obtain explicit owner approval for likeness and cross-view consistency.
- [ ] Commit as `feat: add Waffle raster model sheet`.

---

### Task 5: Transparent Standing Master and Expressions

**Files:**

- Create: `assets/brand/waffle/manifest.json`
- Create: `assets/brand/waffle/poses/standing.png`
- Create: `assets/brand/waffle/canon/expressions/*.png`

- [ ] Produce an approved neutral three-quarter standing master matching the
  model sheet rather than reinterpreting it. Generate on a perfectly flat solid
  `#0000ff` chroma-key background with no shadows, gradients, texture, floor,
  reflections, or blue in the subject.
- [ ] Create neutral, curious, pleased, focused, startled, and sleepy as
  standalone 768x768 transparent head-and-shoulder renders. They are not
  overlays. Preserve the approved head shape, markings, eye spacing, and crop.
- [ ] Use the built-in image-generation path and the installed imagegen
  `remove_chroma_key.py` helper with `--auto-key border --soft-matte
  --transparent-threshold 12 --opaque-threshold 220 --despill`. Validate alpha,
  subject coverage, and blue fringe; retry once with `--edge-contract 1` if
  necessary. If fur edges still fail, stop and ask the owner before switching
  to the CLI `gpt-image-1.5` native-transparency fallback, which requires an
  `OPENAI_API_KEY`.
- [ ] Resolve the Codex bundled Python runtime before matte removal and verify
  it can import Pillow. Run exactly:
  `"$PYTHON" "${CODEX_HOME:-$HOME/.codex}/skills/.system/imagegen/scripts/remove_chroma_key.py" --input <source> --out <final.png> --auto-key border --soft-matte --transparent-threshold 12 --opaque-threshold 220 --despill`.
  If bundled Pillow is unavailable, stop and request approval before installing
  anything.
- [ ] Populate asset-manifest entries for the model sheet, standing master, and
  six expressions. Validate RGBA, declared dimensions, transparent corners for
  standing/expressions, file-size budgets, and privacy-safe provenance.
- [ ] Present the standing master at full size and 320/160/64px plus an
  expression contact sheet on light and dark backgrounds.
- [ ] Obtain explicit owner approval, then commit as
  `feat: add Waffle raster master and expressions`.

---

### Task 6: Documentation Poses

**Files:**

- Create: `assets/brand/waffle/poses/sitting.png`
- Create: `assets/brand/waffle/poses/curled.png`

- [ ] Create transparent sitting and curled poses from the approved standing
  master and model sheet. Do not reuse the rejected geometric SVG render.
- [ ] Use Task 5's flat `#0000ff` chroma-key prompt, installed matte-removal
  command, validation, one `--edge-contract 1` retry, and explicit approval gate
  before any CLI native-transparency fallback.
- [ ] Validate RGBA, dimensions, transparent corners, and small-size legibility.
- [ ] Present 800px, 320px, 160px, and 64px reviews on light and dark
  backgrounds. Obtain explicit owner approval.
- [ ] Add both poses to `manifest.json` and commit as
  `feat: add Waffle raster documentation poses`.

---

### Task 7: Idle Sprite Proof and Milestone QA

**Files:**

- Create: `assets/brand/waffle/animation/idle/manifest.json`
- Create: `assets/brand/waffle/animation/idle/seed.png`
- Create: `assets/brand/waffle/animation/idle/frames/*.png`
- Create: `assets/brand/waffle/qa/approval-record.md`
- Modify: `assets/brand/waffle/README.md`

- [ ] Use the sprite pipeline with the approved standing master. Derive a
  smooth 256x256 standing seed, then build one square 1536x1536 flat `#0000ff`
  edit canvas containing a centred row of six 256x256 slots with the seed in
  slot one. Request the entire
  row in one image-edit call on the same perfectly flat key colour. Remove the
  key with the installed imagegen helper, normalize all slots with one shared
  scale into six 256x256 frames, bottom-centre anchor them, and lock frame 01
  back to the 256x256 standing seed. Do not generate frames independently. If
  chroma removal fails the documented retry, ask before the CLI transparency
  fallback.
- [ ] Animation beats: neutral hold, slight inhale, blink/ear-twitch start,
  blink closed with subtle tail-tip movement, exhale/ear settle, and neutral
  settle. Use durations `[700, 300, 120, 120, 300, 860]` milliseconds.
- [ ] Validate identical 256x256 RGBA frames, transparent corners, anchor
  `{ "x": 128, "y": 240 }`, ordered duration metadata, total duration 2400ms,
  and no private or external references. Verify frame 01 by comparing decoded
  RGBA pixels against the deterministic committed 256x256 seed.
- [ ] Render a preview sheet and present the loop and individual frames on
  light and dark backgrounds.
- [ ] Record every gate decision and run asset tests, privacy scans,
  `mise run fmt`, `mise run vet`, `mise run lint`, and `mise run test`.
- [ ] Obtain final owner approval, update the inventory, and commit as
  `docs: approve Waffle raster asset milestone`.

---

## Completion Boundary

Stop after Task 7. A later plan covers documentation-site integration, broader
interaction states, Hero Waffle, and promotional video composition.
