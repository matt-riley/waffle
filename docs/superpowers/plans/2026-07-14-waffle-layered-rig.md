# Waffle Layered Raster Rig Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a source-locked, tool-neutral layered raster rig from the approved standing Waffle and prove it with a non-rendering Fusion animation test.

**Architecture:** A deterministic Node/PNGJS pipeline partitions the approved standing composite into full-canvas RGBA layers, validates a versioned rig manifest, and recomposes the neutral pose for exact comparison. Small hidden repair plates and closed-lid overlays are kept beneath or hidden at neutral; Fusion consumes the PNGs and manifest but is not the source of truth.

**Tech Stack:** Node.js ESM, PNGJS, Node test runner, JSON manifests, DaVinci Resolve Studio/Fusion through the local MCP.

## Global Constraints

- The approved `assets/brand/waffle/poses/standing.png` is the visible-pixel authority.
- Canvas size is exactly 1536 x 1024 RGBA for every production layer.
- V1 motion is limited to breathing, blink, independent ears, small head tilt/turn, and tail sway.
- No walking, paw gestures, lip sync, or large body deformation.
- Production files are tool-neutral PNG plus JSON; Fusion is a downstream consumer.
- No private real-cat media, share URLs, Resolve-local paths, or identifying metadata enter Git.
- No Resolve render or export is performed through MCP.
- Every Resolve MCP write must be read back and verified.
- Each PNG remains below 10 MB and the complete brand milestone remains below 60 MB.

---

### Task 1: Rig Manifest and Neutral-Recomposition Validator

**Files:**

- Create: `tools/brand-assets/rig-raster.mjs`
- Create: `tools/brand-assets/validate-rig.mjs`
- Create: `tools/brand-assets/test/rig-raster.test.mjs`
- Modify: `tools/brand-assets/package.json`
- Modify: `mise.toml`

**Interfaces:**

- Produces: `readRgba(file): Promise<PNG>`
- Produces: `writeRgba(file, png): Promise<void>`
- Produces: `sourceOver(bottom, top): PNG`
- Produces: `recomposeLayers(manifestPath): Promise<PNG>`
- Produces: `validateRig(manifestPath): Promise<{ layerCount: number, mismatchPixels: number }>`
- Consumes later: schema-v1 `rig.json` with `canvas`, `source`, `layers`, `controls`, and SHA-256 values.

- [ ] **Step 1: Write failing tests for composition and manifest invariants**

Add Node tests that create 4 x 4 RGBA fixtures and assert:

```js
test("recomposes a partitioned source exactly", async (t) => {
  const { manifest, sourcePixels } = await partitionedRigFixture(t);
  const result = await recomposeLayers(manifest);
  assert.deepEqual(result.data, sourcePixels);
});

test("rejects duplicate IDs, missing parents, cycles, bad pivots, and hash drift", async (t) => {
  const fixture = await validRigFixture(t);
  await assert.rejects(() => validateRig(await fixture.withDuplicateId()), /duplicate layer id/);
  await assert.rejects(() => validateRig(await fixture.withMissingParent()), /unknown parent/);
  await assert.rejects(() => validateRig(await fixture.withCycle()), /layer graph contains a cycle/);
  await assert.rejects(() => validateRig(await fixture.withPivot([1.1, 0.5])), /pivot must be normalized/);
  await assert.rejects(() => validateRig(await fixture.withChangedLayerBytes()), /sha256 mismatch/);
});
```

- [ ] **Step 2: Run the tests and confirm RED**

Run: `pnpm --dir tools/brand-assets test -- test/rig-raster.test.mjs`

Expected: FAIL because `rig-raster.mjs` and `validate-rig.mjs` do not exist.

- [ ] **Step 3: Implement the shared PNG and source-over helpers**

Implement `sourceOver` with integer-safe unpremultiplied RGBA output. Treat a fully transparent top pixel as the bottom pixel and a fully opaque top pixel as the top pixel. For partial alpha use:

```js
const outA = topA + Math.round((bottomA * (255 - topA)) / 255);
const outPremul = topC * topA + Math.round((bottomC * bottomA * (255 - topA)) / 255);
const outC = outA === 0 ? 0 : Math.round(outPremul / outA);
```

Require identical canvas dimensions and return a new `PNG`.

- [ ] **Step 4: Implement strict rig validation**

Require this exact manifest surface:

