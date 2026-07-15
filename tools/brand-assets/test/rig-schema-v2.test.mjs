import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import {
  validateMotionClipShape,
  validateRigV2Shape,
  variantForLayer,
} from "../rig-schema-v2.mjs";

const REPOSITORY_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../..");

const PRODUCTION_HIERARCHY = {
  "rear-hip-repair-left": "waffle-root",
  "rear-hock-repair-left": "rear-thigh-left",
  "rear-paw-root-repair-left": "rear-hock-left",
  "rear-paw-left": "rear-hock-left",
  "rear-hock-left": "rear-thigh-left",
  "rear-thigh-left": "waffle-root",
  "rear-hip-repair-right": "waffle-root",
  "rear-hock-repair-right": "rear-thigh-right",
  "rear-paw-root-repair-right": "rear-hock-right",
  "rear-paw-right": "rear-hock-right",
  "rear-hock-right": "rear-thigh-right",
  "rear-thigh-right": "waffle-root",
  "tail-base-mid-repair": "tail-base",
  "tail-mid-tip-repair": "tail-mid",
  "tail-tip": "tail-mid",
  "tail-mid": "tail-base",
  "tail-base": "waffle-root",
  "body-repair": "waffle-root",
  torso: "waffle-root",
  "front-shoulder-repair-left": "waffle-root",
  "front-elbow-repair-left": "front-upper-left",
  "front-wrist-repair-left": "front-lower-left",
  "front-upper-left": "waffle-root",
  "front-lower-left": "front-upper-left",
  "front-paw-left": "front-lower-left",
  "front-shoulder-repair-right": "waffle-root",
  "front-elbow-repair-right": "front-upper-right",
  "front-wrist-repair-right": "front-lower-right",
  "front-upper-right": "waffle-root",
  "front-lower-right": "front-upper-right",
  "front-paw-right": "front-lower-right",
  "ear-left": "head-base",
  "ear-right": "head-base",
  "neck-repair": "torso",
  "head-base": "waffle-root",
  muzzle: "head-base",
  "jaw-closed": "head-base",
  "iris-left": "head-base",
  "iris-right": "head-base",
  "pupil-left": "iris-left",
  "pupil-right": "iris-right",
  "highlight-left": "pupil-left",
  "highlight-right": "pupil-right",
  "upper-lid-left": "head-base",
  "lower-lid-left": "head-base",
  "upper-lid-right": "head-base",
  "lower-lid-right": "head-base",
  whiskers: "head-base",
};

const PRODUCTION_VARIANTS = {
  "front-paw-left": { layer: "front-paw-left", members: ["planted", "lifted", "wave"], neutral: "planted" },
  "front-paw-right": { layer: "front-paw-right", members: ["planted", "lifted"], neutral: "planted" },
  "rear-paw-left": { layer: "rear-paw-left", members: ["planted", "lifted"], neutral: "planted" },
  "rear-paw-right": { layer: "rear-paw-right", members: ["planted", "lifted"], neutral: "planted" },
  "head-base": { layer: "head-base", members: ["neutral", "turn-left", "turn-right"], neutral: "neutral" },
  jaw: { layer: "jaw-closed", members: ["closed", "open"], neutral: "closed" },
};

