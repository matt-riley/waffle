import assert from "node:assert/strict";
import { access, readFile } from "node:fs/promises";
import path from "node:path";
import { test } from "node:test";

import { PNG } from "pngjs";

import {
  assertLoopClosure,
  evaluateClip,
  renderRigFrameSnapshot,
  worldTransforms,
} from "../rig-motion.mjs";
import { validateMotionClipShape, validateRigV2Shape } from "../rig-schema-v2.mjs";

const REPO_ROOT = path.resolve(import.meta.dirname, "../../..");
const RIG_PATH = path.join(REPO_ROOT, "assets/brand/waffle/rigs/standing-v2/rig.json");
const RIG_DIRECTORY = path.dirname(RIG_PATH);
const WAVE_PATH = path.join(RIG_DIRECTORY, "motions/paw-wave.json");
const EXPRESSION_PATH = path.join(RIG_DIRECTORY, "motions/head-face-tail.json");
const NEUTRAL_PATH = path.join(RIG_DIRECTORY, "neutral-reference.png");
const SOLID_ALPHA = 128;

async function exists(file) {
  try {
    await access(file);
    return true;
  } catch {
    return false;
  }
}

const [WAVE_EXISTS, EXPRESSION_EXISTS] = await Promise.all([
  exists(WAVE_PATH),
  exists(EXPRESSION_PATH),
]);

test("paw-wave production clip exists", () => {
  assert.equal(
    WAVE_EXISTS,
    true,
    "missing assets/brand/waffle/rigs/standing-v2/motions/paw-wave.json",
  );
});

test("head-face-tail production clip exists", () => {
  assert.equal(
    EXPRESSION_EXISTS,
    true,
    "missing assets/brand/waffle/rigs/standing-v2/motions/head-face-tail.json",
  );
});

let manifestPromise;
let rasterPromise;

async function manifest() {
  manifestPromise ??= readFile(RIG_PATH, "utf8").then(JSON.parse);
  const value = await manifestPromise;
  validateRigV2Shape(value);
  return value;
}

async function productionClip(file) {
  const [rig, clip] = await Promise.all([
    manifest(),
    readFile(file, "utf8").then(JSON.parse),
  ]);
  validateMotionClipShape(rig, clip);
  assertLoopClosure(rig, clip);
  return { clip, manifest: rig };
}

async function rasters(rig) {
  rasterPromise ??= (async () => {
    const files = new Set([
      ...rig.layers.map(({ file }) => file),
      ...Object.values(rig.variants).flatMap(({ members }) => members.map(({ file }) => file)),
    ]);
    return new Map(await Promise.all([...files].map(async (file) => [
      file,
      PNG.sync.read(await readFile(path.join(RIG_DIRECTORY, file))),
    ])));
  })();
  return rasterPromise;
}

async function snapshot(rig, clip) {
  return { manifest: rig, clip, rasters: await rasters(rig) };
}

function states(rig, clip) {
  return Array.from({ length: clip.frameCount }, (_, frame) => evaluateClip(rig, clip, frame));
}

function collapsed(values) {
  return values.filter((value, index) => index === 0 || value !== values[index - 1]);
}

function neutralMembers(rig) {
  return new Map(Object.entries(rig.variants).map(([setId, set]) => [
    setId,
    set.members.find(({ neutral }) => neutral).id,
  ]));
}

function assertExactNeutralState(rig, evaluated, label) {
  for (const [name, value] of evaluated.controls) {
    assert.equal(value, 0, `${label} control ${name} must be neutral zero`);
  }
  for (const [setId, memberId] of neutralMembers(rig)) {
    assert.equal(evaluated.variants.get(setId), memberId, `${label} ${setId} must select ${memberId}`);
  }
  for (const [layerId, opacity] of evaluated.layerOpacity) {
    assert.equal(opacity, 0, `${label} optional layer ${layerId} must be hidden`);
  }
}

