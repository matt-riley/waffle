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

function bilinearSample(source, x, y, output, outputOffset) {
  if (x < -0.5 || y < -0.5 || x > source.width - 0.5 || y > source.height - 0.5) return;
  const clampedX = Math.max(0, Math.min(source.width - 1, x));
  const clampedY = Math.max(0, Math.min(source.height - 1, y));
  const x0 = Math.floor(clampedX);
  const y0 = Math.floor(clampedY);
  const x1 = Math.min(source.width - 1, x0 + 1);
  const y1 = Math.min(source.height - 1, y0 + 1);
  const fx = clampedX - x0;
  const fy = clampedY - y0;
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
    const offset = (sampleY * source.width + sampleX) * 4;
    const normalizedAlpha = source.data[offset + 3] / 255;
    const weightedAlpha = normalizedAlpha * weight;
    alpha += weightedAlpha;
    red += source.data[offset] * weightedAlpha;
    green += source.data[offset + 1] * weightedAlpha;
    blue += source.data[offset + 2] * weightedAlpha;
  }
  output[outputOffset] = alpha === 0 ? 0 : Math.round(red / alpha);
  output[outputOffset + 1] = alpha === 0 ? 0 : Math.round(green / alpha);
  output[outputOffset + 2] = alpha === 0 ? 0 : Math.round(blue / alpha);
  output[outputOffset + 3] = Math.round(alpha * 255);
}

export function transformRgbaMatrix(source, matrix) {
  if (!Array.isArray(matrix) || matrix.length !== 6 || !matrix.every(Number.isFinite)) {
    throw new Error("transform matrix must contain six finite numbers");
  }
  if (matrix.every((value, index) => value === [1, 0, 0, 1, 0, 0][index])) {
    const identity = new PNG({ width: source.width, height: source.height });
    identity.data.set(source.data);
    return identity;
  }
  const [a, b, c, d, e, f] = matrix;
  const determinant = a * d - b * c;
  if (determinant === 0) throw new Error("transform matrix must be invertible");
  const output = new PNG({ width: source.width, height: source.height });
  for (let y = 0; y < output.height; y += 1) {
    for (let x = 0; x < output.width; x += 1) {
      const translatedX = x - e;
      const translatedY = y - f;
      const sourceX = (d * translatedX - c * translatedY) / determinant;
      const sourceY = (-b * translatedX + a * translatedY) / determinant;
      bilinearSample(source, sourceX, sourceY, output.data, (y * output.width + x) * 4);
    }
  }
  return output;
}

export function transformRgba(source, transform) {
  if (transform.scaleX === 0 || transform.scaleY === 0) throw new Error("transform scale must be non-zero");
  const pivotX = transform.pivot.x * (source.width - 1);
  const pivotY = transform.pivot.y * (source.height - 1);
  const radians = (transform.rotationDegrees * Math.PI) / 180;
  const cosine = Math.cos(radians);
  const sine = Math.sin(radians);
  const a = cosine * transform.scaleX;
  const b = sine * transform.scaleX;
  const c = -sine * transform.scaleY;
  const d = cosine * transform.scaleY;
  const translateX = transform.x * source.width;
  const translateY = transform.y * source.height;
  return transformRgbaMatrix(source, [
    a,
    b,
    c,
    d,
    pivotX - a * pivotX - c * pivotY + translateX,
    pivotY - b * pivotX - d * pivotY + translateY,
  ]);
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
