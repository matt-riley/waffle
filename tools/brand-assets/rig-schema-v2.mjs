import path from "node:path";

const SHA256 = /^[a-f\d]{64}$/u;
const LAYER_ROLES = new Set(["visible", "repair", "overlay", "variant-anchor"]);
const TRANSFORM_PROPERTIES = new Set(["x", "y", "rotationDegrees", "scaleX", "scaleY"]);

function object(value) {
  return value && typeof value === "object" && !Array.isArray(value);
}

function finite(value) {
  return typeof value === "number" && Number.isFinite(value);
}

function normalizedPivot(pivot, label) {
  if (!object(pivot) || !finite(pivot.x) || !finite(pivot.y) || pivot.x < 0 || pivot.x > 1 || pivot.y < 0 || pivot.y > 1) {
    throw new Error(`${label} pivot must be normalized`);
  }
}

function localPath(relative, label) {
  if (typeof relative !== "string" || relative.length === 0 || path.isAbsolute(relative) || /^[a-z][a-z\d+.-]*:/iu.test(relative)) {
    throw new Error(`${label} must be a local relative path`);
  }
  const base = path.resolve(path.sep, "rig");
  const resolved = path.resolve(base, relative);
  if (resolved !== base && !resolved.startsWith(`${base}${path.sep}`)) {
    throw new Error(`${label} must stay inside the rig directory`);
  }
}

function declaredFile(value, label, contained = true) {
  if (!object(value)) throw new Error(`${label} must be an object`);
  if (contained) localPath(value.file, `${label} file`);
  else if (typeof value.file !== "string" || value.file.length === 0 || path.isAbsolute(value.file) || /^[a-z][a-z\d+.-]*:/iu.test(value.file)) {
    throw new Error(`${label} file must be a local relative path`);
  }
  if (typeof value.sha256 !== "string" || !SHA256.test(value.sha256)) {
    throw new Error(`${label} sha256 must be 64 lowercase hex characters`);
  }
}

function validateLayerGraph(rootId, layers) {
  const byId = new Map(layers.map((layer) => [layer.id, layer]));
  for (const layer of layers) {
    if (layer.parent !== rootId && !byId.has(layer.parent)) {
      throw new Error(`layer ${layer.id} has unknown parent ${layer.parent}`);
    }
  }
  for (const layer of layers) {
    const visited = new Set();
    let current = layer;
    while (current.parent !== rootId) {
      if (visited.has(current.id)) throw new Error("layer graph contains a cycle");
      visited.add(current.id);
      current = byId.get(current.parent);
    }
  }
}

function validateLayer(layer, index, ids, orders) {
  if (!object(layer)) throw new Error(`layer ${index + 1} must be an object`);
  if (typeof layer.id !== "string" || layer.id.length === 0) throw new Error(`layer ${index + 1} id must be a non-empty string`);
  if (ids.has(layer.id)) throw new Error(`duplicate layer id: ${layer.id}`);
  ids.add(layer.id);
  if (!Number.isInteger(layer.drawOrder)) throw new Error(`layer ${layer.id} drawOrder must be an integer`);
  if (orders.has(layer.drawOrder)) throw new Error(`duplicate drawOrder: ${layer.drawOrder}`);
  orders.add(layer.drawOrder);
  if (!LAYER_ROLES.has(layer.role)) throw new Error(`layer ${layer.id} has unknown role`);
  if (typeof layer.parent !== "string" || layer.parent.length === 0) throw new Error(`layer ${layer.id} parent must be a non-empty string`);
  if (typeof layer.visibleAtNeutral !== "boolean") throw new Error(`layer ${layer.id} visibleAtNeutral must be boolean`);
  if (layer.blendMode !== "normal") throw new Error(`layer ${layer.id} blendMode must be normal`);
  normalizedPivot(layer.pivot, `layer ${layer.id}`);
  const neutral = layer.neutral;
  if (!object(neutral) || ![neutral.x, neutral.y, neutral.rotationDegrees, neutral.scaleX, neutral.scaleY].every(finite)) {
    throw new Error(`layer ${layer.id} neutral transform values must be finite numbers`);
  }
  if (neutral.x !== 0 || neutral.y !== 0 || neutral.rotationDegrees !== 0 || neutral.scaleX !== 1 || neutral.scaleY !== 1) {
    throw new Error(`layer ${layer.id} neutral transform must be identity`);
  }
  if (!object(layer.limits)) throw new Error(`layer ${layer.id} limits must be an object`);
  for (const [property, range] of Object.entries(layer.limits)) {
    if (!TRANSFORM_PROPERTIES.has(property)) throw new Error(`layer ${layer.id} has unsupported limit ${property}`);
    if (!object(range) || !finite(range.min) || !finite(range.max) || range.min >= range.max) {
      throw new Error(`layer ${layer.id} limit ${property} must have finite min < max`);
    }
  }
  declaredFile(layer, `layer ${layer.id}`);
}

