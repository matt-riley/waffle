# Standing Rig v2 Art Generation Record

This file is the durable, production-safe rebuild record for generated edit plates. Full plates stay outside Git; the committed `repairs.json` and `variants.json` bind every accepted plate by SHA-256, canvas, crop, polygon, chroma thresholds, and semantic state ID.

## Tool mode and references

- Tool: built-in image generation, image-edit mode, one output per call.
- Output observed: 1536x1024 PNG. The tool exposed no model, seed, quality, sampler, or size controls; those values are therefore unavailable rather than silently inferred.
- Background requested for extraction plates: uniform `#ff00ff`, with no shadow, gradient, texture, reflection, floor, text, or watermark.
- Maximum input images: five, in this exact order:
  1. `assets/brand/waffle/poses/standing.png` — edit target, canvas, placement, and standing-pose authority.
  2. `assets/brand/waffle/canon/model-sheet.png` — identity, markings, proportions, rendering style, and directional-view authority.
  3. `assets/brand/waffle/poses/sitting.png` — supporting identity, anatomy, expression, and rendering reference.
  4. `assets/brand/waffle/poses/curled.png` — supporting markings, fur, palette, and rendering reference.
  5. `assets/brand/waffle/poses/standing-airplane-ears.png` — supporting standing registration, ear anatomy, and rendering reference.

## Exact final rebuild prompts

For every accepted state except the two head turns, the exact final prompt is the following prefix, then the state directive listed below, then the suffix, joined with one blank line between blocks.

Exact prefix:

```text
Use case: identity-preserve
Asset type: full-canvas edit plate for a layered 2D character rig
Input images: Image 1 is the edit target and exact canvas/pose authority; Image 2 is the identity, marking, anatomy, proportion, and rendering-style authority; Images 3-5 are supporting identity, anatomy, expression, marking, ear, and rendering references only.
```

Exact state directives:

- `front-paw-left/lifted`: `Primary request: Keep the full standing kitten unchanged except for the kitten's screen-left front paw and its immediately connected lower foreleg. Lift that paw naturally upward in a small walking step, preserving kitten anatomy, banded fur, limb thickness, joint attachment, lighting, and the exact body registration. Do not alter the screen-right front paw or either rear paw.`
- `front-paw-left/wave`: `Primary request: Keep the full standing kitten unchanged except for the kitten's screen-left front paw and its immediately connected lower foreleg. Raise that paw into a compact friendly kitten wave with a bent wrist and paw pad orientation that remains unmistakably feline, preserving banded fur, limb thickness, joint attachment, lighting, and exact body registration. No human arm, hand, fingers, or shoulder drift.`
- `front-paw-right/lifted`: `Primary request: Keep the full standing kitten unchanged except for the kitten's screen-right front paw and its immediately connected lower foreleg. Lift that paw naturally upward in a small walking step, preserving kitten anatomy, banded fur, limb thickness, joint attachment, lighting, and the exact body registration. Do not alter the screen-left front paw or either rear paw.`
- `rear-paw-left/lifted`: `Primary request: Keep the full standing kitten unchanged except for the kitten's screen-left rear paw and its immediately connected hock. Lift that paw naturally upward in a small walking step, preserving kitten anatomy, banded fur, limb thickness, joint attachment, lighting, and the exact body registration. Do not alter either front paw or the screen-right rear paw.`
- `rear-paw-right/lifted`: `Primary request: Keep the full standing kitten unchanged except for the kitten's screen-right rear paw and its immediately connected hock. Lift that paw naturally upward in a small walking step, preserving kitten anatomy, banded fur, limb thickness, joint attachment, lighting, and the exact body registration. Do not alter either front paw or the screen-left rear paw.`
- `upper-lid-left`: `Primary request: Keep the full standing kitten unchanged except for a natural closed upper-eyelid texture over the kitten's screen-left eye. Preserve the eye socket, brow, surrounding fur, stripes, expression, lighting, and exact canvas registration. The result must be a narrow feline lid, not a moved eye, new eye, lower lid, or facial redesign.`
- `upper-lid-right`: `Primary request: Keep the full standing kitten unchanged except for a natural closed upper-eyelid texture over the kitten's screen-right eye. Preserve the eye socket, brow, surrounding fur, stripes, expression, lighting, and exact canvas registration. The result must be a narrow feline lid, not a moved eye, new eye, lower lid, or facial redesign.`
- `jaw/open`: `Primary request: Keep the full standing kitten unchanged except for a small naturally open kitten mouth and connected lower jaw, suitable for a quiet meow. Preserve the nose, muzzle, cheeks, whiskers, head silhouette, expression, lighting, and exact canvas registration. No large gape, extra teeth, tongue distortion, duplicated muzzle, or facial redesign.`

Exact suffix:

