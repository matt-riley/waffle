import { readFile } from "node:fs/promises";
import path from "node:path";

import { PNG } from "pngjs";

import { readRgba, sourceOver, transformRgbaMatrix } from "./rig-raster.mjs";
import {
  validateMotionClipShape,
  validateRigV2Shape,
  variantForLayer,
} from "./rig-schema-v2.mjs";

const IDENTITY = [1, 0, 0, 1, 0, 0];
const TRANSFORM_PROPERTIES = ["x", "y", "rotationDegrees", "scaleX", "scaleY"];

function canonical(value) {
  return value === 0 ? 0 : value;
}

function multiply(left, right) {
  const [a1, b1, c1, d1, e1, f1] = left;
  const [a2, b2, c2, d2, e2, f2] = right;
  return [
    canonical(a1 * a2 + c1 * b2),
    canonical(b1 * a2 + d1 * b2),
    canonical(a1 * c2 + c1 * d2),
    canonical(b1 * c2 + d1 * d2),
    canonical(a1 * e2 + c1 * f2 + e1),
    canonical(b1 * e2 + d1 * f2 + f1),
  ];
}

function localMatrix(manifest, layer, transform) {
  if (!transform || !TRANSFORM_PROPERTIES.every((property) => Number.isFinite(transform[property]))) {
    throw new Error(`layer ${layer.id} local transform must contain finite values`);
  }
  if (transform.scaleX === 0 || transform.scaleY === 0) {
    throw new Error(`layer ${layer.id} local transform scale must be non-zero`);
  }
  const pivotX = layer.pivot.x * (manifest.canvas.width - 1);
  const pivotY = layer.pivot.y * (manifest.canvas.height - 1);
  const radians = (transform.rotationDegrees * Math.PI) / 180;
  const cosine = Math.cos(radians);
  const sine = Math.sin(radians);
  const scaleRotate = [
    canonical(cosine * transform.scaleX),
    canonical(sine * transform.scaleX),
    canonical(-sine * transform.scaleY),
    canonical(cosine * transform.scaleY),
    0,
    0,
  ];

  // Column-vector convention. Points are moved to the pivot origin, scaled,
  // rotated, moved back from the pivot, then translated in canvas-normalized units.
  const toPivotOrigin = [1, 0, 0, 1, -pivotX, -pivotY];
  const fromPivotOrigin = [1, 0, 0, 1, pivotX, pivotY];
  const normalizedTranslation = [
    1,
    0,
    0,
    1,
    transform.x * manifest.canvas.width,
    transform.y * manifest.canvas.height,
  ];
  return multiply(
    normalizedTranslation,
    multiply(fromPivotOrigin, multiply(scaleRotate, toPivotOrigin)),
  );
}

function validateInterpolationKeys(keyframes) {
  if (!Array.isArray(keyframes) || keyframes.length === 0) {
    throw new Error("keyframes must be a non-empty array");
  }
  let previous = -1;
  let valueType;
  for (const [index, keyframe] of keyframes.entries()) {
    if (!keyframe || typeof keyframe !== "object" || Array.isArray(keyframe)) {
      throw new Error(`keyframe ${index + 1} must be an object`);
    }
    if (!Number.isInteger(keyframe.frame) || keyframe.frame < 0) {
      throw new Error(`keyframe ${index + 1} frame must be a non-negative integer`);
    }
    if (keyframe.frame <= previous) throw new Error("keyframe frames must be strictly increasing");
    previous = keyframe.frame;
    if (typeof keyframe.value === "number") {
      if (!Number.isFinite(keyframe.value)) throw new Error("numeric keyframe values must be finite");
    } else if (typeof keyframe.value !== "string") {
      throw new Error("keyframe values must be finite numbers or strings");
    }
    valueType ??= typeof keyframe.value;
    if (typeof keyframe.value !== valueType) throw new Error("keyframe values must have one consistent type");
    if (keyframe.interpolation !== undefined && !["linear", "hold"].includes(keyframe.interpolation)) {
      throw new Error("keyframe interpolation must be linear or hold");
    }
  }
}

export function interpolateKeyframes(keyframes, frame) {
  validateInterpolationKeys(keyframes);
  if (!Number.isFinite(frame)) throw new Error("frame must be finite");
  const first = keyframes[0];
  const last = keyframes.at(-1);
  if (frame < first.frame || frame > last.frame) {
    throw new Error(`frame ${frame} is outside keyframe range ${first.frame}..${last.frame}`);
  }
  const exact = keyframes.find((keyframe) => keyframe.frame === frame);
  if (exact) return exact.value;

  const rightIndex = keyframes.findIndex((keyframe) => keyframe.frame > frame);
  const left = keyframes[rightIndex - 1];
  const right = keyframes[rightIndex];
  if (typeof left.value === "string" || left.interpolation === "hold") return left.value;
  const progress = (frame - left.frame) / (right.frame - left.frame);
  return left.value + (right.value - left.value) * progress;
}

export function worldTransforms(manifest, localTransforms) {
  if (!(localTransforms instanceof Map)) throw new Error("localTransforms must be a Map");
  const byId = new Map(manifest.layers.map((layer) => [layer.id, layer]));
  const world = new Map();

  function resolve(layer) {
    if (world.has(layer.id)) return world.get(layer.id);
    const local = localMatrix(manifest, layer, localTransforms.get(layer.id));
    const parent = layer.parent === manifest.root.id ? IDENTITY : resolve(byId.get(layer.parent));
    const result = multiply(parent, local);
    world.set(layer.id, result);
    return result;
  }

  for (const layer of manifest.layers) resolve(layer);
  return world;
}

