# Waffle Standing Rig v2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a source-locked articulated raster rig that preserves the approved standing Waffle exactly at neutral and supports a convincing three-quarter walk-in-place, feline paw wave, and head/face/tail motion proof in both the local compositor and Fusion.

**Architecture:** Standing v2 extends the existing full-canvas PNG and JSON rig contract without changing standing v1. A schema-v2 manifest describes the hierarchy, conservative controls, per-layer limits, and mutually exclusive artwork variants. Deterministic Node/PNGJS tools partition the approved source, extract tightly bounded repair and variant plates, validate the package, evaluate hierarchical motion clips, and render review-only frames. Fusion is a verified downstream assembly; it is never the source of truth.

**Tech Stack:** Node.js ESM, PNGJS, Node test runner, JSON manifests, SHA-256, mise, DaVinci Resolve Studio/Fusion through the local MCP.

## Global Constraints

- `assets/brand/waffle/poses/standing.png` is the exact visible-pixel authority for neutral.
- Standing v1 and all its commands, manifests, PNGs, hashes, and tests remain unchanged and passing.
- Every production layer is a full-canvas 1536 x 1024 RGBA PNG registered to the approved source.
- `left` and `right` always mean screen-left and screen-right.
- Waffle remains an eternal kitten and cat first: feline joints, diagonal gait, four-legged balance, paw anatomy, markings, and expression are acceptance requirements.
- Generated edit plates remain under ignored `.superpowers/` storage. Only declared, bounded production layers enter Git.
- No private real-cat photo, iCloud/share URL, identifying metadata, editor cache, or local Resolve path enters Git.
- The build writes into a temporary sibling directory, validates there, and promotes only a complete valid package. It never overwrites standing v1.
- Still artwork is reviewed before motion authoring; motion is reviewed before Fusion assembly.
- No Resolve render, `safe_quick_export`, or MCP export is performed. The owner handles delivery exports.
- Every Resolve MCP write is read back before the project is saved.
- Each production PNG is below 10 MB and `assets/brand/waffle/rigs/standing-v2/` is below 60 MB.

---

### Task 1: Schema-v2 Contract and Backward-Compatible Validation

**Files:**

- Create: `tools/brand-assets/rig-schema-v2.mjs`
- Create: `tools/brand-assets/test/rig-schema-v2.test.mjs`
- Modify: `tools/brand-assets/validate-rig.mjs`
- Modify: `tools/brand-assets/test/rig-raster.test.mjs`
- Modify: `mise.toml`

**Interfaces:**

- Preserve: `validateRig(manifestPath): Promise<{ layerCount: number, mismatchPixels: number }>`
- Produce: `validateRigV2Shape(manifest): void`
- Produce: `validateMotionClipShape(manifest, clip): void`
- Produce: `variantForLayer(manifest, layerId, memberId): object`
- Accept: schema versions 1 and 2; dispatch by `schemaVersion` without weakening v1 checks.

- [ ] **Step 1: Write failing schema-v2 tests**

Add fixtures that prove schema v1 still passes and schema v2 rejects duplicate IDs, duplicate draw orders, missing parents, cycles, unknown control bindings, missing variants, two neutral variant members, invalid per-layer limits, unsafe paths, hash drift, dimensions other than 1536 x 1024, and files at or above 10 MB.

Use this exact schema-v2 surface in the valid fixture:

