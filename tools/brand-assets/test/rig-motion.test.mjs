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
  const hiddenRepair = blank();
  const leftLid = blank();
  const rightLid = blank();
  paint(body, 100, 100, [220, 60, 40, 255]);
  paint(planted, 200, 200, [240, 170, 50, 255]);
  paint(lifted, 200, 160, [240, 170, 50, 255]);
  paint(foreground, 100, 100, [25, 80, 180, 255]);
  paint(hiddenRepair, 400, 400, [120, 40, 160, 255]);
  paint(leftLid, 300, 300, [120, 210, 80, 255]);
  paint(rightLid, 301, 300, [80, 190, 220, 255]);
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
    hiddenRepair: path.join(layersDirectory, "hidden-repair.png"),
    leftLid: path.join(layersDirectory, "left-lid.png"),
    rightLid: path.join(layersDirectory, "right-lid.png"),
  };
  await Promise.all([
    writePng(files.source, source),
    writePng(files.reference, reference),
    writePng(files.body, body),
    writePng(files.pawAnchor, pawAnchor),
    writePng(files.planted, planted),
    writePng(files.lifted, lifted),
    writePng(files.foreground, foreground),
    writePng(files.hiddenRepair, hiddenRepair),
    writePng(files.leftLid, leftLid),
    writePng(files.rightLid, rightLid),
  ]);

  const manifest = {
    schemaVersion: 2,
    canvas: { width: 1536, height: 1024 },
    root: { id: "waffle-root", pivot: { x: 0.5, y: 0.5 } },
    source: { file: "source.png", sha256: await sha256(files.source) },
    neutralReference: { file: "neutral-reference.png", sha256: await sha256(files.reference) },
    layers: [
      {
        id: "hidden-repair",
        file: "layers/hidden-repair.png",
        role: "repair",
        parent: "waffle-root",
        drawOrder: 5,
        visibleAtNeutral: false,
        blendMode: "normal",
        pivot: { x: 0.5, y: 0.5 },
        neutral: { x: 0, y: 0, rotationDegrees: 0, scaleX: 1, scaleY: 1 },
        limits: {},
        sha256: await sha256(files.hiddenRepair),
      },
      {
        id: "foreground",
        file: "layers/foreground.png",
        role: "overlay",
        parent: "front-paw-left",
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
      {
        id: "left-lid",
        file: "layers/left-lid.png",
        role: "overlay",
        parent: "body",
        drawOrder: 40,
        visibleAtNeutral: false,
        blendMode: "normal",
        pivot: { x: 0.2, y: 0.3 },
        neutral: { x: 0, y: 0, rotationDegrees: 0, scaleX: 1, scaleY: 1 },
        limits: {},
        sha256: await sha256(files.leftLid),
      },
      {
        id: "right-lid",
        file: "layers/right-lid.png",
        role: "overlay",
        parent: "body",
        drawOrder: 41,
        visibleAtNeutral: false,
        blendMode: "normal",
        pivot: { x: 0.2, y: 0.3 },
        neutral: { x: 0, y: 0, rotationDegrees: 0, scaleX: 1, scaleY: 1 },
        limits: {},
        sha256: await sha256(files.rightLid),
      },
    ],
    variants: {
      "front-paw-left": {
        layer: "front-paw-left",
        members: [
          { id: "planted", file: "variants/front-paw-left/planted.png", neutral: true, sha256: await sha256(files.planted) },
          {
            id: "lifted",
            file: "variants/front-paw-left/lifted.png",
            neutral: false,
            sha256: await sha256(files.lifted),
            layerOverrides: { foreground: { visible: false } },
          },
        ],
      },
    },
    controls: {
      bodyBob: {
        min: -0.015,
        max: 0.015,
        bindings: [{ layer: "body", property: "y", factor: 1 }],
      },
      blinkLeft: {
        min: 0,
        max: 1,
        bindings: [{ layer: "left-lid", property: "opacity", factor: 1 }],
      },
      blinkRight: {
        min: 0,
        max: 1,
        bindings: [{ layer: "right-lid", property: "opacity", factor: 1 }],
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
  return { clip, clipPath, files, manifest, manifestPath, source };
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
  assert.equal(exact.layers.get("left-lid").opacity, 0);
  assert.equal(exact.layers.get("right-lid").opacity, 0);
});

test("evaluateClip resolves numeric variant thresholds unless explicit variant keys override them", async (t) => {
  const { clip, manifest } = await rigFixture(t);
  manifest.controls.pawState = {
    min: -1,
    max: 1,
    bindings: [{
      variant: "front-paw-left",
      thresholds: [
        { max: -0.5, member: "lifted" },
        { max: 0.5, member: "planted" },
        { max: 1, member: "lifted" },
      ],
    }],
  };
  clip.controls.pawState = [
    { frame: 0, value: -1, interpolation: "hold" },
    { frame: 12, value: 0, interpolation: "hold" },
    { frame: 24, value: 1, interpolation: "hold" },
    { frame: 47, value: -1, interpolation: "hold" },
  ];

  assert.equal(evaluateClip(manifest, { ...clip, variants: {} }, 0).variants.get("front-paw-left"), "lifted");
  assert.equal(evaluateClip(manifest, { ...clip, variants: {} }, 12).variants.get("front-paw-left"), "planted");
  assert.equal(evaluateClip(manifest, { ...clip, variants: {} }, 24).variants.get("front-paw-left"), "lifted");
  assert.equal(evaluateClip(manifest, clip, 12).variants.get("front-paw-left"), "lifted", "explicit clip variant wins");
  assert.doesNotThrow(
    () => assertLoopClosure(manifest, { ...clip, variants: {} }),
    "a numeric binding supplies the required frame-zero variant state",
  );
});

test("evaluateClip rejects a hybrid binding before transform and variant evaluation", async (t) => {
  const { clip, manifest } = await rigFixture(t);
  manifest.controls.pawState = {
    min: -1,
    max: 1,
    bindings: [{
      layer: "body",
      property: "x",
      factor: 0.01,
      variant: "front-paw-left",
      thresholds: [
        { max: -0.5, member: "lifted" },
        { max: 0.5, member: "planted" },
        { max: 1, member: "lifted" },
      ],
    }],
  };
  clip.controls.pawState = [{ frame: 0, value: 0 }, { frame: 47, value: 0 }];

  assert.throws(
    () => evaluateClip(manifest, clip, 0),
    /control pawState binding 1 must declare exactly one of layer or variant/,
  );
});

test("evaluateClip discriminates bindings by their own kind field", async (t) => {
  const { clip, manifest } = await rigFixture(t);
  const binding = Object.assign(Object.create({
    layer: "body",
    property: "x",
    factor: 0.01,
  }), {
    variant: "front-paw-left",
    thresholds: [
      { max: -0.5, member: "lifted" },
      { max: 0.5, member: "planted" },
      { max: 1, member: "lifted" },
    ],
  });
  manifest.controls.pawState = { min: -1, max: 1, bindings: [binding] };
  clip.controls.pawState = [{ frame: 0, value: 1 }, { frame: 47, value: 1 }];
  clip.variants = {};

  const evaluated = evaluateClip(manifest, clip, 0);
  assert.equal(evaluated.variants.get("front-paw-left"), "lifted");
  assert.equal(evaluated.layers.get("body").x, 0, "inherited layer fields must not be evaluated as a layer binding");
});

test("production headTurn and jawOpen controls select their registered painted states", async () => {
  const manifest = JSON.parse(await readFile(path.resolve(
    import.meta.dirname,
    "../../../assets/brand/waffle/rigs/standing-v2/rig.json",
  ), "utf8"));
  const clip = {
    schemaVersion: 1,
    id: "variant-control-probe",
    fps: 24,
    frameCount: 3,
    loop: false,
    requiredClosure: { firstFrame: 0, lastFrame: 2 },
    variants: {},
    controls: {
      headTurn: [{ frame: 0, value: -1 }, { frame: 1, value: 0 }, { frame: 2, value: 1 }],
      jawOpen: [{ frame: 0, value: 0 }, { frame: 1, value: 1 }, { frame: 2, value: 0 }],
    },
  };

  assert.equal(evaluateClip(manifest, clip, 0).variants.get("head-base"), "turn-left");
  assert.equal(evaluateClip(manifest, clip, 1).variants.get("head-base"), "neutral");
  assert.equal(evaluateClip(manifest, clip, 2).variants.get("head-base"), "turn-right");
  assert.equal(evaluateClip(manifest, clip, 0).variants.get("jaw"), "closed");
  assert.equal(evaluateClip(manifest, clip, 1).variants.get("jaw"), "open");
});

test("production intermediate headTurn values remain directional turns rather than clip-only expressions", async () => {
  const manifest = JSON.parse(await readFile(path.resolve(
    import.meta.dirname,
    "../../../assets/brand/waffle/rigs/standing-v2/rig.json",
  ), "utf8"));
  const clip = {
    schemaVersion: 1,
    id: "intermediate-head-turn-probe",
    fps: 24,
    frameCount: 2,
    loop: false,
    requiredClosure: { firstFrame: 0, lastFrame: 1 },
    variants: {},
    controls: {
      headTurn: [{ frame: 0, value: -0.6 }, { frame: 1, value: 0.6 }],
    },
  };

  assert.equal(evaluateClip(manifest, clip, 0).variants.get("head-base"), "turn-left");
  assert.equal(evaluateClip(manifest, clip, 1).variants.get("head-base"), "turn-right");
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

test("evaluateClip interpolates member-local variant transform tracks", async (t) => {
  const { clip, manifest } = await rigFixture(t);
  clip.variantTransforms = {
    "front-paw-left": {
      member: "lifted",
      tracks: {
        y: [{ frame: 0, value: 0 }, { frame: 12, value: -0.01 }, { frame: 47, value: 0 }],
        scaleY: [{ frame: 0, value: 1 }, { frame: 12, value: 0.95 }, { frame: 47, value: 1 }],
      },
    },
  };
  assert.deepEqual(evaluateClip(manifest, clip, 6).variantTransforms.get("front-paw-left"), {
    member: "lifted",
    transform: { x: 0, y: -0.005, rotationDegrees: 0, scaleX: 1, scaleY: 0.975 },
  });
});

test("evaluateClip applies private hidden-layer opacity without adding a public rig control", async (t) => {
  const { clip, manifest } = await rigFixture(t);
  clip.layerOpacity = {
    "hidden-repair": [
      { frame: 0, value: 0 },
      { frame: 12, value: 1 },
      { frame: 47, value: 0 },
    ],
  };
  assert.equal(evaluateClip(manifest, clip, 6).layers.get("hidden-repair").opacity, 0.5);
  assert.equal(evaluateClip(manifest, clip, 12).layerOpacity.get("hidden-repair"), 1);
  assert.equal(manifest.controls["hidden-repair"], undefined);
});

test("renderRigFrame includes a hidden repair only while its clip opacity is active", async (t) => {
  const { clip, clipPath, manifest, manifestPath } = await rigFixture(t);
  clip.layerOpacity = {
    "hidden-repair": [
      { frame: 0, value: 0 },
      { frame: 12, value: 1 },
      { frame: 47, value: 0 },
    ],
  };
  await Promise.all([
    writeFile(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`),
    writeFile(clipPath, `${JSON.stringify(clip, null, 2)}\n`),
  ]);

  const neutral = await renderRigFrame(manifestPath, clipPath, 0);
  assert.equal(neutral.data[(400 * 1536 + 400) * 4 + 3], 0);
  const active = await renderRigFrame(manifestPath, clipPath, 12);
  assert.deepEqual(
    [...active.data.subarray((400 * 1536 + 400) * 4, (400 * 1536 + 400) * 4 + 4)],
    [120, 40, 160, 255],
  );
});

test("renderRigFrame applies parentOverride and an opaque member-local transform without moving neutral stance", async (t) => {
  const { clip, clipPath, manifest, manifestPath } = await rigFixture(t);
  manifest.layers.find((layer) => layer.id === "front-paw-left").parent = "waffle-root";
  manifest.variants["front-paw-left"].members.find((member) => member.id === "lifted").parentOverride = "body";
  clip.variants["front-paw-left"] = [
    { frame: 0, value: "planted", interpolation: "hold" },
    { frame: 1, value: "lifted", interpolation: "hold" },
    { frame: 47, value: "planted", interpolation: "hold" },
  ];
  clip.variantTransforms = {
    "front-paw-left": {
      member: "lifted",
      tracks: {
        y: [{ frame: 0, value: 0 }, { frame: 12, value: -0.01 }, { frame: 47, value: 0 }],
      },
    },
  };
  await Promise.all([
    writeFile(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`),
    writeFile(clipPath, `${JSON.stringify(clip, null, 2)}\n`),
  ]);

  const neutral = await renderRigFrame(manifestPath, clipPath, 0);
  assert.equal(neutral.data[(200 * 1536 + 200) * 4 + 3], 255, "root-pinned planted paw stays on its baseline");

  const lifted = await renderRigFrame(manifestPath, clipPath, 12);
  assert.ok(lifted.data[(142 * 1536 + 200) * 4 + 3] > 0, "lifted member follows bodyBob and its member-local transform");
  assert.equal(lifted.data[(160 * 1536 + 200) * 4 + 3], 0, "lifted member is not left at the root-pinned location");
});

test("evaluateClip rejects individually valid controls whose bindings exceed a layer limit together", async (t) => {
  const { clip, manifest } = await rigFixture(t);
  manifest.controls.bodySettle = {
    min: -0.015,
    max: 0.015,
    bindings: [{ layer: "body", property: "y", factor: 1 }],
  };
  clip.controls.bodyBob[0].value = -0.01;
  clip.controls.bodySettle = [
    { frame: 0, value: -0.01 },
    { frame: 47, value: -0.01 },
  ];

  assert.throws(
    () => evaluateClip(manifest, clip, 0),
    /layer body y value -0.02 is outside -0.015..0.015/,
  );
});

test("renderRigFrame composes active variant rasters in draw order", async (t) => {
  const { clipPath, manifestPath } = await rigFixture(t);
  const frame = await renderRigFrame(manifestPath, clipPath, 0);

  assert.deepEqual([...frame.data.subarray((100 * 1536 + 100) * 4, (100 * 1536 + 100) * 4 + 4)], [25, 80, 180, 255]);
  assert.deepEqual([...frame.data.subarray((200 * 1536 + 200) * 4, (200 * 1536 + 200) * 4 + 4)], [240, 170, 50, 255]);
});

test("renderRigFrame applies selected variant member visibility overrides", async (t) => {
  const { clipPath, manifestPath } = await rigFixture(t);
  const frame = await renderRigFrame(manifestPath, clipPath, 12);

  const bodyPixel = [...frame.data.subarray((92 * 1536 + 100) * 4, (92 * 1536 + 100) * 4 + 4)];
  const pawPixel = [...frame.data.subarray((152 * 1536 + 200) * 4, (152 * 1536 + 200) * 4 + 4)];
  assert.deepEqual(bodyPixel.slice(0, 3), [220, 60, 40]);
  assert.ok(bodyPixel[3] > 0);
  assert.deepEqual(pawPixel.slice(0, 3), [240, 170, 50]);
  assert.ok(pawPixel[3] > 0);
});

test("renderRigFrame preserves exact neutral source bytes when hidden layers contain pixels", async (t) => {
  const { clipPath, manifestPath, source } = await rigFixture(t);

  const frame = await renderRigFrame(manifestPath, clipPath, 0);

  assert.deepEqual(frame.data, source.data);
});

test("renderRigFrame activates independent and synchronized blink overlays with scaled alpha", async (t) => {
  const { clip, clipPath, manifestPath, source } = await rigFixture(t);
  delete clip.controls.bodyBob;
  const blinkKeys = (value) => [
    { frame: 0, value: 0 },
    { frame: 12, value },
    { frame: 47, value: 0 },
  ];
  clip.controls.blinkLeft = blinkKeys(0.5);
  await writeFile(clipPath, `${JSON.stringify(clip, null, 2)}\n`);

  const leftOnly = await renderRigFrame(manifestPath, clipPath, 12);
  const leftOffset = (300 * 1536 + 300) * 4;
  const rightOffset = (300 * 1536 + 301) * 4;
  assert.deepEqual([...leftOnly.data.subarray(leftOffset, leftOffset + 3)], [120, 210, 80]);
  assert.equal(leftOnly.data[leftOffset + 3], 128);
  assert.equal(leftOnly.data[rightOffset + 3], 0);

  delete clip.controls.blinkLeft;
  clip.controls.blinkRight = blinkKeys(1);
  await writeFile(clipPath, `${JSON.stringify(clip, null, 2)}\n`);
  const rightOnly = await renderRigFrame(manifestPath, clipPath, 12);
  assert.equal(rightOnly.data[leftOffset + 3], 0);
  assert.deepEqual([...rightOnly.data.subarray(rightOffset, rightOffset + 4)], [80, 190, 220, 255]);

  clip.controls.blinkLeft = blinkKeys(1);
  await writeFile(clipPath, `${JSON.stringify(clip, null, 2)}\n`);
  const synchronized = await renderRigFrame(manifestPath, clipPath, 12);
  assert.equal(synchronized.data[leftOffset + 3], 255);
  assert.equal(synchronized.data[rightOffset + 3], 255);

  const neutral = await renderRigFrame(manifestPath, clipPath, 0);
  assert.deepEqual(neutral.data, source.data);
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

  const changedLayerOpacity = structuredClone(clip);
  changedLayerOpacity.layerOpacity = {
    "hidden-repair": [{ frame: 0, value: 0 }, { frame: 47, value: 1 }],
  };
  assert.throws(() => assertLoopClosure(manifest, changedLayerOpacity), /layer opacity hidden-repair does not close exactly/);

});

test("assertLoopClosure requires every variant to declare its frame-zero state", async (t) => {
  const { clip, manifest } = await rigFixture(t);
  clip.variants["front-paw-left"][0].frame = 1;
  assert.throws(() => assertLoopClosure(manifest, clip), /variant front-paw-left must declare a state at frame 0/);
});

test("standing v2 documentation states the loop source and rebuild prerequisites precisely", async () => {
  const readme = await readFile(path.resolve(
    import.meta.dirname,
    "../../../assets/brand/waffle/rigs/standing-v2/README.md",
  ), "utf8");
  assert.match(
    readme,
    /Every variant set must have a deterministic frame-0 state: an unbound set requires an explicit frame-0 `clip\.variants` key, while a numeric-bound set may derive it from its control/,
  );
  assert.match(readme, /Explicit `clip\.variants` keys override a numeric-derived state/);
  assert.match(readme, /Optional `variantTransforms` provide conservative member-local motion for an opaque selected non-neutral member/);
  assert.match(readme, /A non-neutral member may declare `parentOverride` to follow a different registered parent while active/);
  const prerequisite = readme.indexOf("Before running either rebuild command");
  const firstCommand = readme.indexOf("node tools/brand-assets/build-waffle-standing-rig-v2.mjs");
  assert.ok(prerequisite >= 0 && prerequisite < firstCommand, "concept-plate prerequisite must precede both rebuild commands");
  assert.match(
    readme.slice(prerequisite, firstCommand),
    /ignored concept\/edit plates referenced by both `repairs\.json` and `variants\.json` must already be retained locally/,
  );
});
