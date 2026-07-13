import { mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

import { PNG } from "pngjs";

function positiveInteger(value, label) {
  const number = Number(value);
  if (!Number.isInteger(number) || number <= 0) {
    throw new Error(`${label} must be a positive integer`);
  }
  return number;
}

export function resizeRgba(source, sourceWidth, sourceHeight, width, height) {
  const output = Buffer.alloc(width * height * 4);
  for (let y = 0; y < height; y += 1) {
    const sourceY = ((y + 0.5) * sourceHeight) / height - 0.5;
    const y0 = Math.max(0, Math.floor(sourceY));
    const y1 = Math.min(sourceHeight - 1, y0 + 1);
    const fy = Math.max(0, sourceY - y0);
    for (let x = 0; x < width; x += 1) {
      const sourceX = ((x + 0.5) * sourceWidth) / width - 0.5;
      const x0 = Math.max(0, Math.floor(sourceX));
      const x1 = Math.min(sourceWidth - 1, x0 + 1);
      const fx = Math.max(0, sourceX - x0);
      const samples = [
        [x0, y0, (1 - fx) * (1 - fy)],
        [x1, y0, fx * (1 - fy)],
        [x0, y1, (1 - fx) * fy],
        [x1, y1, fx * fy],
      ];
      let alpha = 0;
      let red = 0;
      let green = 0;
      let blue = 0;
      for (const [sampleX, sampleY, weight] of samples) {
        const offset = (sampleY * sourceWidth + sampleX) * 4;
        const normalizedAlpha = source[offset + 3] / 255;
        const weightedAlpha = normalizedAlpha * weight;
        alpha += weightedAlpha;
        red += source[offset] * weightedAlpha;
        green += source[offset + 1] * weightedAlpha;
        blue += source[offset + 2] * weightedAlpha;
      }
      const outputOffset = (y * width + x) * 4;
      output[outputOffset] = alpha === 0 ? 0 : Math.round(red / alpha);
      output[outputOffset + 1] = alpha === 0 ? 0 : Math.round(green / alpha);
      output[outputOffset + 2] = alpha === 0 ? 0 : Math.round(blue / alpha);
      output[outputOffset + 3] = Math.round(alpha * 255);
    }
  }
  return output;
}

export async function resizePng(input, output, width, height) {
  const decoded = PNG.sync.read(await readFile(input));
  const png = new PNG({ width, height });
  png.data = resizeRgba(decoded.data, decoded.width, decoded.height, width, height);
  await mkdir(path.dirname(output), { recursive: true });
  await writeFile(output, PNG.sync.write(png));
}

async function main(args) {
  if (args.length !== 4) {
    throw new Error("usage: resize-raster.mjs <input.png> <output.png> <width> <height>");
  }
  const width = positiveInteger(args[2], "width");
  const height = positiveInteger(args[3], "height");
  await resizePng(args[0], args[1], width, height);
  console.log(`WROTE ${args[1]} ${width}x${height}`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