```js
{
  schemaVersion: 2,
  canvas: { width: 1536, height: 1024 },
  root: { id: "waffle-root", pivot: { x: 0.52, y: 0.76 } },
  source: { file: "../../../poses/standing.png", sha256: "0".repeat(64) },
  neutralReference: { file: "neutral-reference.png", sha256: "0".repeat(64) },
  layers: [
    {
      id: "torso",
      file: "layers/torso.png",
      role: "visible",
      parent: "waffle-root",
      drawOrder: 20,
      visibleAtNeutral: true,
      blendMode: "normal",
      pivot: { x: 0.52, y: 0.62 },
      neutral: { x: 0, y: 0, rotationDegrees: 0, scaleX: 1, scaleY: 1 },
      limits: {
        x: { min: -0.01, max: 0.01 },
        y: { min: -0.015, max: 0.015 },
        rotationDegrees: { min: -3, max: 3 }
      },
      sha256: "0".repeat(64)
    },
    {
      id: "front-paw-left",
      file: "layers/front-paw-left.png",
      role: "variant-anchor",
      parent: "torso",
      drawOrder: 30,
      visibleAtNeutral: true,
      blendMode: "normal",
      pivot: { x: 0.35, y: 0.82 },
      neutral: { x: 0, y: 0, rotationDegrees: 0, scaleX: 1, scaleY: 1 },
      limits: { rotationDegrees: { min: -8, max: 8 } },
      sha256: "0".repeat(64)
    }
  ],
  variants: {
    "front-paw-left": {
      layer: "front-paw-left",
      members: [
        { id: "planted", file: "variants/front-paw-left/planted.png", neutral: true, sha256: "0".repeat(64) },
        { id: "lifted", file: "variants/front-paw-left/lifted.png", neutral: false, sha256: "0".repeat(64) },
        { id: "wave", file: "variants/front-paw-left/wave.png", neutral: false, sha256: "0".repeat(64) }
      ]
    }
  },
  controls: {
    bodyBob: {
      min: -0.015,
      max: 0.015,
      bindings: [{ layer: "torso", property: "y", factor: 1 }]
    }
  }
}
```

- [ ] **Step 2: Run the focused tests and confirm RED**

Run:

```sh
pnpm --dir tools/brand-assets test -- test/rig-schema-v2.test.mjs test/rig-raster.test.mjs
```

Expected: FAIL because schema v2 is rejected and `rig-schema-v2.mjs` does not exist.

- [ ] **Step 3: Implement shape, graph, variant, binding, and resource validation**

Keep filesystem/hash/PNG checks in `validate-rig.mjs`; put pure manifest rules in `rig-schema-v2.mjs`. Require:

- unique nonempty layer IDs and integer draw orders;
- `role` in `visible`, `repair`, `overlay`, or `variant-anchor`;
- contained nonsymlink layer and variant paths;
- normalized pivots and finite identity neutral transforms;
- one synthetic, non-raster `root.id` named `waffle-root`, known parents, and an acyclic parent graph rooted there;
- finite property limits with `min < max`;
- exactly one neutral member per variant set;
- every variant set references one known anchor layer and every member has a valid PNG/hash;
- every control binding references a known layer, variant set, or supported property;
- exact canvas dimensions for production v2;
- decoded RGBA, safe PNG metadata, per-file size below 10 MB, and package size below 60 MB.

- [ ] **Step 4: Keep neutral recomposition exact for both schema versions**

For v2, resolve the neutral member of each variant anchor and exclude non-neutral members. Sort the resulting visible stack by draw order and require zero decoded-RGBA mismatches against both the approved standing source and `neutral-reference.png`. The failure reports the first `x`, `y`, layer/variant context, and expected/actual RGBA values.

- [ ] **Step 5: Wire both manifests into repository commands**

Change both mise commands to validate v1 and v2:

```toml
run = "pnpm --dir tools/brand-assets test && node tools/brand-assets/validate-raster.mjs --optional assets/brand/waffle/manifest.json assets/brand/waffle/animation/idle/manifest.json && node tools/brand-assets/validate-rig.mjs --optional assets/brand/waffle/rigs/standing-v1/rig.json assets/brand/waffle/rigs/standing-v2/rig.json"
```

The v2 path remains optional until Task 3 promotes the first valid package.

- [ ] **Step 6: Run GREEN and commit**

Run the focused tests, `mise run brand-rig-check`, and `mise run brand-check`. Expected: all current 86 brand tests and the new schema tests pass; v1 reports zero mismatches; absent v2 reports `SKIP`.

```sh
git add tools/brand-assets/rig-schema-v2.mjs tools/brand-assets/test/rig-schema-v2.test.mjs tools/brand-assets/validate-rig.mjs tools/brand-assets/test/rig-raster.test.mjs mise.toml
git commit -m "build: add standing rig v2 schema"
```

---

### Task 2: Hierarchical Motion Evaluator and Clip Validation

**Files:**

- Create: `tools/brand-assets/rig-motion.mjs`
- Create: `tools/brand-assets/test/rig-motion.test.mjs`
- Modify: `tools/brand-assets/rig-raster.mjs`

**Interfaces:**

- Produce: `interpolateKeyframes(keyframes, frame): number|string`
- Produce: `evaluateClip(manifest, clip, frame): { layers: Map, variants: Map, controls: Map }`
- Produce: `worldTransforms(manifest, localTransforms): Map<string, Matrix>`
- Produce: `renderRigFrame(manifestPath, clipPath, frame): Promise<PNG>`
- Produce: `assertLoopClosure(manifest, clip): void`