async function assertExactNeutralRender(rig, clip, frame, label) {
  const [rendered, neutral] = await Promise.all([
    renderRigFrameSnapshot(await snapshot(rig, clip), frame),
    readFile(NEUTRAL_PATH).then(PNG.sync.read),
  ]);
  assert.equal(rendered.width, neutral.width);
  assert.equal(rendered.height, neutral.height);
  assertVisibleRgbaEqual(rendered.data, neutral.data, label);
}

function assertVisibleRgbaEqual(actual, expected, label) {
  assert.equal(actual.length, expected.length, `${label} RGBA byte length`);
  for (let offset = 0; offset < expected.length; offset += 4) {
    const pixel = offset / 4;
    assert.equal(actual[offset + 3], expected[offset + 3], `${label} pixel ${pixel} alpha`);
    if (expected[offset + 3] === 0) continue;
    assert.deepEqual(
      [...actual.subarray(offset, offset + 3)],
      [...expected.subarray(offset, offset + 3)],
      `${label} pixel ${pixel} visible RGB`,
    );
  }
}

test("neutral render comparison ignores hidden RGB while preserving visible RGBA", () => {
  assert.doesNotThrow(() => assertVisibleRgbaEqual(
    Buffer.from([0, 0, 0, 0, 10, 20, 30, 128]),
    Buffer.from([10, 17, 16, 0, 10, 20, 30, 128]),
    "fixture",
  ));
  assert.throws(
    () => assertVisibleRgbaEqual(Buffer.from([10, 20, 30, 127]), Buffer.from([10, 20, 30, 128]), "fixture"),
    /pixel 0 alpha/,
  );
  assert.throws(
    () => assertVisibleRgbaEqual(Buffer.from([10, 20, 31, 128]), Buffer.from([10, 20, 30, 128]), "fixture"),
    /pixel 0 visible RGB/,
  );
});

function controlRange(evaluated, name) {
  const values = evaluated.map(({ controls }) => controls.get(name));
  return { min: Math.min(...values), max: Math.max(...values), values };
}

function strictAbsolutePeaks(values, eligible) {
  const peaks = [];
  for (let frame = 1; frame < values.length - 1; frame += 1) {
    if (!eligible(frame)) continue;
    const previous = Math.abs(values[frame - 1]);
    const current = Math.abs(values[frame]);
    const next = Math.abs(values[frame + 1]);
    if (current >= 1 && current > previous && current > next) peaks.push(frame);
  }
  return peaks;
}

const LEFT_FOREPAW_REGION = { x: 430, y: 700, width: 190, height: 260 };

function distalForepawMetrics(rendered, region = LEFT_FOREPAW_REGION) {
  let bottom = -1;
  let count = 0;
  let sumY = 0;
  for (let y = region.y; y < region.y + region.height; y += 1) {
    for (let x = region.x; x < region.x + region.width; x += 1) {
      const alpha = rendered.data[(y * rendered.width + x) * 4 + 3];
      if (alpha < SOLID_ALPHA) continue;
      bottom = Math.max(bottom, y);
      count += 1;
      sumY += y;
    }
  }
  assert.ok(count > 0, "distal forepaw window must contain opaque artwork");
  return { bottom, centroidY: sumY / count };
}

function localizedCompositeDelta(left, right, region = LEFT_FOREPAW_REGION) {
  let alphaTurnover = 0;
  let coverage = 0;
  let premultipliedColourTurnover = 0;
  for (let y = region.y; y < region.y + region.height; y += 1) {
    for (let x = region.x; x < region.x + region.width; x += 1) {
      const offset = (y * left.width + x) * 4;
      const leftAlpha = left.data[offset + 3];
      const rightAlpha = right.data[offset + 3];
      alphaTurnover += Math.abs(leftAlpha - rightAlpha);
      coverage += Math.max(leftAlpha, rightAlpha);
      for (let channel = 0; channel < 3; channel += 1) {
        const leftPremultiplied = (left.data[offset + channel] * leftAlpha) / 255;
        const rightPremultiplied = (right.data[offset + channel] * rightAlpha) / 255;
        premultipliedColourTurnover += Math.abs(leftPremultiplied - rightPremultiplied);
      }
    }
  }
  assert.ok(coverage > 0, "localized forepaw continuity window must cover visible artwork");
  return (alphaTurnover + premultipliedColourTurnover / 3) / coverage;
}

