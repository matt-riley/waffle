import { access, readFile, stat } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

import { PNG } from "pngjs";

const PNG_SIGNATURE = Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]);
const FORBIDDEN_CHUNKS = new Set(["tEXt", "zTXt", "iTXt", "eXIf", "iCCP"]);
const ALPHA_POLICIES = new Set(["opaque", "transparent-corners", "any"]);
const REQUIRED_ASSET_FIELDS = ["id", "file", "role", "width", "height", "alphaPolicy", "provenance"];
const MAX_ASSET_BYTES = 10 * 1024 * 1024;
const MAX_MILESTONE_BYTES = 60 * 1024 * 1024;

function positiveNumber(value) {
  return typeof value === "number" && Number.isFinite(value) && value > 0;
}

function safeRelativePath(base, relative, label) {
  if (typeof relative !== "string" || relative.length === 0) throw new Error(`${label} must be a non-empty string`);
  if (path.isAbsolute(relative) || /^[a-z][a-z\d+.-]*:/iu.test(relative)) throw new Error(`${label} must be a local relative path`);
  const resolved = path.resolve(base, relative);
  const prefix = `${path.resolve(base)}${path.sep}`;
  if (!resolved.startsWith(prefix)) throw new Error(`${label} must stay inside the manifest directory`);
  return resolved;
}

export function inspectPngBuffer(buffer) {
  if (buffer.length < 33 || !buffer.subarray(0, 8).equals(PNG_SIGNATURE)) throw new Error("file is not a PNG");
  let offset = 8;
  let ihdr;
  const chunks = [];
  while (offset + 12 <= buffer.length) {
    const length = buffer.readUInt32BE(offset);
    const end = offset + 12 + length;
    if (end > buffer.length) throw new Error("PNG contains a truncated chunk");
    const type = buffer.toString("ascii", offset + 4, offset + 8);
    chunks.push(type);
    if (FORBIDDEN_CHUNKS.has(type)) throw new Error(`forbidden PNG chunk ${type}`);
    if (type === "IHDR") {
      if (length !== 13) throw new Error("invalid PNG IHDR length");
      ihdr = {
        width: buffer.readUInt32BE(offset + 8),
        height: buffer.readUInt32BE(offset + 12),
        bitDepth: buffer[offset + 16],
        colorType: buffer[offset + 17],
      };
    }
    offset = end;
  }
  if (!ihdr) throw new Error("PNG is missing IHDR");
  if (ihdr.colorType !== 6) throw new Error("PNG colour type must be RGBA (6)");
  return { ...ihdr, chunks };
}

function validateAlpha(decoded, policy) {
  if (!ALPHA_POLICIES.has(policy)) throw new Error(`unknown alphaPolicy: ${policy}`);
  if (policy === "any") return;
  if (policy === "opaque") {
    for (let offset = 3; offset < decoded.data.length; offset += 4) {
      if (decoded.data[offset] !== 255) throw new Error("alphaPolicy opaque requires all pixels must be opaque");
    }
    return;
  }
  const corners = [
    3,
    (decoded.width - 1) * 4 + 3,
    ((decoded.height - 1) * decoded.width) * 4 + 3,
    (decoded.width * decoded.height - 1) * 4 + 3,
  ];
  if (corners.some((offset) => decoded.data[offset] !== 0)) {
    throw new Error("alphaPolicy transparent-corners requires all four corner pixels must be transparent");
  }
}

export async function validatePng(file, options = {}) {
  const fileStat = await stat(file);
  if (fileStat.size > (options.maxBytes ?? MAX_ASSET_BYTES)) {
    throw new Error(`asset exceeds ${(options.maxBytes ?? MAX_ASSET_BYTES)}-byte budget: ${file}`);
  }
  const buffer = await readFile(file);
  const header = inspectPngBuffer(buffer);
  const decoded = PNG.sync.read(buffer);
  if (options.width !== undefined && (header.width !== options.width || header.height !== options.height)) {
    throw new Error(`declared dimensions ${options.width}x${options.height} do not match PNG ${header.width}x${header.height}`);
  }
  validateAlpha(decoded, options.alphaPolicy ?? "any");
  return { ...header, bytes: fileStat.size, pixels: decoded.data };
}

async function readManifest(manifestPath) {
  let source;
  try {
    source = await readFile(manifestPath, "utf8");
  } catch (error) {
    if (error.code === "ENOENT") throw new Error(`manifest does not exist: ${manifestPath}`);
    throw error;
  }
  try {
    return JSON.parse(source);
  } catch (error) {
    throw new Error(`invalid JSON: ${error.message}`);
  }
}

function requireSchemaVersion(manifest) {
  if (manifest.schemaVersion !== 1) throw new Error("schemaVersion must be 1");
}

