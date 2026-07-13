# Waffle Character Asset System Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deliver Waffle's approved character canon, model and expression sheets, editable Motion Waffle vector master, three standalone SVG poses, transparent PNG previews, and a recorded QA decision.

**Architecture:** Owner-supplied private references feed disposable raster concept studies and then one manually authored, layered SVG source of truth. Small Node-based tools validate that production SVGs are safe, editable, bitmap-free, and structurally consistent, then render deterministic transparent PNG previews with resvg. Every creative stage pauses at a named review gate so identity drift cannot propagate into later assets.

**Tech Stack:** SVG 2 markup, Node.js 26, pnpm 11, `@resvg/resvg-js` 2.6.2, Node's built-in test runner, built-in image generation for disposable concepts, and the visual companion for owner review.

## Global Constraints

- Waffle is an eternal kitten with cat-first anatomy and movement.
- Preserve the forehead M, grey-green eyes, pale muzzle, leg bands, flank swirls, ringed tail, and long kitten proportions in every view and pose.
- AI-generated concepts are disposable derivatives; production assets must be manually reconstructed as editable vector shapes.
- Personal photos, videos, their expiring share URL, and identifying metadata must never be committed.
- Production SVGs must not contain `<image>`, `<script>`, inline event handlers, external asset URLs, or embedded raster data; the standard SVG XML namespace is allowed.
- Hero Waffle and the layered-SVG animation runtime are outside this milestone.
- Do not advance past a creative review gate without explicit owner approval.
- Run `mise run fmt`, `mise run vet`, `mise run lint`, and `mise run test` before declaring the milestone complete.

---

## File Map

**Create:**

- `tools/brand-assets/package.json` — isolated asset-tool dependencies and scripts.
- `tools/brand-assets/validate-svg.mjs` — validates production SVG structure, safety, IDs, and complexity budgets.
- `tools/brand-assets/render-svg.mjs` — renders transparent PNG previews through resvg.
- `tools/brand-assets/render-all.mjs` — regenerates every committed PNG preview.
- `tools/brand-assets/test/validate-svg.test.mjs` — validator regression tests.
- `tools/brand-assets/test/render-svg.test.mjs` — transparent-render regression test.
- `tools/brand-assets/test/fixtures/valid.svg` — minimal valid layered fixture.
- `tools/brand-assets/test/fixtures/embedded-raster.svg` — rejected fixture.
- `assets/brand/waffle/README.md` — asset contract, commands, review status, and output inventory.
- `assets/brand/waffle/canon/character-canon.md` — identity, palette, proportions, personality, and do/don't rules.
- `assets/brand/waffle/canon/reference-review.md` — non-identifying summary of reviewed private references.
- `assets/brand/waffle/canon/model-sheet.svg` — front, three-quarter, profile, and rear/top canonical views.
- `assets/brand/waffle/canon/expression-sheet.svg` — six approved expressions.
- `assets/brand/waffle/source/waffle-motion-master.svg` — named, layered production source of truth.
- `assets/brand/waffle/poses/standing.svg` — standalone neutral standing pose.
- `assets/brand/waffle/poses/sitting.svg` — standalone sitting pose.
- `assets/brand/waffle/poses/curled.svg` — standalone curled/resting pose.
- `assets/brand/waffle/exports/png/*.png` — transparent previews generated from committed SVGs.
- `assets/brand/waffle/qa/approval-record.md` — final gate outcomes and verification evidence.
- `tools/brand-assets/pnpm-lock.yaml` — reproducible asset-tool dependency lock.

**Modify:**

- `.gitignore` — ignore `.superpowers/`, where private references and visual review artifacts are cached.
- `mise.toml` — add `brand-install`, `brand-check`, and `brand-render` tasks without changing existing Go tasks.

---

### Task 1: Reproducible SVG Tooling and Privacy Boundary

**Files:**

- Modify: `.gitignore`
- Modify: `mise.toml`
- Create: `tools/brand-assets/package.json`
- Create: `tools/brand-assets/validate-svg.mjs`
- Create: `tools/brand-assets/render-svg.mjs`
- Create: `tools/brand-assets/render-all.mjs`
- Create: `tools/brand-assets/test/validate-svg.test.mjs`
- Create: `tools/brand-assets/test/render-svg.test.mjs`
- Create: `tools/brand-assets/test/fixtures/valid.svg`
- Create: `tools/brand-assets/test/fixtures/embedded-raster.svg`
- Create: `tools/brand-assets/pnpm-lock.yaml`

**Interfaces:**