```js
{
  schemaVersion: 1,
  canvas: { width: 1536, height: 1024 },
  source: { file: "../../poses/standing.png", sha256: "<64 lowercase hex>" },
  neutralReference: { file: "neutral-reference.png", sha256: "<64 lowercase hex>" },
  layers: [{
    id: "body",
    file: "layers/body.png",
    role: "visible",
    parent: null,
    drawOrder: 20,
    visibleAtNeutral: true,
    blendMode: "normal",
    pivot: { x: 0.5, y: 0.75 },
    neutral: { x: 0, y: 0, rotationDegrees: 0, scaleX: 1, scaleY: 1 },
    sha256: "<64 lowercase hex>"
  }],
  controls: {
    breath: { min: 0, max: 1 },
    headTilt: { min: -3, max: 3 },
    headTurn: { min: -0.015, max: 0.015 },
    blink: { min: 0, max: 1 },
    leftEar: { min: -8, max: 4 },
    rightEar: { min: -4, max: 8 },
    tailSway: { min: -5, max: 5 }
  }
}
```

Validate local containment, no symlink escapes, RGBA/dimensions/metadata through `validatePng`, unique IDs and draw orders, known parents, acyclic parents, normalized pivots, identity neutral transforms, finite control ranges with `min < max`, and exact SHA-256 values.

- [ ] **Step 5: Recompose only neutral-visible layers and compare to both authorities**

Sort by `drawOrder`, skip `visibleAtNeutral: false`, and source-over each layer. Require zero decoded-RGBA mismatches against both the approved source and `neutral-reference.png`. Report the first mismatch coordinate when failing.

- [ ] **Step 6: Wire commands and run GREEN**

Add `"test": "node --test"` if not already present, add a `brand-rig-check` mise task calling:

```sh
node tools/brand-assets/validate-rig.mjs assets/brand/waffle/rigs/standing-v1/rig.json
```

Make the task optional until `rig.json` exists, matching the existing raster-manifest behavior. Run the focused test, then all brand tests. Expected: PASS.

- [ ] **Step 7: Commit**

```sh
git add tools/brand-assets/rig-raster.mjs tools/brand-assets/validate-rig.mjs tools/brand-assets/test/rig-raster.test.mjs tools/brand-assets/package.json mise.toml
git commit -m "build: add layered raster rig validation"
```

---

### Task 2: Source-Locked Visible Layer Package

**Files:**

- Create: `tools/brand-assets/build-waffle-standing-rig.mjs`
- Create: `tools/brand-assets/test/build-waffle-standing-rig.test.mjs`
- Create: `assets/brand/waffle/rigs/standing-v1/masks.json`
- Create: `assets/brand/waffle/rigs/standing-v1/layers/body.png`
- Create: `assets/brand/waffle/rigs/standing-v1/layers/head.png`
- Create: `assets/brand/waffle/rigs/standing-v1/layers/left-ear.png`
- Create: `assets/brand/waffle/rigs/standing-v1/layers/right-ear.png`
- Create: `assets/brand/waffle/rigs/standing-v1/layers/tail-visible.png`
- Create: `assets/brand/waffle/rigs/standing-v1/neutral-reference.png`
- Create: `assets/brand/waffle/rigs/standing-v1/rig.json`

**Interfaces:**

- Consumes: approved 1536 x 1024 `poses/standing.png`.
- Produces: `pointInPolygon(x, y, points): boolean`
- Produces: `partitionSource(source, orderedRegions): Map<string, PNG>`
- Produces: an exact neutral-visible partition in draw order `tail-visible`, `body`, `head`, `left-ear`, `right-ear`.

- [ ] **Step 1: Write failing partition tests**

Create a small source fixture with opaque and partially transparent pixels. Assert every nonzero source pixel is assigned to exactly one visible layer, each output pixel preserves the source RGBA bytes, and recomposition equals the fixture byte-for-byte.

- [ ] **Step 2: Run the tests and confirm RED**

Run: `pnpm --dir tools/brand-assets test -- test/build-waffle-standing-rig.test.mjs`

Expected: FAIL because `build-waffle-standing-rig.mjs` does not exist.

- [ ] **Step 3: Implement deterministic polygon partitioning**

Read ordered regions from `masks.json`. Pixel centres use `(x + 0.5, y + 0.5)` and the even-odd point-in-polygon rule. Regions are evaluated front-to-back so ears win over head, head wins over body, and tail is selected only by its explicit silhouette polygon. Any nontransparent pixel not claimed by a movable region belongs to `body`.

Use masks with these named coordinate groups, refined against the approved image without changing the source:

```json
{
  "canvas": { "width": 1536, "height": 1024 },
  "regionsFrontToBack": [
    { "id": "left-ear", "polygons": [] },
    { "id": "right-ear", "polygons": [] },
    { "id": "head", "polygons": [] },
    { "id": "tail-visible", "polygons": [] }
  ],
  "fallback": "body"
}
```

Populate the arrays with traced integer points from the approved standing master. Keep whiskers in `head`; the head mask must include their full transparent span while excluding chest pixels below the jaw.

- [ ] **Step 4: Build and sanitize the production layers**

Run:

```sh
node tools/brand-assets/build-waffle-standing-rig.mjs \
  assets/brand/waffle/poses/standing.png \
  assets/brand/waffle/rigs/standing-v1/masks.json \
  assets/brand/waffle/rigs/standing-v1
```

Write every output through PNGJS so forbidden ancillary chunks are absent. Copy decoded source pixels into `neutral-reference.png` through the same sanitizer rather than filesystem copy.

- [ ] **Step 5: Create `rig.json` with exact hashes and conservative pivots**

Use full-canvas normalized pivots based on the traced anatomy:

```json
{
  "body": { "x": 0.52, "y": 0.78 },
  "head": { "x": 0.39, "y": 0.43 },
  "left-ear": { "x": 0.315, "y": 0.245 },
  "right-ear": { "x": 0.465, "y": 0.245 },
  "tail-visible": { "x": 0.695, "y": 0.47 }
}
```

Record the approved source SHA-256 and all layer hashes after sanitization.

- [ ] **Step 6: Validate exact neutral fidelity and inspect contact sheets**

Run the rig validator. Create ignored light/dark contact sheets under `.superpowers/reviews/waffle/layered-rig-v1/`. Inspect full size and 320/160 px reductions. Adjust only polygon ownership if a movable part includes unrelated anatomy; never repaint visible pixels.

- [ ] **Step 7: Commit**

```sh
git add tools/brand-assets/build-waffle-standing-rig.mjs tools/brand-assets/test/build-waffle-standing-rig.test.mjs assets/brand/waffle/rigs/standing-v1
git commit -m "feat: add source-locked Waffle rig layers"
```

---

### Task 3: Hidden Repairs, Eyelids, and Motion-Limit Review

**Files:**

- Create: `assets/brand/waffle/rigs/standing-v1/layers/body-repair.png`
- Create: `assets/brand/waffle/rigs/standing-v1/layers/neck-repair.png`
- Create: `assets/brand/waffle/rigs/standing-v1/layers/left-ear-repair.png`
- Create: `assets/brand/waffle/rigs/standing-v1/layers/right-ear-repair.png`
- Create: `assets/brand/waffle/rigs/standing-v1/layers/tail-hidden.png`
- Create: `assets/brand/waffle/rigs/standing-v1/layers/left-eye-lid.png`
- Create: `assets/brand/waffle/rigs/standing-v1/layers/right-eye-lid.png`
- Modify: `assets/brand/waffle/rigs/standing-v1/rig.json`
- Create: `assets/brand/waffle/rigs/standing-v1/README.md`

**Interfaces:**

- Consumes: exact visible layers and anatomical masks from Task 2.
- Produces: neutral-hidden repair layers and blink overlays.
- Produces: final manifest layer order and safe control ranges.

- [ ] **Step 1: Create an ignored repair plate from the approved standing master**

Load the approved source as the edit target. Use built-in image editing with this invariant-heavy prompt:

```text
Use case: precise-object-edit
Asset type: hidden repair plate for a layered 2D animation rig
Primary request: reconstruct only the small hidden fur areas behind the head, both ear bases, and the tail root, plus natural closed upper eyelids matching this exact kitten
Input image: the approved standing Waffle is the edit target and identity authority
Constraints: preserve the exact canvas, pose, proportions, face, markings, colours, lighting, fur rendering, whiskers, and all visible pixels; repairs must match adjacent stripe direction and outline weight; no new pose and no restyling
Avoid: generic tabby drift, changed eyes, changed muzzle, changed silhouette, extra limbs, text, watermark, background, or shadow
```

Keep the generated plate under `.superpowers/concepts/waffle/`; never commit the whole generated image.

- [ ] **Step 2: Extract only declared hidden zones**

Align the repair plate to the approved source using canvas bounds and unchanged landmarks. Copy pixels only inside manually traced hidden-zone polygons. Every repair pixel must be fully covered by the neutral visible stack, so adding repair layers does not change neutral recomposition. If generated alignment or texture is inadequate, synthesize the tiny patch locally from adjacent painted pixels rather than expanding the generated region.