const PRODUCTION_CONTROLS = {
  breath: { range: [0, 1], bindings: [["torso", "scaleX", 0.004], ["torso", "scaleY", 0.008]] },
  bodyBob: { range: [-0.015, 0.015], bindings: [["torso", "y", 1]] },
  bodyLean: { range: [-3, 3], bindings: [["torso", "rotationDegrees", 1]] },
  weightShift: { range: [-1, 1], bindings: [["torso", "x", 0.008]] },
  headTilt: { range: [-5, 5], bindings: [["head-base", "rotationDegrees", 1]] },
  headTurn: {
    range: [-1, 1],
    variants: [{
      variant: "head-base",
      thresholds: [
        { max: -0.5, member: "turn-left" },
        { max: 0.5, member: "neutral" },
        { max: 1, member: "turn-right" },
      ],
    }],
  },
  gazeX: { range: [-0.012, 0.012], bindings: [["pupil-left", "x", 1], ["pupil-right", "x", 1]] },
  gazeY: { range: [-0.009, 0.009], bindings: [["pupil-left", "y", 1], ["pupil-right", "y", 1]] },
  blinkLeft: { range: [0, 1], bindings: [["upper-lid-left", "opacity", 1], ["lower-lid-left", "opacity", 1]] },
  blinkRight: { range: [0, 1], bindings: [["upper-lid-right", "opacity", 1], ["lower-lid-right", "opacity", 1]] },
  earLeft: { range: [-6, 6], bindings: [["ear-left", "rotationDegrees", 1]] },
  earRight: { range: [-6, 6], bindings: [["ear-right", "rotationDegrees", 1]] },
  jawOpen: {
    range: [0, 1],
    variants: [{
      variant: "jaw",
      thresholds: [{ max: 0.5, member: "closed" }, { max: 1, member: "open" }],
    }],
  },
  tailBase: { range: [-8, 8], bindings: [["tail-base", "rotationDegrees", 1]] },
  tailMid: { range: [-12, 12], bindings: [["tail-mid", "rotationDegrees", 1]] },
  tailTip: { range: [-15, 15], bindings: [["tail-tip", "rotationDegrees", 1]] },
  rearThighLeft: { range: [-10, 10], bindings: [["rear-thigh-left", "rotationDegrees", 1]] },
  rearHockLeft: { range: [-12, 12], bindings: [["rear-hock-left", "rotationDegrees", 1]] },
  rearPawLeft: { range: [-6, 6], bindings: [["rear-paw-left", "rotationDegrees", 1]] },
  rearThighRight: { range: [-10, 10], bindings: [["rear-thigh-right", "rotationDegrees", 1]] },
  rearHockRight: { range: [-12, 12], bindings: [["rear-hock-right", "rotationDegrees", 1]] },
  rearPawRight: { range: [-6, 6], bindings: [["rear-paw-right", "rotationDegrees", 1]] },
  frontUpperLeft: { range: [-12, 12], bindings: [["front-upper-left", "rotationDegrees", 1]] },
  frontLowerLeft: { range: [-14, 14], bindings: [["front-lower-left", "rotationDegrees", 1]] },
  frontPawLeft: { range: [-8, 8], bindings: [["front-paw-left", "rotationDegrees", 1]] },
  frontUpperRight: { range: [-12, 12], bindings: [["front-upper-right", "rotationDegrees", 1]] },
  frontLowerRight: { range: [-14, 14], bindings: [["front-lower-right", "rotationDegrees", 1]] },
  frontPawRight: { range: [-8, 8], bindings: [["front-paw-right", "rotationDegrees", 1]] },
};

function validManifest() {
  return {
    schemaVersion: 2,
    canvas: { width: 1536, height: 1024 },
    root: { id: "waffle-root", pivot: { x: 0.52, y: 0.76 } },
    source: { file: "../../../poses/standing.png", sha256: "0".repeat(64) },
    neutralReference: { file: "neutral-reference.png", sha256: "0".repeat(64) },
    layers: [
      {
        id: "torso",
        file: "layers/torso.png",
        role: "visible",
        parent: "waffle-root",
        drawOrder: 20,
        visibleAtNeutral: true,
        blendMode: "normal",
        pivot: { x: 0.52, y: 0.62 },
        neutral: { x: 0, y: 0, rotationDegrees: 0, scaleX: 1, scaleY: 1 },
        limits: {
          x: { min: -0.01, max: 0.01 },
          y: { min: -0.015, max: 0.015 },
          rotationDegrees: { min: -3, max: 3 },
        },
        sha256: "0".repeat(64),
      },
      {
        id: "front-paw-left",
        file: "layers/front-paw-left.png",
        role: "variant-anchor",
        parent: "torso",
        drawOrder: 30,
        visibleAtNeutral: true,
        blendMode: "normal",
        pivot: { x: 0.35, y: 0.82 },
        neutral: { x: 0, y: 0, rotationDegrees: 0, scaleX: 1, scaleY: 1 },
        limits: { rotationDegrees: { min: -8, max: 8 } },
        sha256: "0".repeat(64),
      },
    ],
    variants: {
      "front-paw-left": {
        layer: "front-paw-left",
        members: [
          { id: "planted", file: "variants/front-paw-left/planted.png", neutral: true, sha256: "0".repeat(64) },
          { id: "lifted", file: "variants/front-paw-left/lifted.png", neutral: false, sha256: "0".repeat(64) },
          { id: "wave", file: "variants/front-paw-left/wave.png", neutral: false, sha256: "0".repeat(64) },
        ],
      },
    },
    controls: {
      bodyBob: {
        min: -0.015,
        max: 0.015,
        bindings: [{ layer: "torso", property: "y", factor: 1 }],
      },
    },
  };
}

