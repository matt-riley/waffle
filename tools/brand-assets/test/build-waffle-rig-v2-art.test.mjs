import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtemp, mkdir, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { test } from "node:test";

import { PNG } from "pngjs";

import { buildRigV2Art, extractBounded } from "../build-waffle-rig-v2-art.mjs";
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
  assert.deepEqual(await readFile(path.join(inputs.outputDirectory, "repairs.json")), repairsBytes);
  assert.deepEqual(await readFile(path.join(inputs.outputDirectory, "variants.json")), variantsBytes);
  assert.deepEqual(await validateRig(inputs.manifestPath), { layerCount: 3, mismatchPixels: 0 });
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