function checkedControlValue(control, name, value) {
  if (!Number.isFinite(value) || value < control.min || value > control.max) {
    throw new Error(`control ${name} value ${value} is outside ${control.min}..${control.max}`);
  }
  return value;
}

function checkedLayerLimits(layer, transform) {
  for (const [property, range] of Object.entries(layer.limits)) {
    const value = transform[property];
    if (!Number.isFinite(value) || value < range.min || value > range.max) {
      throw new Error(`layer ${layer.id} ${property} value ${value} is outside ${range.min}..${range.max}`);
    }
  }
}

export function evaluateClip(manifest, clip, frame) {
  validateRigV2Shape(manifest);
  validateMotionClipShape(manifest, clip);
  if (!Number.isInteger(frame) || frame < 0 || frame >= clip.frameCount) {
    throw new Error("frame must be an integer inside the clip");
  }

  const controls = new Map();
  for (const [name, control] of Object.entries(manifest.controls)) {
    const keyframes = clip.controls[name];
    const value = keyframes === undefined ? 0 : interpolateKeyframes(keyframes, frame);
    controls.set(name, checkedControlValue(control, name, value));
  }

  const variants = new Map();
  for (const [setId, variantSet] of Object.entries(manifest.variants)) {
    const keyframes = clip.variants[setId];
    const memberId = keyframes === undefined
      ? variantSet.members.find((member) => member.neutral).id
      : interpolateKeyframes(keyframes, Math.min(frame, keyframes.at(-1).frame));
    variantForLayer(manifest, variantSet.layer, memberId);
    variants.set(setId, memberId);
  }

  const layers = new Map(manifest.layers.map((layer) => [layer.id, { ...layer.neutral }]));
  for (const [name, value] of controls) {
    for (const binding of manifest.controls[name].bindings) {
      if (!("layer" in binding)) continue;
      const transform = layers.get(binding.layer);
      transform[binding.property] += value * binding.factor;
    }
  }
  for (const layer of manifest.layers) checkedLayerLimits(layer, layers.get(layer.id));

  return { layers, variants, controls };
}

function exactMapEntry(map, key) {
  if (!map.has(key)) throw new Error(`missing evaluated value for ${key}`);
  return map.get(key);
}

function matricesEqual(left, right) {
  return left.length === right.length && left.every((value, index) => value === right[index]);
}

export function assertLoopClosure(manifest, clip) {
  validateRigV2Shape(manifest);
  validateMotionClipShape(manifest, clip);
  for (const setId of Object.keys(manifest.variants)) {
    if (!clip.variants[setId]?.some((keyframe) => keyframe.frame === 0)) {
      throw new Error(`variant ${setId} must declare a state at frame 0`);
    }
  }

  for (let frame = 0; frame < clip.frameCount; frame += 1) {
    const evaluated = evaluateClip(manifest, clip, frame);
    for (const [name, control] of Object.entries(manifest.controls)) {
      checkedControlValue(control, name, exactMapEntry(evaluated.controls, name));
    }
  }

  const first = evaluateClip(manifest, clip, clip.requiredClosure.firstFrame);
  const last = evaluateClip(manifest, clip, clip.requiredClosure.lastFrame);
  for (const name of Object.keys(manifest.controls)) {
    if (exactMapEntry(first.controls, name) !== exactMapEntry(last.controls, name)) {
      throw new Error(`control ${name} does not close exactly`);
    }
  }
  for (const setId of Object.keys(manifest.variants)) {
    if (exactMapEntry(first.variants, setId) !== exactMapEntry(last.variants, setId)) {
      throw new Error(`variant ${setId} does not close exactly`);
    }
  }

  const firstWorld = worldTransforms(manifest, first.layers);
  const lastWorld = worldTransforms(manifest, last.layers);
  for (const layer of manifest.layers) {
    if (!matricesEqual(firstWorld.get(layer.id), lastWorld.get(layer.id))) {
      throw new Error(`layer ${layer.id} world transform does not close exactly`);
    }
  }
}

export async function renderRigFrame(manifestPath, clipPath, frame) {
  const [manifest, clip] = await Promise.all([
    readFile(manifestPath, "utf8").then(JSON.parse),
    readFile(clipPath, "utf8").then(JSON.parse),
  ]);
  const evaluated = evaluateClip(manifest, clip, frame);
  const world = worldTransforms(manifest, evaluated.layers);
  const directory = path.dirname(manifestPath);
  let output = new PNG({ width: manifest.canvas.width, height: manifest.canvas.height });

  for (const layer of manifest.layers.toSorted((left, right) => left.drawOrder - right.drawOrder)) {
    const file = layer.role === "variant-anchor"
      ? variantForLayer(manifest, layer.id, evaluated.variants.get(
        Object.entries(manifest.variants).find(([, set]) => set.layer === layer.id)[0],
      )).file
      : layer.file;
    const raster = await readRgba(path.resolve(directory, file));
    output = sourceOver(output, transformRgbaMatrix(raster, world.get(layer.id)));
  }
  return output;
}