function horizontalChestSeamWitness(rendered) {
  let witness = { count: 0, y: -1 };
  for (let y = 510; y < 575; y += 1) {
    let count = 0;
    for (let x = 460; x < 650; x += 1) {
      const current = (y * rendered.width + x) * 4;
      const above = ((y - 1) * rendered.width + x) * 4;
      if (rendered.data[current + 3] < SOLID_ALPHA || rendered.data[above + 3] < SOLID_ALPHA) continue;
      const colourStep = [0, 1, 2].reduce((sum, channel) => (
        sum + Math.abs(rendered.data[current + channel] - rendered.data[above + channel])
      ), 0);
      if (colourStep >= 45) count += 1;
    }
    if (count > witness.count) witness = { count, y };
  }
  return witness;
}

test("paw wave is a 24 fps 72-frame exact-neutral all-fours loop", { skip: !WAVE_EXISTS }, async () => {
  const { manifest: rig, clip } = await productionClip(WAVE_PATH);
  assert.equal(clip.id, "paw-wave");
  assert.equal(clip.fps, 24);
  assert.equal(clip.frameCount, 72);
  assert.equal(clip.loop, true);
  assert.deepEqual(clip.requiredClosure, { firstFrame: 0, lastFrame: 71 });

  const evaluated = states(rig, clip);
  assertExactNeutralState(rig, evaluated[0], "frame 0");
  assertExactNeutralState(rig, evaluated[71], "frame 71");
  await assertExactNeutralRender(rig, clip, 0, "frame 0");
  await assertExactNeutralRender(rig, clip, 71, "frame 71");
});

test("paw wave transfers weight screen-right before lifting the screen-left foreleg", { skip: !WAVE_EXISTS }, async () => {
  const { manifest: rig, clip } = await productionClip(WAVE_PATH);
  const evaluated = states(rig, clip);
  const leftChain = evaluated.map(({ variants }) => variants.get("front-chain-left"));
  const firstLift = leftChain.findIndex((member) => member !== "neutral");
  assert.ok(firstLift > 1, "the screen-left paw must remain planted during a visible preparation phase");

  const transferFrames = evaluated
    .slice(0, firstLift)
    .map(({ controls }, frame) => ({ frame, value: controls.get("weightShift") }))
    .filter(({ value }) => value > 0);
  assert.ok(transferFrames.length > 0, "positive weightShift must move the torso away from the screen-left paw before lift");
  assert.ok(transferFrames.at(-1).frame < firstLift, "weight transfer must precede the first lifted member");

  for (const setId of ["front-paw-right", "rear-paw-left", "rear-paw-right"]) {
    assert.ok(
      evaluated.every(({ variants }) => variants.get(setId) === "planted"),
      `${setId} must remain planted throughout the wave`,
    );
  }
});

