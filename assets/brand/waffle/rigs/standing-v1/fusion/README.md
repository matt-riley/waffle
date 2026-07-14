# Fusion Assembly

The disposable Resolve project `_mcp_waffle_smoke_test`, timeline `Waffle MCP Smoke Test`, contains the verified Fusion assembly for this rig. It is a downstream preview, not the production source of truth; `../rig.json` and the registered PNG layers remain authoritative.

## Graph

Each production PNG has its own Loader. The active graph is:

```text
tail-hidden + tail-visible -> TailTransform -------------------+
body-repair ---------------------------------------------------+-> body -> neck-repair --+
left-ear -> LeftEarTransform --+                                                      |
right-ear -> RightEarTransform -+-> head -> hidden ear repairs -> eyelid overlays     |
                                      -> HeadTransform --------------------------------+-> BreathTransform -> MediaOut1
```

The actual Merge chain preserves every manifest draw order. Hidden ear-repair Merges remain at Blend 0 for manual reserve use. Left and right eyelid Merges use Blend 0 for open and Blend 1 for closed. `MediaOut1` reads only from `BreathTransform`; the original whole-image transform was removed.

Fusion point coordinates use a bottom-left Y origin, so every manifest pivot `{x, y}` is entered as `[x, 1 - y]`. The verified pivots are:

| Control | Fusion node | Pivot |
| --- | --- | --- |
| Tail sway | `TailTransform` | `[0.677, 0.565]` |
| Screen-left ear | `LeftEarTransform` | `[0.326, 0.814]` |
| Screen-right ear | `RightEarTransform` | `[0.469, 0.810]` |
| Head tilt | `HeadTransform` | `[0.391, 0.565]` |
| Breathing | `BreathTransform` | `[0.520, 0.220]` |

## Idle test

The disposable timeline is 24 fps and 120 frames. Frame 119 returns to neutral. The saved test uses smooth Fusion splines and stays inside `rig.json` limits:

| Control | Keyframes |
| --- | --- |
| Tail angle | `0:0`, `30:3`, `60:0`, `90:-3`, `119:0` |
| Head angle | `0:0`, `30:0.6`, `60:0`, `90:-0.5`, `119:0` |
| Breath size | `0:1`, `30:1.005`, `60:1`, `90:1.004`, `119:1` |
| Screen-left ear angle | `0:0`, `45:-1.5`, `55:0`, `119:0` |
| Screen-right ear angle | `0:0`, `80:1.5`, `90:0`, `119:0` |
| Both eyelid blends | `0:0`, `50:0`, `53:1`, `55:1`, `58:0`, `119:0` |

Resolve may create an additional neutral spline key at the playhead when the first key is added. In the verified project that key is frame 21 with the same neutral value, so it does not change the motion.

## Use in another project

Create a Fusion Composition, load every PNG from `../layers/`, reproduce the manifest draw order, and apply the pivot conversion above. Keep Loader paths project-relative or relinkable; do not copy worktree-specific absolute paths into repository files. Verify the frame-zero output against `../neutral-reference.png` before animating.

Do not use the Resolve MCP to render or export. The owner performs exports directly in Resolve after previewing the saved composition.
