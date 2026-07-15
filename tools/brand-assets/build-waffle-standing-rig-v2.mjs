import { createHash } from "node:crypto";
import {
  lstat,
  mkdir,
  readFile,
  readdir,
  rename,
  rm,
  writeFile,
} from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

import {
  applyUnderlaps as applySourceUnderlaps,
  partitionSource,
  pointInPolygon,
} from "./build-waffle-standing-rig.mjs";
import { readRgba, writeRgba } from "./rig-raster.mjs";
import { validateRig } from "./validate-rig.mjs";

export { applySourceUnderlaps as applyUnderlaps, partitionSource };

export function applyCoveredUnderlaps(source, layers, regions, definitions) {
  const definitionById = new Map(definitions.map((definition) => [definition.id, definition]));
  for (const region of regions) {
    if (!Array.isArray(region.underlapPolygons) || region.underlapPolygons.length === 0) continue;
    const moving = definitionById.get(region.id);
    for (let y = 0; y < source.height; y += 1) {
      for (let x = 0; x < source.width; x += 1) {
        const offset = (y * source.width + x) * 4;
        if (source.data[offset + 3] !== 255) continue;
        if (!region.underlapPolygons.some((polygon) => pointInPolygon(x + 0.5, y + 0.5, polygon))) continue;
        const owner = [...layers.entries()].find(([, layer]) => layer.data[offset + 3] > 0);
        if (!owner || owner[0] === region.id) continue;
        const cover = definitionById.get(owner[0]);
        if (!moving || !cover || cover.drawOrder <= moving.drawOrder || owner[1].data[offset + 3] !== 255) {
          throw new Error(`underlap ${region.id} at x=${x} y=${y} is not beneath a fully opaque higher-draw-order cover`);
        }
      }
    }
  }
  return applySourceUnderlaps(source, layers, regions);
}

async function sha256(file) {
  return createHash("sha256").update(await readFile(file)).digest("hex");
}

async function exists(file) {
  try {
    await lstat(file);
    return true;
  } catch (error) {
    if (error.code === "ENOENT") return false;
    throw error;
  }
}

function assertSafeTarget(outputDirectory) {
  if (path.resolve(outputDirectory).split(path.sep).includes("standing-v1")) {
    throw new Error("refusing to write into standing-v1");
  }
}

async function classifyExistingTarget(outputDirectory, masksFile) {
  if (!await exists(outputDirectory)) return "absent";
  if (!(await lstat(outputDirectory)).isDirectory()) {
    throw new Error("refusing to replace unrecognized output directory");
  }

  const entries = await readdir(outputDirectory);
  if (entries.length === 1
    && entries[0] === "masks.json"
    && path.resolve(masksFile) === path.join(path.resolve(outputDirectory), "masks.json")) {
    return "bootstrap";
  }

  const manifestFile = path.join(outputDirectory, "rig.json");
  if (!await exists(manifestFile)) throw new Error("refusing to replace unrecognized output directory");
  let manifest;
  try {
    manifest = JSON.parse(await readFile(manifestFile, "utf8"));
  } catch {
    throw new Error("refusing to replace unrecognized output directory");
  }
  if (manifest.schemaVersion !== 2) throw new Error("refusing to replace unrecognized output directory");
  await validateRig(manifestFile);
  return "v2";
}

function orderedDefinitions(masks, layers) {
  const definitions = Object.entries(masks.layerDefinitions ?? {})
    .map(([id, definition]) => ({ id, ...definition }))
    .toSorted((left, right) => left.drawOrder - right.drawOrder);
  if (definitions.length !== layers.size || definitions.some((definition) => !layers.has(definition.id))) {
    throw new Error("layerDefinitions must describe every partition layer exactly once");
  }
  return definitions;
}

