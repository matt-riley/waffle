import assert from "node:assert/strict";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { test } from "node:test";

import { PNG } from "pngjs";

import { addRepairLayers, buildClosedLid, buildCoveredRepair, buildMappedLid } from "../build-waffle-rig-repairs.mjs";
import { buildStandingRig } from "../build-waffle-standing-rig.mjs";
import { sourceOver } from "../rig-raster.mjs";
import { validateRig } from "../validate-rig.mjs";

function png(width, height) {
  return new PNG({ width, height });
}

function paint(target, x, y, rgba) {
  target.data.set(rgba, (y * target.width + x) * 4);
}

async function workspace(t) {
  const directory = await mkdtemp(path.join(tmpdir(), "waffle-rig-repairs-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  return directory;
}

test("buildCoveredRepair writes only beneath fully opaque cover pixels", () => {
  const source = png(5, 5);
  const cover = png(5, 5);
  for (let y = 0; y < 5; y += 1) {
    for (let x = 0; x < 5; x += 1) paint(source, x, y, [x * 20, y * 20, 40, 255]);
  }
  paint(cover, 2, 2, [1, 1, 1, 255]);
  paint(cover, 3, 2, [1, 1, 1, 254]);

  const repair = buildCoveredRepair(source, cover, {
    polygons: [[[1, 1], [4, 1], [4, 4], [1, 4]]],
    sampleOffset: { x: -1, y: 0 },
  });

  assert.deepEqual([...repair.data.subarray((2 * 5 + 2) * 4, (2 * 5 + 2) * 4 + 4)], [20, 40, 40, 255]);
  assert.equal(repair.data[(2 * 5 + 3) * 4 + 3], 0);
  assert.equal(repair.data[(1 * 5 + 1) * 4 + 3], 0);
});

test("buildCoveredRepair uses a declared opaque fallback when its mapped sample is transparent", () => {
  const source = png(4, 4);
  const cover = png(4, 4);
  paint(source, 3, 3, [210, 130, 40, 255]);
  paint(cover, 1, 1, [1, 1, 1, 255]);

  const repair = buildCoveredRepair(source, cover, {
    polygons: [[[0, 0], [3, 0], [3, 3], [0, 3]]],
    sampleOffset: { x: 0, y: 0 },
    fallbackSample: { x: 3, y: 3 },
  });

  assert.deepEqual([...repair.data.subarray((1 * 4 + 1) * 4, (1 * 4 + 1) * 4 + 4)], [210, 130, 40, 255]);
});

test("buildCoveredRepair can fill a hidden-only patch outside neutral cover", () => {
  const source = png(4, 4);
  const cover = png(4, 4);
  paint(source, 1, 1, [210, 130, 40, 255]);

  const repair = buildCoveredRepair(source, cover, {
    polygons: [[[0, 0], [3, 0], [3, 3], [0, 3]]],
    sampleOffset: { x: 0, y: 0 },
    allowOutsideCover: true,
  });

  assert.deepEqual([...repair.data.subarray((1 * 4 + 1) * 4, (1 * 4 + 1) * 4 + 4)], [210, 130, 40, 255]);
});

test("a covered repair cannot alter the neutral composite", () => {
  const source = png(3, 3);
  const cover = png(3, 3);
  paint(source, 1, 1, [200, 120, 40, 255]);
  paint(cover, 1, 1, [200, 120, 40, 255]);
  const repair = buildCoveredRepair(source, cover, {
    polygons: [[[0, 0], [3, 0], [3, 3], [0, 3]]],
    sampleOffset: { x: 0, y: 0 },
  });

  assert.deepEqual(sourceOver(repair, cover).data, cover.data);
});

test("buildClosedLid stays inside its ellipse and paints a curved dark lash", () => {
  const source = png(11, 11);
  for (let y = 0; y < 11; y += 1) {
    for (let x = 0; x < 11; x += 1) paint(source, x, y, [230, 150 + y, 50, 255]);
  }
  const lid = buildClosedLid(source, {
    center: { x: 5, y: 5 },
    radius: { x: 4, y: 3 },
    sampleBand: { top: 1, bottom: 3 },
    lash: { y: 5, arch: 0.12, thickness: 1, color: [90, 45, 20, 255] },
  });

  assert.equal(lid.data[(5 * 11 + 5) * 4 + 3], 255);
  assert.deepEqual([...lid.data.subarray((5 * 11 + 5) * 4, (5 * 11 + 5) * 4 + 4)], [90, 45, 20, 255]);
  assert.equal(lid.data[0 * 4 + 3], 0);
  assert.equal(lid.data[(5 * 11 + 10) * 4 + 3], 0);
});

test("buildMappedLid scales a reference ellipse and feathers its edge", () => {
  const reference = png(9, 9);
  for (let y = 0; y < 9; y += 1) {
    for (let x = 0; x < 9; x += 1) paint(reference, x, y, [100 + x, 80 + y, 40, 255]);
  }
  const lid = buildMappedLid(reference, { width: 11, height: 11 }, {
    reference: { center: { x: 4, y: 4 }, radius: { x: 3, y: 3 } },
    center: { x: 5, y: 5 },
    radius: { x: 4, y: 4 },
    feather: 0.4,
  });

  assert.deepEqual([...lid.data.subarray((5 * 11 + 5) * 4, (5 * 11 + 5) * 4 + 4)], [104, 84, 40, 255]);
  assert.ok(lid.data[(5 * 11 + 8) * 4 + 3] > 0 && lid.data[(5 * 11 + 8) * 4 + 3] < 255);
  assert.equal(lid.data[(5 * 11 + 10) * 4 + 3], 0);
});

test("addRepairLayers updates a base rig without changing its neutral render", async (t) => {
  const root = await workspace(t);
  const source = png(8, 8);
  for (let y = 1; y < 7; y += 1) {
    for (let x = 1; x < 7; x += 1) paint(source, x, y, [200 + x, 100 + y, 30, 255]);
  }
  const sourceFile = path.join(root, "standing.png");
  const masksFile = path.join(root, "masks.json");
  const outputDirectory = path.join(root, "rigs", "standing-v1");
  await writeFile(sourceFile, PNG.sync.write(source));
  await writeFile(masksFile, JSON.stringify({
    canvas: { width: 8, height: 8 },
    regionsFrontToBack: [{ id: "head", polygons: [[[1, 1], [5, 1], [5, 5], [1, 5]]] }],
    fallback: "body",
    layerDefinitions: {
      body: { parent: null, drawOrder: 20, pivot: { x: 0.5, y: 0.8 } },
      head: { parent: "body", drawOrder: 30, pivot: { x: 0.4, y: 0.4 } },
    },
    controls: { blink: { min: 0, max: 1 } },
  }));
  const manifestPath = await buildStandingRig({ sourceFile, masksFile, outputDirectory });
  const repairsFile = path.join(outputDirectory, "repairs.json");
  const lidReferenceFile = path.join(root, "lid-reference.png");
  const lidReference = png(8, 8);
  for (let y = 0; y < 8; y += 1) {
    for (let x = 0; x < 8; x += 1) paint(lidReference, x, y, [80, 70, 60, 255]);
  }
  await writeFile(lidReferenceFile, PNG.sync.write(lidReference));
  await writeFile(repairsFile, JSON.stringify({
    repairs: [{
      id: "neck-repair",
      cover: "head",
      parent: "body",
      drawOrder: 25,
      pivot: { x: 0.4, y: 0.5 },
      visibleAtNeutral: false,
      polygons: [[[1, 2], [5, 2], [5, 5], [1, 5]]],
      sampleOffset: { x: 0, y: 1 },
    }],
    lids: [{
      id: "left-eye-lid",
      parent: "head",
      drawOrder: 40,
      pivot: { x: 0.35, y: 0.35 },
      center: { x: 3, y: 3 },
      radius: { x: 1, y: 1 },
      reference: { center: { x: 3, y: 3 }, radius: { x: 1, y: 1 } },
      feather: 0,
      sampleBand: { top: 1, bottom: 2 },
      lash: { y: 3, arch: 0.1, thickness: 0.5, color: [90, 45, 20, 255] },
    }],
  }));

  await addRepairLayers({ manifestPath, repairsFile, lidReferenceFile });
  const manifest = JSON.parse(await readFile(manifestPath, "utf8"));
  const result = await validateRig(manifestPath);

  assert.deepEqual(manifest.layers.map((layer) => layer.id), ["body", "neck-repair", "head", "left-eye-lid"]);
  assert.equal(manifest.layers.find((layer) => layer.id === "neck-repair").visibleAtNeutral, false);
  assert.equal(manifest.layers.find((layer) => layer.id === "left-eye-lid").visibleAtNeutral, false);
  const lid = PNG.sync.read(await readFile(path.join(outputDirectory, "layers/left-eye-lid.png")));
  assert.deepEqual([...lid.data.subarray((3 * 8 + 3) * 4, (3 * 8 + 3) * 4 + 4)], [80, 70, 60, 255]);
  assert.deepEqual(result, { layerCount: 4, mismatchPixels: 0 });
});