- [ ] **Step 1: Write failing math and motion-contract tests**

Cover identity, normalized translation, rotation around a registered pivot, parent-to-child transform propagation, draw-order composition, linear and held interpolation, exact keyframe hits, variant holds, range rejection, unknown controls, missing frames, and a loop whose last frame differs from frame 0.

Use this clip contract:

```js
{
  schemaVersion: 1,
  id: "walk-in-place",
  fps: 24,
  frameCount: 48,
  loop: true,
  requiredClosure: { firstFrame: 0, lastFrame: 47 },
  variants: {
    "front-paw-left": [
      { frame: 0, value: "planted", interpolation: "hold" },
      { frame: 12, value: "lifted", interpolation: "hold" },
      { frame: 24, value: "planted", interpolation: "hold" }
    ]
  },
  controls: {
    bodyBob: [
      { frame: 0, value: 0 },
      { frame: 12, value: -0.008 },
      { frame: 24, value: 0 },
      { frame: 36, value: -0.008 },
      { frame: 47, value: 0 }
    ]
  }
}
```

- [ ] **Step 2: Run the focused test and confirm RED**

Run: `pnpm --dir tools/brand-assets test -- test/rig-motion.test.mjs`

Expected: FAIL because the evaluator does not exist.

- [ ] **Step 3: Implement deterministic clip evaluation**

Represent affine transforms as six-number matrices. Apply local transforms in this order: translate to pivot, scale, rotate, translate from pivot, then normalized x/y translation. Multiply each child local matrix by its parent's world matrix from root to leaf. Resolve numeric keys linearly unless `interpolation: "hold"`; variants always hold. Reject extrapolation, NaN, duplicate frame keys, decreasing frames, and out-of-range values.

- [ ] **Step 4: Extend raster transformation without changing v1 behavior**

Add a matrix-based raster transform and keep `transformRgba(source, transform)` as a compatibility wrapper. Identity must still copy bytes exactly. Use inverse mapping and the existing bilinear sampler for non-identity motion.

- [ ] **Step 5: Prove exact loop and neutral-state semantics**

`assertLoopClosure` compares evaluated controls, variants, and world matrices at declared closure frames with exact numeric equality. It also checks that every variant has a state at frame 0 and that all declared controls remain within the manifest ranges on every integer frame.

- [ ] **Step 6: Run GREEN and commit**

Run the focused motion and raster tests, then all brand tests.

```sh
git add tools/brand-assets/rig-motion.mjs tools/brand-assets/test/rig-motion.test.mjs tools/brand-assets/rig-raster.mjs
git commit -m "feat: add deterministic rig motion evaluator"
```

---

### Task 3: Safe v2 Builder and Source-Locked Visible Partition

**Files:**

- Create: `tools/brand-assets/build-waffle-standing-rig-v2.mjs`
- Create: `tools/brand-assets/test/build-waffle-standing-rig-v2.test.mjs`
- Create: `assets/brand/waffle/rigs/standing-v2/masks.json`
- Create through builder: `assets/brand/waffle/rigs/standing-v2/rig.json`
- Create through builder: `assets/brand/waffle/rigs/standing-v2/neutral-reference.png`
- Create through builder: neutral visible PNGs under `assets/brand/waffle/rigs/standing-v2/layers/`

**Visible hierarchy to build:**

```text
waffle-root
├── torso
├── rear-thigh-left -> rear-hock-left -> rear-paw-left
├── rear-thigh-right -> rear-hock-right -> rear-paw-right
├── front-upper-left -> front-lower-left -> front-paw-left
├── front-upper-right -> front-lower-right -> front-paw-right
├── tail-base -> tail-mid -> tail-tip
└── head-base
    ├── ear-left
    ├── ear-right
    ├── iris-left -> pupil-left -> highlight-left
    ├── iris-right -> pupil-right -> highlight-right
    ├── muzzle
    ├── jaw-closed
    └── whiskers
```

- [ ] **Step 1: Write failing builder tests**

Use small fixture canvases to prove front-to-back polygon ownership, exact source-byte preservation, overlap only beneath fully opaque covers, exact layer-definition coverage, temp-directory cleanup on failure, atomic promotion on success, and refusal to write into any path named `standing-v1`.

