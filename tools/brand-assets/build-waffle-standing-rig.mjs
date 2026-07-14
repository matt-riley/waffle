import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

import { PNG } from "pngjs";

import { readRgba, writeRgba } from "./rig-raster.mjs";

export function pointInPolygon(x, y, points) {
  let inside = false;
  for (let current = 0, previous = points.length - 1; current < points.length; previous = current, current += 1) {
    const [currentX, currentY] = points[current];
    const [previousX, previousY] = points[previous];
    const crosses = (currentY > y) !== (previousY > y)
      && x < ((previousX - currentX) * (y - currentY)) / (previousY - currentY) + currentX;
    if (crosses) inside = !inside;
  }
  return inside;
}

export function partitionSource(source, regions, fallbackId) {
  const layers = new Map();
  for (const region of regions) layers.set(region.id, new PNG({ width: source.width, height: source.height }));
  layers.set(fallbackId, new PNG({ width: source.width, height: source.height }));

  for (let y = 0; y < source.height; y += 1) {
    for (let x = 0; x < source.width; x += 1) {
      const offset = (y * source.width + x) * 4;
      if (source.data[offset + 3] === 0) continue;
      const owner = regions.find((region) => region.polygons.some((polygon) => pointInPolygon(x + 0.5, y + 0.5, polygon)));
      layers.get(owner?.id ?? fallbackId).data.set(source.data.subarray(offset, offset + 4), offset);
    }
  }
  return layers;
}

async function sha256(file) {
  return createHash("sha256").update(await readFile(file)).digest("hex");
}

function localRelative(from, to) {
  return path.relative(from, to).split(path.sep).join("/");
}

export async function buildStandingRig({ sourceFile, masksFile, outputDirectory }) {
  const [source, masks] = await Promise.all([
    readRgba(sourceFile),
    readFile(masksFile, "utf8").then(JSON.parse),
  ]);
  if (source.width !== masks.canvas?.width || source.height !== masks.canvas?.height) {
    throw new Error(`mask canvas ${masks.canvas?.width}x${masks.canvas?.height} does not match source ${source.width}x${source.height}`);
  }
  if (!Array.isArray(masks.regionsFrontToBack) || typeof masks.fallback !== "string") {
    throw new Error("masks require regionsFrontToBack and fallback");
  }
  const layers = partitionSource(source, masks.regionsFrontToBack, masks.fallback);
  const layersDirectory = path.join(outputDirectory, "layers");
  await mkdir(layersDirectory, { recursive: true });
  const referenceFile = path.join(outputDirectory, "neutral-reference.png");
  await writeRgba(referenceFile, source);

  const definitions = Object.entries(masks.layerDefinitions ?? {})
    .map(([id, definition]) => ({ id, ...definition }))
    .toSorted((left, right) => left.drawOrder - right.drawOrder);
  if (definitions.length !== layers.size || definitions.some((definition) => !layers.has(definition.id))) {
    throw new Error("layerDefinitions must describe every partition layer exactly once");
  }

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
      sha256: await sha256(file),
    });
  }

  const manifest = {
    schemaVersion: 1,
    canvas: masks.canvas,
    source: {
      file: localRelative(outputDirectory, sourceFile),
      sha256: await sha256(sourceFile),
    },
    neutralReference: {
      file: "neutral-reference.png",
      sha256: await sha256(referenceFile),
    },
    layers: manifestLayers,
    controls: masks.controls,
  };
  const manifestFile = path.join(outputDirectory, "rig.json");
  await writeFile(manifestFile, `${JSON.stringify(manifest, null, 2)}\n`);
  return manifestFile;
}

async function main(args) {
  if (args.length !== 3) throw new Error("usage: build-waffle-standing-rig.mjs <standing.png> <masks.json> <output-directory>");
  const manifest = await buildStandingRig({
    sourceFile: path.resolve(args[0]),
    masksFile: path.resolve(args[1]),
    outputDirectory: path.resolve(args[2]),
  });
  console.log(`WROTE ${manifest}`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
