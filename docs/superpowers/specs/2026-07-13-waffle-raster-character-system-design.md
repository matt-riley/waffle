# Waffle Raster Character System Design

## Decision

Waffle's production artwork will be raster first. Two manually authored SVG
attempts preserved structural editability but lost the approved Balanced
study's cuteness, softness, and recognisable Waffle identity. Mascot likeness
takes priority over CSS-friendly geometry.

The approved canon and Balanced model-sheet study remain the visual direction.
The rejected SVGs are review evidence only and must stay in the ignored
workspace.

## Architecture

The repository source of truth is a tool-neutral raster package:

1. a high-resolution canonical model sheet;
2. approved high-resolution transparent composite masters for each pose;
3. standalone expression renders; and
4. JSON manifests defining dimensions, alpha policy, provenance, animation
   anchors, frame order, and durations.

Krita, OpenRaster, Blender, or another editor may be used during production,
but no proprietary editor file is required to render the first milestone.
Optional `.kra`, `.ora`, or `.blend` working files stay outside Git unless a
later decision explicitly adopts them. A manually painted cutout rig may be
added later; automated image generation is not treated as a reliable way to
decompose a polished composite into seam-safe body parts.

## Web and Video Delivery

Documentation uses pre-rendered transparent PNG sprite sequences for fixed
states such as idle, blink, success, warning, and sleepy. CSS `steps()` or a
small Canvas controller can play or switch sequences without reducing the
artwork to vectors. Static pages use transparent PNG initially; WebP/AVIF
optimization is a later integration concern.

The first idle proof is generated as one whole strip from one owner-approved
standing frame, normalized with one shared scale and bottom-centre anchor, and
locked back to the approved frame in slot one. This protects likeness better
than independently generating frames. Promotional video may later use Blender
or Remotion while its licensing remains appropriate. No animation subscription
is required.

## First Raster Milestone

The milestone delivers:

1. the already-approved character canon and private reference review;
2. a raster model sheet with front, three-quarter, profile, and rear/top views;
3. a transparent neutral-standing composite master;
4. neutral, curious, pleased, focused, startled, and sleepy standalone
   expression renders;
5. standing, sitting, and curled transparent poses plus standing and sitting
   airplane-ear variants;
6. one small idle sprite proof covering breathing, blink, ear twitch, and tail
   motion; and
7. an approval record with identity, transparency, dimensions, alignment,
   small-size, and privacy checks.

The detailed Hero Waffle recreation, documentation site, broad animation
library, and promotional videos remain later work.

## Review Gates

Review and approve these independently:

1. raster model-sheet likeness and cross-view consistency;
2. neutral master likeness, transparent silhouette, and expression language;
3. standing, sitting, and curled poses at documentation sizes; and
4. idle proof and final milestone package.

A failed gate returns to the affected artwork. Passing technical validation is
never evidence that Waffle's identity is acceptable.

## Repository Layout

```text
assets/brand/waffle/
├── README.md
├── canon/
│   ├── character-canon.md
│   ├── reference-review.md
│   ├── model-sheet.png
│   ├── generation-record.md
│   └── expressions/
├── manifest.json
├── poses/
│   ├── standing.png
│   ├── sitting.png
│   ├── curled.png
│   ├── standing-airplane-ears.png
│   └── sitting-airplane-ears.png
├── animation/
│   └── idle/
│       ├── manifest.json
│       └── frames/
└── qa/
    └── approval-record.md
```

Personal photos, videos, share details, identifying metadata, rejected studies,
and editor working files remain under ignored `.superpowers/` paths.

## Acceptance Criteria

- Every deliverable immediately reads as the same eternal kitten and preserves
  the forehead M, grey-green eyes, pale muzzle, banded legs, flank swirls,
  ringed tail, and long kitten proportions.
- Artwork retains the soft illustrated character of the approved Balanced
  study; reject square anatomy, geometric-mask faces, or generic-cat drift.
- Production PNGs use RGBA colour, correct dimensions, and transparent corners
  where transparency is required.
- Presentation-only model sheets may use an opaque uniform warm-white
  background. Runtime cutouts and sprite frames use the built-in imagegen
  chroma-key workflow and validated local matte removal; native-transparency
  CLI fallback requires explicit owner approval.
- The asset manifest uses schema version 1 and records each asset's role,
  dimensions, alpha policy, and privacy-safe provenance.
- The idle proof contains six 256px frames, uses a shared bottom-centre anchor,
  locks frame one to the approved standing master, and has explicit durations.
  It has stable registration, no identity drift, and no edge matte.
- No private reference media or access details enter Git.
- PNG textual, EXIF, and embedded-profile metadata chunks are rejected before
  commit.
- No individual PNG exceeds 10 MB and the complete first raster milestone stays
  below 60 MB unless the owner approves a larger budget.

## Tradeoffs

Raster artwork uses more storage, has a finite resolution ceiling, supports
fixed rather than arbitrary runtime motion in the first milestone, and requires
regeneration after source changes. In return it preserves fur softness, facial
appeal, nuanced markings, and the approved Waffle likeness. Those are the more
important constraints for this mascot.
