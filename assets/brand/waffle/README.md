# Waffle Brand Assets

This tree contains Waffle's reviewed production artwork and deterministic exports. The editable, layered SVG master is the source of truth. Generated studies, optimized copies, PNG previews, and video frames are derivatives and must never overwrite it.

## Directory roles

- `canon/` holds the character rules, non-identifying reference review, model sheet, and expression sheet.
- `source/` holds the layered Motion Waffle vector master.
- `poses/` holds standalone production SVG poses derived from the master.
- `exports/png/` holds deterministic transparent previews rendered from committed SVGs.
- `qa/` records review-gate decisions and verification evidence.

Private photos, videos, generated concept studies, share details, and identifying metadata belong only in the ignored `.superpowers/` workspace. They must never be copied into this tree or committed. Production SVGs must remain editable vector geometry and may not embed raster images or external assets.

## Commands

```sh
mise run brand-install
mise run brand-check
mise run brand-render
```

- `brand-install` restores the pinned asset-tool dependencies from the lockfile.
- `brand-check` runs the tool tests and validates every production SVG for required structure, safety, and complexity limits.
- `brand-render` regenerates the committed transparent PNG previews from the SVG sources.

## Review gates

| Gate | Status |
| --- | --- |
| Character identity, initial palette, and construction ratios | Awaiting owner approval |
| Silhouette and proportions | Not started |
| Face and expression language | Not started |
| Marking placement and tuned palette | Not started |
| Model-sheet views | Not started |
| Final static poses | Not started |

Do not advance a creative stage until its preceding gate is explicitly approved. Palette values in `canon/character-canon.md` remain initial until the marking review gate.
