import { createHash } from "node:crypto";
import { readFile, realpath } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

import { recomposeLayers } from "./rig-raster.mjs";
import { validatePng } from "./validate-raster.mjs";

const SHA256 = /^[a-f\d]{64}$/u;

function object(value) {
  return value && typeof value === "object" && !Array.isArray(value);
}

function finite(value) {
  return typeof value === "number" && Number.isFinite(value);
}

function lexicalPath(base, relative, label) {
  if (typeof relative !== "string" || relative.length === 0 || path.isAbsolute(relative) || /^[a-z][a-z\d+.-]*:/iu.test(relative)) {
    throw new Error(`${label} must be a local relative path`);
  }
  const resolved = path.resolve(base, relative);
  if (resolved !== path.resolve(base) && !resolved.startsWith(`${path.resolve(base)}${path.sep}`)) {
    throw new Error(`${label} must stay inside the rig directory`);
  }
  return resolved;
}

async function containedPath(base, relative, label) {
  const file = lexicalPath(base, relative, label);
  const [physicalBase, physicalFile] = await Promise.all([realpath(base), realpath(file)]);
  if (physicalFile !== physicalBase && !physicalFile.startsWith(`${physicalBase}${path.sep}`)) {
    throw new Error(`${label} must resolve inside the rig directory`);
  }
  return physicalFile;
}

async function sourcePath(directory, relative) {
  const assetRoot = path.resolve(directory, "../..");
  if (typeof relative !== "string" || relative.length === 0 || path.isAbsolute(relative) || /^[a-z][a-z\d+.-]*:/iu.test(relative)) {
    throw new Error("source file must be a local relative path");
  }
  const resolved = path.resolve(directory, relative);
  if (resolved !== assetRoot && !resolved.startsWith(`${assetRoot}${path.sep}`)) {
    throw new Error("source file must stay inside the Waffle asset directory");
  }
  const [physicalBase, physicalFile] = await Promise.all([realpath(assetRoot), realpath(resolved)]);
  if (physicalFile !== physicalBase && !physicalFile.startsWith(`${physicalBase}${path.sep}`)) {
    throw new Error("source file must resolve inside the Waffle asset directory");
  }
  return physicalFile;
}

async function hash(file) {
  return createHash("sha256").update(await readFile(file)).digest("hex");
}

async function verifyHash(file, declared, label) {
  if (typeof declared !== "string" || !SHA256.test(declared)) throw new Error(`${label} sha256 must be 64 lowercase hex characters`);
  if (await hash(file) !== declared) throw new Error(`sha256 mismatch for ${label}`);
}

function validateGraph(layers) {
  const byId = new Map(layers.map((layer) => [layer.id, layer]));
  for (const layer of layers) {
    if (layer.parent !== null && !byId.has(layer.parent)) throw new Error(`layer ${layer.id} has unknown parent ${layer.parent}`);
  }
  for (const layer of layers) {
    const visited = new Set();
    let current = layer;
    while (current?.parent !== null) {
      if (visited.has(current.id)) throw new Error("layer graph contains a cycle");
      visited.add(current.id);
      current = byId.get(current.parent);
    }
  }
}

function firstMismatch(left, right) {
  if (left.width !== right.width || left.height !== right.height) return { x: 0, y: 0 };
  for (let offset = 0; offset < left.data.length; offset += 4) {
    if (left.data[offset + 3] === 0 && right.data[offset + 3] === 0) continue;
    if (!left.data.subarray(offset, offset + 4).equals(right.data.subarray(offset, offset + 4))) {
      const pixel = offset / 4;
      return { x: pixel % left.width, y: Math.floor(pixel / left.width) };
    }
  }
  return null;
}