- [ ] **Step 2: Run the focused test and confirm RED**

Run: `pnpm --dir tools/brand-assets test -- test/build-waffle-standing-rig-v2.test.mjs`

Expected: FAIL because the v2 builder does not exist.

- [ ] **Step 3: Implement the safe build lifecycle**

Reuse `partitionSource` and `applyUnderlaps`, but make the v2 builder write all output into `${outputDirectory}.building-${process.pid}`. Copy the validated `masks.json` build input into the temporary package before promotion. Run manifest, PNG, hash, neutral-composite, file-size, and package-size validation inside the temporary directory. Rename the validated directory into place only when the target does not already exist or is an explicitly recognized v2 build. Preserve the previous valid v2 directory until the replacement validates.

- [ ] **Step 4: Trace source masks and register pivots**

Create ordered polygons for the torso, four three-part legs, three tail sections, head base, full ears, eye components, muzzle, closed jaw, and whiskers. Pivots sit at feline anatomical joints: shoulder, elbow/wrist, hip, hock, tail joints, neck, ear bases, and jaw hinge. Record conservative rotation and translation limits per articulated layer.

- [ ] **Step 5: Build the production neutral partition**

Run:

```sh
node tools/brand-assets/build-waffle-standing-rig-v2.mjs \
  assets/brand/waffle/poses/standing.png \
  assets/brand/waffle/rigs/standing-v2/masks.json \
  assets/brand/waffle/rigs/standing-v2
```

Expected: the temporary package validates and is promoted; decoded neutral mismatch count is zero against the source and neutral reference.

- [ ] **Step 6: Review the neutral partition before repairs**

Generate ignored isolation sheets under `.superpowers/reviews/waffle/standing-rig-v2/neutral-layers/` showing every layer over checkerboard and in composite on warm white and charcoal. Inspect full size, 320 px, and 160 px. Correct polygon ownership or pivots only; do not repaint source-visible pixels.

- [ ] **Step 7: Run GREEN and commit**

Run the focused builder test, `mise run brand-check`, and `git diff --check`.

```sh
git add tools/brand-assets/build-waffle-standing-rig-v2.mjs tools/brand-assets/test/build-waffle-standing-rig-v2.test.mjs assets/brand/waffle/rigs/standing-v2/masks.json assets/brand/waffle/rigs/standing-v2/rig.json assets/brand/waffle/rigs/standing-v2/neutral-reference.png assets/brand/waffle/rigs/standing-v2/layers
git commit -m "feat: partition Waffle standing rig v2"
```

---

### Task 4: Concealed Joint Repairs and Variant Extraction Pipeline

**Files:**

- Create: `tools/brand-assets/build-waffle-rig-v2-art.mjs`
- Create: `tools/brand-assets/test/build-waffle-rig-v2-art.test.mjs`
- Create: `assets/brand/waffle/rigs/standing-v2/repairs.json`
- Create: `assets/brand/waffle/rigs/standing-v2/variants.json`
- Create through builder: repair PNGs under `assets/brand/waffle/rigs/standing-v2/layers/`
- Create through builder: alternate PNGs under `assets/brand/waffle/rigs/standing-v2/variants/`

**Required repair/variant artwork:**

- Repair plates: `body-repair`, `neck-repair`, `front-shoulder-repair-left`, `front-shoulder-repair-right`, `rear-hip-repair-left`, `rear-hip-repair-right`, `front-elbow-repair-left`, `front-elbow-repair-right`, `rear-hock-repair-left`, `rear-hock-repair-right`, `front-wrist-repair-left`, `front-wrist-repair-right`, `rear-paw-root-repair-left`, `rear-paw-root-repair-right`, `tail-base-mid-repair`, and `tail-mid-tip-repair`.
- `front-paw-left`: `planted`, `lifted`, `wave`.
- `front-paw-right`: `planted`, `lifted`.
- Both rear paws: `planted`, `lifted`.
- `head-base`: `neutral`, `turn-left`, `turn-right`.
- Both eyes: independent upper lid, lower lid, iris, pupil, and highlight layers.
- `jaw`: `closed`, `open`.

- [ ] **Step 1: Write failing extraction and containment tests**

Test that an edit plate is accepted only when its source hash, declared crop/mask, canvas, and expected variant ID match. Prove pixels outside the declared extraction polygons are transparent, the neutral member reproduces its source-owned pixels exactly, repairs do not alter neutral, hashes update deterministically, and failed extraction leaves the approved package untouched.