- Produces: `pnpm --dir tools/brand-assets test`, `node tools/brand-assets/validate-svg.mjs <paths...>`, and `node tools/brand-assets/render-svg.mjs <input.svg> <output.png> <width>`.
- Produces mise tasks `brand-install`, `brand-check`, and `brand-render` for later tasks.

- [ ] **Step 1: Add package metadata, failing validator tests, and SVG fixtures**

Create tests using `node:test` that assert the valid fixture passes and the raster fixture fails with `embedded raster/image elements are forbidden`. The valid fixture must contain `viewBox="0 0 64 64"` and groups named `silhouette`, `face`, `markings`, and `shading`. The rejected fixture must contain an `<image href="data:image/png;base64,...">` element.

Create `package.json` with `private: true`, `type: "module"`, `packageManager: "pnpm@11.7.0"`, exact dependencies `@resvg/resvg-js: 2.6.2`, `fast-xml-parser: 5.9.3`, and `pngjs: 7.0.0`, and scripts `test`, `validate`, and `render`.

- [ ] **Step 2: Run the validator test to verify RED**

Run: `node --test tools/brand-assets/test/validate-svg.test.mjs`

Expected: FAIL with `ERR_MODULE_NOT_FOUND` for `validate-svg.mjs`.

- [ ] **Step 3: Implement `validate-svg.mjs`**

Export `validateSvg(path, requirements = {})`. Read UTF-8 markup, call `XMLValidator.validate` from `fast-xml-parser`, and return `{ path, elementCount, pathCount, ids }`. Throw an error when the root is not SVG, `viewBox` is missing, required IDs are absent, duplicate IDs exist, total elements exceed 500, paths exceed 240, or forbidden content is found. Reject `<image>`, `<script>`, `data:image/`, `javascript:`, attributes beginning with `on`, and external values in `href`, `xlink:href`, CSS `url(...)`, or paint-server attributes. Allow only the exact root namespace `xmlns="http://www.w3.org/2000/svg"`; do not reject the namespace as an external URL.

Define filename policies in the CLI: `model-sheet.svg` requires all four view IDs and their uniquely prefixed layer IDs; `expression-sheet.svg` requires all six expression IDs; `waffle-motion-master.svg` requires the rig-layer IDs from Task 4; each pose requires `silhouette`, `face`, `markings`, and `shading`.

The CLI must validate every supplied path, print `PASS <path> elements=<n> paths=<n>`, and exit non-zero on the first failure.

- [ ] **Step 4: Install dependencies and verify the validator is GREEN**

Run: `pnpm --dir tools/brand-assets install`

Expected: `tools/brand-assets/pnpm-lock.yaml` is created.

Run: `node --test tools/brand-assets/test/validate-svg.test.mjs`

Expected: validator tests PASS.

- [ ] **Step 5: Add the failing transparent-render test**

Render the valid fixture at width 256, parse the PNG with `pngjs`, and assert width `256`, height `256`, and alpha `0` at pixel `(0, 0)`.

- [ ] **Step 6: Run the render test to verify RED**

Run: `node --test tools/brand-assets/test/render-svg.test.mjs`

Expected: FAIL with `ERR_MODULE_NOT_FOUND` for `render-svg.mjs`.

- [ ] **Step 7: Implement `render-svg.mjs`**

Export `renderSvg(inputPath, outputPath, width)`. Construct `new Resvg(svg, { fitTo: { mode: "width", value: width } })`, call `.render().asPng()`, and write the buffer. Do not set a background colour.

- [ ] **Step 8: Add the privacy ignore and mise tasks**

Add `/.superpowers/` to `.gitignore`. Add mise tasks whose commands are:

```toml
[tasks.brand-install]
run = "pnpm --dir tools/brand-assets install --frozen-lockfile"

[tasks.brand-check]
run = "pnpm --dir tools/brand-assets test && node tools/brand-assets/validate-svg.mjs assets/brand/waffle/canon/model-sheet.svg assets/brand/waffle/canon/expression-sheet.svg assets/brand/waffle/source/waffle-motion-master.svg assets/brand/waffle/poses/standing.svg assets/brand/waffle/poses/sitting.svg assets/brand/waffle/poses/curled.svg"

[tasks.brand-render]
run = "node tools/brand-assets/render-all.mjs"
```

Also create `render-all.mjs` to render the two sheets at width 1600 and each pose at width 800 into `assets/brand/waffle/exports/png/`.

- [ ] **Step 9: Verify the complete tool suite is GREEN**

Run: `pnpm --dir tools/brand-assets test`

Expected: all validator and renderer tests PASS.

- [ ] **Step 10: Commit**

