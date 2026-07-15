import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtemp, mkdir, readFile, rm, symlink, truncate, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { test } from "node:test";

import { PNG } from "pngjs";

import { recomposeLayers, sourceOver, transformRgba } from "../rig-raster.mjs";
import { validateRig } from "../validate-rig.mjs";

async function workspace(t) {
  const directory = await mkdtemp(path.join(tmpdir(), "waffle-rig-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  return directory;
}

function rgba(width, height, pixels) {
  const png = new PNG({ width, height });
  png.data.set(pixels);
  return png;
}

async function writePng(file, png) {
  await mkdir(path.dirname(file), { recursive: true });
  await writeFile(file, PNG.sync.write(png));
}

async function sha256(file) {
  return createHash("sha256").update(await readFile(file)).digest("hex");
}

function assertRenderedEqual(actual, expected) {
  assert.equal(actual.length, expected.length);
  for (let offset = 0; offset < actual.length; offset += 4) {
    assert.equal(actual[offset + 3], expected[offset + 3], `alpha differs at pixel ${offset / 4}`);
    if (expected[offset + 3] === 0) continue;
    assert.deepEqual(
      [...actual.subarray(offset, offset + 3)],
      [...expected.subarray(offset, offset + 3)],
      `RGB differs at visible pixel ${offset / 4}`,
    );
  }
}

async function rigFixture(t) {
  const directory = await workspace(t);
  const layersDirectory = path.join(directory, "layers");
  await mkdir(layersDirectory);

  const red = [220, 60, 40, 255];
  const gold = [240, 170, 50, 255];
  const blank = () => rgba(3, 3, new Array(3 * 3 * 4).fill(0));
  const paint = (png, x, y, color) => png.data.set(color, (y * png.width + x) * 4);
  const source = blank();
  const back = blank();
  const front = blank();
  paint(source, 0, 0, [10, 17, 16, 0]);
  paint(source, 1, 0, gold);
  paint(source, 1, 1, red);
  paint(source, 2, 1, red);
  paint(back, 1, 1, red);
  paint(back, 2, 1, red);
  paint(front, 1, 0, gold);

  const sourceFile = path.join(directory, "source.png");
  const referenceFile = path.join(directory, "neutral-reference.png");
  const backFile = path.join(layersDirectory, "back.png");
  const frontFile = path.join(layersDirectory, "front.png");
  await Promise.all([
    writePng(sourceFile, source),
    writePng(referenceFile, source),
    writePng(backFile, back),
    writePng(frontFile, front),
  ]);

  const manifest = {
    schemaVersion: 1,
    canvas: { width: 3, height: 3 },
    source: { file: "source.png", sha256: await sha256(sourceFile) },
    neutralReference: { file: "neutral-reference.png", sha256: await sha256(referenceFile) },
    layers: [
      {
        id: "back",
        file: "layers/back.png",
        role: "visible",
        parent: null,
        drawOrder: 10,
        visibleAtNeutral: true,
        blendMode: "normal",
        pivot: { x: 0.5, y: 0.5 },
        neutral: { x: 0, y: 0, rotationDegrees: 0, scaleX: 1, scaleY: 1 },
        sha256: await sha256(backFile),
      },
      {
        id: "front",
        file: "layers/front.png",
        role: "visible",
        parent: "back",
        drawOrder: 20,
        visibleAtNeutral: true,
        blendMode: "normal",
        pivot: { x: 0.5, y: 0.5 },
        neutral: { x: 0, y: 0, rotationDegrees: 0, scaleX: 1, scaleY: 1 },
        sha256: await sha256(frontFile),
      },
    ],
    controls: {
      breath: { min: 0, max: 1 },
      headTilt: { min: -3, max: 3 },
    },
  };
  const manifestFile = path.join(directory, "rig.json");

  async function save(next = manifest) {
    await writeFile(manifestFile, `${JSON.stringify(next, null, 2)}\n`);
    return manifestFile;
  }

  return { directory, manifest, manifestFile, save, source };
}

async function rigV2Fixture(t) {
  const root = await workspace(t);
  const directory = path.join(root, "brand", "waffle", "rigs", "standing-v2");
  const posesDirectory = path.join(root, "brand", "poses");
  await Promise.all([
    mkdir(path.join(directory, "layers"), { recursive: true }),
    mkdir(path.join(directory, "variants", "front-paw-left"), { recursive: true }),
    mkdir(posesDirectory, { recursive: true }),
  ]);

  const blank = () => rgba(1536, 1024, new Uint8Array(1536 * 1024 * 4));
  const source = blank();
  const torso = blank();
  const planted = blank();
  const alternate = blank();
  torso.data.set([220, 60, 40, 255], (700 * 1536 + 800) * 4);
  planted.data.set([240, 170, 50, 255], (800 * 1536 + 540) * 4);
  alternate.data.set([240, 170, 50, 255], (760 * 1536 + 500) * 4);
  source.data.set(torso.data);
  source.data.set(planted.data.subarray((800 * 1536 + 540) * 4, (800 * 1536 + 540) * 4 + 4), (800 * 1536 + 540) * 4);

  const files = {
    source: path.join(posesDirectory, "standing.png"),
    reference: path.join(directory, "neutral-reference.png"),
    torso: path.join(directory, "layers", "torso.png"),
    anchor: path.join(directory, "layers", "front-paw-left.png"),
    planted: path.join(directory, "variants", "front-paw-left", "planted.png"),
    lifted: path.join(directory, "variants", "front-paw-left", "lifted.png"),
    wave: path.join(directory, "variants", "front-paw-left", "wave.png"),
  };
  await Promise.all([
    writePng(files.source, source),
    writePng(files.reference, source),
    writePng(files.torso, torso),
    writePng(files.anchor, planted),
    writePng(files.planted, planted),
    writePng(files.lifted, alternate),
    writePng(files.wave, alternate),
  ]);

  const manifest = {
    schemaVersion: 2,
    canvas: { width: 1536, height: 1024 },
    root: { id: "waffle-root", pivot: { x: 0.52, y: 0.76 } },
    source: { file: "../../../poses/standing.png", sha256: await sha256(files.source) },
    neutralReference: { file: "neutral-reference.png", sha256: await sha256(files.reference) },
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
        sha256: await sha256(files.torso),
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
        sha256: await sha256(files.anchor),
      },
    ],
    variants: {
      "front-paw-left": {
        layer: "front-paw-left",
        members: [
          { id: "planted", file: "variants/front-paw-left/planted.png", neutral: true, sha256: await sha256(files.planted) },
          { id: "lifted", file: "variants/front-paw-left/lifted.png", neutral: false, sha256: await sha256(files.lifted) },
          { id: "wave", file: "variants/front-paw-left/wave.png", neutral: false, sha256: await sha256(files.wave) },
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
  const manifestFile = path.join(directory, "rig.json");

  async function save() {
    await writeFile(manifestFile, `${JSON.stringify(manifest, null, 2)}\n`);
    return manifestFile;
  }

  return { directory, files, manifest, manifestFile, save };
}

test("sourceOver keeps transparent and opaque pixels exact", () => {
  const bottom = rgba(2, 1, [20, 40, 60, 128, 80, 90, 100, 255]);
  const top = rgba(2, 1, [0, 0, 0, 0, 220, 180, 140, 255]);

  const result = sourceOver(bottom, top);

  assert.deepEqual([...result.data], [20, 40, 60, 128, 220, 180, 140, 255]);
});

test("transformRgba preserves identity and rotates around a normalized pivot", () => {
  const source = rgba(5, 5, new Array(5 * 5 * 4).fill(0));
  source.data.set([220, 100, 30, 255], (2 * 5 + 3) * 4);

  const identity = transformRgba(source, {
    pivot: { x: 0.5, y: 0.5 },
    x: 0,
    y: 0,
    rotationDegrees: 0,
    scaleX: 1,
    scaleY: 1,
  });
  const rotated = transformRgba(source, {
    pivot: { x: 0.5, y: 0.5 },
    x: 0,
    y: 0,
    rotationDegrees: 90,
    scaleX: 1,
    scaleY: 1,
  });

  assert.deepEqual(identity.data, source.data);
  assert.equal(rotated.data[(3 * 5 + 2) * 4 + 3], 255);
  assert.equal(rotated.data[(2 * 5 + 3) * 4 + 3], 0);
  assert.equal([...rotated.data].filter((_, index) => index % 4 === 3 && rotated.data[index] > 0).length, 1);
});

test("transformRgba clears pixels whose inverse sample is outside the canvas", () => {
  const source = rgba(3, 3, new Array(3 * 3 * 4).fill(255));
  const translated = transformRgba(source, {
    pivot: { x: 0.5, y: 0.5 },
    x: 2,
    y: 0,
    rotationDegrees: 0,
    scaleX: 1,
    scaleY: 1,
  });

  assert.ok([...translated.data].every((value) => value === 0));
});

test("recomposes partitioned neutral layers with exact visible RGBA", async (t) => {
  const fixture = await rigFixture(t);
  await fixture.save();

  const result = await recomposeLayers(fixture.manifestFile);

  assertRenderedEqual(result.data, fixture.source.data);
});

test("validates a complete rig and reports zero neutral mismatches", async (t) => {
  const fixture = await rigFixture(t);
  await fixture.save();

  const result = await validateRig(fixture.manifestFile);

  assert.deepEqual(result, { layerCount: 2, mismatchPixels: 0 });
});

test("validates a complete v2 rig using only the neutral variant", async (t) => {
  const fixture = await rigV2Fixture(t);
  await fixture.save();

  const result = await validateRig(fixture.manifestFile);

  assert.deepEqual(result, { layerCount: 2, mismatchPixels: 0 });
});

test("rejects v2 hash drift and files at the 10 MB boundary", async (t) => {
  const fixture = await rigV2Fixture(t);
  fixture.manifest.variants["front-paw-left"].members[0].sha256 = "0".repeat(64);
  await fixture.save();
  await assert.rejects(() => validateRig(fixture.manifestFile), /sha256 mismatch for variant front-paw-left\/planted/);

  await truncate(fixture.files.planted, 10 * 1024 * 1024);
  fixture.manifest.variants["front-paw-left"].members[0].sha256 = await sha256(fixture.files.planted);
  await fixture.save();
  await assert.rejects(() => validateRig(fixture.manifestFile), /asset exceeds 10485759-byte budget/);
});

test("rejects v2 layer and variant paths that use symlinks", async (t) => {
  const fixture = await rigV2Fixture(t);
  await rm(fixture.files.planted);
  await symlink(fixture.files.anchor, fixture.files.planted);
  fixture.manifest.variants["front-paw-left"].members[0].sha256 = await sha256(fixture.files.planted);
  await fixture.save();

  await assert.rejects(() => validateRig(fixture.manifestFile), /variant front-paw-left\/planted file must not use symlinks/);
});

test("rejects a v2 package at the 60 MB boundary", async (t) => {
  const fixture = await rigV2Fixture(t);
  const undeclaredPackageFile = path.join(fixture.directory, "oversize-package.bin");
  await writeFile(undeclaredPackageFile, "");
  await truncate(undeclaredPackageFile, 60 * 1024 * 1024);
  await fixture.save();

  await assert.rejects(() => validateRig(fixture.manifestFile), /rig package must be below 62914560 bytes/);
});

test("reports v2 neutral mismatch coordinates, context, and RGBA values", async (t) => {
  const fixture = await rigV2Fixture(t);
  const changed = PNG.sync.read(await readFile(fixture.files.planted));
  changed.data.set([10, 20, 30, 255], (600 * 1536 + 600) * 4);
  await writePng(fixture.files.planted, changed);
  fixture.manifest.variants["front-paw-left"].members[0].sha256 = await sha256(fixture.files.planted);
  await fixture.save();

  await assert.rejects(
    () => validateRig(fixture.manifestFile),
    /x=600 y=600 context=layer torso,variant front-paw-left\/planted expected=\[0,0,0,0\] actual=\[10,20,30,255\]/,
  );
});

test("rejects duplicate layer IDs and draw orders", async (t) => {
  const fixture = await rigFixture(t);
  fixture.manifest.layers[1].id = "back";
  await fixture.save();
  await assert.rejects(() => validateRig(fixture.manifestFile), /duplicate layer id: back/);

  fixture.manifest.layers[1].id = "front";
  fixture.manifest.layers[1].drawOrder = 10;
  await fixture.save();
  await assert.rejects(() => validateRig(fixture.manifestFile), /duplicate drawOrder: 10/);
});

test("rejects missing parents and cyclic layer graphs", async (t) => {
  const fixture = await rigFixture(t);
  fixture.manifest.layers[1].parent = "missing";
  await fixture.save();
  await assert.rejects(() => validateRig(fixture.manifestFile), /unknown parent missing/);

  fixture.manifest.layers[1].parent = "back";
  fixture.manifest.layers[0].parent = "front";
  await fixture.save();
  await assert.rejects(() => validateRig(fixture.manifestFile), /layer graph contains a cycle/);
});

test("rejects invalid pivots, transforms, controls, and blend modes", async (t) => {
  const fixture = await rigFixture(t);
  fixture.manifest.layers[0].pivot.x = 1.1;
  await fixture.save();
  await assert.rejects(() => validateRig(fixture.manifestFile), /pivot must be normalized/);

  fixture.manifest.layers[0].pivot.x = 0.5;
  fixture.manifest.layers[0].neutral.rotationDegrees = Number.NaN;
  await fixture.save();
  await assert.rejects(() => validateRig(fixture.manifestFile), /neutral transform values must be finite numbers/);

  fixture.manifest.layers[0].neutral.rotationDegrees = 0;
  fixture.manifest.layers[0].blendMode = "multiply";
  await fixture.save();
  await assert.rejects(() => validateRig(fixture.manifestFile), /blendMode must be normal/);

  fixture.manifest.layers[0].blendMode = "normal";
  fixture.manifest.controls.breath = { min: 1, max: 1 };
  await fixture.save();
  await assert.rejects(() => validateRig(fixture.manifestFile), /control breath must have finite min < max/);
});

test("rejects file hash drift and neutral pixel drift", async (t) => {
  const fixture = await rigFixture(t);
  await fixture.save();
  fixture.manifest.layers[0].sha256 = "0".repeat(64);
  await fixture.save();
  await assert.rejects(() => validateRig(fixture.manifestFile), /sha256 mismatch for layer back/);

  fixture.manifest.layers[0].sha256 = await sha256(path.join(fixture.directory, "layers/back.png"));
  const changed = rgba(3, 3, new Array(3 * 3 * 4).fill(0));
  changed.data.set([220, 60, 40, 255], (1 * 3 + 1) * 4);
  changed.data.set([10, 20, 30, 255], (1 * 3 + 2) * 4);
  await writePng(path.join(fixture.directory, "layers/back.png"), changed);
  fixture.manifest.layers[0].sha256 = await sha256(path.join(fixture.directory, "layers/back.png"));
  await fixture.save();
  await assert.rejects(() => validateRig(fixture.manifestFile), /neutral recomposition differs at x=2 y=1/);
});

test("rejects paths and symlinks escaping the rig directory", async (t) => {
  const fixture = await rigFixture(t);
  fixture.manifest.layers[0].file = "../outside.png";
  await fixture.save();
  await assert.rejects(() => validateRig(fixture.manifestFile), /layer back file must stay inside the rig directory/);

  const outside = await workspace(t);
  const outsideFile = path.join(outside, "outside.png");
  await writePng(outsideFile, rgba(3, 3, new Array(36).fill(0)));
  const link = path.join(fixture.directory, "layers/linked.png");
  await symlink(outsideFile, link);
  fixture.manifest.layers[0].file = "layers/linked.png";
  fixture.manifest.layers[0].sha256 = await sha256(outsideFile);
  await fixture.save();
  await assert.rejects(() => validateRig(fixture.manifestFile), /layer back file must resolve inside the rig directory/);
});