test("accepts the standing rig v2 manifest contract", () => {
  assert.doesNotThrow(() => validateRigV2Shape(validManifest()));
});

test("rejects duplicate layer IDs and draw orders", () => {
  const duplicateId = validManifest();
  duplicateId.layers[1].id = "torso";
  assert.throws(() => validateRigV2Shape(duplicateId), /duplicate layer id: torso/);

  const duplicateOrder = validManifest();
  duplicateOrder.layers[1].drawOrder = 20;
  assert.throws(() => validateRigV2Shape(duplicateOrder), /duplicate drawOrder: 20/);
});

test("rejects a raster layer that collides with the synthetic root ID", () => {
  const manifest = validManifest();
  manifest.layers[0].id = "waffle-root";
  manifest.layers[1].parent = "waffle-root";
  manifest.controls.bodyBob.bindings[0].layer = "waffle-root";

  assert.throws(() => validateRigV2Shape(manifest), /layer id waffle-root collides with synthetic root/);
});

test("rejects missing parents and cycles below the synthetic root", () => {
  const missingParent = validManifest();
  missingParent.layers[1].parent = "missing";
  assert.throws(() => validateRigV2Shape(missingParent), /unknown parent missing/);

  const cycle = validManifest();
  cycle.layers[0].parent = "front-paw-left";
  assert.throws(() => validateRigV2Shape(cycle), /layer graph contains a cycle/);
});

test("rejects unknown control binding targets and properties", () => {
  const unknownLayer = validManifest();
  unknownLayer.controls.bodyBob.bindings[0].layer = "missing";
  assert.throws(() => validateRigV2Shape(unknownLayer), /unknown layer missing/);

  const unknownProperty = validManifest();
  unknownProperty.controls.bodyBob.bindings[0].property = "visibility";
  assert.throws(() => validateRigV2Shape(unknownProperty), /unsupported property visibility/);

  const unknownVariant = validManifest();
  unknownVariant.controls.pawState = {
    min: 0,
    max: 1,
    bindings: [{ variant: "missing" }],
  };
  assert.throws(() => validateRigV2Shape(unknownVariant), /unknown variant set missing/);
});

test("requires control bindings to declare exactly one discriminated binding kind", async () => {
  const manifestPath = path.join(REPOSITORY_ROOT, "assets/brand/waffle/rigs/standing-v2/rig.json");
  const production = JSON.parse(await readFile(manifestPath, "utf8"));
  Object.assign(production.controls.headTurn.bindings[0], {
    layer: "head-base",
    property: "rotationDegrees",
    factor: 1,
  });
  assert.throws(
    () => validateRigV2Shape(production),
    /control headTurn binding 1 must declare exactly one of layer or variant/,
    "a production headTurn-shaped hybrid must not be accepted as a layer binding",
  );

  const neither = validManifest();
  neither.controls.bodyBob.bindings[0] = { property: "y", factor: 1 };
  assert.throws(
    () => validateRigV2Shape(neither),
    /control bodyBob binding 1 must declare exactly one of layer or variant/,
  );
});

