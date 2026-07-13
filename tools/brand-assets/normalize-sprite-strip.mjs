import { mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

import { PNG } from "pngjs";

import { resizeRgba } from "./resize-raster.mjs";

function positiveInteger(value, label) {
  const number = Number(value);
  if (!Number.isInteger(number) || number <= 0) throw new Error(`${label} must be a positive integer`);
  return number;
}

function alphaBounds(image, left, right) {
  let minX = right;
  let minY = image.height;
  let maxX = -1;
  let maxY = -1;
  for (let y = 0; y < image.height; y += 1) {
    for (let x = left; x < right; x += 1) {
      if (image.data[(y * image.width + x) * 4 + 3] === 0) continue;
      minX = Math.min(minX, x);
      minY = Math.min(minY, y);
      maxX = Math.max(maxX, x);
      maxY = Math.max(maxY, y);
    }
  }
  if (maxX < 0) throw new Error(`sprite slot at x=${left} has no visible pixels`);
  return { minX, minY, maxX, maxY, width: maxX - minX + 1, height: maxY - minY + 1 };
}

function crop(image, bounds) {
  const data = Buffer.alloc(bounds.width * bounds.height * 4);
  for (let y = 0; y < bounds.height; y += 1) {
    const start = ((bounds.minY + y) * image.width + bounds.minX) * 4;
    image.data.copy(data, y * bounds.width * 4, start, start + bounds.width * 4);
  }
  return data;
}

export async function normalizeSpriteStrip(stripPath, outputDirectory, seedPath, options = {}) {
  const frameCount = options.frameCount ?? 6;
  const outputSize = options.outputSize ?? 256;
  const anchorX = options.anchorX ?? outputSize / 2;
  const anchorY = options.anchorY ?? 240;
  const strip = PNG.sync.read(await readFile(stripPath));
  if (strip.width % frameCount !== 0) throw new Error("strip width must divide evenly into frame count");
  const slotWidth = strip.width / frameCount;
  const bounds = Array.from({ length: frameCount }, (_, index) =>
    alphaBounds(strip, index * slotWidth, (index + 1) * slotWidth));
  const maxWidth = Math.max(...bounds.map((value) => value.width));
  const maxHeight = Math.max(...bounds.map((value) => value.height));
  const scale = Math.min(outputSize / maxWidth, anchorY / maxHeight);
  await mkdir(outputDirectory, { recursive: true });

  for (let index = 0; index < frameCount; index += 1) {
    const outputPath = path.join(outputDirectory, `frame-${String(index + 1).padStart(2, "0")}.png`);
    if (index === 0) {
      const seed = PNG.sync.read(await readFile(seedPath));
      if (seed.width !== outputSize || seed.height !== outputSize) {
        throw new Error(`seed must be ${outputSize}x${outputSize}`);
      }
      await writeFile(outputPath, await readFile(seedPath));
      continue;
    }
    const box = bounds[index];
    const width = Math.max(1, Math.round(box.width * scale));
    const height = Math.max(1, Math.round(box.height * scale));
    const resized = resizeRgba(crop(strip, box), box.width, box.height, width, height);
    const frame = new PNG({ width: outputSize, height: outputSize });
    const left = Math.round(anchorX - width / 2);
    const top = Math.round(anchorY - height);
    for (let y = 0; y < height; y += 1) {
      if (top + y < 0 || top + y >= outputSize) continue;
      for (let x = 0; x < width; x += 1) {
        if (left + x < 0 || left + x >= outputSize) continue;
        const sourceOffset = (y * width + x) * 4;
        const targetOffset = ((top + y) * outputSize + left + x) * 4;
        resized.copy(frame.data, targetOffset, sourceOffset, sourceOffset + 4);
      }
    }
    await writeFile(outputPath, PNG.sync.write(frame));
  }
  return { frameCount, outputSize, anchorX, anchorY, scale };
}

async function main(args) {
  if (args.length < 3 || args.length > 7) {
    throw new Error("usage: normalize-sprite-strip.mjs <strip.png> <output-dir> <seed.png> [frames] [size] [anchor-x] [anchor-y]");
  }
  const options = {
    frameCount: positiveInteger(args[3] ?? 6, "frame count"),
    outputSize: positiveInteger(args[4] ?? 256, "output size"),
    anchorX: Number(args[5] ?? 128),
    anchorY: Number(args[6] ?? 240),
  };
  if (!Number.isFinite(options.anchorX) || !Number.isFinite(options.anchorY)) throw new Error("anchors must be numbers");
  const result = await normalizeSpriteStrip(args[0], args[1], args[2], options);
  console.log(`WROTE ${result.frameCount} frames ${result.outputSize}x${result.outputSize}`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