export async function validateAssetManifest(manifestPath) {
  const manifest = await readManifest(manifestPath);
  requireSchemaVersion(manifest);
  if (!Array.isArray(manifest.assets)) throw new Error("assets must be an array");
  const directory = path.dirname(manifestPath);
  const seen = new Set();
  const pending = [];
  let totalBytes = 0;

  for (const [index, asset] of manifest.assets.entries()) {
    if (!asset || typeof asset !== "object" || Array.isArray(asset)) throw new Error(`asset ${index + 1} must be an object`);
    const label = typeof asset.id === "string" && asset.id ? asset.id : String(index + 1);
    for (const field of REQUIRED_ASSET_FIELDS) {
      if (!(field in asset) || asset[field] === "") throw new Error(`asset ${label} is missing ${field}`);
    }
    if (typeof asset.id !== "string" || asset.id.length === 0) {
      throw new Error(`asset ${index + 1} id must be a non-empty string`);
    }
    if (seen.has(asset.id)) throw new Error(`duplicate asset id: ${asset.id}`);
    seen.add(asset.id);
    if (!positiveNumber(asset.width) || !positiveNumber(asset.height)) throw new Error(`asset ${label} width and height must be positive numbers`);
    if (!ALPHA_POLICIES.has(asset.alphaPolicy)) throw new Error(`unknown alphaPolicy: ${asset.alphaPolicy}`);
    if (typeof asset.role !== "string" || typeof asset.provenance !== "string") throw new Error(`asset ${label} role and provenance must be strings`);
    const file = safeRelativePath(directory, asset.file, `asset ${label} file`);
    try {
      const fileStat = await stat(file);
      totalBytes += fileStat.size;
    } catch (error) {
      if (error.code === "ENOENT") throw new Error(`asset file does not exist: ${asset.file}`);
      throw error;
    }
    pending.push({ asset, file });
  }
  if (totalBytes > MAX_MILESTONE_BYTES) throw new Error(`manifest assets exceed ${MAX_MILESTONE_BYTES}-byte budget`);
  for (const { asset, file } of pending) {
    await validatePng(file, { width: asset.width, height: asset.height, alphaPolicy: asset.alphaPolicy });
  }
  return { kind: "assets", count: manifest.assets.length, bytes: totalBytes };
}

export async function validateIdleManifest(manifestPath) {
  const manifest = await readManifest(manifestPath);
  requireSchemaVersion(manifest);
  if (!manifest.canvas || !positiveNumber(manifest.canvas.width) || !positiveNumber(manifest.canvas.height)) {
    throw new Error("canvas width and height must be positive numbers");
  }
  if (!manifest.anchor || typeof manifest.anchor.x !== "number" || typeof manifest.anchor.y !== "number" || !Number.isFinite(manifest.anchor.x) || !Number.isFinite(manifest.anchor.y)) {
    throw new Error("anchor x and y must be numbers");
  }
  if (manifest.anchor.x < 0 || manifest.anchor.x > manifest.canvas.width || manifest.anchor.y < 0 || manifest.anchor.y > manifest.canvas.height) {
    throw new Error("anchor must be inside the canvas");
  }
  if (typeof manifest.loop !== "boolean") throw new Error("loop must be boolean");
  if (!Array.isArray(manifest.frames) || manifest.frames.length === 0) throw new Error("frames must be a non-empty array");
  const directory = path.dirname(manifestPath);
  const files = [];
  let durationMs = 0;
  const validatedFrames = [];
  for (const [index, frame] of manifest.frames.entries()) {
    if (!frame || typeof frame !== "object" || typeof frame.file !== "string" || frame.file === "") {
      throw new Error(`frame ${index + 1} file must be a non-empty string`);
    }
    if (!positiveNumber(frame.durationMs)) throw new Error(`frame ${index + 1} durationMs must be a positive number`);
    files.push(frame.file);
    durationMs += frame.durationMs;
    const file = safeRelativePath(directory, frame.file, `frame ${index + 1} file`);
    let result;
    try {
      result = await validatePng(file, {
        width: manifest.canvas.width,
        height: manifest.canvas.height,
        alphaPolicy: "transparent-corners",
      });
    } catch (error) {
      if (error.code === "ENOENT") throw new Error(`frame file does not exist: ${frame.file}`);
      const dimensions = error.message.match(/PNG (\d+x\d+)$/u);
      if (dimensions) {
        throw new Error(`frame ${index + 1} dimensions ${dimensions[1]} do not match canvas ${manifest.canvas.width}x${manifest.canvas.height}`);
      }
      throw error;
    }
    validatedFrames.push(result);
  }
  const sorted = [...files].sort((left, right) => left.localeCompare(right));
  if (new Set(files).size !== files.length || files.some((file, index) => file !== sorted[index])) {
    throw new Error("frame files must be ordered and unique");
  }
  const seedFile = safeRelativePath(directory, manifest.seed, "seed");
  const seed = await validatePng(seedFile, {
    width: manifest.canvas.width,
    height: manifest.canvas.height,
    alphaPolicy: "transparent-corners",
  });
  if (!validatedFrames[0].pixels.equals(seed.pixels)) throw new Error("frame 01 pixels do not match seed");
  return { kind: "idle", count: manifest.frames.length, durationMs };
}

export async function validateManifest(manifestPath) {
  const manifest = await readManifest(manifestPath);
  return Array.isArray(manifest.assets)
    ? validateAssetManifest(manifestPath)
    : validateIdleManifest(manifestPath);
}

async function main(args) {
  const optional = args[0] === "--optional";
  const paths = optional ? args.slice(1) : args;
  if (paths.length === 0) throw new Error("usage: validate-raster.mjs [--optional] <manifest.json...>");
  for (const manifestPath of paths) {
    if (optional) {
      try {
        await access(manifestPath);
      } catch (error) {
        if (error.code === "ENOENT") {
          console.log(`SKIP ${manifestPath} (not present)`);
          continue;
        }
        throw error;
      }
    }
    const result = await validateManifest(manifestPath);
    if (result.kind === "assets") console.log(`PASS ${manifestPath} assets=${result.count} bytes=${result.bytes}`);
    else console.log(`PASS ${manifestPath} frames=${result.count} durationMs=${result.durationMs}`);
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
