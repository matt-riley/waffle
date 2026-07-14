import { createHash } from "node:crypto";
import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

import { PNG } from "pngjs";

import { pointInPolygon } from "./build-waffle-standing-rig.mjs";
import { readRgba, writeRgba } from "./rig-raster.mjs";

function clamp(value, min, max) {
  return Math.max(min, Math.min(max, value));
}

export function buildCoveredRepair(source, cover, specification) {
  if (source.width !== cover.width || source.height !== cover.height) {
    throw new Error("source and cover must use identical dimensions");
  }
  const output = new PNG({ width: source.width, height: source.height });
  const offsetX = specification.sampleOffset?.x ?? 0;
  const offsetY = specification.sampleOffset?.y ?? 0;
  for (let y = 0; y < source.height; y += 1) {
    for (let x = 0; x < source.width; x += 1) {
      if (!specification.polygons.some((polygon) => pointInPolygon(x + 0.5, y + 0.5, polygon))) continue;
      const targetOffset = (y * source.width + x) * 4;
      if (!specification.allowOutsideCover && cover.data[targetOffset + 3] !== 255) continue;
      const sampleX = clamp(x + offsetX, 0, source.width - 1);
      const sampleY = clamp(y + offsetY, 0, source.height - 1);
      let sampleOffset = (sampleY * source.width + sampleX) * 4;
      if (source.data[sampleOffset + 3] === 0 && specification.fallbackSample) {
        const fallbackX = clamp(specification.fallbackSample.x, 0, source.width - 1);
        const fallbackY = clamp(specification.fallbackSample.y, 0, source.height - 1);
        sampleOffset = (fallbackY * source.width + fallbackX) * 4;
      }
      if (source.data[sampleOffset + 3] === 0) continue;
      output.data.set(source.data.subarray(sampleOffset, sampleOffset + 4), targetOffset);
    }
  }
  return output;
}

export function buildClosedLid(source, specification) {
  const output = new PNG({ width: source.width, height: source.height });
  const { center, radius, sampleBand, lash } = specification;
  const left = Math.floor(center.x - radius.x);
  const right = Math.ceil(center.x + radius.x);
  const top = Math.floor(center.y - radius.y);
  const bottom = Math.ceil(center.y + radius.y);

  for (let y = top; y <= bottom; y += 1) {
    for (let x = left; x <= right; x += 1) {
      if (x < 0 || x >= source.width || y < 0 || y >= source.height) continue;
      const dx = (x - center.x) / radius.x;
      const dy = (y - center.y) / radius.y;
      if (dx * dx + dy * dy > 1) continue;
      const normalizedY = clamp((y - top) / Math.max(1, bottom - top), 0, 1);
      const sampleY = Math.round(sampleBand.top + normalizedY * (sampleBand.bottom - sampleBand.top));
      const sampleX = clamp(x, 0, source.width - 1);
      const sampleOffset = (clamp(sampleY, 0, source.height - 1) * source.width + sampleX) * 4;
      const targetOffset = (y * source.width + x) * 4;
      output.data.set(source.data.subarray(sampleOffset, sampleOffset + 4), targetOffset);
    }
  }

  for (let x = left; x <= right; x += 1) {
    if (x < 0 || x >= source.width) continue;
    const dx = x - center.x;
    const curveY = lash.y - lash.arch * dx * dx;
    for (let y = Math.floor(curveY - lash.thickness); y <= Math.ceil(curveY + lash.thickness); y += 1) {
      if (y < 0 || y >= source.height || Math.abs(y - curveY) > lash.thickness) continue;
      const ellipseX = (x - center.x) / radius.x;
      const ellipseY = (y - center.y) / radius.y;
      if (ellipseX * ellipseX + ellipseY * ellipseY > 1) continue;
      output.data.set(lash.color, (y * source.width + x) * 4);
    }
  }
  return output;
}

