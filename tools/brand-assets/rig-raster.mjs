import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";

import { PNG } from "pngjs";

export async function readRgba(file) {
  return PNG.sync.read(await readFile(file));
}

export async function writeRgba(file, png) {
  await writeFile(file, PNG.sync.write(png));
}

export function sourceOver(bottom, top) {
  if (bottom.width !== top.width || bottom.height !== top.height) {
    throw new Error("sourceOver requires identical canvas dimensions");
  }
  const output = new PNG({ width: bottom.width, height: bottom.height });
  for (let offset = 0; offset < output.data.length; offset += 4) {
    const topA = top.data[offset + 3];
    if (topA === 0) {
      output.data.set(bottom.data.subarray(offset, offset + 4), offset);
      continue;
    }
    if (topA === 255) {
      output.data.set(top.data.subarray(offset, offset + 4), offset);
      continue;
    }
    const bottomA = bottom.data[offset + 3];
    const outA = topA + Math.round((bottomA * (255 - topA)) / 255);
    output.data[offset + 3] = outA;
    for (let channel = 0; channel < 3; channel += 1) {
      const topPremultiplied = top.data[offset + channel] * topA;
      const bottomPremultiplied = Math.round(
        (bottom.data[offset + channel] * bottomA * (255 - topA)) / 255,
      );
      output.data[offset + channel] = outA === 0
        ? 0
        : Math.round((topPremultiplied + bottomPremultiplied) / outA);
    }
  }
  return output;
}

export async function recomposeLayers(manifestPath) {
  const manifest = JSON.parse(await readFile(manifestPath, "utf8"));
  const directory = path.dirname(manifestPath);
  let output = new PNG({ width: manifest.canvas.width, height: manifest.canvas.height });
  const layers = manifest.layers
    .filter((layer) => layer.visibleAtNeutral)
    .toSorted((left, right) => left.drawOrder - right.drawOrder);
  for (const layer of layers) {
    output = sourceOver(output, await readRgba(path.resolve(directory, layer.file)));
  }
  return output;
}
