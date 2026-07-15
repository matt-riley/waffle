# Waffle Standing Rig v2 Design

## Decision

Build Standing Rig v2 as a source-locked articulated hybrid raster rig. The approved 1536 x 1024 three-quarter standing PNG remains the neutral-pose authority. Its visible pixels are partitioned into articulated pieces; new painting is limited to concealed joint overlap, alternate paw states, small head-turn plates, independent eye components, eyelids, and a restrained jaw plate.

V2 must support a convincing three-quarter walk-in-place loop and a natural front-paw wave that begins and ends on all fours. It also adds multi-segment tail bending, gaze, fuller head movement, independent blinks, and mouth opening. A side-profile travelling walk is a separate future rig because forcing it from the three-quarter source would compromise Waffle's markings and anatomy.

## Identity and Fidelity Constraints

The existing character canon remains binding. Waffle is an eternal kitten and cat first. Her gait, balance, joint direction, weight transfer, paw shapes, tail motion, and return to four-legged stance must read as feline rather than as a human mascot.

The following neutral features may not drift:

- forehead M, crown and cheek stripes;
- grey-green irises, pupil proportions, highlights, and pale eye surrounds;
- pink nose, pale muzzle, chin, chest and belly;
- leg bands, flank swirls, and tail rings;
- large ears, long whiskers, long kitten legs, and overall silhouette; and
- the exact decoded visible RGBA pixels of `assets/brand/waffle/poses/standing.png`.

Generated or painted alternate states must match those anchors, the established light direction, fur texture, outline weight, colour palette, and three-quarter perspective. Private real-cat references and generated full-frame studies remain ignored workspace material and never enter Git.

## Deliverables

The repository owns the tool-neutral package under `assets/brand/waffle/rigs/standing-v2/`:

- full-canvas 1536 x 1024 RGBA PNG layers;
- schema-versioned `rig.json` with hierarchy, pivots, controls, variants, limits, and hashes;
- deterministic `masks.json`, `repairs.json`, and `variants.json` build inputs;
- `neutral-reference.png`, decoded from the approved standing source;
- source-derived visible layers and separately identified repair/variant layers;
- a deterministic local motion-preview compositor;
- validation tests for schema, paths, hashes, hierarchy, variants, controls, and neutral reconstruction;
- ignored full-resolution and documentation-size motion review frames; and
- a downstream Fusion assembly and workflow record.

Standing Rig v1 remains unchanged as the lightweight idle rig.

## Layer Architecture

`left` and `right` always mean screen-left and screen-right. Every layer is registered to the complete source canvas so its neutral position is unambiguous.

```text
waffle-root
├── body-repair
├── torso
├── neck-repair
├── rear-leg-left
│   ├── rear-thigh-left
│   ├── rear-hock-left
│   └── rear-paw-left
├── rear-leg-right
│   ├── rear-thigh-right
│   ├── rear-hock-right
│   └── rear-paw-right
├── front-leg-left
│   ├── front-upper-left
│   ├── front-lower-left
│   └── front-paw-left: planted | lifted | wave
├── front-leg-right
│   ├── front-upper-right
│   ├── front-lower-right
│   └── front-paw-right: planted | lifted
├── tail
│   ├── tail-base
│   ├── tail-mid
│   └── tail-tip
└── head
    ├── head-base: neutral | turn-left | turn-right
    ├── ear-left
    ├── ear-right
    ├── eye-left
    │   ├── iris-left
    │   ├── pupil-left
    │   ├── highlight-left
    │   ├── upper-lid-left
    │   └── lower-lid-left
    ├── eye-right
    │   ├── iris-right
    │   ├── pupil-right
    │   ├── highlight-right
    │   ├── upper-lid-right
    │   └── lower-lid-right
    ├── muzzle
    ├── jaw: closed | open
    └── whiskers
```

Each upper/lower limb boundary includes source-matched underlap and a concealed repair plate. Paws use painted state swaps rather than extreme rotations. The tail pieces overlap beneath the preceding section so rings remain connected. Whiskers stay on a separate rigid facial layer and never undergo mesh deformation.

## Source-Locked Artwork Pipeline

1. Trace ordered polygons against the approved standing master.
2. Partition neutral visible pixels deterministically. Overlap is allowed only where the upper layer is fully opaque or the validator can prove neutral source-over equivalence.
3. Build concealed shoulder, elbow, wrist, hip, hock, neck, and tail-joint repairs from tightly constrained source-matched edit plates.
4. Create alternate paw, head-turn, eye, lid, and jaw plates as precise edits of the approved source and model sheet.
5. Extract only declared variant zones. The full generated edit stays under ignored `.superpowers/` review storage.
6. Sanitize each production PNG through PNGJS, calculate SHA-256, and validate metadata, dimensions, alpha, and local containment.
7. Recompose the neutral-visible stack and require zero visible-pixel mismatches against both the approved source and neutral reference.

When a generated plate drifts in identity, perspective, anatomy, marking placement, or texture, it is rejected rather than corrected by enlarging a mask or relaxing validation. A new plate is produced from the approved source. Alternate artwork is never allowed to silently replace neutral source pixels.

## Rig Manifest and Controls

V2 extends the current manifest contract with named variant sets and articulated control bindings. Every variant declares exactly one neutral member, compatible dimensions, local paths, SHA-256 values, parent, pivot, draw order, and mutually exclusive visibility.

