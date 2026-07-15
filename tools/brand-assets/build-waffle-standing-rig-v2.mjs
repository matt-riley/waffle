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

const SAFE_LAYER_ID = /^[a-z][a-z\d]*(?:-[a-z\d]+)*$/u;
const AUTHORITATIVE_PACKAGE_FILES = ["repairs.json", "variants.json", "GENERATION.md", "README.md"];

function containedOutputPath(temporaryDirectory, ...parts) {
  const base = path.resolve(temporaryDirectory);
  const derived = path.resolve(base, ...parts);
  if (!derived.startsWith(`${base}${path.sep}`)) {
    throw new Error(`derived output path must stay inside temporary package: ${parts.join("/")}`);
  }
  return derived;
}

function outputPlan(masks, temporaryDirectory) {
  const definitions = Object.entries(masks.layerDefinitions ?? {})
    .map(([id, definition]) => ({ id, ...definition }))
    .toSorted((left, right) => left.drawOrder - right.drawOrder);
  const layerFiles = new Map();
  for (const definition of definitions) {
    if (!SAFE_LAYER_ID.test(definition.id)) throw new Error(`unsafe layer id: ${definition.id}`);
    layerFiles.set(definition.id, containedOutputPath(temporaryDirectory, "layers", `${definition.id}.png`));
  }
  return {
    definitions,
    layerFiles,
    layersDirectory: containedOutputPath(temporaryDirectory, "layers"),
    manifestFile: containedOutputPath(temporaryDirectory, "rig.json"),
    masksFile: containedOutputPath(temporaryDirectory, "masks.json"),
    referenceFile: containedOutputPath(temporaryDirectory, "neutral-reference.png"),
  };
}

function promotionPaths(outputDirectory) {
  return {
    marker: `${outputDirectory}.promotion.json`,
    markerTemporary: `${outputDirectory}.promotion.json.building-${process.pid}`,
    previous: `${outputDirectory}.previous-${process.pid}`,
  };
}

async function validateV2Directory(directory) {
  const info = await lstat(directory);
  if (!info.isDirectory() || info.isSymbolicLink()) throw new Error("promotion recovery package must be a nonsymlink directory");
  const manifestFile = path.join(directory, "rig.json");
  const manifest = JSON.parse(await readFile(manifestFile, "utf8"));
  if (manifest.schemaVersion !== 2) throw new Error("promotion recovery package must be schema v2");
  await validateRig(manifestFile);
}

async function recoveryPreviousEntries(outputDirectory) {
  const parent = path.dirname(outputDirectory);
  const prefix = `${path.basename(outputDirectory)}.previous-`;
  return (await readdir(parent, { withFileTypes: true }))
    .filter((entry) => entry.name.startsWith(prefix))
    .map((entry) => entry.name)
    .toSorted();
}

async function recoverPrevious({ marker, outputDirectory, previousDirectory, renamePath }) {
  const targetExists = await exists(outputDirectory);
  const previousExists = await exists(previousDirectory);
  if (!targetExists) {
    if (!previousExists) throw new Error("promotion recovery state has no recoverable package");
    await validateV2Directory(previousDirectory);
    await renamePath(previousDirectory, outputDirectory);
    await validateV2Directory(outputDirectory);
    if (marker) await rm(marker, { force: true });
    return;
  }

  try {
    await validateV2Directory(outputDirectory);
  } catch (error) {
    throw new Error(`ambiguous promotion recovery state: target is invalid: ${error.message}`);
  }
  if (previousExists) {
    try {
      await validateV2Directory(previousDirectory);
    } catch (error) {
      throw new Error(`ambiguous promotion recovery state: previous package is invalid: ${error.message}`);
    }
    await rm(previousDirectory, { recursive: true });
  }
  if (marker) await rm(marker, { force: true });
}

async function recoverInterruptedPromotion(outputDirectory, renamePath) {
  const paths = promotionPaths(outputDirectory);
  const entries = await recoveryPreviousEntries(outputDirectory);
  const markerExists = await exists(paths.marker);
  if (!markerExists) {
    if (entries.length === 0) return;
    if (entries.length !== 1 || entries[0] !== path.basename(paths.previous)) {
      throw new Error("ambiguous promotion recovery state");
    }
    await recoverPrevious({
      marker: null,
      outputDirectory,
      previousDirectory: paths.previous,
      renamePath,
    });
    return;
  }

  const markerInfo = await lstat(paths.marker);
  if (!markerInfo.isFile() || markerInfo.isSymbolicLink()) throw new Error("ambiguous promotion recovery state");
  let marker;
  try {
    marker = JSON.parse(await readFile(paths.marker, "utf8"));
  } catch {
    throw new Error("ambiguous promotion recovery state");
  }
  const outputName = path.basename(outputDirectory);
  const previousPattern = new RegExp(`^${outputName.replace(/[.*+?^${}()|[\]\\]/gu, "\\$&")}\\.previous-\\d+$`, "u");
  if (marker.schemaVersion !== 1
    || marker.outputDirectory !== outputName
    || typeof marker.previousDirectory !== "string"
    || !previousPattern.test(marker.previousDirectory)) {
    throw new Error("ambiguous promotion recovery state");
  }
  if (entries.length > 1 || (entries.length === 1 && entries[0] !== marker.previousDirectory)) {
    throw new Error("ambiguous promotion recovery state");
  }
  const previousDirectory = path.join(path.dirname(outputDirectory), marker.previousDirectory);
  await recoverPrevious({
    marker: paths.marker,
    outputDirectory,
    previousDirectory,
    renamePath,
  });
}