- [ ] **Step 2: Run the focused test and confirm RED**

Run: `pnpm --dir tools/brand-assets test -- test/build-waffle-rig-v2-art.test.mjs`

Expected: FAIL because the art extractor does not exist.

- [ ] **Step 3: Implement declared-zone extraction**

`repairs.json` describes cover layer, parent, draw order, pivot, polygons, sample/edit source, and whether a pixel may exist outside the neutral cover. `variants.json` describes set ID, anchor layer, registration pivot, neutral member, each member's ignored edit-plate input, and extraction polygons. The tool copies the production `masks.json`, `repairs.json`, and `variants.json` inputs into its temporary package, sanitizes through PNGJS, writes only bounded output, hashes it, and calls the schema-v2 validator before promotion.

- [ ] **Step 4: Produce constrained edit plates from the approved source**

Use the image-generation/editing workflow with the approved standing source as the primary reference and the approved model sheet/poses as identity checks. Produce one narrowly scoped edit plate at a time. Preserve the forehead M, cheek/crown stripes, grey-green eyes, pink nose, pale muzzle/chest/belly, limb bands, flank swirls, tail rings, large ears, whiskers, long kitten legs, light direction, outline weight, and three-quarter perspective.

Store full edit plates only under `.superpowers/concepts/waffle/standing-rig-v2/`. Reject any plate with identity, anatomy, texture, perspective, marking, or lighting drift; do not enlarge its extraction zone to hide the failure.

- [ ] **Step 5: Extract repairs and variants into the production package**

Run:

```sh
node tools/brand-assets/build-waffle-rig-v2-art.mjs \
  assets/brand/waffle/rigs/standing-v2/rig.json \
  assets/brand/waffle/rigs/standing-v2/repairs.json \
  assets/brand/waffle/rigs/standing-v2/variants.json
```

Expected: all declared layers are registered full-canvas RGBA PNGs, hashes are updated, and neutral mismatch count remains zero.

- [ ] **Step 6: Pass the still-art review gate**

Create ignored contact sheets for every repair and alternate state at full resolution, 320 px, and 160 px on warm white and charcoal. Review likeness, anatomy, seams, markings, perspective, eye bounds, blink silhouette, jaw expression, whiskers, tail continuity, and variant registration. Do not start motion clips until every still is accepted.

- [ ] **Step 7: Run GREEN and commit**

Run focused tests, all brand tests, metadata/hash checks, package-size checks, and `git diff --check`.

```sh
git add tools/brand-assets/build-waffle-rig-v2-art.mjs tools/brand-assets/test/build-waffle-rig-v2-art.test.mjs assets/brand/waffle/rigs/standing-v2/repairs.json assets/brand/waffle/rigs/standing-v2/variants.json assets/brand/waffle/rigs/standing-v2/layers assets/brand/waffle/rigs/standing-v2/variants assets/brand/waffle/rigs/standing-v2/rig.json
git commit -m "feat: add standing rig v2 art variants"
```

---

### Task 5: Final Manifest, Controls, and Package Documentation

**Files:**

- Modify: `assets/brand/waffle/rigs/standing-v2/rig.json`
- Create: `assets/brand/waffle/rigs/standing-v2/README.md`
- Modify: `assets/brand/waffle/README.md`
- Modify: `assets/brand/waffle/manifest.json`
- Modify: `tools/brand-assets/test/rig-schema-v2.test.mjs`

- [ ] **Step 1: Add a failing production-manifest contract test**

Assert the exact required layer/variant IDs, hierarchy, control names, ranges, and bindings. The test must fail if any required joint, face component, neutral member, or motion control is absent.

- [ ] **Step 2: Run the focused test and confirm RED**

Run: `pnpm --dir tools/brand-assets test -- test/rig-schema-v2.test.mjs`

Expected: FAIL until the complete production manifest surface is present.

- [ ] **Step 3: Declare exact global controls**

Use these ranges without widening them:

```js
{
  breath: [0, 1],
  bodyBob: [-0.015, 0.015],
  bodyLean: [-3, 3],
  weightShift: [-1, 1],
  headTilt: [-5, 5],
  headTurn: [-1, 1],
  gazeX: [-0.012, 0.012],
  gazeY: [-0.009, 0.009],
  blinkLeft: [0, 1],
  blinkRight: [0, 1],
  earLeft: [-6, 6],
  earRight: [-6, 6],
  jawOpen: [0, 1],
  tailBase: [-8, 8],
  tailMid: [-12, 12],
  tailTip: [-15, 15]
}
```