Raw control limits are intentionally conservative:

| Control | Range | Meaning |
| --- | --- | --- |
| `breath` | `0..1` | restrained body breathing |
| `bodyBob` | `-0.015..0.015` | normalized vertical displacement |
| `bodyLean` | `-3..3` | root degrees around low chest pivot |
| `weightShift` | `-1..1` | left/right feline support shift |
| `headTilt` | `-5..5` | head degrees around neck |
| `headTurn` | `-1..1` | blend/state selection across painted head plates |
| `gazeX` | `-0.012..0.012` | normalized pupil displacement |
| `gazeY` | `-0.009..0.009` | normalized pupil displacement |
| `blinkLeft` | `0..1` | left lid closure |
| `blinkRight` | `0..1` | right lid closure |
| `earLeft` | `-6..6` | left-ear degrees |
| `earRight` | `-6..6` | right-ear degrees |
| `jawOpen` | `0..1` | jaw state/blend |
| `tailBase` | `-8..8` | base degrees |
| `tailMid` | `-12..12` | mid-section degrees |
| `tailTip` | `-15..15` | tip degrees |
| limb rotations | per-layer limits | anatomical shoulder, elbow, hip, hock, and paw motion |

Limb limits are recorded per layer rather than as one global range. The screen-left front paw exposes `planted`, `lifted`, and `wave`; the other front paw exposes `planted` and `lifted`. State changes must occur while the relevant plate is concealed by motion or aligned at a registered interchange pose.

## Motion Data Flow

The tool-neutral motion preview consumes `rig.json` plus a JSON motion clip. Each clip supplies frame rate, duration, variant selections, and control keyframes. The compositor evaluates parent transforms from root to leaf, applies the active variant, and source-overs layers in draw order. It outputs ignored PNG review frames and a contact sheet; it does not become a production video renderer.

Fusion mirrors the same hierarchy with Loaders, Transforms, and Merges. A controller group exposes the manifest control names. Fusion pivots convert manifest top-left Y coordinates using `fusionY = 1 - manifestY`. Every write is read back. Resolve is saved only after node, path, pivot, connection, variant-visibility, and keyframe verification succeeds.

No Resolve MCP render or export is performed. The owner handles delivery exports.

## Required Motion Tests

### Walk in place

- 48 frames at 24 fps; frame 47 matches frame 0.
- Natural feline diagonal gait: opposing front/rear support pairs, not human arm/leg opposition.
- Paws remain on the baseline during stance and use painted lifted states during swing.
- Torso bob and weight transfer remain within declared limits.
- All four legs return to exact neutral registration at the loop boundary.
- No exposed shoulder, elbow, wrist, hip, hock, or paw seams.

### Paw wave

- 72 frames at 24 fps; begins and ends on all fours in neutral registration.
- Weight transfers away from the screen-left front leg before it lifts.
- The screen-left front paw changes from planted to lifted to wave at registered interchange poses.
- Two small feline paw motions occur without turning the foreleg into a human arm.
- Head, ears, gaze, blink, and tail add secondary motion while preserving balance.

### Head, face, and tail proof

- 48-frame head look from neutral to left plate, through neutral, to right plate, and back.
- Pupils remain inside the approved iris bounds at every gaze extreme.
- Independent and synchronized blink states cover the open eyes without melting or resizing them.
- Jaw-open state preserves muzzle, nose, whiskers, and kitten expression.
- Tail base, mid, and tip form a continuous curve with connected rings and no visible gaps.

## Validation and Review Gates

Automated validation proves:

- every production layer is 1536 x 1024 RGBA and uses a contained nonsymlink path;
- PNGs contain no forbidden text, EXIF, or embedded-profile chunks;
- all hashes, IDs, draw orders, parents, pivots, variants, and controls are valid;
- the parent graph is acyclic and every control binding references a known layer/variant;
- exactly one member of every variant set is neutral;
- the visible neutral composite matches the approved source with zero visible mismatches;
- motion clips stay inside declared ranges and begin/end at their required loop state;
- each PNG is below 10 MB and the complete brand package remains below 60 MB; and
- committed files contain no private share URLs, real-cat media, ignored review paths, or identifying metadata.

Visual review occurs at full resolution, 320 px, and 160 px on warm-white and charcoal backgrounds. It checks likeness, gait, weight, paw anatomy, joint seams, marking continuity, head-turn fidelity, eye bounds, blink shape, mouth expression, whiskers, tail continuity, and loop smoothness. Generated plates must pass still review before they enter a motion test; motion tests must pass before Fusion assembly.

## Error Handling and Safe Failure

Build tools fail without overwriting approved production files when a manifest, source hash, variant, or layer is invalid. New output is built in a temporary directory and promoted only after validation. Existing v1 files are never rewritten by v2 commands. Review-generation failures remain ignored local artifacts and cannot alter the neutral master.

Fusion work is confined to the disposable `_mcp_waffle_smoke_test` project unless the owner explicitly selects another project. The graph is saved incrementally after verified milestones so a Resolve crash cannot destroy the tool-neutral package.

## Completion Boundary

Standing Rig v2 is complete only when the tool-neutral package, exact neutral reconstruction, approved alternate artwork, all required motion tests, Fusion assembly, and repository checks pass. It does not include a side-profile travelling walk, sitting or curled articulation, speech lip sync, prop interaction, documentation-site integration, Remotion integration, or final video export.