function validateVariants(manifest, byId) {
  if (!object(manifest.variants)) throw new Error("variants must be an object");
  const anchoredLayers = new Set();
  for (const [setId, variantSet] of Object.entries(manifest.variants)) {
    if (!object(variantSet)) throw new Error(`variant set ${setId} must be an object`);
    const layer = byId.get(variantSet.layer);
    if (!layer || layer.role !== "variant-anchor") throw new Error(`variant set ${setId} references unknown anchor layer ${variantSet.layer}`);
    if (anchoredLayers.has(layer.id)) throw new Error(`variant-anchor ${layer.id} has multiple variant sets`);
    anchoredLayers.add(layer.id);
    if (!Array.isArray(variantSet.members) || variantSet.members.length === 0) throw new Error(`variant set ${setId} members must be a non-empty array`);
    const memberIds = new Set();
    let neutralCount = 0;
    for (const [index, member] of variantSet.members.entries()) {
      if (!object(member)) throw new Error(`variant ${setId} member ${index + 1} must be an object`);
      if (typeof member.id !== "string" || member.id.length === 0) throw new Error(`variant ${setId} member ${index + 1} id must be a non-empty string`);
      if (memberIds.has(member.id)) throw new Error(`duplicate variant member id: ${setId}/${member.id}`);
      memberIds.add(member.id);
      if (typeof member.neutral !== "boolean") throw new Error(`variant ${setId}/${member.id} neutral must be boolean`);
      if (member.neutral) neutralCount += 1;
      declaredFile(member, `variant ${setId}/${member.id}`);
    }
    if (neutralCount !== 1) throw new Error(`variant set ${setId} must have exactly one neutral member`);
  }
  for (const layer of manifest.layers) {
    if (layer.role === "variant-anchor" && !anchoredLayers.has(layer.id)) {
      throw new Error(`variant-anchor ${layer.id} requires a variant set`);
    }
  }
}

function validateControls(manifest, byId) {
  if (!object(manifest.controls) || Object.keys(manifest.controls).length === 0) throw new Error("controls must be a non-empty object");
  for (const [name, control] of Object.entries(manifest.controls)) {
    if (!object(control) || !finite(control.min) || !finite(control.max) || control.min >= control.max) {
      throw new Error(`control ${name} must have finite min < max`);
    }
    if (!Array.isArray(control.bindings) || control.bindings.length === 0) throw new Error(`control ${name} bindings must be a non-empty array`);
    for (const [index, binding] of control.bindings.entries()) {
      if (!object(binding)) throw new Error(`control ${name} binding ${index + 1} must be an object`);
      if ("layer" in binding) {
        if (!byId.has(binding.layer)) throw new Error(`control ${name} binding references unknown layer ${binding.layer}`);
        if (!TRANSFORM_PROPERTIES.has(binding.property)) throw new Error(`control ${name} binding has unsupported property ${binding.property}`);
        if (!finite(binding.factor)) throw new Error(`control ${name} binding factor must be finite`);
      } else if ("variant" in binding) {
        if (!Object.hasOwn(manifest.variants, binding.variant)) throw new Error(`control ${name} binding references unknown variant set ${binding.variant}`);
      } else {
        throw new Error(`control ${name} binding must reference a layer or variant set`);
      }
    }
  }
}

export function validateRigV2Shape(manifest) {
  if (!object(manifest) || manifest.schemaVersion !== 2) throw new Error("schemaVersion must be 2");
  if (!object(manifest.canvas) || manifest.canvas.width !== 1536 || manifest.canvas.height !== 1024) {
    throw new Error("canvas must be exactly 1536x1024");
  }
  if (!object(manifest.root) || manifest.root.id !== "waffle-root") throw new Error("root.id must be waffle-root");
  if ("file" in manifest.root) throw new Error("root must be synthetic and non-raster");
  normalizedPivot(manifest.root.pivot, "root");
  declaredFile(manifest.source, "source", false);
  declaredFile(manifest.neutralReference, "neutralReference");
  if (!Array.isArray(manifest.layers) || manifest.layers.length === 0) throw new Error("layers must be a non-empty array");

  const ids = new Set();
  const orders = new Set();
  for (const [index, layer] of manifest.layers.entries()) validateLayer(layer, index, ids, orders);
  validateLayerGraph(manifest.root.id, manifest.layers);
  const byId = new Map(manifest.layers.map((layer) => [layer.id, layer]));
  validateVariants(manifest, byId);
  validateControls(manifest, byId);
}