test("rejects binding-kind fields and unknown keys on the other control binding shape", () => {
  for (const [label, mutate, message] of [
    ["layer thresholds", (binding) => { binding.thresholds = []; }, /layer binding has unsupported field thresholds/],
    ["layer states", (binding) => { binding.states = {}; }, /layer binding has unsupported field states/],
    ["layer unknown", (binding) => { binding.note = "drift"; }, /layer binding has unsupported field note/],
  ]) {
    const manifest = validManifest();
    mutate(manifest.controls.bodyBob.bindings[0]);
    assert.throws(() => validateRigV2Shape(manifest), message, label);
  }

  for (const [label, mutate, message] of [
    ["variant property", (binding) => { binding.property = "x"; }, /variant binding has unsupported field property/],
    ["variant factor", (binding) => { binding.factor = 1; }, /variant binding has unsupported field factor/],
    ["variant states", (binding) => { binding.states = {}; }, /variant binding has unsupported field states/],
    ["variant unknown", (binding) => { binding.note = "drift"; }, /variant binding has unsupported field note/],
  ]) {
    const manifest = validManifest();
    manifest.controls.pawState = {
      min: -1,
      max: 1,
      bindings: [{
        variant: "front-paw-left",
        thresholds: [
          { max: -0.5, member: "lifted" },
          { max: 0.5, member: "planted" },
          { max: 1, member: "wave" },
        ],
      }],
    };
    mutate(manifest.controls.pawState.bindings[0]);
    assert.throws(() => validateRigV2Shape(manifest), message, label);
  }
});

test("validates opacity bindings against the layer neutral opacity and control range", () => {
  const valid = validManifest();
  valid.layers.push({
    ...structuredClone(valid.layers[0]),
    id: "upper-lid-left",
    file: "layers/upper-lid-left.png",
    parent: "front-paw-left",
    drawOrder: 40,
    visibleAtNeutral: false,
    limits: {},
  });
  valid.controls.blinkLeft = {
    min: 0,
    max: 1,
    bindings: [{ layer: "upper-lid-left", property: "opacity", factor: 1 }],
  };
  assert.doesNotThrow(() => validateRigV2Shape(valid));

  const overflow = structuredClone(valid);
  overflow.controls.blinkLeft.bindings[0].factor = 1.01;
  assert.throws(() => validateRigV2Shape(overflow), /opacity must remain inside 0\.\.1/);

  const visibleOverflow = structuredClone(valid);
  visibleOverflow.controls.blinkLeft.bindings[0].layer = "torso";
  assert.throws(() => validateRigV2Shape(visibleOverflow), /opacity must remain inside 0\.\.1/);
});

test("requires deterministic complete numeric variant thresholds", () => {
  const valid = validManifest();
  valid.controls.pawState = {
    min: -1,
    max: 1,
    bindings: [{
      variant: "front-paw-left",
      thresholds: [
        { max: -0.5, member: "lifted" },
        { max: 0.5, member: "planted" },
        { max: 1, member: "wave" },
      ],
    }],
  };
  assert.doesNotThrow(() => validateRigV2Shape(valid));

  for (const [label, mutate, message] of [
    ["null map", (manifest) => { manifest.controls.pawState.bindings[0].thresholds = null; }, /thresholds must be a non-empty array/],
    ["unknown member", (manifest) => { manifest.controls.pawState.bindings[0].thresholds[0].member = "missing"; }, /unknown member missing/],
    ["missing neutral", (manifest) => { manifest.controls.pawState.bindings[0].thresholds[1].member = "lifted"; }, /must map the neutral member planted at value 0/],
    ["out of range", (manifest) => { manifest.controls.pawState.bindings[0].thresholds[0].max = -2; }, /threshold must be inside -1\.\.1/],
    ["ambiguous", (manifest) => { manifest.controls.pawState.bindings[0].thresholds[1].max = -0.5; }, /thresholds must be strictly increasing/],
    ["incomplete", (manifest) => { manifest.controls.pawState.bindings[0].thresholds.at(-1).max = 0.9; }, /last threshold must equal control max 1/],
  ]) {
    const invalid = structuredClone(valid);
    mutate(invalid);
    assert.throws(() => validateRigV2Shape(invalid), message, label);
  }
});

