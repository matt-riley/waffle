import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { test } from "node:test";

import { PNG } from "pngjs";

import {
  assertLoopClosure,
  evaluateClip,
  interpolateKeyframes,
  renderRigFrame,
  worldTransforms,
} from "../rig-motion.mjs";

const IDENTITY = [1, 0, 0, 1, 0, 0];

function rgba(width, height, pixels = []) {
  const png = new PNG({ width, height });
  png.data.set(pixels);
  return png;
}

function paint(png, x, y, color) {
  png.data.set(color, (y * png.width + x) * 4);
}

async function writePng(file, png) {
  await mkdir(path.dirname(file), { recursive: true });
  await writeFile(file, PNG.sync.write(png));
}

async function sha256(file) {
  return createHash("sha256").update(await readFile(file)).digest("hex");
}

function walkClip() {
  return {
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
        { frame: 24, value: "planted", interpolation: "hold" },
      ],
    },
    controls: {
      bodyBob: [
        { frame: 0, value: 0 },
        { frame: 12, value: -0.008 },
        { frame: 24, value: 0 },
        { frame: 36, value: -0.008 },
        { frame: 47, value: 0 },
      ],
    },
  };
}

async function rigFixture(t) {
  const directory = await mkdtemp(path.join(tmpdir(), "waffle-rig-motion-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const layersDirectory = path.join(directory, "layers");
  const variantsDirectory = path.join(directory, "variants", "front-paw-left");
  await Promise.all([
    mkdir(layersDirectory, { recursive: true }),
    mkdir(variantsDirectory, { recursive: true }),
  ]);

  const blank = () => rgba(1536, 1024);
  const source = blank();
  const reference = blank();
  const body = blank();
  const pawAnchor = blank();
  const planted = blank();
  const lifted = blank();
  const foreground = blank();
  paint(body, 100, 100, [220, 60, 40, 255]);
  paint(planted, 200, 200, [240, 170, 50, 255]);
  paint(lifted, 200, 160, [240, 170, 50, 255]);
  paint(foreground, 100, 100, [25, 80, 180, 255]);
  paint(source, 100, 100, [25, 80, 180, 255]);
  paint(source, 200, 200, [240, 170, 50, 255]);
  reference.data.set(source.data);
  pawAnchor.data.set(planted.data);

  const files = {
    source: path.join(directory, "source.png"),
    reference: path.join(directory, "neutral-reference.png"),
    body: path.join(layersDirectory, "body.png"),
    pawAnchor: path.join(layersDirectory, "front-paw-left.png"),
    planted: path.join(variantsDirectory, "planted.png"),
    lifted: path.join(variantsDirectory, "lifted.png"),
    foreground: path.join(layersDirectory, "foreground.png"),
  };
  await Promise.all([
    writePng(files.source, source),
    writePng(files.reference, reference),
    writePng(files.body, body),
    writePng(files.pawAnchor, pawAnchor),
    writePng(files.planted, planted),
    writePng(files.lifted, lifted),
    writePng(files.foreground, foreground),
  ]);

  const manifest = {
    schemaVersion: 2,
    canvas: { width: 1536, height: 1024 },
    root: { id: "waffle-root", pivot: { x: 0.5, y: 0.5 } },
    source: { file: "source.png", sha256: await sha256(files.source) },
    neutralReference: { file: "neutral-reference.png", sha256: await sha256(files.reference) },
    layers: [
      {
        id: "foreground",
        file: "layers/foreground.png",
        role: "overlay",
        parent: "waffle-root",
        drawOrder: 30,
        visibleAtNeutral: true,
        blendMode: "normal",
        pivot: { x: 0.5, y: 0.5 },
        neutral: { x: 0, y: 0, rotationDegrees: 0, scaleX: 1, scaleY: 1 },
        limits: {},
        sha256: await sha256(files.foreground),
      },
      {
        id: "body",
        file: "layers/body.png",
        role: "visible",
        parent: "waffle-root",
        drawOrder: 10,
        visibleAtNeutral: true,
        blendMode: "normal",
        pivot: { x: 0.25, y: 0.5 },
        neutral: { x: 0, y: 0, rotationDegrees: 0, scaleX: 1, scaleY: 1 },
        limits: {
          x: { min: -0.5, max: 0.5 },
          y: { min: -0.015, max: 0.015 },
          rotationDegrees: { min: -180, max: 180 },
        },
        sha256: await sha256(files.body),
      },
      {
        id: "front-paw-left",
        file: "layers/front-paw-left.png",
        role: "variant-anchor",
        parent: "body",
        drawOrder: 20,
        visibleAtNeutral: true,
        blendMode: "normal",
        pivot: { x: 0.125, y: 0.75 },
        neutral: { x: 0, y: 0, rotationDegrees: 0, scaleX: 1, scaleY: 1 },
        limits: { x: { min: -0.5, max: 0.5 } },
        sha256: await sha256(files.pawAnchor),
      },
    ],
    variants: {
      "front-paw-left": {
        layer: "front-paw-left",
        members: [
          { id: "planted", file: "variants/front-paw-left/planted.png", neutral: true, sha256: await sha256(files.planted) },
          { id: "lifted", file: "variants/front-paw-left/lifted.png", neutral: false, sha256: await sha256(files.lifted) },
        ],
      },
    },
    controls: {
      bodyBob: {
        min: -0.015,
        max: 0.015,
        bindings: [{ layer: "body", property: "y", factor: 1 }],
      },
    },
  };
  const manifestPath = path.join(directory, "rig.json");
  const clipPath = path.join(directory, "walk-in-place.json");
  const clip = walkClip();
  await Promise.all([
    writeFile(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`),
    writeFile(clipPath, `${JSON.stringify(clip, null, 2)}\n`),
  ]);
  return { clip, clipPath, manifest, manifestPath };
}

function point(matrix, x, y) {
  return {
    x: matrix[0] * x + matrix[2] * y + matrix[4],
    y: matrix[1] * x + matrix[3] * y + matrix[5],
  };
}

function assertPointClose(actual, expected) {
  assert.ok(Math.abs(actual.x - expected.x) < 1e-10, `expected x=${expected.x}, got ${actual.x}`);
  assert.ok(Math.abs(actual.y - expected.y) < 1e-10, `expected y=${expected.y}, got ${actual.y}`);
}

test("worldTransforms preserves identity and converts normalized translation to canvas pixels", async (t) => {
  const { manifest } = await rigFixture(t);
  const identity = new Map(manifest.layers.map((layer) => [layer.id, { ...layer.neutral }]));
  assert.deepEqual(worldTransforms(manifest, identity).get("body"), IDENTITY);

  identity.get("body").x = 0.25;
  identity.get("body").y = -0.5;
  assert.deepEqual(worldTransforms(manifest, identity).get("body"), [1, 0, 0, 1, 384, -512]);
});

test("worldTransforms rotates around the registered pivot and propagates parent motion", async (t) => {
  const { manifest } = await rigFixture(t);
  const transforms = new Map(manifest.layers.map((layer) => [layer.id, { ...layer.neutral }]));
  transforms.get("body").rotationDegrees = 90;
  transforms.get("front-paw-left").x = 0.125;

  const world = worldTransforms(manifest, transforms);
  const pivot = { x: 0.25 * (1536 - 1), y: 0.5 * (1024 - 1) };
  assertPointClose(point(world.get("body"), pivot.x, pivot.y), pivot);
  assertPointClose(point(world.get("body"), pivot.x + 1, pivot.y), { x: pivot.x, y: pivot.y + 1 });
  assertPointClose(point(world.get("front-paw-left"), 0, 0), point(world.get("body"), 192, 0));
});

test("interpolateKeyframes resolves linear, held, and exact keyframe values", () => {
  const linear = [{ frame: 0, value: 0 }, { frame: 10, value: 1 }, { frame: 20, value: 0 }];
  assert.equal(interpolateKeyframes(linear, 5), 0.5);
  assert.equal(interpolateKeyframes(linear, 10), 1);
  assert.equal(interpolateKeyframes([
    { frame: 0, value: 2, interpolation: "hold" },
    { frame: 10, value: 5 },
  ], 9), 2);
  assert.equal(interpolateKeyframes([
    { frame: 0, value: "planted", interpolation: "hold" },
    { frame: 10, value: "lifted", interpolation: "hold" },
  ], 9), "planted");
});

test("interpolateKeyframes rejects extrapolation, NaN, duplicate, and decreasing keys", () => {
  assert.throws(() => interpolateKeyframes([{ frame: 1, value: 0 }], 0), /outside keyframe range/);
  assert.throws(() => interpolateKeyframes([{ frame: 0, value: 0 }], 1), /outside keyframe range/);
  assert.throws(() => interpolateKeyframes([{ frame: 0, value: Number.NaN }], 0), /finite/);
  assert.throws(() => interpolateKeyframes([
    { frame: 0, value: 0 },
    { frame: 0, value: 1 },
  ], 0), /strictly increasing/);
  assert.throws(() => interpolateKeyframes([
    { frame: 1, value: 0 },
    { frame: 0, value: 1 },
  ], 0), /strictly increasing/);
});

test("evaluateClip returns deterministic controls, variants, and bound local transforms", async (t) => {
  const { clip, manifest } = await rigFixture(t);
  const between = evaluateClip(manifest, clip, 6);
  assert.equal(between.controls.get("bodyBob"), -0.004);
  assert.equal(between.layers.get("body").y, -0.004);
  assert.equal(between.variants.get("front-paw-left"), "planted");

  const exact = evaluateClip(manifest, clip, 12);
  assert.equal(exact.controls.get("bodyBob"), -0.008);
  assert.equal(exact.variants.get("front-paw-left"), "lifted");
  assert.equal(evaluateClip(manifest, clip, 23).variants.get("front-paw-left"), "lifted");
  assert.equal(evaluateClip(manifest, clip, 24).variants.get("front-paw-left"), "planted");
});

test("evaluateClip rejects out-of-range values, unknown controls, and missing frames", async (t) => {
  const { clip, manifest } = await rigFixture(t);
  clip.controls.bodyBob[1].value = -0.02;
  assert.throws(() => evaluateClip(manifest, clip, 12), /outside -0.015..0.015/);

  const unknown = walkClip();
  unknown.controls.unknown = [{ frame: 0, value: 0 }, { frame: 47, value: 0 }];
  assert.throws(() => evaluateClip(manifest, unknown, 0), /unknown control unknown/);

  const missingStart = walkClip();
  missingStart.controls.bodyBob.shift();
  assert.throws(() => evaluateClip(manifest, missingStart, 0), /outside keyframe range/);
  assert.throws(() => evaluateClip(manifest, walkClip(), -1), /frame must be an integer inside the clip/);
  assert.throws(() => evaluateClip(manifest, walkClip(), 48), /frame must be an integer inside the clip/);
});

test("renderRigFrame composes active variant rasters in draw order", async (t) => {
  const { clipPath, manifestPath } = await rigFixture(t);
  const frame = await renderRigFrame(manifestPath, clipPath, 0);

  assert.deepEqual([...frame.data.subarray((100 * 1536 + 100) * 4, (100 * 1536 + 100) * 4 + 4)], [25, 80, 180, 255]);
  assert.deepEqual([...frame.data.subarray((200 * 1536 + 200) * 4, (200 * 1536 + 200) * 4 + 4)], [240, 170, 50, 255]);
});

test("assertLoopClosure accepts exact closure and rejects changed controls or variants", async (t) => {
  const { clip, manifest } = await rigFixture(t);
  assert.doesNotThrow(() => assertLoopClosure(manifest, clip));

  const changedControl = structuredClone(clip);
  changedControl.controls.bodyBob.at(-1).value = 0.001;
  assert.throws(() => assertLoopClosure(manifest, changedControl), /control bodyBob does not close exactly/);

  const changedVariant = structuredClone(clip);
  changedVariant.variants["front-paw-left"].push({ frame: 47, value: "lifted", interpolation: "hold" });
  assert.throws(() => assertLoopClosure(manifest, changedVariant), /variant front-paw-left does not close exactly/);
});

test("assertLoopClosure requires every variant to declare its frame-zero state", async (t) => {
  const { clip, manifest } = await rigFixture(t);
  clip.variants["front-paw-left"][0].frame = 1;
  assert.throws(() => assertLoopClosure(manifest, clip), /variant front-paw-left must declare a state at frame 0/);
});
