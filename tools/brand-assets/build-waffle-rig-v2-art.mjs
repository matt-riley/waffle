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
  let alpha = 0;
  for (const polygon of containing) {
    let distance = Infinity;
    for (let index = 0; index < polygon.length; index += 1) {
      distance = Math.min(distance, pointToSegmentDistance(
        x,
        y,
        polygon[index],
        polygon[(index + 1) % polygon.length],
      ));
    }
    alpha = Math.max(alpha, smoothstep((distance - 0.5) / pixels));
  }
  return alpha;
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

function removeAlphaIslandsBelowPixels(image, minimumPixels) {
  if (!Number.isInteger(minimumPixels) || minimumPixels <= 0) {
    throw new Error("removeAlphaIslandsBelowPixels must be a positive integer");
  }
  const { width, height } = image;
  const visited = new Uint8Array(width * height);
  for (let start = 0; start < visited.length; start += 1) {
    if (visited[start] || image.data[start * 4 + 3] === 0) continue;
    const component = [];
    const queue = [start];
    visited[start] = 1;
    while (queue.length > 0) {
      const index = queue.pop();
      component.push(index);
      const x = index % width;
      const y = Math.floor(index / width);
      for (const neighbor of [
        x > 0 ? index - 1 : -1,
        x + 1 < width ? index + 1 : -1,
        y > 0 ? index - width : -1,
        y + 1 < height ? index + width : -1,
      ]) {
        if (neighbor < 0 || visited[neighbor] || image.data[neighbor * 4 + 3] === 0) continue;
        visited[neighbor] = 1;
        queue.push(neighbor);
      }
    }
    if (component.length >= minimumPixels) continue;
    for (const index of component) image.data.fill(0, index * 4, index * 4 + 4);
  }
  return image;
}

function distanceToTransparent(image, x, y, limit) {
  let nearest = Infinity;
  const radius = Math.ceil(limit);
  for (let sampleY = y - radius; sampleY <= y + radius; sampleY += 1) {
    for (let sampleX = x - radius; sampleX <= x + radius; sampleX += 1) {
      const distance = Math.hypot(sampleX - x, sampleY - y);
      if (distance >= nearest || distance > limit) continue;
      if (sampleX < 0 || sampleY < 0 || sampleX >= image.width || sampleY >= image.height
        || image.data[(sampleY * image.width + sampleX) * 4 + 3] === 0) {
        nearest = distance;
      }
    }
  }
  return nearest;
}

function featherInternalProximalCut(image, fullCat, specification) {
  if (!object(specification)
    || !Number.isInteger(specification.startY)
    || !Number.isInteger(specification.endY)
    || !Number.isFinite(specification.fullCatBoundaryPreservePixels)
    || specification.startY < 0
    || specification.endY <= specification.startY
    || specification.endY >= image.height
    || specification.fullCatBoundaryPreservePixels < 0) {
    throw new Error("proximalFeather must declare an increasing in-canvas y range and non-negative external-boundary preservation distance");
  }
  for (let y = specification.startY; y <= specification.endY; y += 1) {
    const alphaScale = smoothstep((y - specification.startY) / (specification.endY - specification.startY));
    for (let x = 0; x < image.width; x += 1) {
      const offset = (y * image.width + x) * 4;
      if (image.data[offset + 3] === 0
        || (specification.fullCatBoundaryPreservePixels > 0
          && distanceToTransparent(fullCat, x, y, specification.fullCatBoundaryPreservePixels)
            < specification.fullCatBoundaryPreservePixels)) continue;
      image.data[offset + 3] = clampByte(image.data[offset + 3] * alphaScale);
      if (image.data[offset + 3] <= ALPHA_NOISE_FLOOR) image.data.fill(0, offset, offset + 4);
    }
  }
  return image;
}