export async function validateRig(manifestPath) {
  const manifest = JSON.parse(await readFile(manifestPath, "utf8"));
  if (manifest.schemaVersion !== 1) throw new Error("schemaVersion must be 1");
  if (!object(manifest.canvas) || !Number.isInteger(manifest.canvas.width) || !Number.isInteger(manifest.canvas.height) || manifest.canvas.width <= 0 || manifest.canvas.height <= 0) {
    throw new Error("canvas width and height must be positive integers");
  }
  if (!object(manifest.source) || !object(manifest.neutralReference)) throw new Error("source and neutralReference are required");
  if (!Array.isArray(manifest.layers) || manifest.layers.length === 0) throw new Error("layers must be a non-empty array");
  if (!object(manifest.controls) || Object.keys(manifest.controls).length === 0) throw new Error("controls must be a non-empty object");

  const directory = path.dirname(manifestPath);
  const source = await sourcePath(directory, manifest.source.file);
  const reference = await containedPath(directory, manifest.neutralReference.file, "neutralReference file");
  await verifyHash(source, manifest.source.sha256, "source");
  await verifyHash(reference, manifest.neutralReference.sha256, "neutralReference");
  await validatePng(source, { width: manifest.canvas.width, height: manifest.canvas.height, alphaPolicy: "transparent-corners" });
  const referencePng = await validatePng(reference, { width: manifest.canvas.width, height: manifest.canvas.height, alphaPolicy: "transparent-corners" });

  const ids = new Set();
  const orders = new Set();
  for (const [index, layer] of manifest.layers.entries()) {
    if (!object(layer)) throw new Error(`layer ${index + 1} must be an object`);
    if (typeof layer.id !== "string" || layer.id.length === 0) throw new Error(`layer ${index + 1} id must be a non-empty string`);
    if (ids.has(layer.id)) throw new Error(`duplicate layer id: ${layer.id}`);
    ids.add(layer.id);
    if (!Number.isInteger(layer.drawOrder)) throw new Error(`layer ${layer.id} drawOrder must be an integer`);
    if (orders.has(layer.drawOrder)) throw new Error(`duplicate drawOrder: ${layer.drawOrder}`);
    orders.add(layer.drawOrder);
    if (!["visible", "repair", "overlay"].includes(layer.role)) throw new Error(`layer ${layer.id} has unknown role`);
    if (typeof layer.visibleAtNeutral !== "boolean") throw new Error(`layer ${layer.id} visibleAtNeutral must be boolean`);
    if (layer.blendMode !== "normal") throw new Error(`layer ${layer.id} blendMode must be normal`);
    if (!object(layer.pivot) || !finite(layer.pivot.x) || !finite(layer.pivot.y) || layer.pivot.x < 0 || layer.pivot.x > 1 || layer.pivot.y < 0 || layer.pivot.y > 1) {
      throw new Error(`layer ${layer.id} pivot must be normalized`);
    }
    const values = layer.neutral && [layer.neutral.x, layer.neutral.y, layer.neutral.rotationDegrees, layer.neutral.scaleX, layer.neutral.scaleY];
    if (!values || !values.every(finite)) throw new Error(`layer ${layer.id} neutral transform values must be finite numbers`);
    const file = await containedPath(directory, layer.file, `layer ${layer.id} file`);
    await verifyHash(file, layer.sha256, `layer ${layer.id}`);
    await validatePng(file, { width: manifest.canvas.width, height: manifest.canvas.height, alphaPolicy: "transparent-corners" });
  }
  validateGraph(manifest.layers);

  for (const [name, range] of Object.entries(manifest.controls)) {
    if (!object(range) || !finite(range.min) || !finite(range.max) || range.min >= range.max) {
      throw new Error(`control ${name} must have finite min < max`);
    }
  }

  const composite = await recomposeLayers(manifestPath);
  const sourcePng = await validatePng(source, { width: manifest.canvas.width, height: manifest.canvas.height, alphaPolicy: "transparent-corners" });
  for (const comparison of [sourcePng, referencePng]) {
    const mismatch = firstMismatch(composite, { width: manifest.canvas.width, height: manifest.canvas.height, data: comparison.pixels });
    if (mismatch) throw new Error(`neutral recomposition differs at x=${mismatch.x} y=${mismatch.y}`);
  }
  return { layerCount: manifest.layers.length, mismatchPixels: 0 };
}

async function main(args) {
  const optional = args[0] === "--optional";
  const paths = optional ? args.slice(1) : args;
  if (paths.length === 0) throw new Error("usage: validate-rig.mjs [--optional] <rig.json...>");
  for (const manifestPath of paths) {
    try {
      const result = await validateRig(manifestPath);
      console.log(`PASS ${manifestPath} layers=${result.layerCount} mismatchPixels=${result.mismatchPixels}`);
    } catch (error) {
      if (optional && error.code === "ENOENT") {
        console.log(`SKIP ${manifestPath} (not present)`);
        continue;
      }
      throw error;
    }
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