Declare each shoulder, elbow, wrist, hip, hock, and paw limit on its layer. Bind head turn, jaw, and paw states to registered variants rather than extreme raster rotation.

- [ ] **Step 4: Document package ownership and constraints**

The v2 README records source authority, screen-relative naming, hierarchy, variant semantics, control units, clip contract, neutral-zero-mismatch guarantee, build commands, review gates, Fusion coordinate conversion, no-MCP-export rule, and the side-profile travelling walk exclusion. Update the top-level brand inventory without embedding local paths or review assets.

- [ ] **Step 5: Run GREEN and commit**

Run the production contract test and `mise run brand-check`. Expected: v1 and v2 both validate with zero neutral mismatches.

```sh
git add assets/brand/waffle/rigs/standing-v2/rig.json assets/brand/waffle/rigs/standing-v2/README.md assets/brand/waffle/README.md assets/brand/waffle/manifest.json tools/brand-assets/test/rig-schema-v2.test.mjs
git commit -m "docs: define standing rig v2 controls"
```

---

### Task 6: Motion Preview CLI and Review Sheets

**Files:**

- Create: `tools/brand-assets/render-waffle-rig-motion.mjs`
- Create: `tools/brand-assets/test/render-waffle-rig-motion.test.mjs`
- Modify: `tools/brand-assets/package.json`
- Modify: `.gitignore` only if the existing `/.superpowers/` rule does not cover the chosen output path

**CLI:**

```sh
node tools/brand-assets/render-waffle-rig-motion.mjs \
  assets/brand/waffle/rigs/standing-v2/rig.json \
  assets/brand/waffle/rigs/standing-v2/motions/walk-in-place.json \
  .superpowers/reviews/waffle/standing-rig-v2/walk \
  --frames all --background warm-white --contact-sheet
```

- [ ] **Step 1: Write failing CLI/output tests**

Prove deterministic frame names (`frame-0000.png`), exact frame count, full-canvas RGBA output, explicit warm-white/charcoal/checkerboard review backgrounds, contact-sheet cell order, refusal to write inside `assets/`, and stable hashes for a tiny fixture clip.

- [ ] **Step 2: Run the focused test and confirm RED**

Run: `pnpm --dir tools/brand-assets test -- test/render-waffle-rig-motion.test.mjs`

Expected: FAIL because the CLI does not exist.

- [ ] **Step 3: Implement review-only rendering**

The CLI validates rig and clip first, renders into a temporary ignored directory, and promotes only complete review output. It emits full-size frames plus 320 px and 160 px sheets. It never writes MP4, production sprites, or files beneath `assets/`.

- [ ] **Step 4: Run GREEN and commit**

Run the focused CLI test and all brand tests.

```sh
git add tools/brand-assets/render-waffle-rig-motion.mjs tools/brand-assets/test/render-waffle-rig-motion.test.mjs tools/brand-assets/package.json .gitignore
git commit -m "feat: render rig motion review frames"
```

---

### Task 7: Three-Quarter Walk-in-Place Clip

**Files:**

- Create: `assets/brand/waffle/rigs/standing-v2/motions/walk-in-place.json`
- Create: `tools/brand-assets/test/waffle-walk-motion.test.mjs`

- [ ] **Step 1: Write failing walk acceptance tests**

Assert 24 fps, 48 frames, frame 47 exactly equal to frame 0, all four legs at exact neutral registration at closure, opposing front/rear diagonal support pairs, planted paw baseline stability during stance, lifted variant use during swing, body bob/lean/weight transfer within limits, and no undefined layer/variant state on any frame.

- [ ] **Step 2: Run the focused test and confirm RED**

Run: `pnpm --dir tools/brand-assets test -- test/waffle-walk-motion.test.mjs`

Expected: FAIL because the walk clip does not exist.

- [ ] **Step 3: Author the feline gait**

Use four phases across frames 0, 12, 24, 36, and closure at 47. Front-left/rear-right and front-right/rear-left form the diagonal support pairs. Keep stance paws fixed on the neutral baseline; use lifted art only during swing. Add small torso bob and lateral weight transfer without human arm/leg opposition or an upright bounce.

- [ ] **Step 4: Render and inspect the full loop**