test("paw wave swaps one opaque source-locked screen-left foreleg instead of articulating cut layers", { skip: !WAVE_EXISTS }, async () => {
  const { manifest: rig, clip } = await productionClip(WAVE_PATH);
  const evaluated = states(rig, clip);

  assert.deepEqual(
    collapsed(evaluated.map(({ variants }) => variants.get("front-chain-left"))),
    ["neutral", "paw-landing", "paw-lifted", "paw-wave", "paw-lifted", "paw-wave", "paw-lifted", "paw-landing", "neutral"],
  );
  assert.ok(
    evaluated.every(({ variants }) => variants.get("front-paw-left") === "planted"),
    "the detached paw set must stay planted because the opaque chain member owns the complete foreleg",
  );

  for (const frame of [14, 20, 27, 39, 52]) {
    for (const control of ["frontUpperLeft", "frontLowerLeft", "frontPawLeft"]) {
      assert.equal(evaluated[frame].controls.get(control), 0, `frame ${frame} ${control} must remain source-locked`);
    }
  }

  for (const control of ["breath", "bodyBob", "bodyLean", "headTilt"]) {
    assert.ok(
      evaluated.every((state) => state.controls.get(control) === 0),
      `${control} must stay source-locked because this proof has no opaque full-body joint cover`,
    );
  }
  assert.ok(
    Math.max(...evaluated.map(({ controls }) => controls.get("weightShift"))) <= 0.001,
    "pre-lift weight intent must stay subpixel-safe",
  );

  for (const layerId of [
    "body-repair",
    "front-shoulder-repair-left",
    "front-elbow-repair-left",
    "front-wrist-repair-left",
    "neck-repair",
  ]) {
    assert.ok(
      evaluated.every(({ layerOpacity }) => (layerOpacity.get(layerId) ?? 0) === 0),
      `${layerId} must remain hidden because source-locked members cannot open articulated seams`,
    );
  }
  for (const layerId of ["walk-socket-front-left", "walk-cover-front-left"]) {
    assert.equal(evaluated[0].layerOpacity.get(layerId), 0, `${layerId} must start hidden`);
    for (const frame of [14, 20, 39, 60, 67]) {
      assert.equal(evaluated[frame].layerOpacity.get(layerId), 1, `${layerId} must cover frame ${frame}`);
    }
    assert.equal(evaluated[68].layerOpacity.get(layerId), 0, `${layerId} must hide before neutral lockback`);
    assert.equal(evaluated[71].layerOpacity.get(layerId), 0, `${layerId} must close hidden`);
  }
});

test("paw wave renders a registered entry lift and a progressive landing without a one-frame paw snap", { skip: !WAVE_EXISTS }, async () => {
  const { manifest: rig, clip } = await productionClip(WAVE_PATH);
  const motion = await snapshot(rig, clip);
  const rendered = new Map();
  const frame = async (number) => {
    if (!rendered.has(number)) rendered.set(number, await renderRigFrameSnapshot(motion, number));
    return rendered.get(number);
  };
  const metric = async (number) => distalForepawMetrics(await frame(number));

  const entryFrames = [13, 14, 15, 16, 17, 18, 19];
  const entry = await Promise.all(entryFrames.map(metric));
  assert.ok(
    Math.abs(entry[1].bottom - entry[0].bottom) <= 12,
    `entry lift cannot teleport the paw ${Math.abs(entry[1].bottom - entry[0].bottom)} px in one frame`,
  );
  assert.ok(entry[1].bottom - entry.at(-1).bottom >= 18, "entry must visibly lift across several rendered frames");
  for (let index = 1; index < entry.length; index += 1) {
    assert.ok(
      entry[index - 1].bottom - entry[index].bottom >= 0
        && entry[index - 1].bottom - entry[index].bottom <= 12,
      `entry frame ${entryFrames[index]} must advance upward by at most 12 px`,
    );
  }

  const descentFrames = Array.from({ length: 13 }, (_, index) => index + 48);
  const descent = await Promise.all(descentFrames.map(metric));
  assert.ok(
    descent.at(-1).bottom - descent[0].bottom >= 18,
    "complete source-locked foreleg must visibly descend before its registered landing",
  );
  assert.ok(new Set(descent.map(({ bottom }) => bottom)).size >= 6, "landing must progress across several rendered positions");
  for (let index = 1; index < descent.length; index += 1) {
    const step = descent[index].bottom - descent[index - 1].bottom;
    assert.ok(step >= 0 && step <= 12, `landing frame ${descentFrames[index]} advances ${step} px instead of descending continuously`);
  }

  const preHandoff = localizedCompositeDelta(await frame(58), await frame(59));
  const landingHandoff = localizedCompositeDelta(await frame(59), await frame(60));
  assert.ok(preHandoff > 0, "descent must still visibly advance immediately before landing");
  assert.ok(
    landingHandoff <= 0.42,
    `registered landing handoff exceeds localized composite budget: ${landingHandoff.toFixed(4)}`,
  );
});

