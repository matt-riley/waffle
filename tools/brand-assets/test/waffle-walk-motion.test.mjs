import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { test } from "node:test";

import { PNG } from "pngjs";

import { assertLoopClosure, evaluateClip, renderRigFrame, renderRigFrameSnapshot, worldTransforms } from "../rig-motion.mjs";
import { validateMotionClipShape, validateRigV2Shape } from "../rig-schema-v2.mjs";

const REPO_ROOT = path.resolve(import.meta.dirname, "../../..");
const RIG_PATH = path.join(REPO_ROOT, "assets/brand/waffle/rigs/standing-v2/rig.json");
const CLIP_PATH = path.join(REPO_ROOT, "assets/brand/waffle/rigs/standing-v2/motions/walk-in-place.json");
const REPAIRS_PATH = path.join(REPO_ROOT, "assets/brand/waffle/rigs/standing-v2/repairs.json");
const PHASES = [0, 12, 24, 36, 47];
const PAW_SETS = [
  "front-chain-left",
  "front-chain-right",
  "rear-chain-left",
  "rear-chain-right",
];
const LIMB_CONTROLS = {
  "front-chain-left": ["frontUpperLeft", "frontLowerLeft", "frontPawLeft"],
  "front-chain-right": ["frontUpperRight", "frontLowerRight", "frontPawRight"],
  "rear-chain-left": ["rearThighLeft", "rearHockLeft", "rearPawLeft"],
  "rear-chain-right": ["rearThighRight", "rearHockRight", "rearPawRight"],
};
const CHAIN_ANCHORS = {
  "front-chain-left": "front-upper-left",
  "front-chain-right": "front-upper-right",
  "rear-chain-left": "rear-thigh-left",
  "rear-chain-right": "rear-thigh-right",
};
const SOCKETS = {
  "front-chain-left": "walk-socket-front-left",
  "front-chain-right": "walk-socket-front-right",
  "rear-chain-left": "walk-socket-rear-left",
  "rear-chain-right": "walk-socket-rear-right",
};
const CAPS = {
  "front-chain-left": ["walk-socket-front-left", "walk-cover-front-left"],
  "front-chain-right": ["walk-socket-front-right", "walk-cover-front-right"],
  "rear-chain-left": ["walk-socket-rear-left"],
  "rear-chain-right": ["walk-socket-rear-right"],
};
const WALK_CHAIN_WINDOWS = {
  "front-chain-left": {
    region: { x: 450, y: 510, width: 200, height: 150 },
    travelRegion: { x: 410, y: 700, width: 200, height: 245 },
    landingHandoffMin: 0.002,
    landingHandoffMax: 0.05,
    memberSwitchMax: 0.38,
    entry: 1,
    peak: 12,
    exit: 24,
  },
  "front-chain-right": {
    region: { x: 595, y: 510, width: 200, height: 150 },
    travelRegion: { x: 600, y: 700, width: 180, height: 245 },
    landingHandoffMin: 0.002,
    landingHandoffMax: 0.05,
    memberSwitchMax: 0.28,
    entry: 25,
    peak: 36,
    exit: 47,
  },
  "rear-chain-left": {
    region: { x: 735, y: 690, width: 220, height: 90 },
    travelRegion: { x: 735, y: 720, width: 190, height: 190 },
    landingHandoffMin: 0.002,
    landingHandoffMax: 0.16,
    memberSwitchMax: 0.46,
    entry: 25,
    peak: 36,
    exit: 47,
  },
  "rear-chain-right": {
    region: { x: 900, y: 690, width: 230, height: 90 },
    travelRegion: { x: 920, y: 720, width: 200, height: 200 },
    landingHandoffMin: 0.002,
    landingHandoffMax: 0.16,
    memberSwitchMax: 0.68,
    entry: 1,
    peak: 12,
    exit: 24,
  },
};
const FRONT_PAW_LANDINGS = {
  "front-chain-left": {
    region: { x: 440, y: 780, width: 175, height: 190 },
    neutral: 0,
    peak: 12,
    finalActive: 23,
    swap: 24,
  },
  "front-chain-right": {
    region: { x: 620, y: 780, width: 140, height: 190 },
    neutral: 24,
    peak: 36,
    finalActive: 46,
    swap: 47,
  },
};

async function productionWalk() {
  const [manifest, clip] = await Promise.all([
    readFile(RIG_PATH, "utf8").then(JSON.parse),
    readFile(CLIP_PATH, "utf8").then(JSON.parse),
  ]);
  validateRigV2Shape(manifest);
  validateMotionClipShape(manifest, clip);
  return { manifest, clip };
}