async function writeTemporaryPackage({ sourceFile, masksBytes, masks, temporaryDirectory }) {
  const source = await readRgba(sourceFile);
  if (source.width !== masks.canvas?.width || source.height !== masks.canvas?.height) {
    throw new Error(`mask canvas ${masks.canvas?.width}x${masks.canvas?.height} does not match source ${source.width}x${source.height}`);
  }
  if (!Array.isArray(masks.regionsFrontToBack) || typeof masks.fallback !== "string") {
    throw new Error("masks require regionsFrontToBack and fallback");
  }

  const layers = partitionSource(source, masks.regionsFrontToBack, masks.fallback);
  const definitions = orderedDefinitions(masks, layers);
  applyCoveredUnderlaps(source, layers, masks.regionsFrontToBack, definitions);

  const layersDirectory = path.join(temporaryDirectory, "layers");
  await mkdir(layersDirectory, { recursive: true });
  await writeFile(path.join(temporaryDirectory, "masks.json"), masksBytes);

  const referenceFile = path.join(temporaryDirectory, "neutral-reference.png");
  await writeRgba(referenceFile, source);

  const manifestLayers = [];
  for (const definition of definitions) {
    const file = path.join(layersDirectory, `${definition.id}.png`);
    await writeRgba(file, layers.get(definition.id));
    manifestLayers.push({
      id: definition.id,
      file: `layers/${definition.id}.png`,
      role: "visible",
      parent: definition.parent,
      drawOrder: definition.drawOrder,
      visibleAtNeutral: true,
      blendMode: "normal",
      pivot: definition.pivot,
      neutral: { x: 0, y: 0, rotationDegrees: 0, scaleX: 1, scaleY: 1 },
      limits: definition.limits,
      sha256: await sha256(file),
    });
  }

  const manifest = {
    schemaVersion: 2,
    canvas: masks.canvas,
    root: masks.root,
    source: {
      file: "../../poses/standing.png",
      sha256: await sha256(sourceFile),
    },
    neutralReference: {
      file: "neutral-reference.png",
      sha256: await sha256(referenceFile),
    },
    layers: manifestLayers,
    variants: masks.variants ?? {},
    controls: masks.controls,
  };
  const manifestFile = path.join(temporaryDirectory, "rig.json");
  await writeFile(manifestFile, `${JSON.stringify(manifest, null, 2)}\n`);
  await validateRig(manifestFile);
}

async function promote({ outputDirectory, temporaryDirectory, targetKind }) {
  if (targetKind === "absent") {
    await rename(temporaryDirectory, outputDirectory);
    return;
  }

  const previousDirectory = `${outputDirectory}.previous-${process.pid}`;
  await rm(previousDirectory, { recursive: true, force: true });
  await rename(outputDirectory, previousDirectory);
  try {
    await rename(temporaryDirectory, outputDirectory);
  } catch (error) {
    await rename(previousDirectory, outputDirectory);
    throw error;
  }
  await rm(previousDirectory, { recursive: true, force: true });
}

export async function buildStandingRigV2({ sourceFile, masksFile, outputDirectory }) {
  sourceFile = path.resolve(sourceFile);
  masksFile = path.resolve(masksFile);
  outputDirectory = path.resolve(outputDirectory);
  assertSafeTarget(outputDirectory);

  const targetKind = await classifyExistingTarget(outputDirectory, masksFile);
  const masksBytes = await readFile(masksFile);
  const masks = JSON.parse(masksBytes.toString("utf8"));
  const temporaryDirectory = `${outputDirectory}.building-${process.pid}`;
  await rm(temporaryDirectory, { recursive: true, force: true });

  try {
    await writeTemporaryPackage({ sourceFile, masksBytes, masks, temporaryDirectory });
    await promote({ outputDirectory, temporaryDirectory, targetKind });
  } catch (error) {
    await rm(temporaryDirectory, { recursive: true, force: true });
    throw error;
  }
  return path.join(outputDirectory, "rig.json");
}

async function main(args) {
  if (args.length !== 3) {
    throw new Error("usage: build-waffle-standing-rig-v2.mjs <standing.png> <masks.json> <output-directory>");
  }
  const manifest = await buildStandingRigV2({
    sourceFile: args[0],
    masksFile: args[1],
    outputDirectory: args[2],
  });
  console.log(`WROTE ${manifest}`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