test("paw wave carries a staggered seam-safe restless tail slam and closes exactly", { skip: !WAVE_EXISTS }, async () => {
  const { manifest: rig, clip } = await productionClip(WAVE_PATH);
  const evaluated = states(rig, clip);
  const values = Object.fromEntries(["tailBase", "tailMid", "tailTip"].map((name) => [
    name,
    evaluated.map(({ controls }) => controls.get(name)),
  ]));

  for (const [name, minimumRange] of [["tailBase", 8], ["tailMid", 14], ["tailTip", 22]]) {
    const range = Math.max(...values[name]) - Math.min(...values[name]);
    assert.ok(range >= minimumRange, `${name} range ${range} must read as restless secondary motion`);
    assert.equal(values[name][0], 0, `${name} must start neutral`);
    assert.equal(values[name][71], 0, `${name} must close neutral`);
  }

  const negativePeaks = ["tailBase", "tailMid", "tailTip"].map((name) => values[name].indexOf(Math.min(...values[name])));
  assert.equal(new Set(negativePeaks).size, 3, "tail base, mid, and tip slam extrema must be staggered");
  assert.ok(
    Math.max(...values.tailTip.slice(1).map((value, frame) => Math.abs(value - values.tailTip[frame]))) >= 3,
    "tail tip must contain a quick expressive slam rather than a gentle pendulum",
  );

  for (let frame = 1; frame < 71; frame += 1) {
    assert.equal(evaluated[frame].layers.get("tail-base-mid-repair").opacity, 1, `frame ${frame} base-mid tail repair`);
    assert.equal(evaluated[frame].layers.get("tail-mid-tip-repair").opacity, 1, `frame ${frame} mid-tip tail repair`);
  }
  for (const frame of [0, 71]) {
    assert.equal(evaluated[frame].layers.get("tail-base-mid-repair").opacity, 0, `frame ${frame} base-mid repair closure`);
    assert.equal(evaluated[frame].layers.get("tail-mid-tip-repair").opacity, 0, `frame ${frame} mid-tip repair closure`);
  }
});

test("moving source-locked foreleg remains concealed below the registered chest without a horizontal raster edge", { skip: !WAVE_EXISTS }, async () => {
  const { manifest: rig, clip } = await productionClip(WAVE_PATH);
  const motion = await snapshot(rig, clip);
  for (const frame of [15, 58, 59]) {
    const rendered = await renderRigFrameSnapshot(motion, frame);
    const witness = horizontalChestSeamWitness(rendered);
    assert.ok(
      witness.count <= 20,
      `frame ${frame} exposes a ${witness.count}-pixel horizontal chest edge at y=${witness.y}`,
    );
  }
});

test("paw wave uses registered opaque chain states for exactly two small feline peaks", { skip: !WAVE_EXISTS }, async () => {
  const { manifest: rig, clip } = await productionClip(WAVE_PATH);
  const evaluated = states(rig, clip);
  const members = evaluated.map(({ variants }) => variants.get("front-chain-left"));
  assert.deepEqual(
    collapsed(members),
    ["neutral", "paw-landing", "paw-lifted", "paw-wave", "paw-lifted", "paw-wave", "paw-lifted", "paw-landing", "neutral"],
  );

  const track = clip.variants["front-chain-left"];
  assert.ok(track, "front-chain-left must declare explicit registered state boundaries");
  assert.ok(track.every(({ interpolation }) => interpolation === "hold"), "painted paw states must change discretely at held keys");
  const transitionFrames = members.flatMap((member, frame) => (
    frame > 0 && member !== members[frame - 1] ? [frame] : []
  ));
  const keyedFrames = new Set(track.map(({ frame }) => frame));
  assert.ok(transitionFrames.every((frame) => keyedFrames.has(frame)), "every painted state change must occur on a registered keyframe");

  const waveStarts = members.flatMap((member, frame) => (
    member === "paw-wave" && members[frame - 1] !== "paw-wave" ? [frame] : []
  ));
  assert.deepEqual(waveStarts, [20, 33], "the two held paw shapes must read as separate feline gestures");
  for (const control of ["frontUpperLeft", "frontLowerLeft", "frontPawLeft"]) {
    assert.ok(evaluated.every((state) => state.controls.get(control) === 0), `${control} must not articulate the opaque chain`);
  }
});

