# Waffle Standing Rig v2

Standing v2 is Waffle's source-locked, tool-neutral articulated raster rig. The approved `poses/standing.png` composite is the sole visible-pixel authority for the neutral pose; `neutral-reference.png` is its byte-identical package copy. Every production raster is a registered 1536 x 1024 RGBA PNG, and `rig.json` owns the hierarchy, pivots, hashes, variants, controls, and anatomical limits.

`left` and `right` always mean screen-left and screen-right. They never mean Waffle's anatomical left and right.

## Hierarchy and artwork ownership

`waffle-root` is synthetic and has no raster. It parents the torso, both rear leg chains, both front leg chains, the three-part tail, and `head-base`. Each front chain is shoulder/upper leg to elbow/lower leg to wrist/paw; each rear chain is hip/thigh to hock to paw. The head owns both ears, both iris/pupil/highlight stacks, four eyelid overlays, muzzle, jaw, and rigid whiskers. Concealed repair layers cover the neck and articulated limb and tail joints, but remain hidden at neutral.

Source-derived visible layers own neutral pixels. Repair and non-neutral variant layers own only their declared bounded regions. `masks.json`, `repairs.json`, and `variants.json` are deterministic build inputs; `GENERATION.md` records the production-safe provenance for painted plates without making ignored working material part of the package.

## Variants

Variant sets are mutually exclusive and registered to their anchor layer. Each set has exactly one neutral member:

| Set | Members | Neutral |
| --- | --- | --- |
| `front-chain-left` | `neutral`, `low-lift`, `landing`, `paw-lifted`, `paw-wave`, `paw-landing` | `neutral` |
| `front-chain-right` | `neutral`, `landing`, `low-lift` | `neutral` |
| `rear-chain-left` | `neutral`, `landing`, `low-lift` | `neutral` |
| `rear-chain-right` | `neutral`, `landing`, `low-lift` | `neutral` |
| `front-paw-left` | `planted`, `lifted`, `wave` | `planted` |
| `front-paw-right` | `planted`, `lifted` | `planted` |
| `rear-paw-left` | `planted`, `lifted` | `planted` |
| `rear-paw-right` | `planted`, `lifted` | `planted` |
| `head-base` | `neutral`, `turn-left`, `turn-right`, `blink-left` (clip-only), `blink-right` (clip-only) | `neutral` |
| `jaw` | `closed`, `open` | `closed` |

The four chain sets are the walk-in-place swing states. Each opaque `low-lift` painting replaces the complete declared leg subtree and follows the source-locked torso through its private parent override. Each discrete `landing` painting bakes the registered interchange offset into the approved low-lift upper, applies a baked premultiplied blend through the overlap band, and restores exact neutral distal pixels below the concealed seam; it has no parent override or member transform. A matching hidden, feathered `walk-socket-*` underlay fills the vacated root from the approved standing master. The two front joints also use compact feathered `walk-cover-*` layers; the rear members instead blend directly into shaped underlays so no source rectangle can switch above the legs.

The paw-wave acceptance clip selects `front-chain-left/paw-lifted`, `paw-wave`, and `paw-landing` as complete opaque screen-left foreleg paintings. They suppress the descendant articulated lower leg, paw, elbow repair, and wrist repair together, so lifting the paw cannot uncover the gaps and edge fragments produced by rotating a segmented raster limb. `paw-landing` is sampled directly from the approved standing source and provides a source-exact return to the planted pose. The older `front-paw-left` local variants remain available for conservative fallback assembly, but the accepted paw-wave clip does not use them.

Paw, head-turn, blink, and jaw extremes use registered paintings, not aggressive raster rotation. Transition a painting only at a registered interchange pose while motion conceals the change. Turned and blinking head paintings replace all declared descendant face layers as one coherent state. The two painted blink heads are clip-only: they may be selected explicitly by a clip, but are excluded from the numeric `headTurn` thresholds.

## Controls and clips

Normalized translation controls use canvas fractions; rotation controls use degrees; `breath`, `weightShift`, turn/state, blink, and jaw controls are unitless. The exact ranges and bindings in `rig.json` are authoritative and must not be widened. Shoulder/upper-leg, elbow/lower-leg, wrist/paw, hip/thigh, hock, and rear-paw rotations additionally obey their per-layer limits. `headTurn` and `jawOpen` bind numeric values to registered variant members through ordered, upper-inclusive thresholds: the first member covers the control minimum through its `max`, and each later member covers values above the preceding threshold through its own `max`. Thresholds are strictly increasing and the last equals the control maximum. Explicit `clip.variants` keys override a numeric-derived state when both target the same set.

`blinkLeft` and `blinkRight` retain the original articulated upper/lower-lid opacity bindings as a legacy fallback for downstream experiments. Motion review showed that compositing those small lid fragments over the live eye produced visible rectangular edges, jagged eye detail, and progressive fidelity loss. The accepted `paw-wave` and `head-face-tail` clips therefore hold both controls at `0` and select the complete source-locked `head-base/blink-left` and `blink-right` paintings instead. Each painted blink head owns the whole registered face for its held frame and hides the ear, muzzle, jaw, eye, lid, highlight, and whisker descendants, preventing partial-feature mixing. This is a deliberate fidelity-preserving deviation from the initial articulated-lid plan.

