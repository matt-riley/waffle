import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtemp, mkdir, readFile, readdir, rename, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { test } from "node:test";

import { PNG } from "pngjs";

import * as rigV2Art from "../build-waffle-rig-v2-art.mjs";

import {
  buildHybridLandingPlate,
  buildRigV2Art,
  extractBounded,
  maskToFullCatInterior,
} from "../build-waffle-rig-v2-art.mjs";
import { buildStandingRigV2 } from "../build-waffle-standing-rig-v2.mjs";
import { readRgba } from "../rig-raster.mjs";
import { validateRig } from "../validate-rig.mjs";

const CANVAS = Object.freeze({ width: 1536, height: 1024 });

function sha256Bytes(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

async function sha256(file) {
  return sha256Bytes(await readFile(file));
}

function png() {
  return new PNG({ ...CANVAS });
}

function paint(target, x, y, rgba) {
  target.data.set(rgba, (y * target.width + x) * 4);
}

function residualMagentaPixels(image, exempt) {
  let count = 0;
  for (let offset = 0; offset < image.data.length; offset += 4) {
    const red = image.data[offset];
    const green = image.data[offset + 1];
    const blue = image.data[offset + 2];
    const alpha = image.data[offset + 3];
    const sourceOwned = exempt
      && alpha <= exempt.data[offset + 3]
      && image.data.subarray(offset, offset + 3).equals(exempt.data.subarray(offset, offset + 3));
    if (!sourceOwned && alpha > 0 && red - green >= 16 && blue - green >= 16) count += 1;
  }
  return count;
}

function complementaryGreenPixels(image, exempt) {
  let count = 0;
  for (let offset = 0; offset < image.data.length; offset += 4) {
    const red = image.data[offset];
    const green = image.data[offset + 1];
    const blue = image.data[offset + 2];
    const alpha = image.data[offset + 3];
    const sourceOwned = exempt
      && alpha <= exempt.data[offset + 3]
      && image.data.subarray(offset, offset + 3).equals(exempt.data.subarray(offset, offset + 3));
    if (!sourceOwned
      && alpha > 0
      && alpha < 255
      && green >= 160
      && blue <= 25
      && green - red >= 25
      && green - blue >= 80) count += 1;
  }
  return count;
}

function innerEdgeHueCandidates(image, {
  endX = image.width - 1,
  endY = image.height - 1,
  startX = 0,
  startY = 0,
} = {}) {
  const candidates = [];
  for (let y = startY; y <= endY; y += 1) {
    let innerEdgeX = -1;
    for (let x = startX; x <= endX; x += 1) {
      if (image.data[(y * image.width + x) * 4 + 3] > 8) {
        innerEdgeX = x;
        break;
      }
    }
    if (innerEdgeX < 0) continue;
    for (let x = innerEdgeX; x <= Math.min(innerEdgeX + 3, endX); x += 1) {
      const offset = (y * image.width + x) * 4;
      const red = image.data[offset];
      const green = image.data[offset + 1];
      const blue = image.data[offset + 2];
      const alpha = image.data[offset + 3];
      if (alpha > 8 && red - green >= 70 && blue + 25 >= green) candidates.push({ x, y });
    }
  }
  return candidates;
}

function alphaIslandSizes(image, threshold = 8) {
  const visited = new Uint8Array(image.width * image.height);
  const sizes = [];
  const neighbours = [
    [-1, -1], [0, -1], [1, -1],
    [-1, 0], [1, 0],
    [-1, 1], [0, 1], [1, 1],
  ];
  for (let start = 0; start < visited.length; start += 1) {
    if (visited[start] || image.data[start * 4 + 3] <= threshold) continue;
    let size = 0;
    const pending = [start];
    visited[start] = 1;
    while (pending.length > 0) {
      const pixel = pending.pop();
      size += 1;
      const x = pixel % image.width;
      const y = Math.floor(pixel / image.width);
      for (const [dx, dy] of neighbours) {
        const nextX = x + dx;
        const nextY = y + dy;
        if (nextX < 0 || nextY < 0 || nextX >= image.width || nextY >= image.height) continue;
        const next = nextY * image.width + nextX;
        if (visited[next] || image.data[next * 4 + 3] <= threshold) continue;
        visited[next] = 1;
        pending.push(next);
      }
    }
    sizes.push(size);
  }
  return sizes.toSorted((left, right) => right - left);
}

function alphaBounds(image, threshold = 8) {
  let minX = image.width;
  let minY = image.height;
  let maxX = -1;
  let maxY = -1;
  for (let y = 0; y < image.height; y += 1) {
    for (let x = 0; x < image.width; x += 1) {
      if (image.data[(y * image.width + x) * 4 + 3] <= threshold) continue;
      minX = Math.min(minX, x);
      minY = Math.min(minY, y);
      maxX = Math.max(maxX, x);
      maxY = Math.max(maxY, y);
    }
  }
  return { minX, minY, maxX, maxY, centerX: (minX + maxX) / 2 };
}

function artRecoveryPaths(outputDirectory) {
  return {
    marker: `${outputDirectory}.art-promotion.json`,
    previous: `${outputDirectory}.art-previous-${process.pid}`,
    temporary: `${outputDirectory}.art-building-${process.pid}`,
  };
}

async function workspace(t) {
  const directory = await mkdtemp(path.join(tmpdir(), "waffle-rig-v2-art-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  return directory;
}

async function packageSnapshot(directory) {
  const entries = [];
  async function visit(current) {
    for (const entry of (await readdir(current, { withFileTypes: true })).toSorted((a, b) => a.name.localeCompare(b.name))) {
      const file = path.join(current, entry.name);
      if (entry.isDirectory()) await visit(file);
      else entries.push([path.relative(directory, file), sha256Bytes(await readFile(file))]);
    }
  }
  await visit(directory);
  return entries;
}

function masks() {
  return {
    canvas: { ...CANVAS },
    root: { id: "waffle-root", pivot: { x: 0.5, y: 0.75 } },
    regionsFrontToBack: [{
      id: "head-base",
      polygons: [[[700, 500], [702, 500], [702, 502], [700, 502]]],
    }],
    fallback: "torso",
    layerDefinitions: {
      torso: {
        parent: "waffle-root",
        drawOrder: 100,
        pivot: { x: 0.5, y: 0.7 },
        limits: { y: { min: -0.01, max: 0.01 } },
      },
      "head-base": {
        parent: "torso",
        drawOrder: 200,
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
      headTurn: {
        min: -1,
        max: 1,
        bindings: [{
          variant: "head-base",
          thresholds: [
            { max: -0.5, member: "turn-left" },
            { max: 1, member: "neutral" },
          ],
        }],
      },
    },
  };
}

async function fixture(t) {
  const root = await workspace(t);
  const waffleDirectory = path.join(root, "assets", "brand", "waffle");
  const sourceFile = path.join(waffleDirectory, "poses", "standing.png");
  const outputDirectory = path.join(waffleDirectory, "rigs", "standing-v2");
  const masksFile = path.join(root, "inputs", "masks.json");
  const plateFile = path.join(root, "inputs", "head-turn-left.png");
  await Promise.all([
    mkdir(path.dirname(sourceFile), { recursive: true }),
    mkdir(path.dirname(masksFile), { recursive: true }),
    mkdir(path.dirname(outputDirectory), { recursive: true }),
  ]);
  const source = png();
  paint(source, 650, 500, [180, 90, 30, 255]);
  paint(source, 700, 500, [210, 130, 45, 255]);
  paint(source, 701, 500, [220, 140, 55, 255]);
  const plate = PNG.sync.read(PNG.sync.write(source));
  paint(plate, 699, 500, [170, 100, 40, 255]);
  paint(plate, 700, 500, [180, 110, 50, 255]);
  paint(plate, 701, 500, [190, 120, 60, 255]);
  await Promise.all([
    writeFile(sourceFile, PNG.sync.write(source)),
    writeFile(plateFile, PNG.sync.write(plate)),
    writeFile(masksFile, `${JSON.stringify(masks(), null, 2)}\n`),
  ]);
  await buildStandingRigV2({ sourceFile, masksFile, outputDirectory });
  const manifestPath = path.join(outputDirectory, "rig.json");
  const sourceHash = await sha256(sourceFile);
  const repairsFile = path.join(outputDirectory, "repairs.json");
  const variantsFile = path.join(outputDirectory, "variants.json");
  const repairInput = {
    kind: "source-sample",
    file: "../../poses/standing.png",
    sourceSha256: sourceHash,
    canvas: { ...CANVAS },
    expectedId: "neck-repair",
    crop: { x: 699, y: 499, width: 4, height: 4 },
  };
  const repairs = {
    schemaVersion: 1,
    source: { file: "../../poses/standing.png", sha256: sourceHash },
    canvas: { ...CANVAS },
    repairs: [{
      id: "neck-repair",
      cover: "head-base",
      parent: "torso",
      drawOrder: 190,
      pivot: { x: 0.46, y: 0.5 },
      visibleAtNeutral: false,
      allowOutsideNeutralCover: false,
      polygons: [[[700, 500], [702, 500], [702, 502], [700, 502]]],
      input: repairInput,
    }],
  };
  const variants = {
    schemaVersion: 1,
    source: { file: "../../poses/standing.png", sha256: sourceHash },
    canvas: { ...CANVAS },
    sets: [{
      id: "head-base",
      anchorLayer: "head-base",
      registrationPivot: { x: 0.46, y: 0.5 },
      neutralMember: "neutral",
      members: [{
        id: "neutral",
        neutral: true,
        polygons: [[[700, 500], [702, 500], [702, 502], [700, 502]]],
        input: {
          kind: "anchor-layer",
          layer: "head-base",
          sourceSha256: sourceHash,
          canvas: { ...CANVAS },
          expectedVariantId: "head-base/neutral",
          crop: { x: 699, y: 499, width: 4, height: 4 },
        },
      }, {
        id: "turn-left",
        neutral: false,
        polygons: [[[699, 499], [703, 499], [703, 503], [699, 503]]],
        input: {
          kind: "edit-plate",
          file: path.relative(outputDirectory, plateFile),
          sha256: await sha256(plateFile),
          sourceSha256: sourceHash,
          canvas: { ...CANVAS },
          expectedVariantId: "head-base/turn-left",
          crop: { x: 699, y: 499, width: 4, height: 4 },
          constrainToAnchorAlpha: true,
        },
      }],
    }],
  };
  await Promise.all([
    writeFile(repairsFile, `${JSON.stringify(repairs, null, 2)}\n`),
    writeFile(variantsFile, `${JSON.stringify(variants, null, 2)}\n`),
  ]);
  return {
    manifestPath,
    outputDirectory,
    plateFile,
    repairs,
    repairsFile,
    sourceFile,
    sourceHash,
    variants,
    variantsFile,
  };
}

test("extractBounded copies only pixels inside both the declared crop and polygons", () => {
  const source = new PNG({ width: 6, height: 4 });
  for (let y = 0; y < source.height; y += 1) {
    for (let x = 0; x < source.width; x += 1) paint(source, x, y, [x * 20, y * 30, 40, 255]);
  }

  const extracted = extractBounded(source, {
    crop: { x: 1, y: 1, width: 4, height: 2 },
    polygons: [[[2, 1], [4, 1], [4, 3], [2, 3]]],
  });

  assert.deepEqual([...extracted.data.subarray((1 * 6 + 2) * 4, (1 * 6 + 2) * 4 + 4)], [40, 30, 40, 255]);
  assert.equal(extracted.data[(1 * 6 + 1) * 4 + 3], 0);
  assert.equal(extracted.data[(1 * 6 + 4) * 4 + 3], 0);
  assert.equal(extracted.data[(0 * 6 + 2) * 4 + 3], 0);
});

test("extractBounded removes a declared flat chroma key without affecting bounded subject pixels", () => {
  const source = new PNG({ width: 4, height: 3 });
  for (let offset = 0; offset < source.data.length; offset += 4) source.data.set([255, 0, 255, 255], offset);
  paint(source, 2, 1, [220, 140, 55, 255]);

  const extracted = extractBounded(source, {
    crop: { x: 1, y: 0, width: 3, height: 3 },
    polygons: [[[1, 0], [4, 0], [4, 3], [1, 3]]],
    chromaKey: { rgb: [255, 0, 255], transparentThreshold: 12, opaqueThreshold: 220 },
  });

  assert.equal(extracted.data[(1 * 4 + 1) * 4 + 3], 0);
  assert.deepEqual([...extracted.data.subarray((1 * 4 + 2) * 4, (1 * 4 + 2) * 4 + 4)], [220, 140, 55, 255]);
});

test("extractBounded unmixed antialiased key edges and zeros transparent RGB", () => {
  const source = new PNG({ width: 5, height: 3 });
  for (let offset = 0; offset < source.data.length; offset += 4) source.data.set([255, 0, 255, 255], offset);
  paint(source, 2, 1, [236, 48, 224, 255]);
  paint(source, 3, 1, [220, 140, 55, 255]);

  const extracted = extractBounded(source, {
    crop: { x: 0, y: 0, width: 5, height: 3 },
    polygons: [[[0, 0], [5, 0], [5, 3], [0, 3]]],
    chromaKey: { rgb: [255, 0, 255], transparentThreshold: 12, opaqueThreshold: 220 },
  });

  const edge = [...extracted.data.subarray((1 * 5 + 2) * 4, (1 * 5 + 2) * 4 + 4)];
  assert.ok(edge[3] > 0 && edge[3] < 255, `expected partial edge alpha, got ${edge}`);
  assert.ok(edge[0] - edge[1] < 16 || edge[2] - edge[1] < 16, `residual magenta edge ${edge}`);
  assert.equal(complementaryGreenPixels(extracted), 0, `complementary green edge ${edge}`);
  assert.deepEqual([...extracted.data.subarray(0, 4)], [0, 0, 0, 0]);
  assert.equal(residualMagentaPixels(extracted), 0);
});

test("extractBounded feathers a bounded edit before it is blended into its anchor", () => {
  const source = new PNG({ width: 7, height: 7 });
  for (let offset = 0; offset < source.data.length; offset += 4) source.data.set([180, 90, 30, 255], offset);

  const extracted = extractBounded(source, {
    crop: { x: 1, y: 1, width: 5, height: 5 },
    polygons: [[[1, 1], [6, 1], [6, 6], [1, 6]]],
    edgeFeatherPixels: 2,
  });

  assert.equal(extracted.data[(1 * 7 + 1) * 4 + 3], 0);
  assert.ok(extracted.data[(2 * 7 + 2) * 4 + 3] > 0);
  assert.equal(extracted.data[(3 * 7 + 3) * 4 + 3], 255);
});

test("maskToFullCatInterior can preserve a tightly declared source-boundary relief", () => {
  const fullCat = new PNG({ width: 7, height: 7 });
  for (let y = 2; y <= 4; y += 1) {
    for (let x = 2; x <= 4; x += 1) paint(fullCat, x, y, [180, 90, 30, 255]);
  }
  const support = PNG.sync.read(PNG.sync.write(fullCat));

  maskToFullCatInterior(support, fullCat, 2, [[[1, 2], [3, 2], [3, 5], [1, 5]]]);

  assert.deepEqual([...support.data.subarray((3 * 7 + 2) * 4, (3 * 7 + 2) * 4 + 4)], [180, 90, 30, 255]);
  assert.deepEqual([...support.data.subarray((3 * 7 + 4) * 4, (3 * 7 + 4) * 4 + 4)], [0, 0, 0, 0]);
});

test("maskToFullCatInterior feathers the interior edge of a source-boundary relief", () => {
  const fullCat = new PNG({ width: 9, height: 9 });
  for (let y = 2; y <= 6; y += 1) {
    for (let x = 2; x <= 6; x += 1) paint(fullCat, x, y, [180, 90, 30, 255]);
  }
  const support = PNG.sync.read(PNG.sync.write(fullCat));

  maskToFullCatInterior(support, fullCat, 2, [[[1, 1], [5, 1], [5, 8], [1, 8]]], 1);

  const featheredAlpha = support.data[(4 * 9 + 2) * 4 + 3];
  assert.ok(featheredAlpha > 0 && featheredAlpha < 255, `expected feathered relief alpha, got ${featheredAlpha}`);
  assert.equal(support.data[(4 * 9 + 3) * 4 + 3], 255);
  assert.equal(support.data[(4 * 9 + 6) * 4 + 3], 0);
});

test("hybrid landing plate preserves its lifted start, blends premultiplied color and transparency, and locks the neutral distal exactly", () => {
  const lowLift = new PNG({ width: 8, height: 8 });
  const neutral = new PNG({ width: 8, height: 8 });
  for (let y = 0; y <= 4; y += 1) {
    for (let x = 1; x <= 3; x += 1) paint(lowLift, x, y, [180, 90, 30, 255]);
  }
  for (let y = 3; y < 8; y += 1) {
    for (let x = 3; x <= 5; x += 1) paint(neutral, x, y, [220, 140, 55, 255]);
  }
  paint(neutral, 7, 7, [255, 0, 255, 255]);

  const landing = buildHybridLandingPlate(lowLift, neutral, {
    baseOffsetPixels: { x: 1, y: 0 },
    seamY: 5,
    transitionStartY: 3,
    neutralDistalPolygons: [[[3, 3], [6, 3], [6, 8], [3, 8]]],
  });

  assert.deepEqual([...landing.data.subarray((2 * 8 + 1) * 4, (2 * 8 + 1) * 4 + 4)], [0, 0, 0, 0]);
  assert.deepEqual([...landing.data.subarray((2 * 8 + 3) * 4, (2 * 8 + 3) * 4 + 4)], [180, 90, 30, 255]);
  assert.deepEqual([...landing.data.subarray((3 * 8 + 4) * 4, (3 * 8 + 4) * 4 + 4)], [180, 90, 30, 255]);
  assert.deepEqual([...landing.data.subarray((3 * 8 + 5) * 4, (3 * 8 + 5) * 4 + 4)], [0, 0, 0, 0]);
  assert.deepEqual([...landing.data.subarray((4 * 8 + 2) * 4, (4 * 8 + 2) * 4 + 4)], [180, 90, 30, 128]);
  assert.deepEqual([...landing.data.subarray((4 * 8 + 4) * 4, (4 * 8 + 4) * 4 + 4)], [200, 115, 43, 255]);
  assert.deepEqual([...landing.data.subarray((4 * 8 + 5) * 4, (4 * 8 + 5) * 4 + 4)], [220, 140, 55, 128]);
  for (let y = 5; y < 8; y += 1) {
    for (let x = 0; x < 8; x += 1) {
      const offset = (y * 8 + x) * 4;
      const expected = x >= 3 && x <= 5 ? neutral.data.subarray(offset, offset + 4) : new Uint8Array(4);
      assert.deepEqual([...landing.data.subarray(offset, offset + 4)], [...expected], `neutral distal mismatch at ${x},${y}`);
    }
  }
  assert.deepEqual(alphaIslandSizes(landing), [25]);
});

test("inner-edge despill copies a bounded tabby hue while preserving alpha and every undeclared pixel", () => {
  const image = new PNG({ width: 12, height: 5 });
  for (let x = 2; x <= 9; x += 1) paint(image, x, 2, [220, 140, 55, 255]);
  paint(image, 2, 2, [210, 80, 70, 20]);
  for (let x = 3; x <= 5; x += 1) paint(image, x, 2, [210, 80, 70, 255]);
  paint(image, 7, 2, [205, 75, 65, 255]);
  paint(image, 2, 1, [210, 80, 70, 255]);
  for (let x = 2; x <= 9; x += 1) paint(image, x, 3, [205, 75, 65, 128]);
  const before = PNG.sync.read(PNG.sync.write(image));

  const corrected = rigV2Art.despillInnerEdge(image, {
    bounds: { x: 2, y: 2, width: 8, height: 2 },
    edgeDepthPixels: 4,
    sampleSearchPixels: 8,
    alphaThreshold: 8,
    redOverGreenMinimum: 70,
    blueBelowGreenMaximum: 25,
    minimumSampleAlpha: 240,
  });

  for (let offset = 3; offset < corrected.data.length; offset += 4) {
    assert.equal(corrected.data[offset], before.data[offset], `alpha drift at pixel ${Math.floor(offset / 4)}`);
  }
  for (let x = 2; x <= 5; x += 1) {
    const offset = (2 * image.width + x) * 4;
    assert.deepEqual([...corrected.data.subarray(offset, offset + 3)], [220, 140, 55]);
  }
  for (let x = 2; x <= 5; x += 1) {
    const offset = (3 * image.width + x) * 4;
    assert.deepEqual(
      [...corrected.data.subarray(offset, offset + 3)],
      [220, 140, 55],
      "a fully contaminated edge row must use the nearest neighbouring-row tabby sample",
    );
  }
  for (let pixel = 0; pixel < image.width * image.height; pixel += 1) {
    const x = pixel % image.width;
    const y = Math.floor(pixel / image.width);
    if ((y === 2 || y === 3) && x >= 2 && x <= 5) continue;
    const offset = pixel * 4;
    assert.deepEqual(
      [...corrected.data.subarray(offset, offset + 4)],
      [...before.data.subarray(offset, offset + 4)],
      `undeclared pixel drift at ${x},${y}`,
    );
  }
  assert.deepEqual(image.data, before.data, "despill must not mutate its input raster");
});

test("builder extracts registered variants, preserves the neutral anchor exactly, and keeps repairs out of neutral", async (t) => {
  const inputs = await fixture(t);
  const originalAnchor = await readRgba(path.join(inputs.outputDirectory, "layers", "head-base.png"));
  const repairsBytes = await readFile(inputs.repairsFile);
  const variantsBytes = await readFile(inputs.variantsFile);

  const result = await buildRigV2Art(inputs);
  const manifest = JSON.parse(await readFile(inputs.manifestPath, "utf8"));
  const neutral = await readRgba(path.join(inputs.outputDirectory, "variants", "head-base", "neutral.png"));
  const turnLeft = await readRgba(path.join(inputs.outputDirectory, "variants", "head-base", "turn-left.png"));
  const repair = await readRgba(path.join(inputs.outputDirectory, "layers", "neck-repair.png"));

  assert.deepEqual(result, { layerCount: 3, mismatchPixels: 0 });
  assert.deepEqual(neutral.data, originalAnchor.data);
  assert.equal(turnLeft.data[(500 * CANVAS.width + 700) * 4], 180);
  assert.equal(turnLeft.data[(500 * CANVAS.width + 699) * 4 + 3], 0);
  assert.equal(repair.data[(500 * CANVAS.width + 650) * 4 + 3], 0);
  assert.equal(manifest.layers.find((layer) => layer.id === "neck-repair").visibleAtNeutral, false);
  assert.equal(manifest.layers.find((layer) => layer.id === "head-base").role, "variant-anchor");
  assert.equal(manifest.variants["head-base"].members.filter((member) => member.neutral).length, 1);
  assert.deepEqual(manifest.controls, masks().controls, "art pass must restore authoritative controls from masks.json");
  assert.deepEqual(await readFile(path.join(inputs.outputDirectory, "repairs.json")), repairsBytes);
  assert.deepEqual(await readFile(path.join(inputs.outputDirectory, "variants.json")), variantsBytes);
  assert.deepEqual(await validateRig(inputs.manifestPath), { layerCount: 3, mismatchPixels: 0 });
});

test("builder preserves clip-only variant metadata in the generated rig", async (t) => {
  const inputs = await fixture(t);
  const turnLeft = inputs.variants.sets[0].members[1];
  inputs.variants.sets[0].members.push({
    ...structuredClone(turnLeft),
    id: "blink-left",
    clipOnly: true,
    input: {
      ...structuredClone(turnLeft.input),
      expectedVariantId: "head-base/blink-left",
    },
  });
  await writeFile(inputs.variantsFile, `${JSON.stringify(inputs.variants, null, 2)}\n`);

  await buildRigV2Art(inputs);

  const manifest = JSON.parse(await readFile(inputs.manifestPath, "utf8"));
  const blink = manifest.variants["head-base"].members.find(({ id }) => id === "blink-left");
  assert.equal(blink.clipOnly, true);
});

test("builder can preserve source-owned anchor pixels outside a bounded variant edit", async (t) => {
  const inputs = await fixture(t);
  inputs.variants.sets[0].members[1].polygons = [[[700, 500], [701, 500], [701, 501], [700, 501]]];
  inputs.variants.sets[0].members[1].input.preserveAnchorOutsidePolygons = true;
  await writeFile(inputs.variantsFile, `${JSON.stringify(inputs.variants, null, 2)}\n`);

  await buildRigV2Art(inputs);

  const turnLeft = await readRgba(path.join(inputs.outputDirectory, "variants", "head-base", "turn-left.png"));
  assert.equal(turnLeft.data[(500 * CANVAS.width + 700) * 4], 180);
  assert.equal(turnLeft.data[(500 * CANVAS.width + 701) * 4], 220);
});

test("builder rejects mismatched source hash, canvas, declared crop, edit-plate hash, and expected variant ID without touching the package", async (t) => {
  const cases = [
    ["source hash", (inputs) => { inputs.variants.source.sha256 = "0".repeat(64); }, /source sha256 mismatch/],
    ["canvas", (inputs) => { inputs.variants.sets[0].members[1].input.canvas.width = 1535; }, /input canvas must be exactly 1536x1024/],
    ["crop", (inputs) => { inputs.variants.sets[0].members[1].input.crop = { x: 700, y: 500, width: 1, height: 1 }; }, /polygon must stay inside declared crop/],
    ["edit plate hash", (inputs) => { inputs.variants.sets[0].members[1].input.sha256 = "0".repeat(64); }, /sha256 mismatch for edit plate/],
    ["variant ID", (inputs) => { inputs.variants.sets[0].members[1].input.expectedVariantId = "head-base/turn-right"; }, /expected variant id must be head-base\/turn-left/],
  ];

  for (const [label, mutate, pattern] of cases) {
    await t.test(label, async (subtest) => {
      const inputs = await fixture(subtest);
      const before = await packageSnapshot(inputs.outputDirectory);
      mutate(inputs);
      await writeFile(inputs.variantsFile, `${JSON.stringify(inputs.variants, null, 2)}\n`);

      await assert.rejects(buildRigV2Art(inputs), pattern);

      assert.deepEqual(await packageSnapshot(inputs.outputDirectory), before.slice(0, -1).concat([
        ["variants.json", sha256Bytes(await readFile(inputs.variantsFile))],
      ]).toSorted((a, b) => a[0].localeCompare(b[0])));
      assert.deepEqual((await readdir(path.dirname(inputs.outputDirectory)))
        .filter((entry) => entry.includes(".art-building-") || entry.includes(".art-previous-")), []);
    });
  }
});

test("builder writes deterministic hashes and outputs when repeated from an approved package", async (t) => {
  const inputs = await fixture(t);
  await buildRigV2Art(inputs);
  const first = await packageSnapshot(inputs.outputDirectory);

  await buildRigV2Art(inputs);
  const second = await packageSnapshot(inputs.outputDirectory);

  assert.deepEqual(second, first);
});

test("startup restores the approved package after promotion and immediate restoration both fail", async (t) => {
  const inputs = await fixture(t);
  const originalManifest = await readFile(inputs.manifestPath);
  const paths = artRecoveryPaths(inputs.outputDirectory);
  const invalidVariantsFile = path.join(path.dirname(inputs.outputDirectory), "invalid-variants.json");
  inputs.variants.source.sha256 = "0".repeat(64);
  await writeFile(invalidVariantsFile, `${JSON.stringify(inputs.variants, null, 2)}\n`);

  await assert.rejects(buildRigV2Art({
    ...inputs,
    renamePath: async (from, to) => {
      if (from === paths.temporary && to === inputs.outputDirectory) throw new Error("injected art promotion failure");
      if (from === paths.previous && to === inputs.outputDirectory) throw new Error("injected art restoration failure");
      return rename(from, to);
    },
  }), /injected art restoration failure/);

  await assert.rejects(readFile(inputs.manifestPath), { code: "ENOENT" });
  assert.deepEqual(await readFile(path.join(paths.previous, "rig.json")), originalManifest);
  assert.equal(JSON.parse(await readFile(paths.marker, "utf8")).previousDirectory, path.basename(paths.previous));

  await assert.rejects(buildRigV2Art({ ...inputs, variantsFile: invalidVariantsFile }), /source sha256 mismatch/);

  assert.deepEqual(await readFile(inputs.manifestPath), originalManifest);
  await assert.rejects(readFile(paths.marker), { code: "ENOENT" });
  await assert.rejects(readFile(path.join(paths.previous, "rig.json")), { code: "ENOENT" });
});

test("production art package has the complete registered inventory and no residual key pixels", async () => {
  const productionDirectory = path.resolve(
    import.meta.dirname,
    "../../../assets/brand/waffle/rigs/standing-v2",
  );
  const [manifest, repairs, variants, sourceImage] = await Promise.all([
    readFile(path.join(productionDirectory, "rig.json"), "utf8").then(JSON.parse),
    readFile(path.join(productionDirectory, "repairs.json"), "utf8").then(JSON.parse),
    readFile(path.join(productionDirectory, "variants.json"), "utf8").then(JSON.parse),
    readRgba(path.resolve(productionDirectory, "../../poses/standing.png")),
  ]);
  const expectedRepairs = [
    "body-repair", "neck-repair",
    "front-shoulder-repair-left", "front-shoulder-repair-right",
    "rear-hip-repair-left", "rear-hip-repair-right",
    "front-elbow-repair-left", "front-elbow-repair-right",
    "rear-hock-repair-left", "rear-hock-repair-right",
    "front-wrist-repair-left", "front-wrist-repair-right",
    "rear-paw-root-repair-left", "rear-paw-root-repair-right",
    "tail-base-mid-repair", "tail-mid-tip-repair",
    "walk-socket-front-left", "walk-socket-front-right",
    "walk-socket-rear-left", "walk-socket-rear-right",
    "paw-wave-chest-cover-left",
    "walk-cover-front-left", "walk-cover-front-right",
  ].toSorted();
  const expectedOverlays = ["upper-lid-left", "lower-lid-left", "upper-lid-right", "lower-lid-right"].toSorted();
  const expectedMembers = {
    "front-chain-left": ["neutral", "low-lift", "landing", "paw-lifted", "paw-wave", "paw-landing"],
    "front-chain-right": ["neutral", "low-lift", "landing"],
    "rear-chain-left": ["neutral", "low-lift", "landing"],
    "rear-chain-right": ["neutral", "low-lift", "landing"],
    "front-paw-left": ["planted", "lifted", "wave"],
    "front-paw-right": ["planted", "lifted"],
    "rear-paw-left": ["planted", "lifted"],
    "rear-paw-right": ["planted", "lifted"],
    "head-base": ["neutral", "turn-left", "turn-right", "blink-left", "blink-right"],
    jaw: ["closed", "open"],
  };
  const replacedHeadChildren = [
    "ear-left", "ear-right", "muzzle", "jaw-closed",
    "iris-left", "iris-right", "pupil-left", "pupil-right",
    "highlight-left", "highlight-right", "whiskers",
    "upper-lid-left", "lower-lid-left", "upper-lid-right", "lower-lid-right",
  ].toSorted();
  const replacedChainChildren = {
    "front-chain-left": ["front-lower-left", "front-paw-left", "front-elbow-repair-left", "front-wrist-repair-left"],
    "front-chain-right": ["front-lower-right", "front-paw-right", "front-elbow-repair-right", "front-wrist-repair-right"],
    "rear-chain-left": ["rear-hock-left", "rear-paw-left", "rear-hock-repair-left", "rear-paw-root-repair-left"],
    "rear-chain-right": ["rear-hock-right", "rear-paw-right", "rear-hock-repair-right", "rear-paw-root-repair-right"],
  };

  assert.deepEqual(repairs.repairs.map((entry) => entry.id).toSorted(), expectedRepairs);
  assert.deepEqual(repairs.overlays.map((entry) => entry.id).toSorted(), expectedOverlays);
  assert.deepEqual(variants.sets.map((entry) => entry.id).toSorted(), Object.keys(expectedMembers).toSorted());

  const layerById = new Map(manifest.layers.map((layer) => [layer.id, layer]));
  for (const specification of [...repairs.repairs, ...repairs.overlays]) {
    const layer = layerById.get(specification.id);
    assert.ok(layer, `missing production layer ${specification.id}`);
    assert.equal(layer.parent, specification.parent);
    assert.equal(layer.drawOrder, specification.drawOrder);
    assert.deepEqual(layer.pivot, specification.pivot);
    assert.equal(layer.role, specification.role ?? "repair");
    assert.equal(layer.visibleAtNeutral, false);
  }

  for (const specification of variants.sets) {
    const actual = manifest.variants[specification.id];
    const anchor = layerById.get(specification.anchorLayer);
    assert.ok(actual && anchor, `missing production variant set ${specification.id}`);
    assert.deepEqual(specification.registrationPivot, anchor.pivot);
    assert.deepEqual(actual.members.map((member) => member.id), expectedMembers[specification.id]);
    assert.equal(actual.members.filter((member) => member.neutral).length, 1);
    assert.equal(actual.members.find((member) => member.neutral).id, specification.neutralMember);
    if (specification.id === "head-base") {
      assert.equal(actual.members.find((member) => member.neutral).layerOverrides, undefined);
      for (const member of actual.members.filter((entry) => !entry.neutral)) {
        assert.deepEqual(Object.keys(member.layerOverrides).toSorted(), replacedHeadChildren);
        assert.ok(Object.values(member.layerOverrides).every((override) => override.visible === false));
      }
    }
    if (Object.hasOwn(replacedChainChildren, specification.id)) {
      const neutralMember = actual.members.find((member) => member.neutral);
      assert.equal(neutralMember.layerOverrides, undefined, `${specification.id} neutral must remain override-free`);
      assert.equal(neutralMember.parentOverride, undefined, `${specification.id} neutral must retain its authored hierarchy`);
      for (const member of actual.members.filter((candidate) => !candidate.neutral)) {
        assert.equal(
          member.parentOverride,
          member.id === "landing" ? undefined : "torso",
          `${specification.id}/${member.id} parent space`,
        );
        assert.deepEqual(Object.keys(member.layerOverrides).toSorted(), replacedChainChildren[specification.id].toSorted());
        assert.ok(Object.values(member.layerOverrides).every((override) => override.visible === false));
        const image = await readRgba(path.join(productionDirectory, member.file));
        const sourceExempt = member.id === "landing" || member.id === "paw-landing" ? sourceImage : undefined;
        assert.equal(residualMagentaPixels(image, sourceExempt), 0, `${specification.id}/${member.id} residual chroma`);
        assert.equal(complementaryGreenPixels(image, sourceExempt), 0, `${specification.id}/${member.id} complementary chroma`);
        const islands = alphaIslandSizes(image);
        assert.equal(islands.filter((size) => size >= 64).length, 1, `${specification.id}/${member.id} must have one coherent limb and no duplicate art island`);
        assert.ok(islands[0] >= 8_000 && islands[0] <= 70_000, `${specification.id}/${member.id} must stay tightly limb-bounded`);
        assert.ok((islands[1] ?? 0) < 64, `${specification.id}/${member.id} may only retain tiny isolated fur-edge wisps`);
        const bounds = alphaBounds(image);
        const screenDivider = specification.id.startsWith("front-") ? 620 : 920;
        assert.equal(bounds.centerX < screenDivider, specification.id.endsWith("-left"), `${specification.id}/${member.id} screen-side ownership`);
      }
    }
    const neutral = actual.members.find((member) => member.neutral);
    const anchorImage = await readRgba(path.join(productionDirectory, anchor.file));
    assert.deepEqual(
      (await readRgba(path.join(productionDirectory, neutral.file))).data,
      anchorImage.data,
      `neutral anchor drift for ${specification.id}`,
    );
    for (const member of actual.members.filter((entry) => !entry.neutral)) {
      const image = await readRgba(path.join(productionDirectory, member.file));
      const exempt = member.id === "landing" || member.id === "paw-landing" ? sourceImage : anchorImage;
      assert.equal(residualMagentaPixels(image, exempt), 0, `residual chroma in ${specification.id}/${member.id}`);
      assert.equal(complementaryGreenPixels(image, exempt), 0, `complementary chroma in ${specification.id}/${member.id}`);
    }
  }
});

test("production paw edit plates declare alpha-island cleanup only where detached source artifacts exist", async () => {
  const productionDirectory = path.resolve(
    import.meta.dirname,
    "../../../assets/brand/waffle/rigs/standing-v2",
  );
  const variants = JSON.parse(await readFile(path.join(productionDirectory, "variants.json"), "utf8"));
  const expectedCleanup = new Set([
    "front-paw-left/lifted",
    "front-paw-left/wave",
    "front-paw-right/lifted",
  ]);

  for (const set of variants.sets.filter(({ id }) => id === "front-paw-left" || id === "front-paw-right")) {
    for (const member of set.members) {
      const id = `${set.id}/${member.id}`;
      assert.equal(
        member.input.removeAlphaIslandsBelowPixels,
        expectedCleanup.has(id) ? 2_000 : undefined,
        `${id} cleanup threshold`,
      );
    }
  }
});

test("production paw cleanup removes detached plate artifacts without removing the meaningful paw", async () => {
  const productionDirectory = path.resolve(
    import.meta.dirname,
    "../../../assets/brand/waffle/rigs/standing-v2",
  );
  const manifest = JSON.parse(await readFile(path.join(productionDirectory, "rig.json"), "utf8"));

  for (const [setId, memberId] of [
    ["front-paw-left", "lifted"],
    ["front-paw-left", "wave"],
    ["front-paw-right", "lifted"],
  ]) {
    const member = manifest.variants[setId].members.find(({ id }) => id === memberId);
    const image = await readRgba(path.join(productionDirectory, member.file));
    const islands = alphaIslandSizes(image, 0);
    assert.equal(islands.length, 1, `${setId}/${memberId} must contain only the meaningful paw island`);
    assert.ok(islands[0] > 10_000, `${setId}/${memberId} meaningful paw was removed or truncated`);
  }
});

test("production screen-right head turn keeps the complete outer ear inside its extraction polygon", async () => {
  const productionDirectory = path.resolve(
    import.meta.dirname,
    "../../../assets/brand/waffle/rigs/standing-v2",
  );
  const manifest = JSON.parse(await readFile(path.join(productionDirectory, "rig.json"), "utf8"));
  const member = manifest.variants["head-base"].members.find(({ id }) => id === "turn-right");
  const image = await readRgba(path.join(productionDirectory, member.file));
  let outerEarPixels = 0;
  for (let y = 100; y <= 260; y += 1) {
    for (let x = 805; x <= 850; x += 1) {
      if (image.data[(y * image.width + x) * 4 + 3] > 8) outerEarPixels += 1;
    }
  }
  assert.ok(outerEarPixels > 250, `screen-right ear is vertically clipped (${outerEarPixels} retained pixels)`);
});

test("production painted head states retain connected neck and upper-shoulder coverage", async () => {
  const productionDirectory = path.resolve(
    import.meta.dirname,
    "../../../assets/brand/waffle/rigs/standing-v2",
  );
  const manifest = JSON.parse(await readFile(path.join(productionDirectory, "rig.json"), "utf8"));
  for (const memberId of ["turn-left", "turn-right", "blink-left", "blink-right"]) {
    const member = manifest.variants["head-base"].members.find(({ id }) => id === memberId);
    const image = await readRgba(path.join(productionDirectory, member.file));
    const alpha = image.data[(440 * image.width + 740) * 4 + 3];
    assert.ok(alpha > 160, `${memberId} cuts off the connected shoulder underlay (${alpha} alpha)`);
  }
});

test("production neck repair feathers its source-sample boundary instead of exposing a rectangle", async () => {
  const productionDirectory = path.resolve(
    import.meta.dirname,
    "../../../assets/brand/waffle/rigs/standing-v2",
  );
  const image = await readRgba(path.join(productionDirectory, "layers/neck-repair.png"));
  const alphaAt = (x, y) => image.data[(y * image.width + x) * 4 + 3];
  assert.ok(alphaAt(585, 425) > 200, "neck repair must retain an opaque concealed centre");
  assert.ok(alphaAt(500, 425) > 160, "neck repair must underlay the screen-left turn seam");
  assert.ok(alphaAt(720, 425) > 160, "neck repair must underlay the screen-right turn seam");
  assert.ok(alphaAt(480, 450) > 160, "neck repair must widen below the turned-head silhouette");
  assert.ok(alphaAt(470, 390) < 64, "neck repair must not protrude beside the screen-right head turn");
  for (const [x, y] of [[450, 440], [750, 440], [585, 370], [585, 499]]) {
    assert.ok(alphaAt(x, y) < 64, `neck repair hard edge remains at ${x},${y}`);
  }
});

test("rear-left low-lift and landing inner edges contain no more pink key spill than the neutral control", async () => {
  const productionDirectory = path.resolve(
    import.meta.dirname,
    "../../../assets/brand/waffle/rigs/standing-v2",
  );
  const manifest = JSON.parse(await readFile(path.join(productionDirectory, "rig.json"), "utf8"));
  const members = new Map(manifest.variants["rear-chain-left"].members.map((member) => [member.id, member]));
  const images = new Map(await Promise.all([...members].map(async ([id, member]) => [
    id,
    await readRgba(path.join(productionDirectory, member.file)),
  ])));
  const bounds = { startX: 760, endX: 920, startY: 760, endY: 905 };
  const neutralCandidates = innerEdgeHueCandidates(images.get("neutral"), bounds);
  assert.equal(neutralCandidates.length, 0, "neutral rear-left control must have no pink inner-edge spill");
  for (const id of ["low-lift", "landing"]) {
    const candidates = innerEdgeHueCandidates(images.get(id), bounds);
    assert.equal(
      candidates.length,
      neutralCandidates.length,
      `${id} has ${candidates.length} pink inner-edge pixels; first at ${candidates[0]?.x},${candidates[0]?.y}`,
    );
  }
});
