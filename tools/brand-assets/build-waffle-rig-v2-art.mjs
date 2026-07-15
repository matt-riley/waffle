import { createHash } from "node:crypto";
import {
  cp,
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

import { PNG } from "pngjs";

import { pointInPolygon } from "./build-waffle-standing-rig.mjs";
import { readRgba, writeRgba } from "./rig-raster.mjs";
import { validateRig } from "./validate-rig.mjs";

const SAFE_ID = /^[a-z][a-z\d]*(?:-[a-z\d]+)*$/u;
const SHA256 = /^[a-f\d]{64}$/u;
const ALPHA_NOISE_FLOOR = 8;
const KEY_DOMINANCE_THRESHOLD = 16;

function object(value) {
  return value && typeof value === "object" && !Array.isArray(value);
}

async function sha256(file) {
  return createHash("sha256").update(await readFile(file)).digest("hex");
}

function assertCanvas(canvas, expected, label) {
  if (!object(canvas)
    || canvas.width !== expected.width
    || canvas.height !== expected.height) {
    throw new Error(`${label} canvas must be exactly ${expected.width}x${expected.height}`);
  }
}

function assertSha256(value, label) {
  if (typeof value !== "string" || !SHA256.test(value)) {
    throw new Error(`${label} sha256 must be 64 lowercase hex characters`);
  }
}

function assertSafeId(value, label) {
  if (typeof value !== "string" || !SAFE_ID.test(value)) throw new Error(`${label} must be a safe kebab-case ID`);
}

function resolveInput(base, relative, label) {
  if (typeof relative !== "string"
    || relative.length === 0
    || path.isAbsolute(relative)
    || /^[a-z][a-z\d+.-]*:/iu.test(relative)) {
    throw new Error(`${label} must be a local relative path`);
  }
  return path.resolve(base, relative);
}

function validateCrop(crop, canvas, polygons) {
  if (!object(crop)
    || ![crop.x, crop.y, crop.width, crop.height].every(Number.isInteger)
    || crop.x < 0
    || crop.y < 0
    || crop.width <= 0
    || crop.height <= 0
    || crop.x + crop.width > canvas.width
    || crop.y + crop.height > canvas.height) {
    throw new Error("crop must be a positive integer rectangle inside the canvas");
  }
  if (!Array.isArray(polygons) || polygons.length === 0) throw new Error("extraction polygons must be a non-empty array");
  for (const polygon of polygons) {
    if (!Array.isArray(polygon) || polygon.length < 3) throw new Error("extraction polygon must contain at least three points");
    for (const point of polygon) {
      if (!Array.isArray(point)
        || point.length !== 2
        || !point.every(Number.isFinite)
        || point[0] < crop.x
        || point[0] > crop.x + crop.width
        || point[1] < crop.y
        || point[1] > crop.y + crop.height) {
        throw new Error("polygon must stay inside declared crop");
      }
    }
  }
}

function clampByte(value) {
  return Math.max(0, Math.min(255, Math.round(value)));
}

function smoothstep(value) {
  const clamped = Math.max(0, Math.min(1, value));
  return clamped * clamped * (3 - 2 * clamped);
}

function spillChannels(key) {
  const maximum = Math.max(...key);
  if (maximum < 128) return [];
  return key.flatMap((value, index) => value >= maximum - 16 && value >= 128 ? [index] : []);
}

function channelDistance(rgb, key) {
  return Math.max(...rgb.map((value, index) => Math.abs(value - key[index])));
}

function keyDominance(rgb, key) {
  const spill = spillChannels(key);
  if (spill.length === 0) return 0;
  const other = [0, 1, 2].filter((index) => !spill.includes(index));
  const keyStrength = spill.length > 1
    ? Math.min(...spill.map((index) => rgb[index]))
    : rgb[spill[0]];
  const otherStrength = Math.max(0, ...other.map((index) => rgb[index]));
  return keyStrength - otherStrength;
}

function dominanceAlpha(rgb, key) {
  const dominance = keyDominance(rgb, key);
  if (dominance <= 0) return 255;
  const spill = spillChannels(key);
  const other = [0, 1, 2].filter((index) => !spill.includes(index));
  const otherStrength = Math.max(0, ...other.map((index) => rgb[index]));
  const denominator = Math.max(1, Math.max(...key) - otherStrength);
  return clampByte(255 * (1 - Math.min(1, dominance / denominator)));
}

function keyLikeRgb(rgb, chromaKey) {
  return channelDistance(rgb, chromaKey.rgb) <= 32
    || keyDominance(rgb, chromaKey.rgb) >= KEY_DOMINANCE_THRESHOLD;
}

function localEdgeRgb(source, x, y, chromaKey) {
  const maximumRadius = 12;
  for (let radius = 1; radius <= maximumRadius; radius += 1) {
    const samples = [];
    for (let dy = -radius; dy <= radius; dy += 1) {
      for (let dx = -radius; dx <= radius; dx += 1) {
        if (Math.max(Math.abs(dx), Math.abs(dy)) !== radius) continue;
        const sampleX = x + dx;
        const sampleY = y + dy;
        if (sampleX < 0 || sampleY < 0 || sampleX >= source.width || sampleY >= source.height) continue;
        const sampleOffset = (sampleY * source.width + sampleX) * 4;
        if (source.data[sampleOffset + 3] < 128) continue;
        const rgb = [...source.data.subarray(sampleOffset, sampleOffset + 3)];
        if (!keyLikeRgb(rgb, chromaKey)) samples.push(rgb);
      }
    }
    if (samples.length > 0) {
      return [0, 1, 2].map((channel) => clampByte(
        samples.reduce((sum, sample) => sum + sample[channel], 0) / samples.length,
      ));
    }
  }
  return undefined;
}

function neutralRgb(rgb) {
  const luminance = clampByte(0.2126 * rgb[0] + 0.7152 * rgb[1] + 0.0722 * rgb[2]);
  return [luminance, luminance, luminance];
}

function sanitizeChromaPixel(source, sourceOffset, data, offset, chromaKey) {
  const rgb = [data[offset], data[offset + 1], data[offset + 2]];
  const sourceAlpha = data[offset + 3];
  const distance = channelDistance(rgb, chromaKey.rgb);
  if (!keyLikeRgb(rgb, chromaKey)) return;

  const ratio = (distance - chromaKey.transparentThreshold)
    / (chromaKey.opaqueThreshold - chromaKey.transparentThreshold);
  const matteAlpha = Math.min(clampByte(255 * smoothstep(ratio)), dominanceAlpha(rgb, chromaKey.rgb));
  const outputAlpha = clampByte(sourceAlpha * matteAlpha / 255);
  if (outputAlpha <= ALPHA_NOISE_FLOOR || matteAlpha === 0) {
    data.fill(0, offset, offset + 4);
    return;
  }

  const matte = matteAlpha / 255;
  const unmixed = rgb.map((value, index) => clampByte(
    (value - (1 - matte) * chromaKey.rgb[index]) / matte,
  ));
  const sourceX = (sourceOffset / 4) % source.width;
  const sourceY = Math.floor(sourceOffset / 4 / source.width);
  const decontaminated = localEdgeRgb(source, sourceX, sourceY, chromaKey) ?? neutralRgb(unmixed);
  data.set([...decontaminated, outputAlpha], offset);
}

function pointToSegmentDistance(x, y, start, end) {
  const dx = end[0] - start[0];
  const dy = end[1] - start[1];
  const lengthSquared = dx * dx + dy * dy;
  if (lengthSquared === 0) return Math.hypot(x - start[0], y - start[1]);
  const progress = Math.max(0, Math.min(1, ((x - start[0]) * dx + (y - start[1]) * dy) / lengthSquared));
  return Math.hypot(x - (start[0] + progress * dx), y - (start[1] + progress * dy));
}

function edgeFeatherAlpha(x, y, polygons, pixels) {
  if (pixels === undefined || pixels === 0) return 1;
  if (!Number.isFinite(pixels) || pixels < 0) throw new Error("edgeFeatherPixels must be a non-negative number");
  const containing = polygons.filter((polygon) => pointInPolygon(x, y, polygon));
  let distance = Infinity;
  for (const polygon of containing) {
    for (let index = 0; index < polygon.length; index += 1) {
      distance = Math.min(distance, pointToSegmentDistance(
        x,
        y,
        polygon[index],
        polygon[(index + 1) % polygon.length],
      ));
    }
  }
  return smoothstep((distance - 0.5) / pixels);
}

export function extractBounded(source, specification) {
  const canvas = { width: source.width, height: source.height };
  validateCrop(specification.crop, canvas, specification.polygons);
  const output = new PNG(canvas);
  const sampleOffset = specification.sampleOffset ?? { x: 0, y: 0 };
  if (!Number.isInteger(sampleOffset.x) || !Number.isInteger(sampleOffset.y)) {
    throw new Error("sampleOffset must contain integer x and y values");
  }
  const fallback = specification.fallbackSample;
  const chromaKey = specification.chromaKey;
  if (chromaKey && (!Array.isArray(chromaKey.rgb)
    || chromaKey.rgb.length !== 3
    || !chromaKey.rgb.every((value) => Number.isInteger(value) && value >= 0 && value <= 255)
    || !Number.isFinite(chromaKey.transparentThreshold)
    || !Number.isFinite(chromaKey.opaqueThreshold)
    || chromaKey.transparentThreshold < 0
    || chromaKey.opaqueThreshold <= chromaKey.transparentThreshold)) {
    throw new Error("chromaKey must declare RGB and increasing transparent/opaque thresholds");
  }
  if (fallback && (!Number.isInteger(fallback.x)
    || !Number.isInteger(fallback.y)
    || fallback.x < 0
    || fallback.y < 0
    || fallback.x >= source.width
    || fallback.y >= source.height)) {
    throw new Error("fallbackSample must be an integer point inside the canvas");
  }
  const left = specification.crop.x;
  const top = specification.crop.y;
  const right = left + specification.crop.width;
  const bottom = top + specification.crop.height;
  for (let y = top; y < bottom; y += 1) {
    for (let x = left; x < right; x += 1) {
      const centreX = x + 0.5;
      const centreY = y + 0.5;
      if (!specification.polygons.some((polygon) => pointInPolygon(centreX, centreY, polygon))) continue;
      const sampleX = x + sampleOffset.x;
      const sampleY = y + sampleOffset.y;
      let sourceOffset = sampleX >= 0 && sampleY >= 0 && sampleX < source.width && sampleY < source.height
        ? (sampleY * source.width + sampleX) * 4
        : -1;
      if ((sourceOffset < 0 || source.data[sourceOffset + 3] === 0) && fallback) {
        sourceOffset = (fallback.y * source.width + fallback.x) * 4;
      }
      if (sourceOffset < 0 || source.data[sourceOffset + 3] === 0) continue;
      const targetOffset = (y * source.width + x) * 4;
      output.data.set(source.data.subarray(sourceOffset, sourceOffset + 4), targetOffset);
      if (chromaKey) sanitizeChromaPixel(source, sourceOffset, output.data, targetOffset, chromaKey);
      const feather = edgeFeatherAlpha(centreX, centreY, specification.polygons, specification.edgeFeatherPixels);
      output.data[targetOffset + 3] = clampByte(output.data[targetOffset + 3] * feather);
      if (output.data[targetOffset + 3] <= ALPHA_NOISE_FLOOR) output.data.fill(0, targetOffset, targetOffset + 4);
    }
  }
  return output;
}

function exactPixels(left, right) {
  return left.width === right.width && left.height === right.height && left.data.equals(right.data);
}

function maskToOpaqueCover(image, cover) {
  for (let offset = 0; offset < image.data.length; offset += 4) {
    if (cover.data[offset + 3] === 255) continue;
    image.data.fill(0, offset, offset + 4);
  }
  return image;
}

function maskToOwnedAlpha(image, owner) {
  for (let offset = 0; offset < image.data.length; offset += 4) {
    if (owner.data[offset + 3] > 0) continue;
    image.data.fill(0, offset, offset + 4);
  }
  return image;
}

function compositePatchOntoAnchor(patch, anchor) {
  const output = PNG.sync.read(PNG.sync.write(anchor));
  for (let offset = 0; offset < patch.data.length; offset += 4) {
    const patchAlpha = patch.data[offset + 3];
    if (patchAlpha === 0) continue;
    if (patchAlpha === 255) {
      output.data.set(patch.data.subarray(offset, offset + 4), offset);
      continue;
    }
    const anchorAlpha = output.data[offset + 3];
    const outputAlpha = patchAlpha + Math.round(anchorAlpha * (255 - patchAlpha) / 255);
    for (let channel = 0; channel < 3; channel += 1) {
      const patchValue = patch.data[offset + channel] * patchAlpha;
      const anchorValue = Math.round(output.data[offset + channel] * anchorAlpha * (255 - patchAlpha) / 255);
      output.data[offset + channel] = outputAlpha === 0 ? 0 : Math.round((patchValue + anchorValue) / outputAlpha);
    }
    output.data[offset + 3] = outputAlpha;
  }
  return output;
}

async function validateInput({
  configDirectory,
  expectedCanvas,
  expectedId,
  expectedSourceHash,
  input,
  polygons,
  source,
  anchor,
}) {
  if (!object(input)) throw new Error(`${expectedId} input must be an object`);
  assertCanvas(input.canvas, expectedCanvas, "input");
  if (input.sourceSha256 !== expectedSourceHash) throw new Error(`input source sha256 mismatch for ${expectedId}`);
  validateCrop(input.crop, expectedCanvas, polygons);

  if (input.kind === "anchor-layer") {
    if (input.expectedVariantId !== expectedId) throw new Error(`expected variant id must be ${expectedId}`);
    if (!anchor || input.layer !== anchor.id) throw new Error(`${expectedId} must reference anchor layer ${anchor?.id ?? "missing"}`);
    return anchor.image;
  }

  if (input.kind === "source-sample") {
    if (input.expectedId !== expectedId) throw new Error(`expected repair id must be ${expectedId}`);
    const file = resolveInput(configDirectory, input.file, `${expectedId} source sample`);
    if (path.resolve(file) !== path.resolve(source.file)) throw new Error(`${expectedId} source sample must use the declared standing source`);
    return source.image;
  }

  if (input.kind !== "edit-plate") throw new Error(`${expectedId} input kind must be anchor-layer, source-sample, or edit-plate`);
  const declared = input.expectedVariantId ?? input.expectedId;
  const label = input.expectedVariantId === undefined ? "repair id" : "variant id";
  if (declared !== expectedId) throw new Error(`expected ${label} must be ${expectedId}`);
  assertSha256(input.sha256, `${expectedId} edit plate`);
  const file = resolveInput(configDirectory, input.file, `${expectedId} edit plate`);
  const info = await lstat(file);
  if (!info.isFile() || info.isSymbolicLink()) throw new Error(`${expectedId} edit plate must be a nonsymlink file`);
  if (await sha256(file) !== input.sha256) throw new Error(`sha256 mismatch for edit plate ${expectedId}`);
  const image = await readRgba(file);
  assertCanvas(image, expectedCanvas, "edit plate");
  return image;
}

function manifestLayer(specification, relativeFile, hash) {
  return {
    id: specification.id,
    file: relativeFile,
    role: specification.role ?? "repair",
    parent: specification.parent,
    drawOrder: specification.drawOrder,
    visibleAtNeutral: specification.visibleAtNeutral ?? false,
    blendMode: "normal",
    pivot: specification.pivot,
    neutral: { x: 0, y: 0, rotationDegrees: 0, scaleX: 1, scaleY: 1 },
    limits: specification.limits ?? {},
    sha256: hash,
  };
}

function validateTopLevel(specification, manifest, actualSourceHash, label) {
  if (!object(specification) || specification.schemaVersion !== 1) throw new Error(`${label} schemaVersion must be 1`);
  assertCanvas(specification.canvas, manifest.canvas, label);
  if (!object(specification.source) || specification.source.file !== manifest.source.file) {
    throw new Error(`${label} source file must match rig source`);
  }
  if (specification.source.sha256 !== manifest.source.sha256
    || specification.source.sha256 !== actualSourceHash) {
    throw new Error(`${label} source sha256 mismatch`);
  }
}

async function writeArtPackage({
  manifestPath,
  repairsBytes,
  repairsFile,
  variantsBytes,
  variantsFile,
  temporaryDirectory,
}) {
  const originalDirectory = path.dirname(manifestPath);
  await cp(originalDirectory, temporaryDirectory, { recursive: true, errorOnExist: true });
  const temporaryManifestPath = path.join(temporaryDirectory, "rig.json");
  const manifest = JSON.parse(await readFile(temporaryManifestPath, "utf8"));
  const repairs = JSON.parse(repairsBytes.toString("utf8"));
  const variants = JSON.parse(variantsBytes.toString("utf8"));
  const sourceFile = path.resolve(originalDirectory, manifest.source.file);
  const actualSourceHash = await sha256(sourceFile);
  validateTopLevel(repairs, manifest, actualSourceHash, "repairs");
  validateTopLevel(variants, manifest, actualSourceHash, "variants");
  await Promise.all([
    writeFile(path.join(temporaryDirectory, "repairs.json"), repairsBytes),
    writeFile(path.join(temporaryDirectory, "variants.json"), variantsBytes),
  ]);
  const source = { file: sourceFile, image: await readRgba(sourceFile) };
  const layerById = new Map(manifest.layers.map((layer) => [layer.id, layer]));
  const repairEntries = [...(repairs.repairs ?? []), ...(repairs.overlays ?? [])];
  const configuredRepairIds = new Set();
  const additions = [];
  for (const repair of repairEntries) {
    assertSafeId(repair.id, "repair id");
    if (configuredRepairIds.has(repair.id)) throw new Error(`duplicate repair id: ${repair.id}`);
    configuredRepairIds.add(repair.id);
    const coverLayer = layerById.get(repair.cover);
    if (!coverLayer) throw new Error(`repair ${repair.id} references unknown cover ${repair.cover}`);
    const cover = await readRgba(path.join(temporaryDirectory, coverLayer.file));
    const input = await validateInput({
      configDirectory: path.dirname(repairsFile),
      expectedCanvas: manifest.canvas,
      expectedId: repair.id,
      expectedSourceHash: manifest.source.sha256,
      input: repair.input,
      polygons: repair.polygons,
      source,
    });
    let image = extractBounded(input, {
      crop: repair.input.crop,
      polygons: repair.polygons,
      sampleOffset: repair.input.sampleOffset,
      fallbackSample: repair.input.fallbackSample,
      chromaKey: repair.input.chromaKey,
      edgeFeatherPixels: repair.input.edgeFeatherPixels,
    });
    if (!repair.allowOutsideNeutralCover) image = maskToOpaqueCover(image, cover);
    const relative = `layers/${repair.id}.png`;
    const file = path.join(temporaryDirectory, relative);
    await writeRgba(file, image);
    additions.push(manifestLayer(repair, relative, await sha256(file)));
  }

  const variantManifest = {};
  const anchoredIds = new Set();
  await rm(path.join(temporaryDirectory, "variants"), { recursive: true, force: true });
  for (const set of variants.sets ?? []) {
    assertSafeId(set.id, "variant set id");
    if (Object.hasOwn(variantManifest, set.id)) throw new Error(`duplicate variant set id: ${set.id}`);
    const anchorLayer = layerById.get(set.anchorLayer);
    if (!anchorLayer) throw new Error(`variant set ${set.id} references unknown anchor layer ${set.anchorLayer}`);
    if (anchoredIds.has(anchorLayer.id)) throw new Error(`anchor layer ${anchorLayer.id} has multiple variant sets`);
    anchoredIds.add(anchorLayer.id);
    if (!object(set.registrationPivot)
      || set.registrationPivot.x !== anchorLayer.pivot.x
      || set.registrationPivot.y !== anchorLayer.pivot.y) {
      throw new Error(`variant set ${set.id} registration pivot must match anchor layer`);
    }
    if (!Array.isArray(set.members) || set.members.length === 0) throw new Error(`variant set ${set.id} members must be non-empty`);
    const neutralMembers = set.members.filter((member) => member.neutral);
    if (neutralMembers.length !== 1 || neutralMembers[0].id !== set.neutralMember) {
      throw new Error(`variant set ${set.id} must declare exactly one matching neutral member`);
    }
    const anchorImage = await readRgba(path.join(temporaryDirectory, anchorLayer.file));
    const members = [];
    const memberIds = new Set();
    for (const member of set.members) {
      assertSafeId(member.id, `variant ${set.id} member id`);
      if (memberIds.has(member.id)) throw new Error(`duplicate variant member id: ${set.id}/${member.id}`);
      memberIds.add(member.id);
      const expectedId = `${set.id}/${member.id}`;
      const input = await validateInput({
        anchor: { id: anchorLayer.id, image: anchorImage },
        configDirectory: path.dirname(variantsFile),
        expectedCanvas: manifest.canvas,
        expectedId,
        expectedSourceHash: manifest.source.sha256,
        input: member.input,
        polygons: member.polygons,
        source,
      });
      let image = extractBounded(input, {
        crop: member.input.crop,
        polygons: member.polygons,
        chromaKey: member.input.chromaKey,
        edgeFeatherPixels: member.input.edgeFeatherPixels,
      });
      if (member.input.constrainToAnchorAlpha) image = maskToOwnedAlpha(image, anchorImage);
      if (member.input.preserveAnchorOutsidePolygons) image = compositePatchOntoAnchor(image, anchorImage);
      if (member.neutral && !exactPixels(image, anchorImage)) {
        throw new Error(`neutral variant ${expectedId} must reproduce all source-owned anchor pixels exactly`);
      }
      const relative = `variants/${set.id}/${member.id}.png`;
      const file = path.join(temporaryDirectory, relative);
      await mkdir(path.dirname(file), { recursive: true });
      await writeRgba(file, image);
      members.push({
        id: member.id,
        file: relative,
        neutral: member.neutral,
        sha256: await sha256(file),
        ...(member.layerOverrides === undefined ? {} : { layerOverrides: member.layerOverrides }),
      });
    }
    anchorLayer.role = "variant-anchor";
    variantManifest[set.id] = { layer: anchorLayer.id, members };
  }

  manifest.layers = manifest.layers
    .filter((layer) => !configuredRepairIds.has(layer.id))
    .concat(additions)
    .toSorted((left, right) => left.drawOrder - right.drawOrder);
  manifest.variants = variantManifest;
  const masks = JSON.parse(await readFile(path.join(temporaryDirectory, "masks.json"), "utf8"));
  manifest.controls = masks.controls;
  await writeFile(temporaryManifestPath, `${JSON.stringify(manifest, null, 2)}\n`);
  return validateRig(temporaryManifestPath);
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

function artPromotionPaths(outputDirectory) {
  return {
    marker: `${outputDirectory}.art-promotion.json`,
    markerTemporary: `${outputDirectory}.art-promotion.json.building-${process.pid}`,
    previous: `${outputDirectory}.art-previous-${process.pid}`,
  };
}

async function artPreviousEntries(outputDirectory) {
  const prefix = `${path.basename(outputDirectory)}.art-previous-`;
  return (await readdir(path.dirname(outputDirectory))).filter((entry) => entry.startsWith(prefix));
}

async function validateArtDirectory(directory) {
  const info = await lstat(directory);
  if (!info.isDirectory() || info.isSymbolicLink()) throw new Error("art recovery package must be a nonsymlink directory");
  await validateRig(path.join(directory, "rig.json"));
}

async function recoverInterruptedArtPromotion(outputDirectory, renamePath) {
  const paths = artPromotionPaths(outputDirectory);
  const previousEntries = await artPreviousEntries(outputDirectory);
  if (!await exists(paths.marker)) {
    if (previousEntries.length > 0) throw new Error("ambiguous art promotion recovery state");
    return;
  }
  const markerInfo = await lstat(paths.marker);
  if (!markerInfo.isFile() || markerInfo.isSymbolicLink()) throw new Error("ambiguous art promotion recovery state");
  const marker = JSON.parse(await readFile(paths.marker, "utf8"));
  const outputName = path.basename(outputDirectory).replace(/[.*+?^${}()|[\]\\]/gu, "\\$&");
  const previousPattern = new RegExp(`^${outputName}\\.art-previous-\\d+$`, "u");
  if (marker.schemaVersion !== 1
    || marker.outputDirectory !== path.basename(outputDirectory)
    || typeof marker.previousDirectory !== "string"
    || !previousPattern.test(marker.previousDirectory)
    || previousEntries.length > 1
    || (previousEntries.length === 1 && previousEntries[0] !== marker.previousDirectory)) {
    throw new Error("ambiguous art promotion recovery state");
  }
  const previousDirectory = path.join(path.dirname(outputDirectory), marker.previousDirectory);
  const outputExists = await exists(outputDirectory);
  const previousExists = await exists(previousDirectory);
  if (!outputExists) {
    if (!previousExists) throw new Error("art promotion recovery state has no recoverable package");
    await validateArtDirectory(previousDirectory);
    await renamePath(previousDirectory, outputDirectory);
    await rm(paths.marker, { force: true });
    return;
  }
  await validateArtDirectory(outputDirectory);
  if (previousExists) {
    await validateArtDirectory(previousDirectory);
    await rm(previousDirectory, { recursive: true });
  }
  await rm(paths.marker, { force: true });
}

async function writeArtPromotionMarker(outputDirectory, previousDirectory, renamePath) {
  const paths = artPromotionPaths(outputDirectory);
  await writeFile(paths.markerTemporary, `${JSON.stringify({
    schemaVersion: 1,
    outputDirectory: path.basename(outputDirectory),
    previousDirectory: path.basename(previousDirectory),
  })}\n`);
  await renamePath(paths.markerTemporary, paths.marker);
  return paths.marker;
}

async function promote(outputDirectory, temporaryDirectory, renamePath) {
  const paths = artPromotionPaths(outputDirectory);
  const marker = await writeArtPromotionMarker(outputDirectory, paths.previous, renamePath);
  try {
    await renamePath(outputDirectory, paths.previous);
  } catch (error) {
    await validateArtDirectory(outputDirectory);
    await rm(marker, { force: true });
    throw error;
  }
  try {
    await renamePath(temporaryDirectory, outputDirectory);
  } catch (promotionError) {
    try {
      await renamePath(paths.previous, outputDirectory);
      await validateArtDirectory(outputDirectory);
      await rm(marker, { force: true });
    } catch (restorationError) {
      throw new Error(restorationError.message, { cause: promotionError });
    }
    throw promotionError;
  }
  try {
    await validateRig(path.join(outputDirectory, "rig.json"));
  } catch (error) {
    await rm(outputDirectory, { recursive: true, force: true });
    await renamePath(paths.previous, outputDirectory);
    await rm(marker, { force: true });
    throw error;
  }
  await rm(paths.previous, { recursive: true, force: true });
  await rm(marker, { force: true });
}

export async function buildRigV2Art({
  manifestPath,
  repairsFile,
  variantsFile,
  renamePath = rename,
}) {
  manifestPath = path.resolve(manifestPath);
  repairsFile = path.resolve(repairsFile);
  variantsFile = path.resolve(variantsFile);
  const outputDirectory = path.dirname(manifestPath);
  if (path.basename(manifestPath) !== "rig.json") throw new Error("manifest path must end in rig.json");
  if (path.resolve(outputDirectory).split(path.sep).includes("standing-v1")) throw new Error("refusing to write into standing-v1");
  await recoverInterruptedArtPromotion(outputDirectory, renamePath);
  const info = await lstat(outputDirectory);
  if (!info.isDirectory() || info.isSymbolicLink()) throw new Error("rig package must be a nonsymlink directory");
  const [repairsBytes, variantsBytes] = await Promise.all([readFile(repairsFile), readFile(variantsFile)]);
  const temporaryDirectory = `${outputDirectory}.art-building-${process.pid}`;
  await rm(temporaryDirectory, { recursive: true, force: true });
  try {
    const result = await writeArtPackage({
      manifestPath,
      repairsBytes,
      repairsFile,
      temporaryDirectory,
      variantsBytes,
      variantsFile,
    });
    await promote(outputDirectory, temporaryDirectory, renamePath);
    return result;
  } catch (error) {
    await rm(temporaryDirectory, { recursive: true, force: true });
    throw error;
  }
}

async function main(args) {
  if (args.length !== 3) {
    throw new Error("usage: build-waffle-rig-v2-art.mjs <rig.json> <repairs.json> <variants.json>");
  }
  const result = await buildRigV2Art({
    manifestPath: args[0],
    repairsFile: args[1],
    variantsFile: args[2],
  });
  console.log(`PASS ${args[0]} layers=${result.layerCount} mismatchPixels=${result.mismatchPixels}`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
