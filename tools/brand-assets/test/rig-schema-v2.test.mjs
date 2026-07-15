import assert from "node:assert/strict";
import { test } from "node:test";

import {
  validateMotionClipShape,
  validateRigV2Shape,
  variantForLayer,
} from "../rig-schema-v2.mjs";

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
  unknownProperty.controls.bodyBob.bindings[0].property = "opacity";
  assert.throws(() => validateRigV2Shape(unknownProperty), /unsupported property opacity/);

  const unknownVariant = validManifest();
  unknownVariant.controls.pawState = {
    min: 0,
    max: 1,
    bindings: [{ variant: "missing" }],
  };
  assert.throws(() => validateRigV2Shape(unknownVariant), /unknown variant set missing/);
});

test("requires every variant anchor to have exactly one neutral variant", () => {
  const missing = validManifest();
  delete missing.variants["front-paw-left"];
  assert.throws(() => validateRigV2Shape(missing), /variant-anchor front-paw-left requires a variant set/);

  const twoNeutral = validManifest();
  twoNeutral.variants["front-paw-left"].members[1].neutral = true;
  assert.throws(() => validateRigV2Shape(twoNeutral), /must have exactly one neutral member/);
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
