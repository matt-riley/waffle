# Waffle Brand Assets

This tree contains Waffle's reviewed raster artwork and machine-readable delivery contracts. Approved high-resolution RGBA PNG composites and schema-versioned JSON manifests are the repository source of truth. The raster architecture preserves Waffle's soft illustration style and likeness instead of reducing her to geometric vector shapes.

## Directory roles

- `canon/` holds character rules, the non-identifying reference review, the approved raster model sheet, its generation record, and standalone expressions.
- `poses/` holds the approved transparent standing, sitting, and curled composites.
- `rigs/standing-v1/` holds the lightweight standing idle rig. `rigs/standing-v2/` holds the source-locked articulated three-quarter rig, registered artwork variants, exact controls, build inputs, and its package contract.
- `animation/idle/` holds the deterministic standing seed, normalized sprite frames, and the animation manifest.
- `manifest.json` declares every static production asset's role, dimensions, alpha policy, and privacy-safe provenance.
- `qa/` records owner decisions and technical verification evidence.

Private photos, videos, generated concept studies, share details, rejected vector studies, editor working files, and identifying metadata belong only in the ignored `.superpowers/` workspace. They must never be copied into this tree or committed. Optional editor files are not required to use or validate the production package.

The general-purpose SVG validator remains under `tools/brand-assets/` for other artwork, but SVG is not a Waffle production format.

## Commands

```sh
mise run brand-install
mise run brand-check
```

- `brand-install` restores the pinned asset-tool dependencies from the lockfile.
- `brand-check` runs all brand-tool tests, then validates the static and idle manifests when they exist. It is safe during early stages when no raster deliverables exist yet.
- `brand-rig-check` validates standing v1 and v2, including each rig's exact neutral reconstruction.

Production helpers are explicit so they never overwrite an approved master:

```sh
node tools/brand-assets/sanitize-png.mjs input.png output.png
node tools/brand-assets/resize-raster.mjs input.png output.png WIDTH HEIGHT
node tools/brand-assets/build-sprite-edit-canvas.mjs seed.png canvas.png
node tools/brand-assets/normalize-sprite-strip.mjs strip.png frames/ seed.png
node tools/brand-assets/validate-raster.mjs assets/brand/waffle/manifest.json
node tools/brand-assets/validate-rig.mjs \
  assets/brand/waffle/rigs/standing-v1/rig.json \
  assets/brand/waffle/rigs/standing-v2/rig.json
```

Sanitization decodes and re-encodes RGBA pixels before validation, dropping ancillary metadata. The sprite helpers use deterministic bilinear resizing, one shared scale, a bottom-centre anchor, and a locked first frame.

Static `manifest.json` files use schema version 1. Every asset requires `id`, `file`, `role`, `width`, `height`, `alphaPolicy`, and `provenance`. Idle manifests additionally declare numeric `canvas` and `anchor` objects, a boolean `loop`, a local `seed`, and ordered frames with a local `file` and positive numeric `durationMs`.

## Review gates

| Gate | Status |
| --- | --- |
| Character canon, palette, and construction ratios | Approved |
| Balanced visual direction | Approved |
| Vector studies | Rejected; preserved privately for review evidence |
| Raster model-sheet likeness and cross-view consistency | Approved |
| Neutral master likeness and expression language | Approved |
| Standing, sitting, curled, and airplane-ear documentation poses | Approved |
| Standing layered raster rig v1 | Approved; tool-neutral package and Fusion idle assembly verified |
| Standing articulated raster rig v2 still artwork | Approved; source-locked neutral, repairs, and registered paw, head, lid, and jaw artwork verified |
| Standing rig v2 motion and Fusion proofs | In progress; motion clips and downstream assembly remain gated |
| Idle proof and final package | Prototype reviewed; final animation pending |

Do not advance a creative stage until its preceding gate is explicitly approved. Passing file validation is never evidence that Waffle's identity is acceptable.
