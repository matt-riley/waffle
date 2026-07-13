import { mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

import { PNG } from "pngjs";

function positiveInteger(value, label) {
  const number = Number(value);
  if (!Number.isInteger(number) || number <= 0) throw new Error(`${label} must be a positive integer`);
  return number;
}

export async function buildSpriteEditCanvas(seedPath, outputPath, options = {}) {
  const slots = options.slots ?? 6;
  const slotSize = options.slotSize ?? 256;
  const canvasSize = options.canvasSize ?? 1536;
  if (slots * slotSize > canvasSize) throw new Error("sprite row does not fit canvas");
  const seed = PNG.sync.read(await readFile(seedPath));
  if (seed.width !== slotSize || seed.height !== slotSize) {
    throw new Error(`seed must be ${slotSize}x${slotSize}`);
  }
  const canvas = new PNG({ width: canvasSize, height: canvasSize });
  for (let offset = 0; offset < canvas.data.length; offset += 4) {
    canvas.data[offset] = 0;
    canvas.data[offset + 1] = 0;
    canvas.data[offset + 2] = 255;
    canvas.data[offset + 3] = 255;
  }
  const originX = Math.floor((canvasSize - slots * slotSize) / 2);
  const originY = Math.floor((canvasSize - slotSize) / 2);
  for (let y = 0; y < slotSize; y += 1) {
    for (let x = 0; x < slotSize; x += 1) {
      const sourceOffset = (y * slotSize + x) * 4;
      const targetOffset = ((originY + y) * canvasSize + originX + x) * 4;
      const alpha = seed.data[sourceOffset + 3] / 255;
      canvas.data[targetOffset] = Math.round(seed.data[sourceOffset] * alpha);
      canvas.data[targetOffset + 1] = Math.round(seed.data[sourceOffset + 1] * alpha);
      canvas.data[targetOffset + 2] = Math.round(
        seed.data[sourceOffset + 2] * alpha + 255 * (1 - alpha),
      );
      canvas.data[targetOffset + 3] = 255;
    }
  }
  await mkdir(path.dirname(outputPath), { recursive: true });
  await writeFile(outputPath, PNG.sync.write(canvas));
  return { width: canvasSize, height: canvasSize, originX, originY };
}

async function main(args) {
  if (args.length < 2 || args.length > 5) {
    throw new Error("usage: build-sprite-edit-canvas.mjs <seed.png> <output.png> [slots] [slot-size] [canvas-size]");
  }
  const options = {
    slots: positiveInteger(args[2] ?? 6, "slots"),
    slotSize: positiveInteger(args[3] ?? 256, "slot size"),
    canvasSize: positiveInteger(args[4] ?? 1536, "canvas size"),
  };
  const result = await buildSpriteEditCanvas(args[0], args[1], options);
  console.log(`WROTE ${args[1]} ${result.width}x${result.height}`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