test("requires every variant anchor to have exactly one neutral variant", () => {
  const missing = validManifest();
  delete missing.variants["front-paw-left"];
  assert.throws(() => validateRigV2Shape(missing), /variant-anchor front-paw-left requires a variant set/);

  const twoNeutral = validManifest();
  twoNeutral.variants["front-paw-left"].members[1].neutral = true;
  assert.throws(() => validateRigV2Shape(twoNeutral), /must have exactly one neutral member/);
});

test("validates member visibility overrides for descendants of a variant anchor", () => {
  const valid = validManifest();
  valid.layers.push({
    ...structuredClone(valid.layers[0]),
    id: "paw-detail",
    file: "layers/paw-detail.png",
    parent: "front-paw-left",
    drawOrder: 40,
  });
  valid.variants["front-paw-left"].members[1].layerOverrides = {
    "paw-detail": { visible: false },
  };
  assert.doesNotThrow(() => validateRigV2Shape(valid));

  const neutralOverride = structuredClone(valid);
  neutralOverride.variants["front-paw-left"].members[0].layerOverrides = {
    "paw-detail": { visible: false },
  };
  assert.throws(() => validateRigV2Shape(neutralOverride), /neutral member cannot declare layer overrides/);

  const unknown = structuredClone(valid);
  unknown.variants["front-paw-left"].members[1].layerOverrides = {
    missing: { visible: false },
  };
  assert.throws(() => validateRigV2Shape(unknown), /override references unknown layer missing/);

  const outsideSubtree = structuredClone(valid);
  outsideSubtree.variants["front-paw-left"].members[1].layerOverrides = {
    torso: { visible: false },
  };
  assert.throws(() => validateRigV2Shape(outsideSubtree), /override layer torso must descend from front-paw-left/);

  const visible = structuredClone(valid);
  visible.variants["front-paw-left"].members[1].layerOverrides["paw-detail"].visible = true;
  assert.throws(() => validateRigV2Shape(visible), /visible must be false/);
});

test("rejects invalid per-layer limits", () => {
  const reversed = validManifest();
  reversed.layers[0].limits.y = { min: 0.01, max: 0.01 };
  assert.throws(() => validateRigV2Shape(reversed), /layer torso limit y must have finite min < max/);

  const unsupported = validManifest();
  unsupported.layers[0].limits.opacity = { min: 0, max: 1 };
  assert.throws(() => validateRigV2Shape(unsupported), /layer torso has unsupported limit opacity/);
});

test("rejects unsafe layer and variant paths", () => {
  const layerEscape = validManifest();
  layerEscape.layers[0].file = "../torso.png";
  assert.throws(() => validateRigV2Shape(layerEscape), /layer torso file must stay inside the rig directory/);

  const variantEscape = validManifest();
  variantEscape.variants["front-paw-left"].members[0].file = "/tmp/planted.png";
  assert.throws(() => validateRigV2Shape(variantEscape), /variant front-paw-left\/planted file must be a local relative path/);
});

test("requires production canvas dimensions", () => {
  const manifest = validManifest();
  manifest.canvas.width = 1535;
  assert.throws(() => validateRigV2Shape(manifest), /canvas must be exactly 1536x1024/);
});

test("resolves requested and neutral variants for an anchor layer", () => {
  const manifest = validManifest();
  assert.equal(variantForLayer(manifest, "front-paw-left").id, "planted");
  assert.equal(variantForLayer(manifest, "front-paw-left", "wave").id, "wave");
  assert.throws(() => variantForLayer(manifest, "front-paw-left", "missing"), /unknown variant member missing/);
});

