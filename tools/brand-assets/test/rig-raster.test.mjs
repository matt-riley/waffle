import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtemp, mkdir, readFile, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { test } from "node:test";

import { PNG } from "pngjs";

import { recomposeLayers, sourceOver } from "../rig-raster.mjs";
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

test("sourceOver keeps transparent and opaque pixels exact", () => {
  const bottom = rgba(2, 1, [20, 40, 60, 128, 80, 90, 100, 255]);
  const top = rgba(2, 1, [0, 0, 0, 0, 220, 180, 140, 255]);

  const result = sourceOver(bottom, top);

  assert.deepEqual([...result.data], [20, 40, 60, 128, 220, 180, 140, 255]);
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