async function writePromotionMarker(outputDirectory, previousDirectory, renamePath) {
  const paths = promotionPaths(outputDirectory);
  const marker = {
    schemaVersion: 1,
    outputDirectory: path.basename(outputDirectory),
    previousDirectory: path.basename(previousDirectory),
  };
  await rm(paths.markerTemporary, { force: true });
  await writeFile(paths.markerTemporary, `${JSON.stringify(marker, null, 2)}\n`, { flag: "wx" });
  await renamePath(paths.markerTemporary, paths.marker);
  return paths.marker;
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

async function readAuthoritativePackageFiles(outputDirectory, targetKind) {
  const files = new Map();
  if (targetKind !== "v2") return files;
  for (const name of AUTHORITATIVE_PACKAGE_FILES) {
    const file = path.join(outputDirectory, name);
    if (!await exists(file)) continue;
    const info = await lstat(file);
    if (!info.isFile() || info.isSymbolicLink()) {
      throw new Error(`authoritative package file ${name} must be a regular nonsymlink file`);
    }
    files.set(name, await readFile(file));
  }
  return files;
}

function orderedDefinitions(definitions, layers) {
  if (definitions.length !== layers.size || definitions.some((definition) => !layers.has(definition.id))) {
    throw new Error("layerDefinitions must describe every partition layer exactly once");
  }
  return definitions;
}

function bootstrapControls(controls, definitions) {
  const baseLayers = new Set(definitions.map((definition) => definition.id));
  return Object.fromEntries(Object.entries(controls ?? {}).filter(([, control]) => (
    Array.isArray(control.bindings)
      && control.bindings.length > 0
      && control.bindings.every((binding) => "layer" in binding && baseLayers.has(binding.layer))
  )));
}

async function writeTemporaryPackage({ sourceFile, masksBytes, masks, plan, preservedFiles }) {
  const source = await readRgba(sourceFile);
  if (source.width !== masks.canvas?.width || source.height !== masks.canvas?.height) {
    throw new Error(`mask canvas ${masks.canvas?.width}x${masks.canvas?.height} does not match source ${source.width}x${source.height}`);
  }
  if (!Array.isArray(masks.regionsFrontToBack) || typeof masks.fallback !== "string") {
    throw new Error("masks require regionsFrontToBack and fallback");
  }

  const layers = partitionSource(source, masks.regionsFrontToBack, masks.fallback);
  const definitions = orderedDefinitions(plan.definitions, layers);
  applyCoveredUnderlaps(source, layers, masks.regionsFrontToBack, definitions);

  await mkdir(plan.layersDirectory, { recursive: true });
  await writeFile(plan.masksFile, masksBytes);
  await Promise.all([...preservedFiles].map(([name, bytes]) => (
    writeFile(containedOutputPath(path.dirname(plan.manifestFile), name), bytes)
  )));

  await writeRgba(plan.referenceFile, source);

  const manifestLayers = [];
  for (const definition of definitions) {
    const file = plan.layerFiles.get(definition.id);
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
      sha256: await sha256(plan.referenceFile),
    },
    layers: manifestLayers,
    variants: masks.variants ?? {},
    controls: bootstrapControls(masks.controls, definitions),
  };
  await writeFile(plan.manifestFile, `${JSON.stringify(manifest, null, 2)}\n`);
  await validateRig(plan.manifestFile);
}

async function validateRestoredTarget(outputDirectory, targetKind, masksFile) {
  if (targetKind === "v2") {
    await validateV2Directory(outputDirectory);
    return;
  }
  if (targetKind === "bootstrap"
    && await classifyExistingTarget(outputDirectory, masksFile) === "bootstrap") return;
  throw new Error("restored package no longer matches its recognized target kind");
}

async function promote({ outputDirectory, temporaryDirectory, targetKind, masksFile, renamePath }) {
  if (targetKind === "absent") {
    await renamePath(temporaryDirectory, outputDirectory);
    return;
  }

  const paths = promotionPaths(outputDirectory);
  const marker = await writePromotionMarker(outputDirectory, paths.previous, renamePath);
  try {
    await renamePath(outputDirectory, paths.previous);
  } catch (error) {
    await validateRestoredTarget(outputDirectory, targetKind, masksFile);
    await rm(marker, { force: true });
    throw error;
  }
  try {
    await renamePath(temporaryDirectory, outputDirectory);
  } catch (promotionError) {
    try {
      await renamePath(paths.previous, outputDirectory);
      await validateRestoredTarget(outputDirectory, targetKind, masksFile);
      await rm(marker, { force: true });
    } catch (restorationError) {
      throw new Error(restorationError.message, { cause: promotionError });
    }
    throw promotionError;
  }
  await validateV2Directory(outputDirectory);
  await rm(paths.previous, { recursive: true });
  await rm(marker, { force: true });
}

export async function buildStandingRigV2({ sourceFile, masksFile, outputDirectory, renamePath = rename }) {
  sourceFile = path.resolve(sourceFile);
  masksFile = path.resolve(masksFile);
  outputDirectory = path.resolve(outputDirectory);
  assertSafeTarget(outputDirectory);

  await recoverInterruptedPromotion(outputDirectory, renamePath);
  const targetKind = await classifyExistingTarget(outputDirectory, masksFile);
  const preservedFiles = await readAuthoritativePackageFiles(outputDirectory, targetKind);
  const masksBytes = await readFile(masksFile);
  const masks = JSON.parse(masksBytes.toString("utf8"));
  const temporaryDirectory = `${outputDirectory}.building-${process.pid}`;
  const plan = outputPlan(masks, temporaryDirectory);
  await rm(temporaryDirectory, { recursive: true, force: true });

  try {
    await writeTemporaryPackage({ sourceFile, masksBytes, masks, plan, preservedFiles });
    await promote({ outputDirectory, temporaryDirectory, targetKind, masksFile, renamePath });
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