test("validates the motion clip shape against controls and variants", () => {
  const manifest = validManifest();
  const clip = {
    schemaVersion: 1,
    id: "walk-in-place",
    fps: 24,
    frameCount: 48,
    loop: true,
    requiredClosure: { firstFrame: 0, lastFrame: 47 },
    variants: {
      "front-paw-left": [
        { frame: 0, value: "planted", interpolation: "hold" },
        { frame: 12, value: "lifted", interpolation: "hold" },
      ],
    },
    controls: {
      bodyBob: [
        { frame: 0, value: 0 },
        { frame: 12, value: -0.008 },
        { frame: 47, value: 0 },
      ],
    },
  };

  assert.doesNotThrow(() => validateMotionClipShape(manifest, clip));

  clip.controls.bodyBob[1].value = -0.02;
  assert.throws(() => validateMotionClipShape(manifest, clip), /control bodyBob value -0.02 is outside -0.015..0.015/);
});

test("production standing rig v2 exposes the complete hierarchy, variants, controls, and joint limits", async () => {
  const manifestPath = path.join(REPOSITORY_ROOT, "assets/brand/waffle/rigs/standing-v2/rig.json");
  const masksPath = path.join(REPOSITORY_ROOT, "assets/brand/waffle/rigs/standing-v2/masks.json");
  const [manifest, masks] = await Promise.all([
    readFile(manifestPath, "utf8").then(JSON.parse),
    readFile(masksPath, "utf8").then(JSON.parse),
  ]);
  assert.doesNotThrow(() => validateRigV2Shape(manifest));
  assert.deepEqual(masks.controls, manifest.controls, "authoritative masks controls must reproduce the final manifest");

  assert.deepEqual(
    Object.fromEntries(manifest.layers.map((layer) => [layer.id, layer.parent])),
    PRODUCTION_HIERARCHY,
    "the production layer IDs and parent hierarchy are a locked contract",
  );

  assert.deepEqual(Object.keys(manifest.variants).toSorted(), Object.keys(PRODUCTION_VARIANTS).toSorted());
  for (const [setId, expected] of Object.entries(PRODUCTION_VARIANTS)) {
    const actual = manifest.variants[setId];
    assert.equal(actual.layer, expected.layer, `${setId} must remain registered to its anchor layer`);
    assert.deepEqual(actual.members.map((member) => member.id), expected.members, `${setId} members drifted`);
    assert.equal(actual.members.find((member) => member.neutral)?.id, expected.neutral, `${setId} neutral member drifted`);
  }

  assert.deepEqual(Object.keys(manifest.controls).toSorted(), Object.keys(PRODUCTION_CONTROLS).toSorted());
  for (const [name, expected] of Object.entries(PRODUCTION_CONTROLS)) {
    const actual = manifest.controls[name];
    assert.deepEqual([actual.min, actual.max], expected.range, `${name} range drifted`);
    const layerBindings = actual.bindings
      .filter((binding) => "layer" in binding)
      .map((binding) => [binding.layer, binding.property, binding.factor]);
    const variantBindings = actual.bindings
      .filter((binding) => "variant" in binding)
      .map((binding) => ({ variant: binding.variant, thresholds: binding.thresholds }));
    assert.deepEqual(layerBindings, expected.bindings ?? [], `${name} layer bindings drifted`);
    assert.deepEqual(variantBindings, expected.variants ?? [], `${name} variant bindings drifted`);
  }

  const jointLimits = {
    "front-upper-left": [-12, 12],
    "front-lower-left": [-14, 14],
    "front-paw-left": [-8, 8],
    "front-upper-right": [-12, 12],
    "front-lower-right": [-14, 14],
    "front-paw-right": [-8, 8],
    "rear-thigh-left": [-10, 10],
    "rear-hock-left": [-12, 12],
    "rear-paw-left": [-6, 6],
    "rear-thigh-right": [-10, 10],
    "rear-hock-right": [-12, 12],
    "rear-paw-right": [-6, 6],
  };
  const byId = new Map(manifest.layers.map((layer) => [layer.id, layer]));
  for (const [layerId, range] of Object.entries(jointLimits)) {
    const actual = byId.get(layerId)?.limits?.rotationDegrees;
    assert.deepEqual([actual?.min, actual?.max], range, `${layerId} anatomical limit drifted`);
  }
});