Render all frames on warm white and charcoal into `.superpowers/reviews/waffle/standing-rig-v2/walk/`. Inspect every frame full size for shoulder, elbow, wrist, hip, hock, paw, neck, and tail seams. Inspect 320 px and 160 px sheets for likeness, gait readability, marking continuity, weight, and loop smoothness. Revise keyframes or rejected artwork; do not relax ranges.

- [ ] **Step 5: Run GREEN and commit**

Run the focused walk test and all brand tests.

```sh
git add assets/brand/waffle/rigs/standing-v2/motions/walk-in-place.json tools/brand-assets/test/waffle-walk-motion.test.mjs
git commit -m "feat: animate Waffle walk in place"
```

---

### Task 8: Paw Wave and Head/Face/Tail Proof Clips

**Files:**

- Create: `assets/brand/waffle/rigs/standing-v2/motions/paw-wave.json`
- Create: `assets/brand/waffle/rigs/standing-v2/motions/head-face-tail.json`
- Create: `tools/brand-assets/test/waffle-expression-motion.test.mjs`

- [ ] **Step 1: Write failing wave and expression acceptance tests**

For the wave, assert 24 fps, 72 frames, exact all-fours neutral at frames 0 and 71, weight transfer away from the screen-left front leg before lift, `planted -> lifted -> wave -> lifted -> planted` at registered state boundaries, and exactly two small wave peaks. Require secondary head, ears, gaze, blink, and tail motion within limits.

For the face/tail proof, assert 24 fps and 48 frames; `neutral -> turn-left -> neutral -> turn-right -> neutral`; pupils remain within the iris alpha bounds at both gaze extremes; independent left/right and synchronized blink states fully cover the open eyes; jaw-open preserves muzzle/nose/whiskers; tail base/mid/tip overlap continuously at every frame.

- [ ] **Step 2: Run the focused test and confirm RED**

Run: `pnpm --dir tools/brand-assets test -- test/waffle-expression-motion.test.mjs`

Expected: FAIL because both clips do not exist.

- [ ] **Step 3: Author the paw wave**

Begin with all paws planted. Shift torso weight toward screen-right, settle the other three paws, then lift the screen-left front leg. Swap paw variants only at registered interchange poses while motion conceals the change. Make two restrained feline paw motions from the wrist/foreleg, return through lifted to planted, restore weight, and finish at exact neutral.

- [ ] **Step 4: Author the head/face/tail proof**

Use the painted head plates for horizontal turns and a small neck tilt for secondary motion. Keep pupil motion inside each iris. Close upper/lower lids over rigid eye components without resizing the eyeballs. Use the open jaw plate without disturbing muzzle, nose, expression, or whiskers. Stagger tail base/mid/tip rotations so the curve flows from base to tip while rings remain connected.

- [ ] **Step 5: Render and pass the motion review gate**

Render both clips on warm white and charcoal under `.superpowers/reviews/waffle/standing-rig-v2/`. Inspect every full-resolution frame and both reduced sizes. Confirm kitten likeness, balance, feline paw motion, registered swaps, head identity, eye bounds, blink shape, jaw expression, whiskers, tail curve, and exact returns to neutral.

- [ ] **Step 6: Run GREEN and commit**

Run focused expression tests and all brand tests.

```sh
git add assets/brand/waffle/rigs/standing-v2/motions/paw-wave.json assets/brand/waffle/rigs/standing-v2/motions/head-face-tail.json tools/brand-assets/test/waffle-expression-motion.test.mjs
git commit -m "feat: animate Waffle wave and expressions"
```

---

### Task 9: Verified Fusion Assembly

**Files:**

- Create: `assets/brand/waffle/rigs/standing-v2/fusion/README.md`
- Create: `assets/brand/waffle/rigs/standing-v2/fusion/assembly-record.json`

- [ ] **Step 1: Re-open the disposable Resolve project safely**

Use `_mcp_waffle_smoke_test` only. Confirm Resolve is responsive, the intended project is active, and no render job is running. If Resolve crashes, stop writes, relaunch, reopen the project, and read the current graph before resuming.

- [ ] **Step 2: Build and verify the neutral graph in milestones**

Create one Loader per active production PNG, one Transform per articulated layer, and ordered Merges matching `drawOrder`. Use repository-absolute Loader paths. Convert each pivot with:

```text
fusionPivotX = manifestPivotX
fusionPivotY = 1 - manifestPivotY
```