Motion clips use schema version 1 and declare `id`, `fps`, `frameCount`, `loop`, `requiredClosure`, variant keyframes, and numeric control keyframes. Numeric keys interpolate linearly unless held; variant keys always hold. Every variant set must have a deterministic frame-0 state: an unbound set requires an explicit frame-0 `clip.variants` key, while a numeric-bound set may derive it from its control. A looping clip must evaluate to exactly equal controls, variants, private layer opacity, member transforms, and world matrices at its closure frames.

Optional `variantTransforms` provide conservative member-local motion for an opaque selected non-neutral member. Each transform targets one known non-neutral member selected by `variants` and may animate normalized `x`/`y`, rotation, and scale tracks within the schema's bounded ranges from frame 0 through the final frame. A non-neutral member may declare `parentOverride` to follow a different registered parent while active. The selected member remains fully opaque and suppresses its declared descendants without alpha overlap. Rendering composes the selected parent's world transform, the anchor layer's local transform, and then the member-local transform around the same registered pivot. Loop endpoints must close exactly.

Optional `layerOpacity` tracks are clip-private visibility channels for registered layers that are hidden at neutral and have role `repair` or `overlay`. They do not add or widen a public rig control, and a target may not also have a public opacity binding. Values are finite from 0 through 1, interpolate linearly unless held, must declare frame 0 and the final frame, and must close exactly for a loop. The walk clip switches each fixed `walk-socket-*` and front `walk-cover-*` layer between Boolean 0 and 1 with its matching discrete `landing` or `low-lift` member.

The v2 motion acceptance clips are 24 fps: 48 frames for walk-in-place, 72 frames for the paw wave, and 48 frames for the head/face/tail proof. This three-quarter rig does not support a side-profile travelling walk; that requires a separate source-locked rig.

## Build and validation

Before running either rebuild command, the ignored concept/edit plates referenced by both `repairs.json` and `variants.json` must already be retained locally. They are not production deliverables, but the full base-then-art rebuild sequence depends on them; if they are missing, do not start either rebuild command and use the committed generated rasters plus validation commands instead.

Run commands from the repository root:

```sh
node tools/brand-assets/build-waffle-standing-rig-v2.mjs \
  assets/brand/waffle/poses/standing.png \
  assets/brand/waffle/rigs/standing-v2/masks.json \
  assets/brand/waffle/rigs/standing-v2
node tools/brand-assets/build-waffle-rig-v2-art.mjs \
  assets/brand/waffle/rigs/standing-v2/rig.json \
  assets/brand/waffle/rigs/standing-v2/repairs.json \
  assets/brand/waffle/rigs/standing-v2/variants.json
mise run brand-rig-check
mise run brand-check
```

The base builder stages a source-only sibling package while preserving the authoritative `repairs.json`, `variants.json`, `GENERATION.md`, package `README.md`, and authored files beneath `motions/`. Controls that target art-pass layers or variant sets are deliberately deferred in the temporary source-only manifest, then restored from authoritative `masks.json` after the art builder registers those targets. Both builders promote only validated packages and never write standing v1.

Validation requires contained nonsymlink paths, current SHA-256 hashes, sanitized RGBA PNGs, exact dimensions, package size limits, an acyclic hierarchy, valid variants and controls, and zero visible neutral mismatches against both the approved source and `neutral-reference.png`.

Artwork must pass still review at full resolution, 320 px, and 160 px on warm-white and charcoal backgrounds before motion authoring. Motion review then checks feline gait and balance, planted baselines, anatomy and joint seams, marking continuity, expression, eye bounds, blink coverage, jaw and whiskers, tail continuity, and exact loop closure before Fusion assembly.

## Fusion handoff

Fusion mirrors `rig.json` with Loaders, Transforms, and Merges; it is downstream, never authoritative. Convert every top-left manifest pivot with `fusionY = 1 - manifestY`. Implement each clip-private `layerOpacity` track on the corresponding hidden repair or overlay Merge's Blend channel, keeping that channel separate from the published controller group; for the walk, the four `walk-socket-*` Loaders remain torso-parented and their Blends follow the exact Boolean 0/1 clip keys. Read back each MCP node, path, pivot, connection, control, variant visibility, private opacity key, and connection write before saving a verified subtree. Do not render or export through the Resolve MCP. The owner handles delivery exports.

The committed [`fusion/README.md`](fusion/README.md) describes the Resolve assembly workflow and boundaries. [`fusion/assembly-record.json`](fusion/assembly-record.json) records the saved MCP assembly/readback capability proof. That proof verifies node creation, connections, spline keyframes, exact readback, and project saving; it is not the full 55-layer Waffle composition and it was not exported.