async function motionSnapshot(manifest, clip) {
  const rigDirectory = path.dirname(RIG_PATH);
  const rasterFiles = new Set([
    ...manifest.layers.map(({ file }) => file),
    ...Object.values(manifest.variants).flatMap(({ members }) => members.map(({ file }) => file)),
  ]);
  const rasters = new Map(await Promise.all([...rasterFiles].map(async (file) => [
    file,
    PNG.sync.read(await readFile(path.join(rigDirectory, file))),
  ])));
  return { manifest, clip, rasters };
}

function localizedCompositeDelta(left, right, region) {
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
  assert.ok(coverage > 0, "localized continuity window must cover visible artwork");
  return {
    alphaRatio: alphaTurnover / coverage,
    colourRatio: premultipliedColourTurnover / (coverage * 3),
    combinedRatio: (alphaTurnover + premultipliedColourTurnover / 3) / coverage,
  };
}

function distalPawMetrics(rendered, region) {
  let bottom = -1;
  let count = 0;
  let sumX = 0;
  let sumY = 0;
  for (let y = region.y; y < region.y + region.height; y += 1) {
    for (let x = region.x; x < region.x + region.width; x += 1) {
      const alpha = rendered.data[(y * rendered.width + x) * 4 + 3];
      if (alpha < 128) continue;
      bottom = Math.max(bottom, y);
      count += 1;
      sumX += x;
      sumY += y;
    }
  }
  assert.ok(count > 0, "distal paw window must contain opaque artwork");
  return { bottom, centroidX: sumX / count, centroidY: sumY / count };
}

function rightmostOpaqueX(rendered, { x, width }, y) {
  for (let candidate = x + width - 1; candidate >= x; candidate -= 1) {
    if (rendered.data[(y * rendered.width + candidate) * 4 + 3] >= 128) return candidate;
  }
  return -1;
}

function halfOpacityInteriorPixels(rendered, region) {
  const witnesses = [];
  for (let y = region.y + 1; y < region.y + region.height - 1; y += 1) {
    for (let x = region.x + 1; x < region.x + region.width - 1; x += 1) {
      let halfOpaque = true;
      for (let offsetY = -1; offsetY <= 1 && halfOpaque; offsetY += 1) {
        for (let offsetX = -1; offsetX <= 1; offsetX += 1) {
          const alpha = rendered.data[((y + offsetY) * rendered.width + x + offsetX) * 4 + 3];
          if (alpha !== 128) {
            halfOpaque = false;
            break;
          }
        }
      }
      if (halfOpaque) witnesses.push({ x, y });
    }
  }
  return witnesses;
}

async function assertLocalizedWalkContinuity(snapshot, setIds = PAW_SETS) {
  const rendered = new Map();
  const frame = async (number) => {
    if (!rendered.has(number)) rendered.set(number, await renderRigFrameSnapshot(snapshot, number));
    return rendered.get(number);
  };

  for (const setId of setIds) {
    const {
      region,
      travelRegion,
      landingHandoffMin,
      landingHandoffMax,
      memberSwitchMax,
      entry,
      peak,
      exit,
    } = WALK_CHAIN_WINDOWS[setId];
    const delta = async (left, right) => localizedCompositeDelta(
      await frame(left),
      await frame(right),
      region,
    );
    const entryHandoff = await delta(entry - 1, entry);
    const entryCompletion = await delta(entry, entry + 1);
    const exitStart = await delta(exit - 2, exit - 1);
    const exitHandoff = await delta(exit - 1, exit);

    // Neutral-to-landing swaps should be small but measurable: the distal is
    // exact neutral and the upper interchange is concealed by fixed support.
    for (const [name, handoff] of [["entry", entryHandoff], ["exit", exitHandoff]]) {
      assert.ok(
        handoff.combinedRatio >= landingHandoffMin,
        `${setId} ${name} omits its registered landing interchange: ${handoff.combinedRatio.toFixed(6)} < ${landingHandoffMin}`,
      );
      assert.ok(
        handoff.combinedRatio <= landingHandoffMax,
        `${setId} ${name} landing interchange exceeds its localized composite budget: ${handoff.combinedRatio.toFixed(6)} > ${landingHandoffMax}`,
      );
    }
    for (const [name, memberSwitch] of [["entry", entryCompletion], ["exit", exitStart]]) {
      assert.ok(memberSwitch.combinedRatio > 0, `${setId} ${name} landing-to-lift switch must visibly advance`);
      assert.ok(
        memberSwitch.combinedRatio <= memberSwitchMax,
        `${setId} ${name} landing-to-lift switch exceeds its localized composite budget: `
          + `${memberSwitch.combinedRatio.toFixed(6)} > ${memberSwitchMax}`,
      );
    }

    const earlyRise = await delta(entry + 1, entry + 2);
    const lateRise = await delta(peak - 1, peak);
    const earlyFall = await delta(peak, peak + 1);
    const lateFall = await delta(exit - 3, exit - 2);
    for (const [name, sample] of [
      ["early rise", earlyRise],
      ["late rise", lateRise],
      ["early fall", earlyFall],
      ["late fall", lateFall],
    ]) {
      assert.ok(sample.combinedRatio > 0, `${setId} ${name} must visibly advance in its local composite`);
    }

    // Each half of the authored arc is linearly interpolated. Compare its end
    // against its own adjacent-frame reference so a one-frame transform jump
    // cannot hide behind a permissive global image-difference threshold.
    assert.ok(
      lateRise.combinedRatio <= earlyRise.combinedRatio * 3,
      `${setId} rise contains an abrupt localized composite step`,
    );
    assert.ok(
      earlyRise.combinedRatio <= lateRise.combinedRatio * 3,
      `${setId} rise contains an abrupt localized composite step`,
    );
    assert.ok(
      earlyFall.combinedRatio <= lateFall.combinedRatio * 3,
      `${setId} fall contains an abrupt localized composite step`,
    );
    assert.ok(
      lateFall.combinedRatio <= earlyFall.combinedRatio * 3,
      `${setId} fall contains an abrupt localized composite step`,
    );

    const fullLift = localizedCompositeDelta(
      await frame(entry),
      await frame(peak),
      travelRegion,
    );
    assert.ok(fullLift.combinedRatio >= 0.2, `${setId} swing must produce readable rendered paw travel`);
  }
}

