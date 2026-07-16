# Fusion assembly handoff

This directory records the verified DaVinci Resolve/Fusion handoff for Waffle's standing-v2 rig. `rig.json` and the motion clips remain the tool-neutral authorities; a Fusion composition is a downstream assembly of those registered raster layers and controls.

## Verified capability proof

`assembly-record.json` captures a Resolve MCP smoke proof saved in project `Untitled Project 2026-07-16_190245`. The proof created an isolated 1920 x 1080, 120-frame timeline named `_mcp_waffle_smoke_test`, inserted one Fusion Composition, created a Background-to-Transform-to-MediaOut chain, animated the Transform Angle through a named spline, read the nodes, connections, and four keys back exactly, and saved the project.

This proves that the MCP path can safely create and inspect Fusion tools, write connections and spline keys, read the resulting state back, and persist it. It is deliberately not the full 55-layer Waffle rig, not an animation-quality review, and not an export. No media was rendered; the owner retains control of delivery exports.

## Production assembly

Build the production composition from `../rig.json`, preserving manifest IDs in Fusion tool names or comments so every node can be traced back to its source layer. For each raster layer:

1. Load the registered 1536 x 1024 RGBA PNG without resampling its neutral pixels.
2. Apply the manifest pivot and parent transform. Convert top-left manifest Y coordinates with `fusionY = 1 - manifestY`.
3. Merge in manifest hierarchy order and preserve the declared sibling ordering.
4. Implement mutually exclusive variant visibility from the selected member and its `layerOverrides`.
5. Put clip-private `layerOpacity` keys on the corresponding Merge Blend channels rather than publishing them as general controls.
6. Read each node, input, pivot, connection, control value, visibility state, and written key back before saving the completed subtree.

The accepted paw wave uses the complete `front-chain-left/paw-lifted`, `paw-wave`, and `paw-landing` paintings. The accepted expression clips use the clip-only complete `head-base/blink-left` and `blink-right` paintings while holding the articulated lid controls at zero. Preserve those choices in Fusion; reconstructing the accepted clips from the segmented paw or lid overlays reintroduces the artifacts those source-locked states were created to remove.

## Evidence boundaries

- `assembly-record.json` is an assembly/readback capability record, not a serialized Fusion composition or substitute for `rig.json`.
- IDs are Resolve object IDs captured during the smoke proof. They establish provenance for that saved project state, not portable references for another Resolve database.
- The proof contains no private filesystem paths, source-media paths, credentials, or export locations.
- A production handoff is complete only after all standing-v2 layers and accepted clip keys are assembled, read back, saved, and visually reviewed on both approved backgrounds.