test("paw wave uses painted blink heads and keeps artifact-prone lid overlays disabled", { skip: !WAVE_EXISTS }, async () => {
  const { manifest: rig, clip } = await productionClip(WAVE_PATH);
  const evaluated = states(rig, clip);
  const moves = (name, minimum = 0) => {
    const { min, max } = controlRange(evaluated, name);
    return max - min > minimum;
  };

  assert.ok(moves("earLeft", 0.25), "screen-left ear must add secondary motion");
  assert.ok(moves("earRight", 0.25), "screen-right ear must add secondary motion");
  assert.equal(evaluated[33].variants.get("head-base"), "blink-left");
  assert.equal(evaluated[34].variants.get("head-base"), "blink-right");
  for (const state of evaluated) {
    assert.equal(state.controls.get("blinkLeft"), 0, "screen-left lid control must remain disabled");
    assert.equal(state.controls.get("blinkRight"), 0, "screen-right lid control must remain disabled");
    for (const side of ["left", "right"]) {
      assert.equal(state.layers.get(`upper-lid-${side}`).opacity, 0, `${side} upper-lid overlay must stay hidden`);
      assert.equal(state.layers.get(`lower-lid-${side}`).opacity, 0, `${side} lower-lid overlay must stay hidden`);
    }
  }
});

test("paw wave source-locks articulated pupil gaze so light backgrounds cannot leak through the painted irises", { skip: !WAVE_EXISTS }, async () => {
  const { manifest: rig, clip } = await productionClip(WAVE_PATH);
  const evaluated = states(rig, clip);
  for (let frame = 0; frame < evaluated.length; frame += 1) {
    assert.equal(evaluated[frame].controls.get("gazeX"), 0, `frame ${frame} gazeX must remain source-locked`);
    assert.equal(evaluated[frame].controls.get("gazeY"), 0, `frame ${frame} gazeY must remain source-locked`);
  }
});

test("head-face-tail proof is 24 fps, 48 frames, and follows neutral-left-neutral-right-neutral", { skip: !EXPRESSION_EXISTS }, async () => {
  const { manifest: rig, clip } = await productionClip(EXPRESSION_PATH);
  assert.equal(clip.id, "head-face-tail");
  assert.equal(clip.fps, 24);
  assert.equal(clip.frameCount, 48);
  assert.equal(clip.loop, true);
  assert.deepEqual(clip.requiredClosure, { firstFrame: 0, lastFrame: 47 });

  const evaluated = states(rig, clip);
  assert.deepEqual(
    collapsed(evaluated.map(({ variants }) => variants.get("head-base"))),
    [
      "neutral", "turn-left", "neutral",
      "blink-left", "neutral", "blink-right", "neutral",
      "turn-right", "neutral", "blink-left", "blink-right", "neutral",
    ],
  );
  assertExactNeutralState(rig, evaluated[0], "frame 0");
  assertExactNeutralState(rig, evaluated[47], "frame 47");
  await assertExactNeutralRender(rig, clip, 0, "frame 0");
  await assertExactNeutralRender(rig, clip, 47, "frame 47");
});