export function variantForLayer(manifest, layerId, memberId) {
  const entry = Object.entries(manifest.variants ?? {}).find(([, variantSet]) => variantSet.layer === layerId);
  if (!entry) throw new Error(`layer ${layerId} has no variant set`);
  const [setId, variantSet] = entry;
  const member = memberId === undefined
    ? variantSet.members.find((candidate) => candidate.neutral)
    : variantSet.members.find((candidate) => candidate.id === memberId);
  if (!member) throw new Error(`unknown variant member ${memberId ?? "neutral"} for ${setId}`);
  return member;
}

function validateKeyframes(keyframes, frameCount, label, validateValue) {
  if (!Array.isArray(keyframes) || keyframes.length === 0) throw new Error(`${label} keyframes must be a non-empty array`);
  let previous = -1;
  for (const [index, keyframe] of keyframes.entries()) {
    if (!object(keyframe)) throw new Error(`${label} keyframe ${index + 1} must be an object`);
    if (!Number.isInteger(keyframe.frame) || keyframe.frame < 0 || keyframe.frame >= frameCount) {
      throw new Error(`${label} keyframe ${index + 1} frame must be inside the clip`);
    }
    if (keyframe.frame <= previous) throw new Error(`${label} keyframe frames must be strictly increasing`);
    previous = keyframe.frame;
    validateValue(keyframe);
  }
}

export function validateMotionClipShape(manifest, clip) {
  if (!object(clip) || clip.schemaVersion !== 1) throw new Error("motion clip schemaVersion must be 1");
  if (typeof clip.id !== "string" || clip.id.length === 0) throw new Error("motion clip id must be a non-empty string");
  if (!finite(clip.fps) || clip.fps <= 0) throw new Error("motion clip fps must be positive");
  if (!Number.isInteger(clip.frameCount) || clip.frameCount <= 0) throw new Error("motion clip frameCount must be a positive integer");
  if (typeof clip.loop !== "boolean") throw new Error("motion clip loop must be boolean");
  if (!object(clip.requiredClosure)
    || !Number.isInteger(clip.requiredClosure.firstFrame)
    || !Number.isInteger(clip.requiredClosure.lastFrame)
    || clip.requiredClosure.firstFrame < 0
    || clip.requiredClosure.lastFrame >= clip.frameCount
    || clip.requiredClosure.firstFrame > clip.requiredClosure.lastFrame) {
    throw new Error("motion clip requiredClosure must contain valid firstFrame and lastFrame");
  }
  if (!object(clip.variants) || !object(clip.controls)) throw new Error("motion clip variants and controls must be objects");
  for (const [setId, keyframes] of Object.entries(clip.variants)) {
    const variantSet = manifest.variants?.[setId];
    if (!variantSet) throw new Error(`motion clip references unknown variant set ${setId}`);
    validateKeyframes(keyframes, clip.frameCount, `variant ${setId}`, (keyframe) => {
      if (!variantSet.members.some((member) => member.id === keyframe.value)) throw new Error(`variant ${setId} has unknown member ${keyframe.value}`);
      if (keyframe.interpolation !== undefined && keyframe.interpolation !== "hold") throw new Error(`variant ${setId} interpolation must be hold`);
    });
  }
  for (const [name, keyframes] of Object.entries(clip.controls)) {
    const control = manifest.controls?.[name];
    if (!control) throw new Error(`motion clip references unknown control ${name}`);
    validateKeyframes(keyframes, clip.frameCount, `control ${name}`, (keyframe) => {
      if (!finite(keyframe.value)) throw new Error(`control ${name} values must be finite`);
      if (keyframe.value < control.min || keyframe.value > control.max) {
        throw new Error(`control ${name} value ${keyframe.value} is outside ${control.min}..${control.max}`);
      }
      if (keyframe.interpolation !== undefined && !["linear", "hold"].includes(keyframe.interpolation)) {
        throw new Error(`control ${name} interpolation must be linear or hold`);
      }
    });
  }
}