```bash
git add .gitignore mise.toml tools/brand-assets
git commit -m "build: add Waffle asset validation tools"
```

---

### Task 2: Canon, Palette, and Private Reference Review

**Files:**

- Create: `assets/brand/waffle/README.md`
- Create: `assets/brand/waffle/canon/character-canon.md`
- Create: `assets/brand/waffle/canon/reference-review.md`
- Local only: `.superpowers/references/waffle/`

**Interfaces:**

- Consumes: owner-supplied private album and the approved design spec.
- Produces: named palette tokens and proportional rules consumed by every SVG task.

- [ ] **Step 1: Acquire private references into the ignored cache**

Use the album from the active task context. Save the original generated seed plus the owner-supplied photos/videos under `.superpowers/references/waffle/`. If the browser presents a download permission prompt, request the narrow download confirmation before accepting it. Do not rename files with dates, locations, or people.

- [ ] **Step 2: Prove the privacy boundary**

Run: `git status --short -- .superpowers`

Expected: no output because `/.superpowers/` is ignored.

Run: `rg -n "share\.icloud\.com|0b3jA_|EXIF|GPS" assets tools mise.toml .gitignore`

Expected: no matches.

- [ ] **Step 3: Write `character-canon.md`**

Record the exact identity anchors from the design. Define these initial palette tokens, to be visually tuned only at the marking review gate:

```text
ginger-base    #E99A42
ginger-light   #F5C579
ginger-shadow  #B9662D
stripe         #8D481F
muzzle-cream   #F8E2B9
eye            #7E8A68
eye-dark       #30352C
nose           #D98278
outline        #613619
```

Specify body length `3.1 head widths`, shoulder height `1.45 head heights`, tail length `0.9 body lengths`, ears `0.48 head heights`, and eyes separated by `0.42 eye widths`. Mark these as vector construction ratios rather than biological measurements.

- [ ] **Step 4: Write `reference-review.md` and `README.md`**

State only that 11 owner-supplied photos and 3 owner-supplied videos were reviewed on 2026-07-13. Summarize useful views and motion cues without filenames, URLs, dates, locations, or people. In `README.md`, document source-of-truth rules, directory roles, validation/render commands, current gate status, and the prohibition on committing private media.

- [ ] **Step 5: Review and commit**

Run: `rg -n "share\.icloud\.com|0b3jA_|GPS" assets/brand/waffle`

Expected: no matches.

Present the canon and palette to the owner. Do not continue until approved.

```bash
git add assets/brand/waffle/README.md assets/brand/waffle/canon/character-canon.md assets/brand/waffle/canon/reference-review.md
git commit -m "docs: define Waffle character canon"
```

---

### Task 3: Silhouette Studies and Canonical Model Sheet

**Files:**

- Create: `assets/brand/waffle/canon/model-sheet.svg`
- Local only: `.superpowers/concepts/waffle/model-sheet-*.png`

**Interfaces:**

- Consumes: canon palette, ratios, private references, and seed illustration.
- Produces: approved view silhouettes and coordinate conventions for the vector master and poses.

- [ ] **Step 1: Generate three disposable model-sheet studies**

Make three separate built-in image-generation calls using the seed as a style reference and the clearest front, profile, full-body, belly, and tail photos as identity references. Use this normalized prompt for each call, changing only the final simplification line to `minimal`, `balanced`, or `detailed`:

```text
Use case: stylized-concept
Asset type: disposable character model-sheet study
Primary request: Design the same eternal orange-tabby kitten in front, three-quarter, profile, and rear/top views. Cat-first anatomy; long slightly lanky kitten proportions; warm ginger classic-tabby coat; strong forehead M; pale muzzle, chin, eye surrounds, and belly; grey-green eyes; pink nose; banded legs; broken flank swirls; distinctly ringed tail.
Style/medium: clean vector-friendly storybook mascot concept, balanced simplification
Composition/framing: four full-body orthographic views aligned to one baseline, generous spacing
Constraints: identical proportions and markings in every view; no accessories; no text; no logo; plain warm-white background; production reference only
Avoid: adult-cat proportions, chibi shortening, permanent biped stance, human hands, clothing, photorealism, painterly fur, gradients that obscure markings
Simplification: <minimal|balanced|detailed>
```

- [ ] **Step 2: Present the three studies in the visual companion**

Label them Minimal, Balanced, and Detailed. Ask the owner to approve one silhouette language or request one bounded revision. Store the selected direction and feedback in the task record, not in the SVG.

- [ ] **Step 3: Author the model sheet as vector geometry**

