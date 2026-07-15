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
| `front-paw-left` | `planted`, `lifted`, `wave` | `planted` |
| `front-paw-right` | `planted`, `lifted` | `planted` |
| `rear-paw-left` | `planted`, `lifted` | `planted` |
| `rear-paw-right` | `planted`, `lifted` | `planted` |
| `head-base` | `neutral`, `turn-left`, `turn-right` | `neutral` |
| `jaw` | `closed`, `open` | `closed` |

Paw, head-turn, and jaw extremes use these registered paintings, not aggressive raster rotation. Swap a paw only at a registered interchange pose while motion conceals the change. Turned head paintings replace the declared descendant face layers as a coherent state.

## Controls and clips

Normalized translation controls use canvas fractions; rotation controls use degrees; `breath`, `weightShift`, turn/state, blink, and jaw controls are unitless. The exact ranges and bindings in `rig.json` are authoritative and must not be widened. Shoulder/upper-leg, elbow/lower-leg, wrist/paw, hip/thigh, hock, and rear-paw rotations additionally obey their per-layer limits. `headTurn` and `jawOpen` bind numeric values to registered variant members through ordered, upper-inclusive thresholds: the first member covers the control minimum through its `max`, and each later member covers values above the preceding threshold through its own `max`. Thresholds are strictly increasing and the last equals the control maximum. Explicit `clip.variants` keys override a numeric-derived state when both target the same set.

`blinkLeft` and `blinkRight` independently drive the opacity of their source-matched upper and lower lid overlays from 0 to 1. Opacity activation never translates or rotates the painted lids, so their full-canvas registration stays exact. Equal left/right values produce a synchronized blink.

Motion clips use schema version 1 and declare `id`, `fps`, `frameCount`, `loop`, `requiredClosure`, variant keyframes, and numeric control keyframes. Numeric keys interpolate linearly unless held; variant keys always hold. Every variant set must have a deterministic frame-0 state: an unbound set requires an explicit frame-0 `clip.variants` key, while a numeric-bound set may derive it from its control. A looping clip must evaluate to exactly equal controls, variants, and world matrices at its closure frames.

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

The base builder stages a source-only sibling package while preserving the authoritative `repairs.json`, `variants.json`, `GENERATION.md`, and package `README.md` needed by the art pass. Controls that target art-pass layers or variant sets are deliberately deferred in the temporary source-only manifest, then restored from authoritative `masks.json` after the art builder registers those targets. Both builders promote only validated packages and never write standing v1.

Validation requires contained nonsymlink paths, current SHA-256 hashes, sanitized RGBA PNGs, exact dimensions, package size limits, an acyclic hierarchy, valid variants and controls, and zero visible neutral mismatches against both the approved source and `neutral-reference.png`.

Artwork must pass still review at full resolution, 320 px, and 160 px on warm-white and charcoal backgrounds before motion authoring. Motion review then checks feline gait and balance, planted baselines, anatomy and joint seams, marking continuity, expression, eye bounds, blink coverage, jaw and whiskers, tail continuity, and exact loop closure before Fusion assembly.

## Fusion handoff

Fusion mirrors `rig.json` with Loaders, Transforms, and Merges; it is downstream, never authoritative. Convert every top-left manifest pivot with `fusionY = 1 - manifestY`. Read back each MCP node, path, pivot, connection, control, variant visibility, and keyframe write before saving a verified subtree. Do not render or export through the Resolve MCP. The owner handles delivery exports.