function mapObject(map) {
  return Object.fromEntries([...map].toSorted(([left], [right]) => left.localeCompare(right, "en")));
}

function swingMember(evaluated, setId) {
  return evaluated.variants.get(setId);
}

function swingActive(evaluated, setId) {
  return Number(swingMember(evaluated, setId) !== "neutral");
}

test("production walk declares the exact 24 fps 48-frame loop and five gait phases", async () => {
  const { clip } = await productionWalk();
  assert.equal(clip.id, "walk-in-place");
  assert.equal(clip.fps, 24);
  assert.equal(clip.frameCount, 48);
  assert.equal(clip.loop, true);
  assert.deepEqual(clip.requiredClosure, { firstFrame: 0, lastFrame: 47 });

  for (const name of ["bodyBob", "bodyLean", "weightShift", ...Object.values(LIMB_CONTROLS).flat()]) {
    assert.deepEqual(
      clip.controls[name].filter(({ frame }) => PHASES.includes(frame)).map(({ frame }) => frame),
      PHASES,
      `${name} must explicitly register every gait phase`,
    );
  }
  for (const setId of ["front-chain-left", "rear-chain-right"]) {
    assert.deepEqual(clip.variants[setId].map(({ frame, value }) => [frame, value]), [
      [0, "neutral"], [1, "landing"], [2, "low-lift"], [23, "landing"], [24, "neutral"], [47, "neutral"],
    ]);
  }
  for (const setId of ["front-chain-right", "rear-chain-left"]) {
    assert.deepEqual(clip.variants[setId].map(({ frame, value }) => [frame, value]), [
      [0, "neutral"], [25, "landing"], [26, "low-lift"], [46, "landing"], [47, "neutral"],
    ]);
  }
});

test("production walk uses feline diagonal swing and support pairs", async () => {
  const { manifest, clip } = await productionWalk();
  let leftFrontRightRearSwingFrames = 0;
  let rightFrontLeftRearSwingFrames = 0;

  for (let frame = 0; frame < clip.frameCount; frame += 1) {
    const evaluated = evaluateClip(manifest, clip, frame);
    const frontLeftLifted = swingActive(evaluated, "front-chain-left");
    const frontRightLifted = swingActive(evaluated, "front-chain-right");
    const rearLeftLifted = swingActive(evaluated, "rear-chain-left");
    const rearRightLifted = swingActive(evaluated, "rear-chain-right");

    assert.equal(frontLeftLifted, rearRightLifted, `frame ${frame} must keep front-left and rear-right paired`);
    assert.equal(frontRightLifted, rearLeftLifted, `frame ${frame} must keep front-right and rear-left paired`);
    assert.ok(frontLeftLifted + frontRightLifted <= 1, `frame ${frame} cannot lift both diagonal pairs`);
    leftFrontRightRearSwingFrames += frontLeftLifted;
    rightFrontLeftRearSwingFrames += frontRightLifted;
  }

  assert.ok(leftFrontRightRearSwingFrames >= 12, "front-left/rear-right must complete a readable swing");
  assert.ok(rightFrontLeftRearSwingFrames >= 12, "front-right/rear-left must complete a readable swing");
  assert.equal(swingMember(evaluateClip(manifest, clip, 12), "front-chain-left"), "low-lift");
  assert.equal(swingMember(evaluateClip(manifest, clip, 12), "rear-chain-right"), "low-lift");
  assert.equal(swingMember(evaluateClip(manifest, clip, 36), "front-chain-right"), "low-lift");
  assert.equal(swingMember(evaluateClip(manifest, clip, 36), "rear-chain-left"), "low-lift");
});