Create a `1600 900` viewBox with four artboards/groups: `view-front`, `view-three-quarter`, `view-profile`, and `view-rear-top`. Each view must contain uniquely prefixed child groups, for example `front-silhouette`, `front-face`, `front-markings`, and `front-shading`; apply the same prefix pattern to the other views. Use only `<path>`, `<ellipse>`, `<circle>`, `<g>`, `<defs>`, `<clipPath>`, and gradients; do not trace every strand of fur. Keep the entire sheet below 240 paths and 500 total elements.

- [ ] **Step 4: Validate and render**

Run: `node tools/brand-assets/validate-svg.mjs assets/brand/waffle/canon/model-sheet.svg`

Expected: PASS with all required view and layer IDs present.

Run: `node tools/brand-assets/render-svg.mjs assets/brand/waffle/canon/model-sheet.svg assets/brand/waffle/exports/png/model-sheet.png 1600`

Expected: transparent `1600×900` PNG.

- [ ] **Step 5: Review gate and commit**

Show the SVG on both white and dark checkerboard backgrounds, plus crops at 160 px character height. Obtain explicit approval for silhouette and proportions.

```bash
git add assets/brand/waffle/canon/model-sheet.svg assets/brand/waffle/exports/png/model-sheet.png
git commit -m "feat: add Waffle model sheet"
```

---

### Task 4: Motion Master, Face, and Expression Language

**Files:**

- Create: `assets/brand/waffle/source/waffle-motion-master.svg`
- Create: `assets/brand/waffle/canon/expression-sheet.svg`
- Create: `assets/brand/waffle/exports/png/expression-sheet.png`

**Interfaces:**

- Consumes: approved three-quarter model-sheet view and canon palette.
- Produces: named animation-ready groups and six approved facial states used by poses and later layered-SVG animation work.

- [ ] **Step 1: Add a failing structural fixture test for the motion master**

Extend `validate-svg.test.mjs` with required master IDs: `tail`, `torso`, `rear-leg-left`, `rear-leg-right`, `front-leg-left`, `front-leg-right`, `head`, `ear-left`, `ear-right`, `eye-left`, `eye-right`, `eyelids`, `muzzle`, `mouth`, `whiskers`, `markings`, and `shading`.

Run: `pnpm --dir tools/brand-assets test`

Expected: FAIL until a valid master fixture supplies every required ID.

- [ ] **Step 2: Build the layered motion master**

Use a `0 0 800 800` viewBox and place the neutral standing three-quarter character on a transparent background. Order groups back-to-front so hidden limb and tail segments exist rather than being cut away. Put pivots in `data-pivot-x` and `data-pivot-y` attributes on head, ear, limb, and tail groups. Keep marking shapes separate from body shapes and clipped to their anatomical group.

- [ ] **Step 3: Build the expression sheet**

Create six `320×320` face cells with IDs `neutral`, `curious`, `pleased`, `focused`, `startled`, and `sleepy`. Preserve head shape, forehead M, eye spacing, and muzzle proportions across cells; change only eyelids, pupils, brows/forehead accents, ear angle, and mouth.

- [ ] **Step 4: Validate, render, and review**

Run: `node tools/brand-assets/validate-svg.mjs assets/brand/waffle/canon/model-sheet.svg assets/brand/waffle/canon/expression-sheet.svg assets/brand/waffle/source/waffle-motion-master.svg`

Expected: all three supplied SVGs PASS without attempting the not-yet-created poses.

Run: `node tools/brand-assets/render-svg.mjs assets/brand/waffle/canon/expression-sheet.svg assets/brand/waffle/exports/png/expression-sheet.png 1600`

Expected: transparent PNG with all six expressions.

Present 100%, 25%, and 64 px face crops. Obtain explicit approval for face, markings, and palette.

- [ ] **Step 5: Commit**

```bash
git add tools/brand-assets/test assets/brand/waffle/source/waffle-motion-master.svg assets/brand/waffle/canon/expression-sheet.svg assets/brand/waffle/exports/png/expression-sheet.png
git commit -m "feat: add Waffle motion master and expressions"
```

---

### Task 5: Standing, Sitting, and Curled SVG Poses

**Files:**

- Create: `assets/brand/waffle/poses/standing.svg`
- Create: `assets/brand/waffle/poses/sitting.svg`
- Create: `assets/brand/waffle/poses/curled.svg`
- Create: `assets/brand/waffle/exports/png/standing.png`
- Create: `assets/brand/waffle/exports/png/sitting.png`
- Create: `assets/brand/waffle/exports/png/curled.png`

**Interfaces:**