export function buildMappedLid(reference, canvas, specification) {
  const output = new PNG({ width: canvas.width, height: canvas.height });
  const { center, radius } = specification;
  const feather = specification.feather ?? 0;
  const left = Math.floor(center.x - radius.x);
  const right = Math.ceil(center.x + radius.x);
  const top = Math.floor(center.y - radius.y);
  const bottom = Math.ceil(center.y + radius.y);
  for (let y = top; y <= bottom; y += 1) {
    for (let x = left; x <= right; x += 1) {
      if (x < 0 || x >= output.width || y < 0 || y >= output.height) continue;
      const normalizedX = (x - center.x) / radius.x;
      const normalizedY = (y - center.y) / radius.y;
      const distance = Math.sqrt(normalizedX * normalizedX + normalizedY * normalizedY);
      if (distance > 1) continue;
      const sampleX = clamp(
        Math.round(specification.reference.center.x + normalizedX * specification.reference.radius.x),
        0,
        reference.width - 1,
      );
      const sampleY = clamp(
        Math.round(specification.reference.center.y + normalizedY * specification.reference.radius.y),
        0,
        reference.height - 1,
      );
      const sampleOffset = (sampleY * reference.width + sampleX) * 4;
      const targetOffset = (y * output.width + x) * 4;
      output.data.set(reference.data.subarray(sampleOffset, sampleOffset + 4), targetOffset);
      if (feather > 0 && distance > 1 - feather) {
        output.data[targetOffset + 3] = Math.round(
          output.data[targetOffset + 3] * clamp((1 - distance) / feather, 0, 1),
        );
      }
    }
  }
  return output;
}

async function sha256(file) {
  return createHash("sha256").update(await readFile(file)).digest("hex");
}

function manifestLayer(specification, file, hash, role, visibleAtNeutral) {
  return {
    id: specification.id,
    file,
    role,
    parent: specification.parent,
    drawOrder: specification.drawOrder,
    visibleAtNeutral,
    blendMode: "normal",
    pivot: specification.pivot,
    neutral: { x: 0, y: 0, rotationDegrees: 0, scaleX: 1, scaleY: 1 },
    sha256: hash,
  };
}

export async function addRepairLayers({ manifestPath, repairsFile, lidReferenceFile }) {
  const directory = path.dirname(manifestPath);
  const manifest = JSON.parse(await readFile(manifestPath, "utf8"));
  const specification = JSON.parse(await readFile(repairsFile, "utf8"));
  const source = await readRgba(path.resolve(directory, manifest.source.file));
  const lidReference = lidReferenceFile ? await readRgba(lidReferenceFile) : null;
  const existing = new Map(manifest.layers.map((layer) => [layer.id, layer]));
  const additions = [];

  for (const repair of specification.repairs ?? []) {
    const coverLayer = existing.get(repair.cover);
    if (!coverLayer) throw new Error(`repair ${repair.id} references unknown cover ${repair.cover}`);
    const cover = await readRgba(path.resolve(directory, coverLayer.file));
    const output = buildCoveredRepair(source, cover, repair);
    const relative = `layers/${repair.id}.png`;
    const file = path.join(directory, relative);
    await writeRgba(file, output);
    additions.push(manifestLayer(
      repair,
      relative,
      await sha256(file),
      "repair",
      repair.visibleAtNeutral ?? true,
    ));
  }

  for (const lid of specification.lids ?? []) {
    const output = lidReference && lid.reference
      ? buildMappedLid(lidReference, manifest.canvas, lid)
      : buildClosedLid(source, lid);
    const relative = `layers/${lid.id}.png`;
    const file = path.join(directory, relative);
    await writeRgba(file, output);
    additions.push(manifestLayer(lid, relative, await sha256(file), "overlay", false));
  }

  const additionIds = new Set(additions.map((layer) => layer.id));
  manifest.layers = manifest.layers
    .filter((layer) => !additionIds.has(layer.id))
    .concat(additions)
    .toSorted((left, right) => left.drawOrder - right.drawOrder);
  await writeFile(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`);
  return manifestPath;
}

async function main(args) {
  if (args.length < 2 || args.length > 3) throw new Error("usage: build-waffle-rig-repairs.mjs <rig.json> <repairs.json> [lid-reference.png]");
  const result = await addRepairLayers({
    manifestPath: path.resolve(args[0]),
    repairsFile: path.resolve(args[1]),
    lidReferenceFile: args[2] ? path.resolve(args[2]) : undefined,
  });
  console.log(`UPDATED ${result}`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
