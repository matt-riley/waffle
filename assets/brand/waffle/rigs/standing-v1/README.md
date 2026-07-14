# Waffle Standing Rig v1

This is the source-locked layered raster rig for the approved standing Waffle pose. The PNG layers preserve the approved illustration at neutral while exposing a deliberately conservative motion envelope for idle animation in Fusion or any compositor that supports RGBA layers, transforms, and normal source-over blending.

## Source of truth

- `rig.json` is the tool-neutral runtime contract: canvas, source hash, layer stack, pivots, parents, controls, and file hashes.
- `neutral-reference.png` is an exact decoded copy of `poses/standing.png`.
- `layers/` contains full-canvas RGBA PNGs. No editor project is required.
- `masks.json` and `repairs.json` are deterministic build inputs.
- `left` and `right` refer to screen-left and screen-right.

Visible source layers partition the approved standing master. The ear tips include opaque underlap pixels and sit behind the head, preventing seams during their small rotations. Hidden repair and eyelid overlays are source-locked hybrid additions. The final eyelid PNGs are production source of truth; the private generation reference used to derive them is intentionally excluded from the repository.

## Motion envelope

Use the limits in `rig.json`. Version 1 supports subtle breathing, head tilt/turn, a blink overlay, independent ear-tip motion, and tail sway. These limits are intentionally small: pushing beyond them exposes the 2D cut boundaries and is not considered supported.

## Rebuild and validate

From the repository root:

```sh
node tools/brand-assets/build-waffle-standing-rig.mjs \
  assets/brand/waffle/poses/standing.png \
  assets/brand/waffle/rigs/standing-v1/masks.json \
  assets/brand/waffle/rigs/standing-v1

# Rebuilds all deterministic repairs and the mapped eyelids. The ignored private
# reference is required; omit this command if it is unavailable because the
# committed eyelid PNGs are already the production source of truth.
node tools/brand-assets/build-waffle-rig-repairs.mjs \
  assets/brand/waffle/rigs/standing-v1/rig.json \
  assets/brand/waffle/rigs/standing-v1/repairs.json \
  "$PRIVATE_LID_REFERENCE"

mise run brand-rig-check
```

The validator checks schema, local paths, hashes, RGBA dimensions, parent cycles, pivots, controls, and exact visible-pixel neutral reconstruction. Transparent RGB bytes are ignored because they do not affect the decoded image.