test("production walk uses opaque landing members at every registered interchange frame", async () => {
  const { manifest, clip } = await productionWalk();
  for (const setId of PAW_SETS) {
    assert.equal(clip.variantTransforms[setId].member, "low-lift");
    const low = manifest.variants[setId].members.find(({ id }) => id === "low-lift");
    const landing = manifest.variants[setId].members.find(({ id }) => id === "landing");
    assert.equal(low.parentOverride, "torso");
    assert.equal(landing.parentOverride, undefined, `${setId}/landing baked registration must remain authoritative`);
    assert.ok(Object.values(low.layerOverrides).every(({ visible }) => visible === false));
    assert.deepEqual(landing.layerOverrides, low.layerOverrides);
  }

  for (const setId of PAW_SETS) {
    const { entry, peak, exit } = WALK_CHAIN_WINDOWS[setId];
    const entryState = evaluateClip(manifest, clip, entry);
    const peakState = evaluateClip(manifest, clip, peak);
    const finalActiveState = evaluateClip(manifest, clip, exit - 1);
    assert.equal(swingMember(entryState, setId), "landing", `${setId} entry must use its registered landing painting`);
    assert.equal(swingMember(peakState, setId), "low-lift", `${setId} peak must use the complete lifted render`);
    assert.equal(swingMember(finalActiveState, setId), "landing", `${setId} exit must use its registered landing painting`);
    const entryTransform = entryState.variantTransforms.get(setId).transform;
    const peakTransform = peakState.variantTransforms.get(setId).transform;
    const finalActiveTransform = finalActiveState.variantTransforms.get(setId).transform;
    assert.ok(
      entryTransform.x !== 0 || entryTransform.y !== 0,
      `${setId} must enter on its registered interchange offset`,
    );
    assert.deepEqual(finalActiveTransform, entryTransform, `${setId} must return to the same interchange offset`);
    assert.equal(peakTransform.x, 0, `${setId} peak must keep its authored horizontal registration`);
    assert.ok(
      peakTransform.y <= 0 && peakTransform.y >= -0.015625,
      `${setId} peak may use at most 16 px of vertical registration correction`,
    );
    assert.deepEqual(
      { rotationDegrees: peakTransform.rotationDegrees, scaleX: peakTransform.scaleX, scaleY: peakTransform.scaleY },
      { rotationDegrees: 0, scaleX: 1, scaleY: 1 },
      `${setId} peak must not distort the approved lifted painting`,
    );
  }
});

test("rendered limb-local composites remain registered through swaps and advance smoothly through each arc", async () => {
  const { manifest, clip } = await productionWalk();
  for (const setId of PAW_SETS) {
    const transforms = Array.from(
      { length: clip.frameCount },
      (_, frame) => evaluateClip(manifest, clip, frame).variantTransforms.get(setId).transform,
    );
    for (let frame = 1; frame < transforms.length; frame += 1) {
      const previousMember = swingMember(evaluateClip(manifest, clip, frame - 1), setId);
      const currentMember = swingMember(evaluateClip(manifest, clip, frame), setId);
      if (previousMember === "low-lift" && currentMember === "low-lift") {
        assert.notDeepEqual(transforms[frame], transforms[frame - 1], `${setId} frame ${frame} must not statically hold during swing`);
      }
    }
  }
  await assertLocalizedWalkContinuity(await motionSnapshot(manifest, clip));
});