test("painted head turns stay source-locked instead of rotating open a neck seam", { skip: !EXPRESSION_EXISTS }, async () => {
  const { manifest: rig, clip } = await productionClip(EXPRESSION_PATH);
  const evaluated = states(rig, clip);

  for (const frame of [...Array.from({ length: 7 }, (_, index) => index + 8), ...Array.from({ length: 7 }, (_, index) => index + 28)]) {
    assert.notEqual(evaluated[frame].variants.get("head-base"), "neutral", `frame ${frame} must use a painted head turn`);
    assert.equal(evaluated[frame].controls.get("headTilt"), 0, `frame ${frame} painted head turn must remain source-locked`);
  }
});

function transformPoint(matrix, x, y) {
  return [
    matrix[0] * x + matrix[2] * y + matrix[4],
    matrix[1] * x + matrix[3] * y + matrix[5],
  ];
}

function solidPoints(image) {
  const points = [];
  for (let y = 0; y < image.height; y += 1) {
    for (let x = 0; x < image.width; x += 1) {
      if (image.data[(y * image.width + x) * 4 + 3] >= SOLID_ALPHA) points.push([x, y]);
    }
  }
  return points;
}

test("head-face-tail source-locks articulated pupil gaze so backgrounds cannot leak through the painted irises", { skip: !EXPRESSION_EXISTS }, async () => {
  const { manifest: rig, clip } = await productionClip(EXPRESSION_PATH);
  const evaluated = states(rig, clip);
  for (let frame = 0; frame < evaluated.length; frame += 1) {
    assert.equal(evaluated[frame].controls.get("gazeX"), 0, `frame ${frame} gazeX must remain source-locked`);
    assert.equal(evaluated[frame].controls.get("gazeY"), 0, `frame ${frame} gazeY must remain source-locked`);
  }
});

test("independent and staggered blinks use source-locked painted heads with articulated lids disabled", { skip: !EXPRESSION_EXISTS }, async () => {
  const { manifest: rig, clip } = await productionClip(EXPRESSION_PATH);
  const evaluated = states(rig, clip);
  assert.equal(evaluated[18].variants.get("head-base"), "blink-left");
  assert.equal(evaluated[21].variants.get("head-base"), "blink-right");
  assert.equal(evaluated[39].variants.get("head-base"), "blink-left");
  assert.equal(evaluated[40].variants.get("head-base"), "blink-right");
  assert.ok(clip.variants["head-base"].every(({ interpolation }) => interpolation === "hold"));

  for (const state of evaluated) {
    assert.equal(state.controls.get("blinkLeft"), 0, "painted blink must not drive the screen-left lid control");
    assert.equal(state.controls.get("blinkRight"), 0, "painted blink must not drive the screen-right lid control");
    for (const side of ["left", "right"]) {
      assert.equal(state.layers.get(`upper-lid-${side}`).opacity, 0, `${side} upper-lid overlay must stay hidden`);
      assert.equal(state.layers.get(`lower-lid-${side}`).opacity, 0, `${side} lower-lid overlay must stay hidden`);
    }
  }

  for (const memberId of ["blink-left", "blink-right"]) {
    const member = rig.variants["head-base"].members.find(({ id }) => id === memberId);
    assert.ok(member, `missing painted ${memberId} member`);
    for (const layerId of [
      "ear-left", "ear-right", "muzzle", "jaw-closed",
      "iris-left", "iris-right", "pupil-left", "pupil-right",
      "highlight-left", "highlight-right", "whiskers",
      "upper-lid-left", "lower-lid-left", "upper-lid-right", "lower-lid-right",
    ]) {
      assert.equal(member.layerOverrides[layerId].visible, false, `${memberId} must own ${layerId}`);
    }
  }
});