- [ ] **Step 3: Build closed eyelid overlays**

Crop only the two closed eyelids from the repair plate. Outside the eye ellipses all pixels are transparent. Eyelids are `visibleAtNeutral: false`; each overlay uses the head as parent and the centre of its approved eye as pivot. Do not deform the open eye artwork.

- [ ] **Step 4: Add repairs and controls to the manifest**

Repair layers have `role: "repair"` and `visibleAtNeutral: true`; eyelids have `role: "overlay"` and `visibleAtNeutral: false`. Parent ear repairs to `head`, visible ears to their matching repair group, head/neck repair to `body`, and tail pieces to `body`.

- [ ] **Step 5: Generate safe-range review frames locally**

Use the same PNG compositor plus deterministic affine transforms to create ignored previews for:

- head tilt at -3 and +3 degrees;
- both ears at their min/max rotations;
- tail at -5 and +5 degrees; and
- blink fully closed.

Review on warm white and charcoal backgrounds at 100%, 320 px, and 160 px. Reduce manifest limits when a seam becomes visible; never enlarge repair art merely to support exaggerated motion.

- [ ] **Step 6: Validate and commit**

Run rig validation, all brand tests, metadata scans, and `git diff --check`. Expected: neutral mismatches remain zero, hidden layers validate, and all previews stay within the declared limits.

```sh
git add assets/brand/waffle/rigs/standing-v1
git commit -m "feat: complete Waffle rig repair layers"
```

---

### Task 4: Fusion Assembly and Source-Locked Idle Test

**Files:**

- Create: `assets/brand/waffle/rigs/standing-v1/fusion/README.md`
- Modify: `assets/brand/waffle/README.md`
- Modify: `assets/brand/waffle/manifest.json`
- Local Resolve project: `_mcp_waffle_smoke_test`

**Interfaces:**

- Consumes: final `rig.json` and registered PNG layers.
- Produces: verified Fusion node graph and one four-second idle animation in the disposable project.
- Does not produce: rendered video or Resolve-owned production source of truth.

- [ ] **Step 1: Ask the owner to open Resolve only when Tasks 1-3 are complete**

Confirm the disposable project is active and saved. Do not touch the real Waffle editing project.

- [ ] **Step 2: Import the registered PNG layers and build the graph**

Use one Loader/MediaIn path per PNG, Transform nodes named after manifest IDs, and Merge nodes in exact draw order. Use two-element lists for Fusion point inputs, for example `Pivot: [0.39, 0.43]`. Read back every connection, pivot, and neutral transform.

- [ ] **Step 3: Verify the neutral graph before adding animation**

At frame 0, every transform must equal the manifest neutral value, eyelids must be hidden, and the viewer must match `neutral-reference.png`. Save the project only after graph readback succeeds.

- [ ] **Step 4: Add a four-second 30 fps idle loop**

Use frames 0-119 with frame 119 matching frame 0. Add:

- body breath at frames 0/30/60/90/119 with size `1/1.003/1.006/1.003/1`;
- head tilt at frames 0/40/80/119 with angles `0/0.7/-0.5/0`;
- blink at frames 52/55/58/61 with lid visibility/open amount `0/1/1/0`;
- left-ear twitch at frames 68/73/82 with angles `0/-4/0`; and
- tail sway at frames 0/30/60/90/119 with angles `0/3/0/-3/0`.

Use smooth splines except the short blink hold. Stay inside manifest limits.

- [ ] **Step 5: Strictly read back the final state**

Verify project/timeline IDs, current Fusion page, node connections, all pivots, and every keyframe. Do not rely on undo rollback semantics. Save after exact verification.

- [ ] **Step 6: Update documentation and run final repository checks**

Document layer roles, manifest controls, Fusion import expectations, and the no-MCP-export rule. Add the rig manifest to the static brand inventory without counting private review assets.

Run:

```sh
mise run brand-check
mise run fmt
mise run vet
mise run lint
mise run test
git diff --check
```

Expected: all checks pass; no private reference patterns or forbidden PNG metadata are present.

- [ ] **Step 7: Commit**

```sh
git add assets/brand/waffle/README.md assets/brand/waffle/manifest.json assets/brand/waffle/rigs/standing-v1/fusion/README.md
git commit -m "docs: add Waffle Fusion rig workflow"
```

## Completion Boundary

Stop with the verified animation open in Fusion for owner preview. Do not render,
export, add walking controls, build Remotion integration, or change the approved
standing master.