test("registered handoff frames contain no half-opacity limb interiors", async () => {
  const { manifest, clip } = await productionWalk();
  const snapshot = await motionSnapshot(manifest, clip);
  const handoffs = new Map([
    [1, ["front-chain-left", "rear-chain-right"]],
    [23, ["front-chain-left", "rear-chain-right"]],
    [25, ["front-chain-right", "rear-chain-left"]],
    [46, ["front-chain-right", "rear-chain-left"]],
  ]);
  const failures = [];

  for (const [frame, setIds] of new Map([
    [12, ["front-chain-left", "rear-chain-right"]],
    [36, ["front-chain-right", "rear-chain-left"]],
  ])) {
    const rendered = await renderRigFrameSnapshot(snapshot, frame);
    for (const setId of setIds) {
      assert.equal(
        halfOpacityInteriorPixels(rendered, WALK_CHAIN_WINDOWS[setId].travelRegion).length,
        0,
        `${setId} frame ${frame} opaque pose control must not contain a half-opacity interior`,
      );
    }
  }

  for (const [frame, setIds] of handoffs) {
    const rendered = await renderRigFrameSnapshot(snapshot, frame);
    for (const setId of setIds) {
      const witnesses = halfOpacityInteriorPixels(rendered, WALK_CHAIN_WINDOWS[setId].travelRegion);
      if (witnesses.length > 0) {
        failures.push(`${setId} frame ${frame}: ${witnesses.length} pixels, first at ${witnesses[0].x},${witnesses[0].y}`);
      }
    }
  }

  assert.deepEqual(
    failures,
    [],
    `registered pose handoffs must render one opaque silhouette, not a half-opacity double image:\n${failures.join("\n")}`,
  );
});

test("rear-left landing member has a tapered upper-leg contour without a right-angle ledge", async () => {
  const { manifest, clip } = await productionWalk();
  const snapshot = await motionSnapshot(manifest, clip);
  const landingMember = manifest.variants["rear-chain-left"].members.find(({ id }) => id === "landing");
  const landing = snapshot.rasters.get(landingMember.file);
  const region = { x: 800, width: 115 };

  for (const frame of [25, 46]) {
    assert.equal(swingMember(evaluateClip(manifest, clip, frame), "rear-chain-left"), "landing");
  }

  assert.ok(
    rightmostOpaqueX(landing, region, 740) <= 876,
    "rear-chain-left transition start must not inject the neutral leg ledge at full opacity",
  );
  let previous = -1;
  for (let y = 740; y <= 790; y += 1) {
    const current = rightmostOpaqueX(landing, region, y);
    if (current === -1) continue;
    if (previous !== -1) {
      assert.ok(
        current - previous <= 6,
        `rear-chain-left landing has a ${current - previous}px right-angle ledge before row ${y}`,
      );
    }
    previous = current;
  }
  assert.notEqual(previous, -1, "rear-chain-left landing must retain its distal leg contour");
});

test("both rendered front paws descend and register on their neutral landing through the opaque handoff", async () => {
  const { manifest, clip } = await productionWalk();
  const snapshot = await motionSnapshot(manifest, clip);
  const rendered = new Map();
  const metrics = async (frame, region) => {
    if (!rendered.has(frame)) rendered.set(frame, await renderRigFrameSnapshot(snapshot, frame));
    return distalPawMetrics(rendered.get(frame), region);
  };

  const peakClearances = new Map();
  for (const [setId, { region, neutral, peak, finalActive, swap }] of Object.entries(FRONT_PAW_LANDINGS)) {
    const neutralPaw = await metrics(neutral, region);
    const peakPaw = await metrics(peak, region);
    let previous = peakPaw;
    for (let frame = peak + 1; frame <= finalActive; frame += 1) {
      const current = await metrics(frame, region);
      assert.ok(
        current.bottom >= previous.bottom,
        `${setId} frame ${frame} paw bottom must descend monotonically after peak`,
      );
      previous = current;
    }

    assert.ok(
      neutralPaw.bottom - peakPaw.bottom >= 16 && neutralPaw.bottom - peakPaw.bottom <= 24,
      `${setId} peak must have 16-24 px of rendered paw clearance`,
    );
    peakClearances.set(setId, neutralPaw.bottom - peakPaw.bottom);
    assert.ok(
      Math.abs(previous.bottom - neutralPaw.bottom) <= 1,
      `${setId} frame ${finalActive} paw bottom must land within 1 px before frame ${swap} swap`,
    );
    assert.ok(
      // The approved low-lift and neutral paintings have different distal
      // silhouettes. Keep their floor line exact while allowing the painted
      // area centroid to differ slightly; the broken airborne handoff was
      // more than 5 px away.
      Math.abs(previous.centroidY - neutralPaw.centroidY) <= 4,
      `${setId} frame ${finalActive} paw centroid Y must register within 4 px before frame ${swap} swap`,
    );
    assert.ok(
      Math.abs(previous.centroidX - neutralPaw.centroidX) <= 4,
      `${setId} frame ${finalActive} paw centroid X must register within 4 px before frame ${swap} swap`,
    );
  }

  assert.ok(
    Math.abs(peakClearances.get("front-chain-left") - peakClearances.get("front-chain-right")) <= 4,
    `front paw peak clearances must read as the same gait: left ${peakClearances.get("front-chain-left")} px, `
      + `right ${peakClearances.get("front-chain-right")} px`,
  );
});