- Consumes: approved motion master, face language, palette, and model-sheet views.
- Produces: the first documentation-ready pose pack.

- [ ] **Step 1: Author the neutral standing pose**

Derive geometry from the motion master. Keep four paws grounded, tail in a relaxed upward hook, head in three-quarter view, ears attentive, and expression curious-neutral. Use a square `0 0 800 800` viewBox and required groups `silhouette`, `face`, `markings`, and `shading`.

- [ ] **Step 2: Author the sitting pose**

Keep the spine and rear haunches feline, forelegs straight, paws adjacent, and tail wrapped loosely around the front-right side. Do not widen the torso into an upright human chest.

- [ ] **Step 3: Author the curled pose**

Echo the seed illustration's readable curled silhouette while correcting identity from the real references. Keep both front paws visible, the tail wrapping across the foreground, and the face alert rather than asleep.

- [ ] **Step 4: Validate all production SVGs**

Run: `mise run brand-check`

Expected: all six production SVGs PASS; Node tests PASS.

- [ ] **Step 5: Render all PNG previews**

Run: `mise run brand-render`

Expected: `standing.png`, `sitting.png`, and `curled.png` are each 800 px wide with transparent corner pixels.

- [ ] **Step 6: Review at documentation sizes**

Present all poses at 800, 320, 160, and 64 px character height on light and dark backgrounds. Obtain explicit approval that they read as the same kitten and remain cat first.

- [ ] **Step 7: Commit**

```bash
git add assets/brand/waffle/poses assets/brand/waffle/exports/png
git commit -m "feat: add Waffle documentation poses"
```

---

### Task 6: QA Record, Full Verification, and Milestone Handoff

**Files:**

- Modify: `assets/brand/waffle/README.md`
- Create: `assets/brand/waffle/qa/approval-record.md`
- Local only: `.superpowers/reviews/waffle-first-milestone.html`

**Interfaces:**

- Consumes: every first-milestone asset and all prior owner approvals.
- Produces: auditable acceptance evidence and a stable asset inventory for documentation-site work.

- [ ] **Step 1: Build the private QA contact sheet**

Create an ignored HTML review page containing the model sheet, expression sheet, three poses, seed illustration, and only the minimum private-reference crops needed to compare the forehead M, eyes, muzzle, flank swirls, leg bands, tail rings, and proportions. Do not copy reference media into committed output.

- [ ] **Step 2: Record gate outcomes**

In `approval-record.md`, create one row per deliverable with columns `Deliverable`, `Identity`, `Anatomy`, `Markings`, `Small-size`, `Vector structure`, `Owner decision`, and `Evidence date`. Every cell must be `PASS`, `N/A`, or a concrete revision request; no blank cells.

- [ ] **Step 3: Run asset verification**

Run: `pnpm --dir tools/brand-assets test`

Expected: all tests PASS.

Run: `mise run brand-check`

Expected: all production SVGs PASS.

Run: `git grep -n -E '(<image|data:image|javascript:| on[a-z]+=)' -- 'assets/brand/waffle/**/*.svg'`

Expected: no output.

- [ ] **Step 4: Verify browser-engine parity**

Open the ignored QA page in the in-app Chromium browser and Safari, inspect every SVG on light and dark backgrounds, and store screenshots under `.superpowers/reviews/`. If Firefox is absent, request approval to install it with `brew install --cask firefox`; after approval, repeat the same check in Firefox. Record a PASS for Chromium, Safari, and Firefox in `approval-record.md`; do not substitute resvg output for this browser check.

- [ ] **Step 5: Run repository verification**

Run: `mise run fmt`

Expected: PASS with no unformatted Go files.

Run: `mise run vet`

Expected: PASS.

Run: `mise run lint`

Expected: PASS.

Run: `mise run test`

Expected: all Go tests and zero-network evaluations PASS.

- [ ] **Step 6: Update the asset inventory and get final approval**

Update `README.md` with every committed source/export path, the exact regeneration commands, and status `First static milestone approved`. Present the private contact sheet and recorded checks to the owner. Do not mark the milestone approved until the owner explicitly accepts it.

- [ ] **Step 7: Commit**

```bash
git add assets/brand/waffle/README.md assets/brand/waffle/qa/approval-record.md
git commit -m "docs: approve Waffle static asset milestone"
```

---

## Completion Boundary

Stop after Task 6. The next design/plan cycle covers Hero Waffle, layered-SVG animation with the Web Animations API and optional Motion helpers, documentation-site integration, and Remotion or Blender promo compositions. Do not add animation code, Remotion or Blender project files, rendered video, site framework, or Hero Waffle files during this plan.