test("jaw-open frames preserve the source muzzle, nose, and whisker layers", { skip: !EXPRESSION_EXISTS }, async () => {
  const { manifest: rig, clip } = await productionClip(EXPRESSION_PATH);
  const evaluated = states(rig, clip);
  const openFrames = evaluated.flatMap((frame, index) => frame.variants.get("jaw") === "open" ? [index] : []);
  assert.ok(openFrames.length > 0, "proof must activate the painted open jaw");

  const openMember = rig.variants.jaw.members.find(({ id }) => id === "open");
  assert.equal(openMember.layerOverrides?.muzzle?.visible, undefined, "open jaw cannot hide the muzzle or painted nose");
  assert.equal(openMember.layerOverrides?.whiskers?.visible, undefined, "open jaw cannot hide whiskers");
  for (const frame of openFrames) {
    assert.equal(evaluated[frame].variants.get("head-base"), "neutral", `frame ${frame} jaw proof must retain the articulated face`);
    assert.equal(evaluated[frame].layers.get("muzzle").opacity, 1, `frame ${frame} preserves muzzle and nose`);
    assert.equal(evaluated[frame].layers.get("whiskers").opacity, 1, `frame ${frame} preserves whiskers`);
  }
});

function transformedPixelSet(points, matrix) {
  return new Set(points.map(([x, y]) => {
    const [worldX, worldY] = transformPoint(matrix, x, y);
    return `${Math.round(worldX)},${Math.round(worldY)}`;
  }));
}

function setsTouch(left, right, radius = 1) {
  for (const sample of left) {
    const [x, y] = sample.split(",").map(Number);
    for (let offsetY = -radius; offsetY <= radius; offsetY += 1) {
      for (let offsetX = -radius; offsetX <= radius; offsetX += 1) {
        if (right.has(`${x + offsetX},${y + offsetY}`)) return true;
      }
    }
  }
  return false;
}

test("tail base, mid, and tip remain alpha-connected through registered repair underlaps in both proofs", { skip: !EXPRESSION_EXISTS || !WAVE_EXISTS }, async () => {
  const { manifest: rig } = await productionClip(EXPRESSION_PATH);
  const rasterMap = await rasters(rig);
  const layerById = new Map(rig.layers.map((layer) => [layer.id, layer]));
  const pointCache = new Map();
  const points = (layerId) => {
    if (!pointCache.has(layerId)) {
      const layer = layerById.get(layerId);
      pointCache.set(layerId, solidPoints(rasterMap.get(layer.file)));
    }
    return pointCache.get(layerId);
  };

  const joints = [
    ["tail-base", "tail-base-mid-repair", "tail-mid", "tailMid"],
    ["tail-mid", "tail-mid-tip-repair", "tail-tip", "tailTip"],
  ];
  for (const [label, file] of [["head-face-tail", EXPRESSION_PATH], ["paw-wave", WAVE_PATH]]) {
    const { clip } = await productionClip(file);
    const evaluated = states(rig, clip);
    for (let frame = 0; frame < clip.frameCount; frame += 1) {
      const world = worldTransforms(rig, evaluated[frame].layers);
      for (const [parentId, repairId, childId, control] of joints) {
        const parent = transformedPixelSet(points(parentId), world.get(parentId));
        const child = transformedPixelSet(points(childId), world.get(childId));
        if (setsTouch(parent, child)) continue;

        assert.equal(
          evaluated[frame].layers.get(repairId).opacity,
          1,
          `${label} frame ${frame} ${repairId} must be fully visible when ${control} opens the source seam`,
        );
        const repair = transformedPixelSet(points(repairId), world.get(repairId));
        assert.ok(setsTouch(parent, repair), `${label} frame ${frame} ${repairId} must overlap ${parentId}`);
        assert.ok(setsTouch(repair, child), `${label} frame ${frame} ${repairId} must overlap ${childId}`);
      }
    }
  }
});

test("head proof enables the concealed neck repair for every non-neutral head frame", { skip: !EXPRESSION_EXISTS }, async () => {
  const { manifest: rig, clip } = await productionClip(EXPRESSION_PATH);
  const evaluated = states(rig, clip);
  assert.equal(evaluated[0].layerOpacity.get("neck-repair"), 0);
  for (const frame of [8, 14, 28, 34, 42, 44]) {
    assert.equal(evaluated[frame].layerOpacity.get("neck-repair"), 1, `frame ${frame} neck repair`);
  }
  assert.equal(evaluated[47].layerOpacity.get("neck-repair"), 0);
});