```text
Identity invariants: same eternal orange-tabby kitten; large ears; grey-green eyes; pink nose; pale muzzle, chin, eye surrounds, chest, and belly; strong forehead M; cheek and crown stripes; banded legs; broken flank swirls; ringed tail; long slightly lanky kitten proportions; same warm light direction, outline weight, painterly fur texture, and polished illustration style.
Scene/backdrop: perfectly flat solid #ff00ff chroma-key background for local removal; one uniform color with no shadows, gradients, texture, reflections, floor plane, or lighting variation.
Composition/framing: exact 1536x1024 landscape registration and subject placement from Image 1; full kitten visible with generous existing padding.
Constraints: change only the requested local state; no pasted patch, duplicated feature, extra limb, extra paw, extra eye, extra muzzle, extra ear, anatomy drift, body change, pose change outside the named joint, crop change, cast shadow, contact shadow, reflection, text, or watermark; do not use #ff00ff anywhere in the kitten.
```

The accepted `head-base/turn-left` plate used this exact prompt:

```text
Use case: identity-preserve
Asset type: full-canvas edit plate for a layered 2D character rig
Input images: Image 1 is the edit target and exact canvas/pose authority; Image 2 is the identity, marking, style, and directional-view authority, especially its profile facing the left edge; Images 3-5 are supporting identity, anatomy, expression, and rendering references only.
Primary request: Keep Waffle's standing body, legs, paws, torso, tail, neck attachment, scale, and three-quarter pose unchanged. Turn only the head a clearly readable but conservative 15 degrees toward the LEFT EDGE OF THE CANVAS. The kitten must look toward screen-left. Move the nose and facial centreline subtly about 18-24 pixels toward screen-left relative to Image 1; the screen-left eye is the nearer eye and may read slightly fuller, while the screen-right cheek is the far side. Shift the outer head silhouette coherently: fuller near cheek on screen-left, narrower far cheek on screen-right, forehead M and crown stripes curving toward screen-left, and cheek/crown stripe flow following the turn. Keep the face close enough to Image 1 registration for separately registered face layers.
Identity invariants: same eternal orange-tabby kitten; large ears; grey-green eyes; pink nose; pale muzzle, chin, eye surrounds, chest, and belly; strong forehead M; cheek and crown stripes; banded legs; broken flank swirls; ringed tail; long slightly lanky kitten proportions; same warm light direction, outline weight, painterly fur texture, and polished illustration style.
Scene/backdrop: perfectly flat solid #ff00ff chroma-key background for local removal; one uniform color with no shadows, gradients, texture, reflections, floor plane, or lighting variation.
Composition/framing: exact 1536x1024 landscape registration and subject placement from Image 1; full kitten visible with generous existing padding.
Constraints: change only the directional head construction and its coherent fur/stripe flow; no hard splice, pasted patch, duplicated features, extra eyes, extra muzzle, extra ears, anatomy drift, body change, pose change, crop change, cast shadow, contact shadow, reflection, text, or watermark; do not use #ff00ff anywhere in the kitten.
```

The accepted `head-base/turn-right` plate used this exact prompt:

```text
Use case: identity-preserve
Asset type: full-canvas edit plate for a layered 2D character rig
Input images: Image 1 is the edit target and exact canvas/pose authority; Image 2 is the identity, marking, style, and directional-view authority; Images 3-5 are supporting identity, anatomy, expression, and rendering references only.
Primary request: Keep Waffle's standing body, legs, paws, torso, tail, neck attachment, scale, and three-quarter pose unchanged. Turn only the head a clearly readable but conservative 15 degrees toward the RIGHT EDGE OF THE CANVAS. The kitten must look toward screen-right. Move the nose and facial centreline subtly about 18-24 pixels toward screen-right relative to Image 1; the screen-right eye is the nearer eye and may read slightly fuller, while the screen-left cheek is the far side. Shift the outer head silhouette coherently: fuller near cheek on screen-right, narrower far cheek on screen-left, forehead M and crown stripes curving toward screen-right, and cheek/crown stripe flow following the turn. Keep the face close enough to Image 1 registration for separately registered face layers.
Identity invariants: same eternal orange-tabby kitten; large ears; grey-green eyes; pink nose; pale muzzle, chin, eye surrounds, chest, and belly; strong forehead M; cheek and crown stripes; banded legs; broken flank swirls; ringed tail; long slightly lanky kitten proportions; same warm light direction, outline weight, painterly fur texture, and polished illustration style.
Scene/backdrop: perfectly flat solid #ff00ff chroma-key background for local removal; one uniform color with no shadows, gradients, texture, reflections, floor plane, or lighting variation.
Composition/framing: exact 1536x1024 landscape registration and subject placement from Image 1; full kitten visible with generous existing padding.
Constraints: change only the directional head construction and its coherent fur/stripe flow; no hard splice, pasted patch, duplicated features, extra eyes, extra muzzle, extra ears, anatomy drift, body change, pose change, crop change, cast shadow, contact shadow, reflection, text, or watermark; do not use #ff00ff anywhere in the kitten.
```

