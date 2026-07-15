import assert from "node:assert/strict";
import { mkdtemp, mkdir, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { test } from "node:test";

import { PNG } from "pngjs";

import {
  applyCoveredUnderlaps,
  applyUnderlaps,
  buildStandingRigV2,
  partitionSource,
} from "../build-waffle-standing-rig-v2.mjs";
import { sourceOver } from "../rig-raster.mjs";

function smallSource() {
  const png = new PNG({ width: 4, height: 2 });
  const pixels = [
    [210, 70, 30, 255],
    [220, 100, 40, 180],
    [230, 140, 50, 255],
    [240, 180, 60, 255],
  ];
  for (const [x, pixel] of pixels.entries()) png.data.set(pixel, (png.width + x) * 4);
  return png;
}

async function workspace(t) {
  const directory = await mkdtemp(path.join(tmpdir(), "waffle-rig-v2-build-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  return directory;
}

function productionSource() {
  const png = new PNG({ width: 1536, height: 1024 });
  png.data.set([10, 17, 16, 0], 0);
  png.data.set([210, 70, 30, 255], (500 * png.width + 700) * 4);
  png.data.set([230, 140, 50, 255], (500 * png.width + 701) * 4);
  return png;
}

function validMasks() {
  return {
    canvas: { width: 1536, height: 1024 },
    root: { id: "waffle-root", pivot: { x: 0.5, y: 0.75 } },
    regionsFrontToBack: [
      {
        id: "head-base",
        polygons: [[[700, 500], [701, 500], [701, 501], [700, 501]]],
      },
    ],
    fallback: "torso",
    layerDefinitions: {
      torso: {
        parent: "waffle-root",
        drawOrder: 10,
        pivot: { x: 0.5, y: 0.7 },
        limits: { y: { min: -0.01, max: 0.01 } },
      },
      "head-base": {
        parent: "torso",
        drawOrder: 20,
        pivot: { x: 0.46, y: 0.5 },
        limits: { rotationDegrees: { min: -4, max: 4 } },
      },
    },
    variants: {},
    controls: {
      headTilt: {
        min: -4,
        max: 4,
        bindings: [{ layer: "head-base", property: "rotationDegrees", factor: 1 }],
      },
    },
  };
}

async function productionFixture(t, outputName = "standing-v2") {
  const root = await workspace(t);
  const waffle = path.join(root, "assets", "brand", "waffle");
  const sourceFile = path.join(waffle, "poses", "standing.png");
  const masksFile = path.join(root, "inputs", "masks.json");
  const outputDirectory = path.join(waffle, "rigs", outputName);
  await Promise.all([
    mkdir(path.dirname(sourceFile), { recursive: true }),
    mkdir(path.dirname(masksFile), { recursive: true }),
    mkdir(path.dirname(outputDirectory), { recursive: true }),
  ]);
  await writeFile(sourceFile, PNG.sync.write(productionSource()));
  await writeFile(masksFile, `${JSON.stringify(validMasks(), null, 2)}\n`);
  return { masksFile, outputDirectory, sourceFile };
}

async function builderTemps(outputDirectory) {
  const parent = path.dirname(outputDirectory);
  const prefix = `${path.basename(outputDirectory)}.building-`;
  return (await readdir(parent)).filter((entry) => entry.startsWith(prefix));
}

test("partition assigns overlapping polygons to the first front-to-back owner without changing source bytes", () => {
  const source = smallSource();
  const regions = [
    { id: "front", polygons: [[[0, 0], [3, 0], [3, 2], [0, 2]]] },
    { id: "back", polygons: [[[2, 0], [4, 0], [4, 2], [2, 2]]] },
  ];

  const layers = partitionSource(source, regions, "fallback");
  const overlapOffset = (source.width + 2) * 4;

  assert.deepEqual(
    [...layers.get("front").data.subarray(overlapOffset, overlapOffset + 4)],
    [...source.data.subarray(overlapOffset, overlapOffset + 4)],
  );
  assert.equal(layers.get("back").data[overlapOffset + 3], 0);

  for (let offset = 0; offset < source.data.length; offset += 4) {
    const owners = [...layers.values()].filter((layer) => layer.data[offset + 3] > 0);
    assert.equal(owners.length, source.data[offset + 3] > 0 ? 1 : 0);
    if (owners.length === 1) {
      assert.deepEqual(
        [...owners[0].data.subarray(offset, offset + 4)],
        [...source.data.subarray(offset, offset + 4)],
      );
    }
  }
});

test("underlaps duplicate only fully opaque source pixels beneath a neutral cover", () => {
  const source = smallSource();
  const regions = [
    {
      id: "moving",
      polygons: [[[0, 0], [1, 0], [1, 2], [0, 2]]],
      underlapPolygons: [[[1, 0], [3, 0], [3, 2], [1, 2]]],
    },
    { id: "cover", polygons: [[[1, 0], [3, 0], [3, 2], [1, 2]]] },
  ];
  const layers = partitionSource(source, regions, "fallback");

  applyUnderlaps(source, layers, regions);

  const translucent = (source.width + 1) * 4;
  const opaque = (source.width + 2) * 4;
  assert.equal(layers.get("moving").data[translucent + 3], 0);
  assert.deepEqual(
    [...layers.get("moving").data.subarray(opaque, opaque + 4)],
    [...source.data.subarray(opaque, opaque + 4)],
  );

  let composite = new PNG({ width: source.width, height: source.height });
  for (const id of ["fallback", "moving", "cover"]) composite = sourceOver(composite, layers.get(id));
  assert.deepEqual(composite.data, source.data);
});

test("covered underlaps reject opaque pixels without a higher-draw-order owner", () => {
  const source = smallSource();
  const regions = [
    {
      id: "moving",
      polygons: [[[0, 0], [1, 0], [1, 2], [0, 2]]],
      underlapPolygons: [[[1, 0], [4, 0], [4, 2], [1, 2]]],
    },
    { id: "cover", polygons: [[[1, 0], [3, 0], [3, 2], [1, 2]]] },
  ];
  const layers = partitionSource(source, regions, "fallback");
  const definitions = [
    { id: "fallback", drawOrder: 0 },
    { id: "moving", drawOrder: 10 },
    { id: "cover", drawOrder: 20 },
  ];

  assert.throws(
    () => applyCoveredUnderlaps(source, layers, regions, definitions),
    /underlap moving at x=3 y=1 is not beneath a fully opaque higher-draw-order cover/,
  );
});

test("builder validates exact layer-definition coverage and removes failed temporary output", async (t) => {
  const fixture = await productionFixture(t);
  const masks = validMasks();
  delete masks.layerDefinitions["head-base"];
  await writeFile(fixture.masksFile, JSON.stringify(masks));

  await assert.rejects(
    buildStandingRigV2(fixture),
    /layerDefinitions must describe every partition layer exactly once/,
  );
  assert.deepEqual(await builderTemps(fixture.outputDirectory), []);
  await assert.rejects(readFile(path.join(fixture.outputDirectory, "rig.json")), { code: "ENOENT" });
});

test("builder validates the temporary package, copies masks, and atomically promotes it", async (t) => {
  const fixture = await productionFixture(t);
  const inputMasks = await readFile(fixture.masksFile);

  const manifestPath = await buildStandingRigV2(fixture);
  const manifest = JSON.parse(await readFile(manifestPath, "utf8"));

  assert.equal(manifestPath, path.join(fixture.outputDirectory, "rig.json"));
  assert.equal(manifest.schemaVersion, 2);
  assert.deepEqual(manifest.layers.map((layer) => layer.id), ["torso", "head-base"]);
  assert.deepEqual(await readFile(path.join(fixture.outputDirectory, "masks.json")), inputMasks);
  assert.deepEqual(await builderTemps(fixture.outputDirectory), []);
});

test("builder preserves an existing validated v2 package until its replacement passes", async (t) => {
  const fixture = await productionFixture(t);
  await buildStandingRigV2(fixture);
  const originalManifest = await readFile(path.join(fixture.outputDirectory, "rig.json"));
  const masks = validMasks();
  masks.layerDefinitions["head-base"].parent = "missing";
  await writeFile(fixture.masksFile, JSON.stringify(masks));

  await assert.rejects(buildStandingRigV2(fixture), /unknown parent missing/);

  assert.deepEqual(await readFile(path.join(fixture.outputDirectory, "rig.json")), originalManifest);
  assert.deepEqual(await builderTemps(fixture.outputDirectory), []);
});

test("builder promotes over its masks-only production bootstrap directory", async (t) => {
  const fixture = await productionFixture(t);
  await mkdir(fixture.outputDirectory);
  const masksFile = path.join(fixture.outputDirectory, "masks.json");
  await writeFile(masksFile, await readFile(fixture.masksFile));

  const manifestPath = await buildStandingRigV2({ ...fixture, masksFile });

  assert.equal(manifestPath, path.join(fixture.outputDirectory, "rig.json"));
  assert.deepEqual(await builderTemps(fixture.outputDirectory), []);
});

test("builder refuses to replace an unrelated existing directory", async (t) => {
  const fixture = await productionFixture(t);
  await mkdir(fixture.outputDirectory);
  await writeFile(path.join(fixture.outputDirectory, "keep.txt"), "owner data\n");

  await assert.rejects(buildStandingRigV2(fixture), /refusing to replace unrecognized output directory/);

  assert.equal(await readFile(path.join(fixture.outputDirectory, "keep.txt"), "utf8"), "owner data\n");
  assert.deepEqual(await builderTemps(fixture.outputDirectory), []);
});

test("builder refuses every target path containing a standing-v1 component", async (t) => {
  for (const outputName of ["standing-v1", path.join("standing-v1", "nested")]) {
    const fixture = await productionFixture(t, outputName);
    await assert.rejects(buildStandingRigV2(fixture), /refusing to write into standing-v1/);
    assert.deepEqual(await builderTemps(fixture.outputDirectory), []);
  }
});

test("production masks declare the exact screen-relative visible hierarchy", async () => {
  const masks = JSON.parse(await readFile(path.resolve(
    import.meta.dirname,
    "../../../assets/brand/waffle/rigs/standing-v2/masks.json",
  ), "utf8"));
  const parents = Object.fromEntries(
    Object.entries(masks.layerDefinitions).map(([id, definition]) => [id, definition.parent]),
  );

  assert.deepEqual(parents, {
    "rear-paw-left": "rear-hock-left",
    "rear-hock-left": "rear-thigh-left",
    "rear-thigh-left": "waffle-root",
    "rear-paw-right": "rear-hock-right",
    "rear-hock-right": "rear-thigh-right",
    "rear-thigh-right": "waffle-root",
    "tail-tip": "tail-mid",
    "tail-mid": "tail-base",
    "tail-base": "waffle-root",
    torso: "waffle-root",
    "front-upper-left": "waffle-root",
    "front-lower-left": "front-upper-left",
    "front-paw-left": "front-lower-left",
    "front-upper-right": "waffle-root",
    "front-lower-right": "front-upper-right",
    "front-paw-right": "front-lower-right",
    "ear-left": "head-base",
    "ear-right": "head-base",
    "head-base": "waffle-root",
    muzzle: "head-base",
    "jaw-closed": "head-base",
    "iris-left": "head-base",
    "iris-right": "head-base",
    "pupil-left": "iris-left",
    "pupil-right": "iris-right",
    "highlight-left": "pupil-left",
    "highlight-right": "pupil-right",
    whiskers: "head-base",
  });
  assert.equal(masks.root.id, "waffle-root");
});
