# Waffle Layered Raster Rig Design

## Decision

Build the first animation-ready Waffle as a source-locked hybrid raster rig.
The approved neutral standing PNG remains the visual authority. Its visible
pixels are separated into reusable transparent layers; newly created artwork is
limited to hidden overlap patches needed when those layers move.

The first rig supports idle breathing, blinking, independent ear movement,
small head turns/tilts, and tail swishes. Walking, paw gestures, lip sync, and
large body deformation are later milestones.

## Deliverables

The repository owns a tool-neutral package under
`assets/brand/waffle/rigs/standing-v1/`:

- full-canvas RGBA PNG layers with identical dimensions and registration;
- hidden-area repair layers kept separate from approved visible artwork;
- `rig.json`, defining draw order, pivots, parent relationships, source bounds,
  neutral transforms, and intended motion limits;
- a neutral reference render and deterministic recomposition test; and
- light/dark-background review renders showing neutral and safe motion limits.

Fusion is a downstream consumer. A Fusion composition will load the PNG layers,
reproduce the hierarchy and pivots from `rig.json`, and expose named controls
for the supported movements. The PNG package remains usable without Resolve and
can later feed Remotion or another compositor.

## Layer Architecture

All layers use the approved standing master's 1536 x 1024 canvas so their
neutral placement is unambiguous. Transparent padding is intentional.

The draw stack, back to front, is:

1. `tail-hidden` and `tail-visible`;
2. `body-repair`, `body`, and the four leg regions retained with the body for
   this first non-walking rig;
3. `neck-repair`;
4. `head`;
5. `left-ear-repair`, `left-ear`, `right-ear-repair`, `right-ear`;
6. `left-eye-lid` and `right-eye-lid`, normally hidden; and
7. whisker-safe facial detail retained on the head.

The body remains a single painted unit. Idle breathing uses a very small scale
and vertical offset around a low chest pivot rather than deforming legs. The
head is independently transformable around the lower neck. Each ear rotates
around its anatomical base. The tail rotates and bends only within the modest
range that the repaired root can support. Blinks use painted lid overlays so
the approved eyes are covered rather than distorted.

## Source Lock and Reconstruction

Visible neutral artwork must come directly from the approved standing PNG.
Layer masks partition or overlap those pixels without restyling them. Repair
art lives underneath the visible pieces, so the neutral recomposition is
pixel-identical wherever possible. A narrow anti-aliased boundary tolerance is
allowed only where a cut must cross soft fur.

Hidden artwork may be reconstructed for the neck behind the head, the head
behind each ear, the tail root behind the body, and closed eyelids. Repairs must
match nearby colour, stripe direction, fur texture, lighting, and outline
weight. They must never change the neutral silhouette or visible markings.

If generated inpainting is used, it is treated as a constrained repair input,
not as a new Waffle render. The final patch is cropped, masked, and reviewed in
context. Private real-cat reference media is not required and must not enter
the repository.

## Rig Metadata

`rig.json` uses a versioned schema and records:

- canvas size and approved source asset;
- layer IDs, PNG paths, draw order, parent IDs, visibility, and blend mode;
- normalized pivots and neutral transforms;
- conservative rotation, scale, and translation limits;
- named controls: `breath`, `headTilt`, `headTurn`, `blink`, `leftEar`,
  `rightEar`, and `tailSway`; and
- the SHA-256 of the approved source and every production layer.

Paths are repository-relative. No local Resolve paths, private references, or
editor metadata are stored.

## Fusion Composition

The Fusion composition uses one Loader per production PNG and one Transform per
movable group. Merge order mirrors `rig.json`. Controls are named consistently
with the manifest and default to the exact neutral pose. Motion tests use smooth
splines and conservative ranges:

- breathing: subtle chest-scale/vertical motion;
- blink: quick close, short hold, eased reopen;
- ears: independent small backward/side rotations;
- head: a few degrees of tilt and a very small lateral turn proxy; and
- tail: slow base sway with no large curl or topology change.

No render or export is performed through the Resolve MCP. The owner handles
exporting. Every MCP write is read back and verified because Fusion point values
and undo behavior have previously required strict verification.

## Validation

Automated checks verify:

- every layer is 1536 x 1024 RGBA with transparent corners;
- forbidden PNG metadata chunks are absent;
- `rig.json` paths, dimensions, hashes, parents, pivots, and limits are valid;
- the draw graph is acyclic and layer IDs are unique;
- the neutral layer stack matches the approved source pixel-for-pixel outside
  declared seam-tolerance masks; and
- the complete asset package stays within the existing brand size budget.

Visual review verifies that neutral Waffle is indistinguishable from the
approved master, repaired pixels are invisible at rest, motion exposes no holes
or hard seams, whiskers remain stable, eyes blink without melting, ears retain
their anatomy, and the tail stays attached naturally.

## Acceptance Boundary

This milestone is complete when the tool-neutral package passes validation and
the Fusion rig can preview one source-locked idle loop with a blink, one ear
twitch, a small head tilt, and a tail sway. It does not include final video
rendering, documentation-site integration, walking, gestures, or dialogue
animation.
