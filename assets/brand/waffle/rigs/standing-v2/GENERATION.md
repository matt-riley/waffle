# Standing Rig v2 Art Generation Record

This file is the durable, production-safe rebuild record for generated edit plates. Full plates stay outside Git; the committed `repairs.json` and `variants.json` bind every accepted plate by SHA-256, canvas, crop, polygon, chroma thresholds, and semantic state ID.

## Tool mode and references

- Tool: built-in image generation, image-edit mode, one output per call.
- Output observed: 1536x1024 PNG. The tool exposed no model, seed, quality, sampler, or size controls; those values are therefore unavailable rather than silently inferred.
- Background requested for extraction plates: uniform `#ff00ff`, with no shadow, gradient, texture, reflection, floor, text, or watermark.
- The original paw/face pass used at most five input images in this exact order; the later walk-chain pass used narrower two- or three-image stacks recorded verbatim in its own section below:
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

## Walk-chain low-lift generation record

The walk-chain search made 14 image-edit calls: four accepted plates and 10 retained rejections. Each of the four chain sets has `neutral`, `landing`, and `low-lift` production members. The four `landing` members are deterministic hybrid derivatives of the accepted low-lift plate and the approved neutral source; they required no additional image-edit calls. Earlier high-lift studies are ignored concept-only anatomy references and are not production variant members.

Exact accepted reference order and roles:

- `front-chain-left/low-lift`: Image 1 was rejected low-lift v3 (`8d3279c9c6b9deb1ec5fb7208cf5722851b8ff81f4fc115730746a3d5ef7a746`) as the corrective edit target; Image 2 was `poses/standing.png` as neutral registration and floor-height authority; Image 3 was `canon/model-sheet.png` as identity/style support.
- `front-chain-right/low-lift`: Image 1 was `poses/standing.png` as sole edit target and registration authority; Image 2 was `canon/model-sheet.png` as identity, marking, anatomy, proportion, and style support.
- `rear-chain-left/low-lift`: Image 1 was `poses/standing.png` as edit target, canvas, pose, placement, neutral registration, and baseline authority; Image 2 was `canon/model-sheet.png` as identity/style authority; Image 3 was ignored concept-only `rear-chain-left-lifted-v2-key.png` (`ea7fdb5ce1345202562d42348982ca283f1e0414c82a0db4f4bfee2d44c238e1`) only to identify the intended inner screen-left rear chain and coherent anatomy, with an explicitly lower requested lift.
- `rear-chain-right/low-lift`: Image 1 was `poses/standing.png` as edit target, canvas, placement, neutral registration, and baseline authority; Image 2 was `canon/model-sheet.png` as identity/style authority. The accepted retry deliberately omitted the high-lift study after it biased an earlier attempt upward.

The accepted `front-chain-left/low-lift` plate used this exact prompt:

```text
Use case: identity-preserve
Asset type: corrective full-canvas edit plate for a layered 2D character rig
Input images: Image 1 is the edit target and must remain unchanged except for lowering its already-lifted screen-left front paw. Image 2 is the exact neutral standing registration and floor-height authority. Image 3 is Waffle identity and rendering-style support only.

Primary request: In Image 1, locate the already-raised SCREEN-LEFT FRONT PAW nearest the LEFT EDGE at approximately x=470-570. LOWER that existing paw and its one connected foreleg downward toward the neutral position by approximately 30-35 pixels. Do not raise it. The final paw bottom must sit only 10-18 pixels higher than the corresponding neutral paw bottom in Image 2. The final target toe bottoms must be almost horizontally level with the planted screen-right front toe bottoms, just slightly higher. Retain only a tiny natural flex through the connected wrist, lower foreleg, elbow, upper foreleg, and original shoulder. The paw must look like it has barely left the floor, not like a dangling step.

Scene/backdrop: keep the perfectly flat uniform solid #ff00ff chroma-key background exactly.

Composition/framing: exact 1536x1024 canvas and exact Image 1 registration. Screen-left means x=0 side. Change only the named screen-left front limb chain. Preserve the opposite planted front leg, both rear legs, torso, chest, flank, head, face, ears, whiskers, tail, markings, palette, light, texture, scale, and silhouette exactly.

Constraints: move the existing target paw DOWN by 30-35 pixels. Do not move it up. Exactly one connected target limb and one target paw; exactly four legs and four paws total. No duplicate, extra paw, detached fur island, overlap, anatomy drift, body drift, shoulder relocation, crop change, shadow, text, or watermark. Do not use #ff00ff in the kitten.
```