## Plate ledger

Accepted plate SHA-256s:

- `front-paw-left/lifted`: `e7ddf3afd78b7c54432261201151d257c279c0945e92edb7b259b316bdf39525`
- `front-paw-left/wave`: `858fad1b352f7b8475b78f1584c9ffa87c1225d0f52bbe8d821d5942234c322a`
- `front-paw-right/lifted`: `c7a820517fbbf0624c661f574c4cff93b4d01359c06ef844b1124e95bd894dd2`
- `rear-paw-left/lifted`: `ef2bdf2b6f5ee003064e388abe782e5ab46ca333b6dae8a3275836203a82ed1e`
- `rear-paw-right/lifted`: `b4d0a2605d2d0a01f1740d3b8d8ef819e0c9bc3a55ee39c8d5718e6017fb304d`
- `head-base/turn-left`: `7543bf0311b4ea91e8e66c78783431fb232cb4a9609f219da230183b865489f1`
- `head-base/turn-right`: `6aed5fbe5353d684f383e57590fc9c22230814ddc27d220f8ec168d1e9bf46ad`
- `upper-lid-left`: `ee4ae8f44ed61a5cc1ae36d70eea5f4378ab7e7bfaa7ce955a5797566bdd41ed`
- `upper-lid-right`: `18df8d51fbd80327adaf28aa934bc173215cd4815e569b224683311b9089ba02`
- `jaw/open`: `eb0dbd0a6d6e08c8867fa16e5a2ebd6c3440314ff27c7d2a41686067d9ddab82`

Rejected plate/attempt SHA-256s and reason:

- `front-paw-left/lifted-v1`: `670112812f7a570a1b814eb3ab7a26f8b30c0c756722557196355a7ab6cd8e78` — generated checkerboard pixels.
- `front-paw-left/wave-v1`: `5a79630526dc0740e6338890020ee11753655dd1b9cbc4cd661f5bd88fd601e0` — human-like arm anatomy.
- `head-base/turn-left-v1`: `7e8fd7ed2b98d9ff28ae4bb6e991f331f01d809892b26bc0026777277072b140` — too frontal and partial-feature mixing.
- `head-base/turn-right-v1`: `4e0cf7fa3a860e0ca7c11e521263484bc9ef430a4309f193706b9292d465e644` — too frontal and partial-feature mixing.
- `head-left-attempt-facing-wrong-way`: `f0687209df5b50ae47e97e5dd17f3fc38b0a8c47befa6c13c99d9e6a62927fbf` — readable turn, but toward the wrong screen edge.
- `head-right-attempt-too-frontal`: `06ede747b206dde7c113f4e2ad36f535079ee70dc0e7be5f75ac0f9d8587c1ef` — insufficient directional change.
- `lower-lid-left-v1`: `92181d8a71fb130e3cfdae630d4227019b8d74c157a5ad9ff0feb73388f4264e` — upper-lid/no-op output.
- `lower-lid-left-v2`: `574a2f7b7f29a440a3a86d10fdc4ce12d3c3b73bed409b023d502c1184df84cc` — facial drift.
- `lower-lid-right-v1`: `7316e4fa213c58a4f492b2e89fe891d2ffd8ca0b3ef50fa3a63d3bf508cbeb6e` — upper-lid/no-op output.
- `lower-lid-right-v2`: `b0b81efaf5a299b1de6c31efaeb7e3b836c22ad205e81ef32a6cf08cb1da5bea` — facial drift.

## Deterministic extraction and fallback states

`variants.json` is authoritative for all eight non-neutral variant crops, polygons, chroma thresholds, and optional edge feather. Head turns use crop `[350,40,520,520]`, the declared 13-point face/neck polygon, thresholds `120/235`, and 16 px bounded feather. Paw and jaw crops remain rectangular and use their declared thresholds. `repairs.json` is authoritative for 16 source-sampled concealed repairs and four lid overlays.

The lower lids are not generated states. `lower-lid-left` is deterministically extracted from the accepted `upper-lid-left` plate, and `lower-lid-right` from the accepted `upper-lid-right` plate, with side-specific narrow crescent polygons declared in `repairs.json`. All 16 concealed repairs are deterministic source samples from the approved standing image, optionally with declared sample offsets/fallback samples. No prompt or generated plate is involved in either fallback path.

For complete head turns, each non-neutral head member hides all 15 superseded registered descendants through schema-validated `layerOverrides`: both ears, muzzle, closed jaw, both iris/pupil/highlight chains, all four lid overlays, and whiskers. The selected generated member therefore supplies one complete coherent face; it is never partially mixed with the frontal child chain.