test("front-paw member handoffs stay within three adjacent-frame deltas", async () => {
  const { manifest, clip } = await productionWalk();
  const snapshot = await motionSnapshot(manifest, clip);
  const rendered = new Map();
  const frame = async (number) => {
    if (!rendered.has(number)) rendered.set(number, await renderRigFrameSnapshot(snapshot, number));
    return rendered.get(number);
  };

  for (const [setId, { region, finalActive, swap }] of Object.entries(FRONT_PAW_LANDINGS)) {
    const preceding = localizedCompositeDelta(
      await frame(finalActive - 1),
      await frame(finalActive),
      region,
    );
    const handoff = localizedCompositeDelta(
      await frame(finalActive),
      await frame(swap),
      region,
    );
    assert.ok(
      handoff.combinedRatio <= preceding.combinedRatio * 3,
      `${setId} member handoff is ${handoff.combinedRatio.toFixed(3)} versus `
        + `${preceding.combinedRatio.toFixed(3)} on the preceding descent step`,
    );
  }
});

test("localized composite acceptance rejects a hard raster swap at a selection boundary", async () => {
  const { manifest, clip } = await productionWalk();
  const mutatedManifest = structuredClone(manifest);
  const selected = mutatedManifest.variants["front-chain-left"].members.find(({ id }) => id === "low-lift");
  selected.file = manifest.variants["front-chain-right"].members.find(({ id }) => id === "low-lift").file;
  const mutatedSnapshot = await motionSnapshot(mutatedManifest, clip);

  await assert.rejects(
    () => assertLocalizedWalkContinuity(
      mutatedSnapshot,
      ["front-chain-left"],
    ),
    /front-chain-left (entry landing-to-lift switch exceeds its localized composite budget|early rise must visibly advance)/,
  );
});

test("localized composite acceptance rejects a missing landing interchange inside a smooth arc", async () => {
  const { manifest, clip } = await productionWalk();
  const mutatedClip = structuredClone(clip);
  mutatedClip.variants["front-chain-left"].find(({ frame }) => frame === 1).value = "neutral";
  for (const layerId of CAPS["front-chain-left"]) {
    mutatedClip.layerOpacity[layerId].find(({ frame }) => frame === 1).value = 0;
  }
  const mutatedSnapshot = await motionSnapshot(manifest, mutatedClip);

  await assert.rejects(
    () => assertLocalizedWalkContinuity(
      mutatedSnapshot,
      ["front-chain-left"],
    ),
    /front-chain-left entry omits its registered landing interchange/,
  );
});

test("localized composite acceptance rejects an abrupt in-memory transform step", async () => {
  const { manifest, clip } = await productionWalk();
  const mutatedClip = structuredClone(clip);
  const xTrack = mutatedClip.variantTransforms["front-chain-left"].tracks.x;
  const peakIndex = xTrack.findIndex(({ frame }) => frame === 12);
  xTrack.splice(peakIndex, 0, { frame: 11, value: 0.0045 });
  const peak = xTrack.find(({ frame }) => frame === 12);
  peak.value = 0.025;
  const mutatedSnapshot = await motionSnapshot(manifest, mutatedClip);

  await assert.rejects(
    () => assertLocalizedWalkContinuity(
      mutatedSnapshot,
      ["front-chain-left"],
    ),
    /front-chain-left (rise|fall) contains an abrupt localized composite step/,
  );
});

test("neutral chains stay registered while coherent lifted paintings appear only during swing", async () => {
  const { manifest, clip } = await productionWalk();
  const neutral = evaluateClip(manifest, clip, 0);
  const neutralWorld = worldTransforms(manifest, neutral.layers);

  for (let frame = 0; frame < clip.frameCount; frame += 1) {
    if (![1, 12, 23, 25, 36, 46].includes(frame)) continue;
    const evaluated = evaluateClip(manifest, clip, frame);
    const world = worldTransforms(manifest, evaluated.layers);
    for (const setId of PAW_SETS) {
      const controls = LIMB_CONTROLS[setId];
      const active = swingActive(evaluated, setId);
      assert.ok(controls.every((name) => evaluated.controls.get(name) === 0), `${setId} uses coherent source-locked art, not mixed joint rotation`);
      if (active === 0) {
        const anchor = CHAIN_ANCHORS[setId];
        assert.deepEqual(world.get(anchor), neutralWorld.get(anchor), `${setId} frame ${frame} must keep the stance baseline`);
      }
    }
  }
});