The accepted `front-chain-right/low-lift` plate used this exact prompt:

```text
Use case: identity-preserve
Asset type: full-canvas edit plate for a layered 2D character rig
Input images: Image 1 is the sole edit target and absolute authority for the exact 1536x1024 canvas, kitten placement, standing pose, body registration, and all unchanged anatomy. Image 2 is Waffle identity, marking, anatomy, proportion, and rendering-style support only.

Primary request: Apply one local walking edit to Image 1 only. Lift the INNER FRONT LEG whose paw center is near x=665: this is the SCREEN-RIGHT FRONT LIMB, nearer the RIGHT EDGE of the canvas than the other front limb. It attaches below the screen-right chest near x=690, y=570 and occupies roughly x=600-760. Raise this one paw slightly while keeping one continuous natural kitten chain from the original shoulder through upper leg, elbow, lower leg, wrist, and paw. Aim for 10-20 pixels of clearance above its neutral baseline, with paw bottom around y=925-935. Keep the paw near x=665. This must be the x=665 inner front paw, NOT the outer screen-left paw near x=520.

Scene/backdrop: perfectly flat uniform solid #ff00ff chroma-key background, with no floor, shadow, gradient, texture, reflection, or lighting variation.

Composition/framing: exact 1536x1024 canvas and exact kitten placement from Image 1. The OUTER SCREEN-LEFT front leg and paw centered near x=520 must remain planted at their original baseline and absolutely unchanged. Both rear legs and paws remain planted and unchanged. Preserve head, face, ears, whiskers, chest, torso, flank, tail, markings, palette, lighting, painterly fur, outline, scale, and silhouette outside the named x=665 screen-right front chain.

Constraints: edit only the x=665 SCREEN-RIGHT front chain. Exactly one connected target limb and one target paw; exactly four legs and four paws total. No edit to the x=520 screen-left leg. No crossing or overlap between front legs, duplicate, extra paw, detached fur island, floating fragment, anatomy drift, body drift, shoulder relocation, crop change, shadow, text, or watermark. Do not use #ff00ff in the kitten.
```

The accepted `rear-chain-left/low-lift` plate used this exact prompt:

```text
Use case: identity-preserve
Asset type: full-canvas edit plate for a layered 2D character rig
Input images: Image 1 is the exact edit target, canvas, standing pose, placement, neutral registration, and baseline authority. Image 2 is the identity, markings, proportions, feline anatomy, and painterly rendering-style authority. Image 3 identifies the intended INNER SCREEN-LEFT REAR LEG only; preserve its side and connected anatomy but make the lift much lower.

Primary request: Edit only the INNER REAR LEG visible beneath the belly in the canvas box x=760..920, centred near x=830. This is the kitten's SCREEN-LEFT REAR LEG. Create an almost-neutral low toe-off: keep its paw in x=780..890 and place the bottom of that paw around canvas y=880..895, only 10-20 pixels above the neutral baseline near y=905. The paw must remain near the floor, not at belly height. Keep one coherent connected thigh, hock, and paw attached to the same inner hip. Keep the OUTER SCREEN-RIGHT REAR LEG in x=950..1100, centred near x=1005, fully planted and exactly unchanged from Image 1. Keep both front legs and all other anatomy unchanged.

Identity invariants: same eternal orange-tabby kitten; large ears; grey-green eyes; pink nose; pale muzzle, chin, eye surrounds, chest, and belly; strong forehead M; cheek and crown stripes; banded legs; broken flank swirls; ringed tail; long slightly lanky kitten proportions; same warm light direction, outline weight, painterly fur texture, and polished illustration style.
Scene/backdrop: perfectly flat solid #ff00ff chroma-key background for local removal; one uniform color with no shadows, gradients, texture, reflections, floor plane, or lighting variation.
Composition/framing: exact 1536x1024 landscape registration and exact subject placement from Image 1; full kitten visible with all original padding.
Constraints: change only the inner screen-left rear chain in x=760..920; paw bottom must remain y=880..895; one connected rear leg and one paw; preserve hip attachment, thickness, banding, fur direction, light, perspective, and body registration. Do not edit the outer screen-right rear chain in x=950..1100. No duplicated limb or paw, no extra paw, no missing paw, no floating segment, no wrong-side edit, no altered body/head/tail/front legs, no crop change, no shadows, no reflection, no text, no watermark, and do not use #ff00ff anywhere in the kitten.
```

The accepted `rear-chain-right/low-lift` plate used this exact prompt:

```text
Use case: identity-preserve
Asset type: full-canvas edit plate for a layered 2D character rig
Input images: Image 1 is the exact edit target, canvas, standing pose, placement, neutral registration, and baseline authority. Image 2 is the identity, markings, proportions, feline anatomy, and painterly rendering-style authority.

Primary request: Make an almost indistinguishable near-neutral toe-off. Keep the kitten exactly unchanged except for the OUTER SCREEN-RIGHT REAR PAW AND CONNECTED HOCK near canvas x=1005. Move the bottom of that outer paw upward only 5-8 pixels from Image 1, creating only a hairline strip of flat #ff00ff beneath it. Do not make a normal raised step. The paw must remain nearly at its original floor baseline, with a tiny feline hock flex and one continuous connected thigh, hock, and paw attached to the outer hip. Keep the INNER SCREEN-LEFT REAR LEG beneath the belly near x=830 fully planted and unchanged. Keep both front legs and every other feature exactly unchanged.

Identity invariants: same eternal orange-tabby kitten; large ears; grey-green eyes; pink nose; pale muzzle, chin, eye surrounds, chest, and belly; strong forehead M; cheek and crown stripes; banded legs; broken flank swirls; ringed tail; long slightly lanky kitten proportions; same warm light direction, outline weight, painterly fur texture, and polished illustration style.
Scene/backdrop: perfectly flat solid #ff00ff chroma-key background for local removal; one uniform color with no shadows, gradients, texture, reflections, floor plane, or lighting variation.
Composition/framing: exact 1536x1024 landscape registration and exact subject placement from Image 1; full kitten visible with all original padding.
Constraints: change only the outer screen-right rear paw/hock near x=1005; paw bottom just 5-8 px above its original baseline; keep thigh/hip registration, thickness, bands, fur direction, lighting, and perspective. Do not make more than a tiny toe-off. Do not change the inner screen-left rear leg near x=830. No duplicate or extra paw, no missing paw, no floating segment, no wrong-side edit, no altered body/head/tail/front legs, no crop change, no cast shadow, no contact shadow, no reflection, no text, no watermark, and do not use #ff00ff anywhere in the kitten.
```

## Plate ledger

Accepted plate SHA-256s:

- `front-chain-left/low-lift` (accepted v4, approximately 20 px clearance): `3ed16c9f09d894c02b0415340a0a9fa10c1e817ff02697fa8be4e3f220c75269`
- `front-chain-right/low-lift` (accepted v2, approximately 10-15 px clearance): `12fb49e4481bb6b2aa07f42a0cfa7ed48eee4c100c835f29092f270f1ca5488b`
- `rear-chain-left/low-lift` (accepted v4, measured 13 px lift, `903` to `890`): `236e685fc74ff09b9a3ac3eff8014efe5254fbc7ce10e2ca863e5e50627732ee`
- `rear-chain-right/low-lift` (accepted v4, measured 20 px lift, `918` to `898`): `15680f28838927ce5a5e8ca11ba0acb2e1e07ccef8fddb46f169b6e93c467c1c`
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

### Source-locked motion-review additions

Motion review replaced the initially planned segmented foreleg wave and articulated-lid blink in the accepted clips because those assemblies exposed transparent gaps, rectangular lid edges, jagged eye detail, and progressive loss of Waffle's painted fidelity. The replacement states reuse accepted source material and deterministic extraction; they required no new image-generation calls.

- `front-chain-left/paw-lifted` is a complete foreleg extraction from the accepted `front-paw-left/lifted` plate (`e7ddf3afd78b7c54432261201151d257c279c0945e92edb7b259b316bdf39525`), registered as `front-chain-left/paw-lifted`; committed raster SHA-256 `91029d9baad01ca5069fea8fc32185fc8312ac0ab77092d9b0a71002ecd8181c`.
- `front-chain-left/paw-wave` is a complete foreleg extraction from the accepted `front-paw-left/wave` plate (`858fad1b352f7b8475b78f1584c9ffa87c1225d0f52bbe8d821d5942234c322a`), registered as `front-chain-left/paw-wave`; committed raster SHA-256 `b5fe2fc09ae020617bd87ec16ad127c373cbf0f8c68247b51aa471f2746d6c49`.
- `front-chain-left/paw-landing` is a deterministic source sample from the approved standing master (`3397ec54c93cb12d612353166f20600689323f3bfb93a85cd8c950f4b44a902b`), registered as `front-chain-left/paw-landing`; committed raster SHA-256 `fb85d6a9ad19dae193ddf078bdfb776da63e0deaa72a93596507e1d3d66a3b5c`.
- `head-base/blink-left` is a clip-only complete-head extraction from the accepted `upper-lid-left` plate (`ee4ae8f44ed61a5cc1ae36d70eea5f4378ab7e7bfaa7ce955a5797566bdd41ed`); committed raster SHA-256 `b30f372bc1a0b452f6a279adf4d05d204c3f54708a3b7dafc19b20d7bca45ce3`.
- `head-base/blink-right` is a clip-only complete-head extraction from the accepted `upper-lid-right` plate (`18df8d51fbd80327adaf28aa934bc173215cd4815e569b224683311b9089ba02`); committed raster SHA-256 `b74fd84f8294f3f05232a65d016a05077a34a091e6fe348239fa66eb61dda349`.

The accepted clips select the painted blink heads explicitly and hold the articulated `blinkLeft` and `blinkRight` controls at zero. The articulated lid assets remain registered only as a legacy/fallback mechanism; they are not composited in the accepted `paw-wave` or `head-face-tail` motion.

Rejected plate/attempt SHA-256s and reason:

- `front-chain-left/low-lift-v1`: `475c2ecbfcf2afa6b68bcdfb37f3de7fb5a0abd6211b310e05617a4fa1f59f69` — correct side and connected anatomy, but roughly 50-60 px high instead of 10-20 px, with slight full-body registration drift.
- `front-chain-left/low-lift-v2`: `e3c690893dffa8fed3496ce8e7472b3d441525ed537461a9816d1c807df84dbb` — still roughly 35-45 px above the baseline.
- `front-chain-left/low-lift-v3`: `8d3279c9c6b9deb1ec5fb7208cf5722851b8ff81f4fc115730746a3d5ef7a746` — conventional high dangling step; retained as the corrective edit target for accepted v4.
- `front-chain-right/low-lift-v1`: `86ad5741c9fa67fee7ac289385e0a71e4a22b62e3671e2aec4538a420b339486` — wrong-side/ambiguous edit copied the screen-left reference clearance while the requested inner screen-right paw remained too low.
- `rear-chain-left/low-lift-v1`: `6e8d7e69bb7e5e29e6076a8929a4cae096b99b16e7b5e56c96e79301e35f5a4d` — correct rear chain and coherent anatomy, but above the requested 10-20 px range.
- `rear-chain-left/low-lift-v2`: `50a9a5a306b287ae2dc3744da5aff389ede3e2ec34c28a5511cfba852277e585` — high-lift reference continued to bias the output too high.
- `rear-chain-left/low-lift-v3`: `0a7087009ef0e6085da7ab2ec00db6130d9e7c6d2a11cf65a3d72edb3acdea90` — achieved the low height on the wrong, outer screen-right rear chain.
- `rear-chain-right/low-lift-v1`: `c44891b501dddf6184153fe5430ff448c6218aec7bc271f0310238d206526ab6` — correct side and anatomy, but still read planted with no clear key-colour gap beneath the paw.
- `rear-chain-right/low-lift-v2`: `5eab775897cca40953f774f7249305819a57fda9b4e78cdd5fce8f00dd0f3b12` — high-lift-biased and introduced a forbidden ground shadow/gradient.
- `rear-chain-right/low-lift-v3`: `a54688044730935b64ab5e7370de4acc37729daa14d174e72bc6b4bb0670e29d` — correct side with no duplicate or shadow, but measured 25 px high (`918` to `893`), outside the requested band.
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

`variants.json` is authoritative for all 21 non-neutral production variant crops, polygons, chroma thresholds, and optional edge feather: four low-lift chain members, four deterministic landing members, five local paw states, three source-locked front-chain paw states, two head turns, two clip-only painted blink heads, and one open jaw. Each walk landing member bakes its registered low-lift offset into the proximal painting, uses a vertical premultiplied blend toward the declared neutral distal region through the overlap band, fades pixels outside that distal ownership to transparency, and uses exact neutral-source RGBA below the concealed seam (front `y=681`, rear `y=790`). Walk landing members therefore need neither a generated plate nor a runtime transform. The source-locked `front-chain-left/paw-landing` is instead sampled directly from the approved standing master. Head turns use crop `[350,40,520,520]`, the declared 13-point face/neck polygon, thresholds `120/235`, and 16 px bounded feather. Painted blink heads use their declared complete-head polygons and 16 px bounded feather. Paw and jaw crops remain rectangular and use their declared thresholds. `repairs.json` is authoritative for 23 source-sampled concealed repairs and four lid overlays: 16 baseline joint/body repairs, six walk-only supports, and one paw-wave-only chest cover.

The lower lids are not generated states. `lower-lid-left` is deterministically extracted from the accepted `upper-lid-left` plate, and `lower-lid-right` from the accepted `upper-lid-right` plate, with side-specific narrow crescent polygons declared in `repairs.json`. All 23 concealed repairs are deterministic source samples from the approved standing image, optionally with declared sample offsets/fallback samples. The four `walk-socket-*` underlays and two compact front `walk-cover-*` layers taper to transparent at their authored perimeters; the paw-wave-only chest cover hides the moving full-chain plate's upper boundary during its lifted interval. Rear low-lift members are trimmed above the leg root and blend directly into shaped underlays; no rear above-member cover is used. No prompt or generated plate is involved in either fallback path.

For complete head turns and painted blinks, each non-neutral head member hides all 15 superseded registered descendants through schema-validated `layerOverrides`: both ears, muzzle, closed jaw, both iris/pupil/highlight chains, all four lid overlays, and whiskers. The selected painted member therefore supplies one complete coherent face; it is never partially mixed with the frontal child chain. `blink-left` and `blink-right` are additionally marked `clipOnly`, so numeric `headTurn` evaluation cannot select them accidentally.