After every node/path/pivot/connection write, read it back. Save only after a complete verified subtree: body/legs, tail, head/face, then variants.

- [ ] **Step 3: Add controls and variant visibility**

Create a controller group exposing the manifest control names. Bind transforms within manifest limits. Implement mutually exclusive Loader/Merge visibility for paw, head, and jaw variants. Read back control ranges, bindings, default values, and neutral visibility. At neutral, only the declared neutral member of each set is active.

- [ ] **Step 4: Enter and verify the three proof animations**

Reproduce the JSON clips at 24 fps with the same frame counts and key values. Read back representative keys plus frame 0/last-frame closure for each clip. Save after each verified clip. Do not render or export.

- [ ] **Step 5: Record the downstream assembly**

Write `assembly-record.json` with project name, timeline/composition name, Resolve/Fusion version when available, manifest SHA-256, node IDs, Loader-to-file mapping, manifest-to-Fusion pivot mapping, control bindings, clip/frame mapping, readback checkpoints, and explicit `renderedByMcp: false`. The README explains how the owner can relink, inspect, and export.

- [ ] **Step 6: Commit the verified record**

```sh
git add assets/brand/waffle/rigs/standing-v2/fusion/README.md assets/brand/waffle/rigs/standing-v2/fusion/assembly-record.json
git commit -m "docs: record Waffle standing v2 Fusion rig"
```

Stop with the verified animation open in Fusion for owner preview. Do not export.

---

### Task 10: Final Fidelity, Privacy, Package, and Repository Verification

**Files:**

- Modify only when verification finds a concrete defect in files from Tasks 1-9.

- [ ] **Step 1: Run the entire brand verification stack**

Run:

```sh
pnpm --dir tools/brand-assets test
mise run brand-check
mise run brand-rig-check
git diff --check
```

Expected: all tests pass; v1 and v2 both report zero neutral mismatches; all three clips pass their range and closure checks.

- [ ] **Step 2: Run package and privacy scans**

Verify every committed v2 PNG is 1536 x 1024 RGBA, below 10 MB, hash-matched, nonsymlinked, and free of text/EXIF/profile chunks. Verify the whole v2 directory is below 60 MB. Search tracked files for `icloud`, `share.icloud.com`, `.superpowers`, `/Users/`, real-cat filenames, Resolve cache paths, and private reference extensions. Expected: no private or local-only references.

- [ ] **Step 3: Repeat visual acceptance at all required sizes**

Review neutral, isolated layers, every alternate still, all walk frames, all wave frames, and all head/face/tail frames at full size, 320 px, and 160 px on warm white and charcoal. Record acceptance in the v2 README or assembly record without committing review images.

- [ ] **Step 4: Run the repository-wide verification stack**

Because the user-level test fixtures inherit `commit.gpgsign=true`, keep the process-only override used during baseline verification:

```sh
mise run fmt
mise run build
mise run vet
mise run lint
env GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=commit.gpgsign GIT_CONFIG_VALUE_0=false mise run test
git status --short
```

Expected: formatting, build, vet, lint, all Go race tests, all deterministic evals, and Git whitespace checks pass. The signing override changes no user or repository configuration.

- [ ] **Step 5: Request code review and address findings**

Use `superpowers:requesting-code-review`. Review the complete branch diff against the approved design, including binary inventory, generated-file reproducibility, neutral fidelity, test adequacy, safe promotion, privacy, motion gates, and Fusion evidence. Fix every confirmed finding and rerun the affected focused test plus Steps 1-4.

- [ ] **Step 6: Commit any verification fixes and prepare integration**

If verification changed files:

```sh
git add tools/brand-assets assets/brand/waffle/rigs/standing-v2 assets/brand/waffle/README.md assets/brand/waffle/manifest.json mise.toml
git -c commit.gpgsign=false commit -m "fix: finish Waffle standing rig v2"
```

Use `superpowers:verification-before-completion`, then `superpowers:finishing-a-development-branch`. Do not claim completion until the tool-neutral package, exact neutral reconstruction, approved stills, all three motion proofs, Fusion assembly/readback, privacy checks, size limits, and full repository verification all pass.

## Explicitly Out of Scope

- Side-profile travelling walk.
- Sitting or curled articulated rigs.
- Speech lip sync or phoneme mouth sets.
- Props or object interaction.
- Documentation-site integration.
- Remotion integration.
- Final Resolve render or export.
