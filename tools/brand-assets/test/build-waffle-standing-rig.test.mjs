import assert from "node:assert/strict";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { test } from "node:test";

import { PNG } from "pngjs";

import { applyUnderlaps, buildStandingRig, partitionSource, pointInPolygon } from "../build-waffle-standing-rig.mjs";
import { sourceOver } from "../rig-raster.mjs";

function sourceFixture() {
  const png = new PNG({ width: 4, height: 3 });
  const colours = [
    [220, 80, 30, 255],
    [230, 120, 40, 180],
    [240, 170, 60, 255],
    [250, 210, 100, 255],
  ];
  for (let x = 0; x < colours.length; x += 1) {
    png.data.set(colours[x], (png.width + x) * 4);
  }
  return png;
}

async function workspace(t) {
  const directory = await mkdtemp(path.join(tmpdir(), "waffle-rig-build-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  return directory;
}

test("pointInPolygon uses pixel-centre compatible even-odd geometry", () => {
  const square = [[0, 0], [2, 0], [2, 2], [0, 2]];

  assert.equal(pointInPolygon(0.5, 0.5, square), true);
  assert.equal(pointInPolygon(1.5, 1.5, square), true);
  assert.equal(pointInPolygon(2.5, 1.5, square), false);
});

test("partition assigns every source pixel to exactly one front-to-back region", () => {
  const source = sourceFixture();
  const regions = [
    { id: "front", polygons: [[[0, 0], [2, 0], [2, 3], [0, 3]]] },
    { id: "middle", polygons: [[[1, 0], [3, 0], [3, 3], [1, 3]]] },
  ];

  const layers = partitionSource(source, regions, "back");

  assert.deepEqual([...layers.keys()], ["front", "middle", "back"]);
  for (let pixel = 0; pixel < source.width * source.height; pixel += 1) {
    const offset = pixel * 4;
    const owners = [...layers.values()].filter((layer) => layer.data[offset + 3] > 0);
    const sourceVisible = source.data[offset + 3] > 0;
    assert.equal(owners.length, sourceVisible ? 1 : 0);
    if (sourceVisible) assert.deepEqual([...owners[0].data.subarray(offset, offset + 4)], [...source.data.subarray(offset, offset + 4)]);
  }
});

test("partitioned layers recompose to the exact decoded source", () => {
  const source = sourceFixture();
  const regions = [
    { id: "front", polygons: [[[0, 0], [2, 0], [2, 3], [0, 3]]] },
    { id: "middle", polygons: [[[2, 0], [3, 0], [3, 3], [2, 3]]] },
  ];
  const layers = partitionSource(source, regions, "back");
  let composite = new PNG({ width: source.width, height: source.height });
  for (const id of ["back", "middle", "front"]) composite = sourceOver(composite, layers.get(id));

  assert.deepEqual(composite.data, source.data);
});

test("underlaps duplicate source pixels so a moving layer can stay tucked behind its cover", () => {
  const source = sourceFixture();
  const regions = [
    {
      id: "ear",
      polygons: [[[0, 0], [1, 0], [1, 3], [0, 3]]],
      underlapPolygons: [[[1, 0], [3, 0], [3, 3], [1, 3]]],
    },
    { id: "head", polygons: [[[1, 0], [3, 0], [3, 3], [1, 3]]] },
  ];
  const layers = partitionSource(source, regions, "body");

  applyUnderlaps(source, layers, regions);

  const translucentOffset = (source.width + 1) * 4;
  assert.equal(layers.get("ear").data[translucentOffset + 3], 0);

  const sharedOffset = (source.width + 2) * 4;
  assert.deepEqual(
    [...layers.get("ear").data.subarray(sharedOffset, sharedOffset + 4)],
    [...source.data.subarray(sharedOffset, sharedOffset + 4)],
  );
  assert.deepEqual(
    [...layers.get("head").data.subarray(sharedOffset, sharedOffset + 4)],
    [...source.data.subarray(sharedOffset, sharedOffset + 4)],
  );

  let composite = new PNG({ width: source.width, height: source.height });
  for (const id of ["body", "ear", "head"]) composite = sourceOver(composite, layers.get(id));
  assert.deepEqual(composite.data, source.data);
});

test("buildStandingRig writes registered sanitized layers and a hashed manifest", async (t) => {
  const directory = await workspace(t);
  const sourceFile = path.join(directory, "standing.png");
  const masksFile = path.join(directory, "masks.json");
  const outputDirectory = path.join(directory, "rigs", "standing-v1");
  await writeFile(sourceFile, PNG.sync.write(sourceFixture()));
  await writeFile(masksFile, JSON.stringify({
    canvas: { width: 4, height: 3 },
    regionsFrontToBack: [
      { id: "head", polygons: [[[0, 0], [2, 0], [2, 3], [0, 3]]] },
      { id: "tail-visible", polygons: [[[3, 0], [4, 0], [4, 3], [3, 3]]] },
    ],
    fallback: "body",
    layerDefinitions: {
      "tail-visible": { parent: "body", drawOrder: 10, pivot: { x: 0.8, y: 0.5 } },
      body: { parent: null, drawOrder: 20, pivot: { x: 0.5, y: 0.8 } },
      head: { parent: "body", drawOrder: 30, pivot: { x: 0.3, y: 0.5 } },
    },
    controls: { breath: { min: 0, max: 1 } },
  }));

  const manifestPath = await buildStandingRig({ sourceFile, masksFile, outputDirectory });
  const manifest = JSON.parse(await readFile(manifestPath, "utf8"));

  assert.equal(manifest.schemaVersion, 1);
  assert.deepEqual(manifest.canvas, { width: 4, height: 3 });
  assert.deepEqual(manifest.layers.map((layer) => layer.id), ["tail-visible", "body", "head"]);
  assert.match(manifest.source.sha256, /^[a-f\d]{64}$/u);
  assert.ok(manifest.layers.every((layer) => /^[a-f\d]{64}$/u.test(layer.sha256)));
  assert.deepEqual(
    PNG.sync.read(await readFile(path.join(outputDirectory, "neutral-reference.png"))).data,
    sourceFixture().data,
  );
});