export function maskToFullCatInterior(
  image,
  fullCat,
  minimumDistancePixels,
  boundaryReliefPolygons = [],
  boundaryReliefFeatherPixels = 0,
) {
  if (!Number.isFinite(minimumDistancePixels) || minimumDistancePixels <= 0) {
    throw new Error("internalOnlyDistancePixels must be a positive number");
  }
  if (!Array.isArray(boundaryReliefPolygons)) {
    throw new Error("boundaryReliefPolygons must be an array of in-canvas polygons");
  }
  if (!Number.isFinite(boundaryReliefFeatherPixels) || boundaryReliefFeatherPixels < 0) {
    throw new Error("boundaryReliefFeatherPixels must be a non-negative number");
  }
  if (boundaryReliefPolygons.length > 0) {
    validateCrop(
      { x: 0, y: 0, width: image.width, height: image.height },
      { width: image.width, height: image.height },
      boundaryReliefPolygons,
    );
  }
  for (let y = 0; y < image.height; y += 1) {
    for (let x = 0; x < image.width; x += 1) {
      const offset = (y * image.width + x) * 4;
      if (image.data[offset + 3] === 0) continue;
      if (distanceToTransparent(fullCat, x, y, minimumDistancePixels) < minimumDistancePixels) {
        const insideBoundaryRelief = boundaryReliefPolygons.some((polygon) => pointInPolygon(x, y, polygon));
        if (!insideBoundaryRelief) {
          image.data.fill(0, offset, offset + 4);
        } else if (boundaryReliefFeatherPixels > 0) {
          image.data[offset + 3] = clampByte(
            image.data[offset + 3]
              * edgeFeatherAlpha(x, y, boundaryReliefPolygons, boundaryReliefFeatherPixels),
          );
          if (image.data[offset + 3] <= ALPHA_NOISE_FLOOR) image.data.fill(0, offset, offset + 4);
        }
      }
    }
  }
  return image;
}