test("fixed proximal caps switch Boolean visibility with each discrete swing member without changing the neutral source", async () => {
  const { manifest, clip } = await productionWalk();
  for (let frame = 0; frame < clip.frameCount; frame += 1) {
    const evaluated = evaluateClip(manifest, clip, frame);
    for (const setId of PAW_SETS) {
      const active = swingActive(evaluated, setId);
      for (const layerId of CAPS[setId]) {
        assert.equal(evaluated.layers.get(layerId).opacity, active, `${layerId} must match ${setId} active state at frame ${frame}`);
      }
    }
  }
  for (const [frame, activeSets] of [[12, ["front-chain-left", "rear-chain-right"]], [36, ["front-chain-right", "rear-chain-left"]]]) {
    const evaluated = evaluateClip(manifest, clip, frame);
    for (const setId of activeSets) {
      const member = manifest.variants[setId].members.find(({ id }) => id === "low-lift");
      assert.equal(member.id, "low-lift", `${setId} must use one fully opaque swing painting`);
      assert.ok(Object.values(member.layerOverrides).every(({ visible }) => visible === false), `${setId} must hide every superseded descendant`);
      assert.equal(member.parentOverride, "torso", `${setId} lifted painting must inherit body transfer`);
      const socket = manifest.layers.find(({ id }) => id === SOCKETS[setId]);
      const anchor = manifest.layers.find(({ id }) => id === CHAIN_ANCHORS[setId]);
      assert.equal(socket.parent, "torso", `${socket.id} must remain fixed to the torso`);
      assert.equal(socket.visibleAtNeutral, false, `${socket.id} must not affect neutral reconstruction`);
      assert.ok(socket.drawOrder < anchor.drawOrder, `${socket.id} must underlay the moving chain root`);
    }
  }

  const [source, first, last] = await Promise.all([
    readFile(path.join(REPO_ROOT, "assets/brand/waffle/poses/standing.png")),
    renderRigFrame(RIG_PATH, CLIP_PATH, 0),
    renderRigFrame(RIG_PATH, CLIP_PATH, 47),
  ]);
  const sourcePixels = (await import("pngjs")).PNG.sync.read(source).data;
  assert.ok(last.data.equals(first.data), "frame 47 decoded RGBA must equal rendered frame 0 byte for byte");
  for (let offset = 0; offset < sourcePixels.length; offset += 4) {
    assert.equal(first.data[offset + 3], sourcePixels[offset + 3], `neutral alpha mismatch at pixel ${offset / 4}`);
    if (first.data[offset + 3] !== 0 || sourcePixels[offset + 3] !== 0) {
      assert.deepEqual(
        first.data.subarray(offset, offset + 4),
        sourcePixels.subarray(offset, offset + 4),
        `neutral visible RGBA mismatch at pixel ${offset / 4}`,
      );
    }
  }
});

test("proximal caps are bounded nonempty single components with no distal alpha", async () => {
  const [manifest, repairs] = await Promise.all([
    readFile(RIG_PATH, "utf8").then(JSON.parse),
    readFile(REPAIRS_PATH, "utf8").then(JSON.parse),
  ]);
  for (const socketId of Object.values(SOCKETS)) {
    const layer = manifest.layers.find(({ id }) => id === socketId);
    const specification = repairs.repairs.find(({ id }) => id === socketId);
    const image = PNG.sync.read(await readFile(path.join(path.dirname(RIG_PATH), layer.file)));
    const bounds = specification.input.outputBounds;
    const active = new Set();
    for (let y = 0; y < image.height; y += 1) {
      for (let x = 0; x < image.width; x += 1) {
        const offset = (y * image.width + x) * 4;
        if (image.data[offset + 3] === 0) continue;
        active.add(y * image.width + x);
        assert.ok(
          !(bounds.forbidden ?? []).some((region) => x >= region.x
            && x < region.x + region.width
            && y >= region.y
            && y < region.y + region.height),
          `${socketId} has distal alpha at ${x},${y}`,
        );
        assert.ok(image.data[offset] < 250 || image.data[offset + 2] < 250, `${socketId} retains keyed magenta at ${x},${y}`);
      }
    }
    assert.ok(active.size >= bounds.minNonzeroPixels, `${socketId} must contain a useful proximal cap`);
    const pending = [active.values().next().value];
    const visited = new Set();
    while (pending.length > 0) {
      const pixel = pending.pop();
      if (pixel === undefined || visited.has(pixel) || !active.has(pixel)) continue;
      visited.add(pixel);
      const x = pixel % image.width;
      const y = Math.floor(pixel / image.width);
      if (x > 0) pending.push(pixel - 1);
      if (x + 1 < image.width) pending.push(pixel + 1);
      if (y > 0) pending.push(pixel - image.width);
      if (y + 1 < image.height) pending.push(pixel + image.width);
    }
    assert.equal(visited.size, active.size, `${socketId} must be one connected proximal component`);
  }
});