function assertProximalUnderlayOutput(image, specification, id) {
  const bounds = specification.outputBounds;
  if (!object(bounds)
    || !Number.isInteger(bounds.minNonzeroPixels)
    || bounds.minNonzeroPixels <= 0
    || !Number.isInteger(bounds.maxHeight)
    || bounds.maxHeight <= 0
    || !Number.isInteger(bounds.maxY)
    || !object(bounds.within)
    || !["x", "y", "width", "height"].every((key) => Number.isInteger(bounds.within[key]))
    || bounds.within.width <= 0
    || bounds.within.height <= 0
    || (bounds.forbidden !== undefined && (!Array.isArray(bounds.forbidden)
      || !bounds.forbidden.every((region) => object(region)
        && ["x", "y", "width", "height"].every((key) => Number.isInteger(region[key]))
        && region.width > 0
        && region.height > 0)))) {
    throw new Error(`repair ${id} proximal underlay outputBounds must declare positive coverage, height, max-y, containment, and optional forbidden rectangles`);
  }
  let nonzeroPixels = 0;
  let minX = image.width;
  let minY = image.height;
  let maxX = -1;
  let maxY = -1;
  for (let y = 0; y < image.height; y += 1) {
    for (let x = 0; x < image.width; x += 1) {
      if (image.data[(y * image.width + x) * 4 + 3] === 0) continue;
      nonzeroPixels += 1;
      minX = Math.min(minX, x);
      minY = Math.min(minY, y);
      maxX = Math.max(maxX, x);
      maxY = Math.max(maxY, y);
      if ((bounds.forbidden ?? []).some((region) => x >= region.x
        && x < region.x + region.width
        && y >= region.y
        && y < region.y + region.height)) {
        throw new Error(`repair ${id} proximal underlay contains forbidden distal alpha at ${x},${y}`);
      }
    }
  }
  if (nonzeroPixels < bounds.minNonzeroPixels) {
    throw new Error(`repair ${id} proximal underlay has ${nonzeroPixels} nonzero pixels; expected at least ${bounds.minNonzeroPixels}`);
  }
  const height = maxY - minY + 1;
  const within = bounds.within;
  if (height > bounds.maxHeight
    || maxY > bounds.maxY
    || minX < within.x
    || minY < within.y
    || maxX >= within.x + within.width
    || maxY >= within.y + within.height) {
    throw new Error(`repair ${id} proximal underlay alpha bounds ${minX},${minY}..${maxX},${maxY} exceed the authored joint envelope`);
  }
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

function innerEdgeHueCandidate(data, offset, specification) {
  const red = data[offset];
  const green = data[offset + 1];
  const blue = data[offset + 2];
  const alpha = data[offset + 3];
  return alpha > specification.alphaThreshold
    && red - green >= specification.redOverGreenMinimum
    && blue + specification.blueBelowGreenMaximum >= green;
}

export function despillInnerEdge(image, specification) {
  if (!image || !object(specification) || !object(specification.bounds)) {
    throw new Error("inner-edge despill requires an image and bounds");
  }
  const { bounds } = specification;
  for (const [name, value] of Object.entries(bounds)) {
    if (!Number.isInteger(value) || value < (name === "width" || name === "height" ? 1 : 0)) {
      throw new Error(`inner-edge despill bounds ${name} must be a valid integer`);
    }
  }
  if (bounds.x + bounds.width > image.width || bounds.y + bounds.height > image.height) {
    throw new Error("inner-edge despill bounds must stay inside the canvas");
  }
  for (const name of [
    "edgeDepthPixels",
    "sampleSearchPixels",
    "alphaThreshold",
    "redOverGreenMinimum",
    "blueBelowGreenMaximum",
    "minimumSampleAlpha",
  ]) {
    if (!Number.isInteger(specification[name]) || specification[name] < 0) {
      throw new Error(`inner-edge despill ${name} must be a non-negative integer`);
    }
  }
  if (specification.edgeDepthPixels < 1
    || specification.sampleSearchPixels < specification.edgeDepthPixels
    || specification.alphaThreshold > 254
    || specification.minimumSampleAlpha > 255) {
    throw new Error("inner-edge despill depth, search, and alpha thresholds are invalid");
  }

  const output = new PNG({ width: image.width, height: image.height });
  output.data.set(image.data);
  const endX = bounds.x + bounds.width - 1;
  const endY = bounds.y + bounds.height - 1;

  function rowInnerEdgeX(y) {
    for (let x = bounds.x; x <= endX; x += 1) {
      if (image.data[(y * image.width + x) * 4 + 3] > specification.alphaThreshold) return x;
    }
    return -1;
  }

  function rowSampleOffset(y) {
    const innerEdgeX = rowInnerEdgeX(y);
    if (innerEdgeX < 0) return -1;
    const sampleEndX = Math.min(innerEdgeX + specification.sampleSearchPixels, endX);
    for (let x = innerEdgeX + specification.edgeDepthPixels; x <= sampleEndX; x += 1) {
      const offset = (y * image.width + x) * 4;
      if (image.data[offset + 3] >= specification.minimumSampleAlpha
        && !innerEdgeHueCandidate(image.data, offset, specification)) return offset;
    }
    return -1;
  }

  for (let y = bounds.y; y <= endY; y += 1) {
    const innerEdgeX = rowInnerEdgeX(y);
    if (innerEdgeX < 0) continue;
    let sampleOffset = rowSampleOffset(y);
    for (let distance = 1; sampleOffset < 0 && distance <= specification.sampleSearchPixels; distance += 1) {
      const previousY = y - distance;
      const nextY = y + distance;
      if (previousY >= bounds.y) sampleOffset = rowSampleOffset(previousY);
      if (sampleOffset < 0 && nextY <= endY) sampleOffset = rowSampleOffset(nextY);
    }
    if (sampleOffset < 0) continue;
    const correctionEndX = Math.min(innerEdgeX + specification.edgeDepthPixels - 1, endX);
    for (let x = innerEdgeX; x <= correctionEndX; x += 1) {
      const offset = (y * image.width + x) * 4;
      if (!innerEdgeHueCandidate(image.data, offset, specification)) continue;
      output.data.set(image.data.subarray(sampleOffset, sampleOffset + 3), offset);
    }
  }
  return output;
}

export function buildHybridLandingPlate(lowLift, neutral, specification) {
  if (!lowLift || !neutral || lowLift.width !== neutral.width || lowLift.height !== neutral.height) {
    throw new Error("hybrid landing inputs must have the same canvas");
  }
  const {
    baseOffsetPixels = { x: 0, y: 0 },
    neutralDistalPolygons,
    seamY,
    transitionStartY,
  } = specification ?? {};
  if (!Number.isInteger(seamY)
    || !Number.isInteger(transitionStartY)
    || transitionStartY < 0
    || transitionStartY >= seamY
    || seamY > lowLift.height) {
    throw new Error("hybrid landing seam must be an ordered integer interval inside the canvas");
  }
  if (!Array.isArray(neutralDistalPolygons) || neutralDistalPolygons.length === 0) {
    throw new Error("hybrid landing must declare neutral distal polygons");
  }
  if (!object(baseOffsetPixels)
    || !Number.isInteger(baseOffsetPixels.x)
    || !Number.isInteger(baseOffsetPixels.y)) {
    throw new Error("hybrid landing base offset must use integer pixels");
  }

  const output = new PNG({ width: lowLift.width, height: lowLift.height });
  for (let y = 0; y < lowLift.height; y += 1) {
    const targetY = y + baseOffsetPixels.y;
    if (targetY < 0 || targetY >= output.height) continue;
    for (let x = 0; x < lowLift.width; x += 1) {
      const targetX = x + baseOffsetPixels.x;
      if (targetX < 0 || targetX >= output.width) continue;
      const sourceOffset = (y * lowLift.width + x) * 4;
      const targetOffset = (targetY * output.width + targetX) * 4;
      output.data.set(lowLift.data.subarray(sourceOffset, sourceOffset + 4), targetOffset);
    }
  }
  for (let y = transitionStartY; y < output.height; y += 1) {
    for (let x = 0; x < output.width; x += 1) {
      const offset = (y * output.width + x) * 4;
      const insideDistal = neutralDistalPolygons.some((polygon) => pointInPolygon(x, y, polygon));
      if (y >= seamY) {
        if (insideDistal) output.data.set(neutral.data.subarray(offset, offset + 4), offset);
        else output.data.fill(0, offset, offset + 4);
        continue;
      }
      const progress = (y - transitionStartY) / (seamY - transitionStartY);
      const liftAlpha = output.data[offset + 3];
      const neutralAlpha = insideDistal ? neutral.data[offset + 3] : 0;
      const blendedAlpha = liftAlpha * (1 - progress) + neutralAlpha * progress;
      const outputAlpha = Math.round(blendedAlpha);
      for (let channel = 0; channel < 3; channel += 1) {
        const liftValue = output.data[offset + channel] * liftAlpha * (1 - progress);
        const neutralValue = insideDistal
          ? neutral.data[offset + channel] * neutralAlpha * progress
          : 0;
        output.data[offset + channel] = blendedAlpha === 0
          ? 0
          : Math.round((liftValue + neutralValue) / blendedAlpha);
      }
      output.data[offset + 3] = outputAlpha;
    }
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
  builtMembers,
}) {
  if (!object(input)) throw new Error(`${expectedId} input must be an object`);
  assertCanvas(input.canvas, expectedCanvas, "input");
  if (input.sourceSha256 !== expectedSourceHash) throw new Error(`input source sha256 mismatch for ${expectedId}`);
  validateCrop(input.crop, expectedCanvas, polygons);

  if (input.kind === "anchor-layer") {
    const declared = input.expectedVariantId ?? input.expectedId;
    if (declared !== expectedId) throw new Error(`expected anchor-layer id must be ${expectedId}`);
    if (!anchor || input.layer !== anchor.id) throw new Error(`${expectedId} must reference anchor layer ${anchor?.id ?? "missing"}`);
    return anchor.image;
  }

  if (input.kind === "source-sample") {
    if (input.expectedId !== expectedId) throw new Error(`expected repair id must be ${expectedId}`);
    const file = resolveInput(configDirectory, input.file, `${expectedId} source sample`);
    if (path.resolve(file) !== path.resolve(source.file)) throw new Error(`${expectedId} source sample must use the declared standing source`);
    return source.image;
  }

  if (input.kind === "hybrid-landing") {
    if (input.expectedVariantId !== expectedId) throw new Error(`expected variant id must be ${expectedId}`);
    assertSafeId(input.baseMember, `${expectedId} hybrid base member`);
    const lowLift = builtMembers?.get(input.baseMember);
    if (!lowLift) throw new Error(`${expectedId} hybrid base member ${input.baseMember} must be built first`);
    validateCrop(input.crop, expectedCanvas, input.neutralDistalPolygons);
    return buildHybridLandingPlate(lowLift, source.image, input);
  }

  if (input.kind !== "edit-plate") {
    throw new Error(`${expectedId} input kind must be anchor-layer, source-sample, hybrid-landing, or edit-plate`);
  }
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
      anchor: { id: coverLayer.id, image: cover },
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
    if (repair.input.internalOnlyDistancePixels !== undefined) {
      image = maskToFullCatInterior(
        image,
        source.image,
        repair.input.internalOnlyDistancePixels,
        repair.input.boundaryReliefPolygons,
        repair.input.boundaryReliefFeatherPixels,
      );
    }
    if (repair.input.removeAlphaIslandsBelowPixels !== undefined) {
      image = removeAlphaIslandsBelowPixels(image, repair.input.removeAlphaIslandsBelowPixels);
    }
    if (repair.input.outputBounds !== undefined) {
      assertProximalUnderlayOutput(image, { outputBounds: repair.input.outputBounds }, repair.id);
    }
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
    const builtMemberImages = new Map();
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
        builtMembers: builtMemberImages,
      });
      let image = extractBounded(input, {
        crop: member.input.crop,
        polygons: member.polygons,
        chromaKey: member.input.chromaKey,
        edgeFeatherPixels: member.input.edgeFeatherPixels,
      });
      if (member.input.proximalFeather !== undefined) {
        image = featherInternalProximalCut(image, source.image, member.input.proximalFeather);
      }
      if (member.input.innerEdgeDespill !== undefined) {
        image = despillInnerEdge(image, member.input.innerEdgeDespill);
      }
      if (member.input.removeAlphaIslandsBelowPixels !== undefined) {
        image = removeAlphaIslandsBelowPixels(image, member.input.removeAlphaIslandsBelowPixels);
      }
      if (member.input.constrainToAnchorAlpha) image = maskToOwnedAlpha(image, anchorImage);
      if (member.input.preserveAnchorOutsidePolygons) image = compositePatchOntoAnchor(image, anchorImage);
      if (member.neutral && !exactPixels(image, anchorImage)) {
        throw new Error(`neutral variant ${expectedId} must reproduce all source-owned anchor pixels exactly`);
      }
      builtMemberImages.set(member.id, image);
      const relative = `variants/${set.id}/${member.id}.png`;
      const file = path.join(temporaryDirectory, relative);
      await mkdir(path.dirname(file), { recursive: true });
      await writeRgba(file, image);
      members.push({
        id: member.id,
        file: relative,
        neutral: member.neutral,
        sha256: await sha256(file),
        ...(member.parentOverride === undefined ? {} : { parentOverride: member.parentOverride }),
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