test("active joint interiors stay fully covered while the limb moves under a fixed cap", async () => {
  const { manifest, clip } = await productionWalk();
  const source = PNG.sync.read(await readFile(path.join(REPO_ROOT, "assets/brand/waffle/poses/standing.png")));
  const rigDirectory = path.dirname(RIG_PATH);
  const rasterFiles = new Set([
    ...manifest.layers.map(({ file }) => file),
    ...Object.values(manifest.variants).flatMap(({ members }) => members.map(({ file }) => file)),
  ]);
  const rasters = new Map(await Promise.all([...rasterFiles].map(async (file) => [
    file,
    PNG.sync.read(await readFile(path.join(rigDirectory, file))),
  ])));
  const jointInteriors = {
    "front-chain-left": { x: 610, y: 535, width: 20, height: 55 },
    "front-chain-right": { x: 600, y: 540, width: 20, height: 55 },
    "rear-chain-left": { x: 895, y: 590, width: 30, height: 75 },
    "rear-chain-right": { x: 920, y: 590, width: 25, height: 75 },
  };
  for (let frame = 0; frame < clip.frameCount; frame += 1) {
    const evaluated = evaluateClip(manifest, clip, frame);
    const activeSets = PAW_SETS.filter((setId) => swingActive(evaluated, setId) > 0);
    if (activeSets.length === 0) continue;
    const rendered = await renderRigFrameSnapshot({ manifest, clip, rasters }, frame);
    for (const setId of activeSets) {
      const minimumCoveredAlpha = 255;
      const region = jointInteriors[setId];
      for (let y = region.y; y < region.y + region.height; y += 1) {
        for (let x = region.x; x < region.x + region.width; x += 1) {
          const alphaOffset = (y * source.width + x) * 4 + 3;
          if (source.data[alphaOffset] === 255) {
            assert.ok(
              rendered.data[alphaOffset] >= minimumCoveredAlpha,
              `${setId} frame ${frame} opens a joint hole at ${x},${y}: ${rendered.data[alphaOffset]} < ${minimumCoveredAlpha}`,
            );
          }
        }
      }
    }
  }
});

test("body stays source-locked so planted limbs cannot open transform seams", async () => {
  const { manifest, clip } = await productionWalk();
  const samples = Array.from({ length: clip.frameCount }, (_, frame) => evaluateClip(manifest, clip, frame).controls);
  const values = (name) => samples.map((controls) => controls.get(name));
  const extrema = (name) => [Math.min(...values(name)), Math.max(...values(name))];

  assert.deepEqual(extrema("bodyBob"), [0, 0]);
  assert.deepEqual(extrema("bodyLean"), [0, 0]);
  assert.deepEqual(extrema("weightShift"), [0, 0]);
});

test("every frame evaluates complete in-range states and frame 47 equals frame 0 exactly", async () => {
  const { manifest, clip } = await productionWalk();
  assert.doesNotThrow(() => assertLoopClosure(manifest, clip));

  for (let frame = 0; frame < clip.frameCount; frame += 1) {
    const evaluated = evaluateClip(manifest, clip, frame);
    assert.equal(evaluated.layers.size, manifest.layers.length, `frame ${frame} layer inventory`);
    assert.equal(evaluated.variants.size, Object.keys(manifest.variants).length, `frame ${frame} variant inventory`);
    assert.equal(evaluated.variantTransforms.size, Object.keys(clip.variantTransforms).length, `frame ${frame} variant transform inventory`);
    assert.equal(evaluated.controls.size, Object.keys(manifest.controls).length, `frame ${frame} control inventory`);
    for (const [name, value] of evaluated.controls) {
      const range = manifest.controls[name];
      assert.ok(Number.isFinite(value) && value >= range.min && value <= range.max, `frame ${frame} ${name} range`);
    }
    for (const matrix of worldTransforms(manifest, evaluated.layers).values()) {
      assert.ok(matrix.every(Number.isFinite), `frame ${frame} world matrices must stay finite`);
    }
  }

  const first = evaluateClip(manifest, clip, 0);
  const last = evaluateClip(manifest, clip, 47);
  assert.deepEqual(mapObject(last.controls), mapObject(first.controls));
  assert.deepEqual(mapObject(last.variants), mapObject(first.variants));
  assert.deepEqual(mapObject(last.variantTransforms), mapObject(first.variantTransforms));
  assert.deepEqual(mapObject(worldTransforms(manifest, last.layers)), mapObject(worldTransforms(manifest, first.layers)));
  for (const control of Object.values(LIMB_CONTROLS).flat()) {
    assert.equal(first.controls.get(control), 0, `${control} must close at neutral`);
  }
  for (const setId of PAW_SETS) assert.equal(swingMember(first, setId), "neutral");
});
