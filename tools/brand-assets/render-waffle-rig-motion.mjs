import { createHash } from "node:crypto";
import {
  lstat,
  mkdir,
  readFile,
  readdir,
  realpath,
  rename,
  rm,
  writeFile,
} from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

import { PNG } from "pngjs";

import { assertLoopClosure, evaluateClip, renderRigFrame } from "./rig-motion.mjs";
import { sourceOver } from "./rig-raster.mjs";
import { validateMotionClipShape } from "./rig-schema-v2.mjs";
import { validateRig } from "./validate-rig.mjs";

const BACKGROUNDS = {
  "warm-white": [[250, 247, 240, 255]],
  charcoal: [[35, 32, 30, 255]],
  checkerboard: [[238, 235, 228, 255], [207, 202, 193, 255]],
};
const CHECKER_SIZE = 32;
const CONTACT_COLUMNS = 8;
const CONTACT_WIDTHS = [320, 160];
const SAFE_REVIEW_FILE = /^(?:frame-\d{4,}|contact-sheet-(?:160|320))\.png$/u;
const SHA256 = /^[a-f\d]{64}$/u;

function exists(file) {
  return lstat(file).then(() => true, (error) => {
    if (error.code === "ENOENT") return false;
    throw error;
  });
}

function hashBytes(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

function pathInside(base, candidate) {
  const relative = path.relative(base, candidate);
  return relative.length > 0 && relative !== ".." && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative);
}

async function assertNoSymlinkComponents(base, candidate) {
  const relative = path.relative(base, candidate);
  let current = base;
  for (const part of relative.split(path.sep)) {
    current = path.join(current, part);
    try {
      if ((await lstat(current)).isSymbolicLink()) throw new Error("output path must not use symlinks");
    } catch (error) {
      if (error.code !== "ENOENT") throw error;
    }
  }
}

async function physicalPath(candidate) {
  const missing = [];
  let current = path.resolve(candidate);
  while (!await exists(current)) {
    const parent = path.dirname(current);
    if (parent === current) throw new Error(`cannot resolve output path ${candidate}`);
    missing.unshift(path.basename(current));
    current = parent;
  }
  return path.join(await realpath(current), ...missing);
}

async function safeOutputDirectory(outputDirectory, cwd) {
  const lexicalRoot = path.resolve(cwd);
  const lexicalOutput = path.resolve(lexicalRoot, outputDirectory);
  if (pathInside(lexicalRoot, lexicalOutput)) {
    await assertNoSymlinkComponents(lexicalRoot, lexicalOutput);
  }
  const root = await realpath(lexicalRoot);
  const output = await physicalPath(lexicalOutput);
  const assets = path.join(root, "assets");
  if (output === assets || pathInside(assets, output)) throw new Error("refusing to write inside assets");
  const reviewRoot = path.join(root, ".superpowers");
  if (!pathInside(reviewRoot, output)) throw new Error("output must stay inside .superpowers");
  return output;
}

function parseFrames(value, frameCount) {
  if (value === "all") return Array.from({ length: frameCount }, (_, frame) => frame);
  if (typeof value !== "string" || value.length === 0) throw new Error("frames must be all or a comma-separated list");
  const frames = value.split(",").map((part) => {
    if (!/^\d+$/u.test(part)) throw new Error("frames must be all or a comma-separated list");
    return Number(part);
  });
  if (new Set(frames).size !== frames.length) throw new Error("frames must not contain duplicates");
  for (const frame of frames) {
    if (!Number.isSafeInteger(frame) || frame < 0 || frame >= frameCount) {
      throw new Error(`frame ${frame} is outside 0..${frameCount - 1}`);
    }
  }
  return frames.toSorted((left, right) => left - right);
}

function backgroundRgba(width, height, name) {
  const colors = BACKGROUNDS[name];
  if (!colors) throw new Error("background must be one of warm-white, charcoal, checkerboard");
  const output = new PNG({ width, height });
  for (let y = 0; y < height; y += 1) {
    for (let x = 0; x < width; x += 1) {
      const color = colors.length === 1
        ? colors[0]
        : colors[(Math.floor(x / CHECKER_SIZE) + Math.floor(y / CHECKER_SIZE)) % 2];
      output.data.set(color, (y * width + x) * 4);
    }
  }
  return output;
}

function resizeNearest(source, width) {
  const height = Math.round(width * source.height / source.width);
  const output = new PNG({ width, height });
  for (let y = 0; y < height; y += 1) {
    const sourceY = Math.min(source.height - 1, Math.floor(y * source.height / height));
    for (let x = 0; x < width; x += 1) {
      const sourceX = Math.min(source.width - 1, Math.floor(x * source.width / width));
      const sourceOffset = (sourceY * source.width + sourceX) * 4;
      output.data.set(source.data.subarray(sourceOffset, sourceOffset + 4), (y * width + x) * 4);
    }
  }
  return output;
}

async function contactSheet(frameFiles, cellWidth) {
  const columns = Math.min(CONTACT_COLUMNS, frameFiles.length);
  const rows = Math.ceil(frameFiles.length / columns);
  const cellHeight = Math.round(cellWidth * 1024 / 1536);
  const output = new PNG({ width: cellWidth * columns, height: cellHeight * rows });
  for (const [index, file] of frameFiles.entries()) {
    const frame = PNG.sync.read(await readFile(file));
    const cell = resizeNearest(frame, cellWidth);
    if (cell.height !== cellHeight) throw new Error("contact sheet requires the standing-v2 canvas aspect ratio");
    const column = index % columns;
    const row = Math.floor(index / columns);
    for (let y = 0; y < cellHeight; y += 1) {
      const sourceStart = y * cellWidth * 4;
      const targetStart = ((row * cellHeight + y) * output.width + column * cellWidth) * 4;
      output.data.set(cell.data.subarray(sourceStart, sourceStart + cellWidth * 4), targetStart);
    }
  }
  return output;
}

async function writePng(file, png) {
  const bytes = PNG.sync.write(png);
  await writeFile(file, bytes);
  return {
    file: path.basename(file),
    sha256: hashBytes(bytes),
    width: png.width,
    height: png.height,
  };
}

async function validateReviewDirectory(directory) {
  const directoryInfo = await lstat(directory);
  if (!directoryInfo.isDirectory() || directoryInfo.isSymbolicLink()) {
    throw new Error("review output must be a nonsymlink directory");
  }
  const reviewFile = path.join(directory, "review.json");
  const reviewInfo = await lstat(reviewFile);
  if (!reviewInfo.isFile() || reviewInfo.isSymbolicLink()) throw new Error("review.json must be a nonsymlink file");
  const review = JSON.parse(await readFile(reviewFile, "utf8"));
  if (review.schemaVersion !== 1 || review.kind !== "waffle-rig-motion-review") {
    throw new Error("review output has an unsupported manifest");
  }
  if (!Object.hasOwn(BACKGROUNDS, review.background)
    || review.canvas?.width !== 1536
    || review.canvas?.height !== 1024
    || !Array.isArray(review.frames)
    || review.frames.length === 0
    || review.frames.some((frame, index) => (
      !Number.isSafeInteger(frame)
      || frame < 0
      || (index > 0 && frame <= review.frames[index - 1])
    ))) {
    throw new Error("review output manifest metadata is invalid");
  }
  const hasContactSheets = Array.isArray(review.contactSheets)
    && review.contactSheets.length === CONTACT_WIDTHS.length
    && review.contactSheets.every((width, index) => width === CONTACT_WIDTHS[index]);
  if (!hasContactSheets && (!Array.isArray(review.contactSheets) || review.contactSheets.length !== 0)) {
    throw new Error("review output declared inventory is incomplete");
  }
  if (!Array.isArray(review.files) || review.files.length === 0) throw new Error("review output files must be non-empty");
  const expectedNames = new Set(review.frames.map((frame) => `frame-${String(frame).padStart(4, "0")}.png`));
  if (hasContactSheets) {
    for (const width of CONTACT_WIDTHS) expectedNames.add(`contact-sheet-${width}.png`);
  }
  if (review.files.length !== expectedNames.size
    || review.files.some((entry) => !expectedNames.has(entry?.file))) {
    throw new Error("review output declared inventory is incomplete");
  }
  const names = new Set();
  for (const entry of review.files) {
    if (!entry || typeof entry !== "object" || !SAFE_REVIEW_FILE.test(entry.file) || names.has(entry.file)) {
      throw new Error("review output contains an unsafe or duplicate file");
    }
    names.add(entry.file);
    if (!SHA256.test(entry.sha256) || !Number.isInteger(entry.width) || !Number.isInteger(entry.height)) {
      throw new Error(`review output metadata is invalid for ${entry.file}`);
    }
    const file = path.join(directory, entry.file);
    const info = await lstat(file);
    if (!info.isFile() || info.isSymbolicLink()) throw new Error(`review output ${entry.file} must be a nonsymlink file`);
    const bytes = await readFile(file);
    if (hashBytes(bytes) !== entry.sha256) throw new Error(`review output hash mismatch for ${entry.file}`);
    const png = PNG.sync.read(bytes);
    const sheetMatch = /^contact-sheet-(160|320)\.png$/u.exec(entry.file);
    const cellWidth = sheetMatch ? Number(sheetMatch[1]) : undefined;
    const expectedWidth = cellWidth
      ? cellWidth * Math.min(CONTACT_COLUMNS, review.frames.length)
      : review.canvas.width;
    const expectedHeight = cellWidth
      ? Math.round(cellWidth * review.canvas.height / review.canvas.width)
        * Math.ceil(review.frames.length / CONTACT_COLUMNS)
      : review.canvas.height;
    if (entry.width !== expectedWidth
      || entry.height !== expectedHeight
      || png.width !== entry.width
      || png.height !== entry.height
      || png.colorType !== 6) {
      throw new Error(`review output dimensions or RGBA type mismatch for ${entry.file}`);
    }
  }
  const actual = (await readdir(directory)).toSorted();
  const expected = [...names, "review.json"].toSorted();
  if (actual.length !== expected.length || actual.some((name, index) => name !== expected[index])) {
    throw new Error("review output contains undeclared files");
  }
  return review;
}

function promotionPaths(outputDirectory) {
  return {
    marker: `${outputDirectory}.promotion.json`,
    markerTemporary: `${outputDirectory}.promotion.json.building-${process.pid}`,
    previous: `${outputDirectory}.previous-${process.pid}`,
    temporary: `${outputDirectory}.building-${process.pid}`,
  };
}

async function previousEntries(outputDirectory) {
  const parent = path.dirname(outputDirectory);
  if (!await exists(parent)) return [];
  const prefix = `${path.basename(outputDirectory)}.previous-`;
  return (await readdir(parent)).filter((name) => name.startsWith(prefix)).toSorted();
}

async function recoverInterruptedPromotion(outputDirectory, renamePath) {
  const paths = promotionPaths(outputDirectory);
  const entries = await previousEntries(outputDirectory);
  if (!await exists(paths.marker)) {
    if (entries.length === 0) return;
    if (entries.length !== 1) throw new Error("ambiguous review promotion recovery state");
    const previous = path.join(path.dirname(outputDirectory), entries[0]);
    if (await exists(outputDirectory)) {
      await validateReviewDirectory(outputDirectory);
      await validateReviewDirectory(previous);
      await rm(previous, { recursive: true });
      return;
    }
    await validateReviewDirectory(previous);
    await renamePath(previous, outputDirectory);
    await validateReviewDirectory(outputDirectory);
    return;
  }

  const markerInfo = await lstat(paths.marker);
  if (!markerInfo.isFile() || markerInfo.isSymbolicLink()) throw new Error("ambiguous review promotion recovery state");
  let marker;
  try {
    marker = JSON.parse(await readFile(paths.marker, "utf8"));
  } catch {
    throw new Error("ambiguous review promotion recovery state");
  }
  const prefix = `${path.basename(outputDirectory)}.previous-`;
  if (marker.schemaVersion !== 1
    || marker.outputDirectory !== path.basename(outputDirectory)
    || typeof marker.previousDirectory !== "string"
    || !marker.previousDirectory.startsWith(prefix)
    || marker.previousDirectory.includes(path.sep)
    || entries.length > 1
    || (entries.length === 1 && entries[0] !== marker.previousDirectory)) {
    throw new Error("ambiguous review promotion recovery state");
  }
  const previous = path.join(path.dirname(outputDirectory), marker.previousDirectory);
  if (!await exists(outputDirectory)) {
    if (!await exists(previous)) throw new Error("review promotion recovery has no complete previous output");
    await validateReviewDirectory(previous);
    await renamePath(previous, outputDirectory);
  } else {
    await validateReviewDirectory(outputDirectory);
    if (await exists(previous)) {
      await validateReviewDirectory(previous);
      await rm(previous, { recursive: true });
    }
  }
  await validateReviewDirectory(outputDirectory);
  await rm(paths.marker, { force: true });
}

async function writePromotionMarker(outputDirectory, previousDirectory, renamePath) {
  const paths = promotionPaths(outputDirectory);
  const marker = {
    schemaVersion: 1,
    outputDirectory: path.basename(outputDirectory),
    previousDirectory: path.basename(previousDirectory),
  };
  await rm(paths.markerTemporary, { force: true });
  await writeFile(paths.markerTemporary, `${JSON.stringify(marker, null, 2)}\n`, { flag: "wx" });
  await renamePath(paths.markerTemporary, paths.marker);
}

async function promoteReview(outputDirectory, temporaryDirectory, renamePath) {
  if (!await exists(outputDirectory)) {
    await renamePath(temporaryDirectory, outputDirectory);
    return;
  }
  await validateReviewDirectory(outputDirectory);
  const paths = promotionPaths(outputDirectory);
  await writePromotionMarker(outputDirectory, paths.previous, renamePath);
  try {
    await renamePath(outputDirectory, paths.previous);
  } catch (error) {
    await validateReviewDirectory(outputDirectory);
    await rm(paths.marker, { force: true });
    throw error;
  }
  try {
    await renamePath(temporaryDirectory, outputDirectory);
  } catch (promotionError) {
    try {
      await renamePath(paths.previous, outputDirectory);
      await validateReviewDirectory(outputDirectory);
      await rm(paths.marker, { force: true });
    } catch (restorationError) {
      throw new Error(restorationError.message, { cause: promotionError });
    }
    throw promotionError;
  }
  await validateReviewDirectory(outputDirectory);
  await rm(paths.previous, { recursive: true });
  await rm(paths.marker, { force: true });
}

async function writeTemporaryReview({
  background,
  clip,
  clipPath,
  contactSheet: includeContactSheet,
  frames,
  manifest,
  manifestPath,
  temporaryDirectory,
}) {
  await mkdir(temporaryDirectory);
  const files = [];
  const frameFiles = [];
  for (const frame of frames) {
    const transparent = await renderRigFrame(manifestPath, clipPath, frame);
    const composited = sourceOver(
      backgroundRgba(manifest.canvas.width, manifest.canvas.height, background),
      transparent,
    );
    const file = path.join(temporaryDirectory, `frame-${String(frame).padStart(4, "0")}.png`);
    files.push(await writePng(file, composited));
    frameFiles.push(file);
  }
  if (includeContactSheet) {
    for (const width of CONTACT_WIDTHS) {
      const file = path.join(temporaryDirectory, `contact-sheet-${width}.png`);
      files.push(await writePng(file, await contactSheet(frameFiles, width)));
    }
  }
  files.sort((left, right) => left.file.localeCompare(right.file, "en"));
  const review = {
    schemaVersion: 1,
    kind: "waffle-rig-motion-review",
    rig: path.basename(manifestPath),
    clip: { id: clip.id, file: path.basename(clipPath), fps: clip.fps, frameCount: clip.frameCount },
    canvas: manifest.canvas,
    background,
    frames,
    contactSheets: includeContactSheet ? CONTACT_WIDTHS : [],
    files,
  };
  await writeFile(path.join(temporaryDirectory, "review.json"), `${JSON.stringify(review, null, 2)}\n`);
  await validateReviewDirectory(temporaryDirectory);
}

export async function renderMotionReview(options, { renamePath = rename } = {}) {
  const cwd = await realpath(path.resolve(options.cwd ?? process.cwd()));
  const manifestPath = path.resolve(cwd, options.manifestPath);
  const clipPath = path.resolve(cwd, options.clipPath);
  const outputDirectory = await safeOutputDirectory(options.outputDirectory, cwd);
  if (!Object.hasOwn(BACKGROUNDS, options.background)) {
    throw new Error("background must be one of warm-white, charcoal, checkerboard");
  }

  const [manifest, clip] = await Promise.all([
    readFile(manifestPath, "utf8").then(JSON.parse),
    readFile(clipPath, "utf8").then(JSON.parse),
  ]);
  await validateRig(manifestPath);
  validateMotionClipShape(manifest, clip);
  for (let frame = 0; frame < clip.frameCount; frame += 1) evaluateClip(manifest, clip, frame);
  if (clip.loop) assertLoopClosure(manifest, clip);
  const frames = parseFrames(options.frames, clip.frameCount);

  await mkdir(path.dirname(outputDirectory), { recursive: true });
  await assertNoSymlinkComponents(cwd, outputDirectory);
  await recoverInterruptedPromotion(outputDirectory, renamePath);
  const paths = promotionPaths(outputDirectory);
  await rm(paths.temporary, { recursive: true, force: true });
  try {
    await writeTemporaryReview({
      background: options.background,
      clip,
      clipPath,
      contactSheet: options.contactSheet === true,
      frames,
      manifest,
      manifestPath,
      temporaryDirectory: paths.temporary,
    });
    await promoteReview(outputDirectory, paths.temporary, renamePath);
  } catch (error) {
    await rm(paths.temporary, { recursive: true, force: true });
    throw error;
  }
  return outputDirectory;
}

function parseArguments(args) {
  if (args.length < 3) {
    throw new Error("usage: render-waffle-rig-motion.mjs <rig.json> <clip.json> <output-directory> --frames <all|list> --background <warm-white|charcoal|checkerboard> [--contact-sheet]");
  }
  const options = {
    manifestPath: args[0],
    clipPath: args[1],
    outputDirectory: args[2],
    contactSheet: false,
  };
  for (let index = 3; index < args.length; index += 1) {
    const argument = args[index];
    if (argument === "--contact-sheet") {
      options.contactSheet = true;
      continue;
    }
    if (["--frames", "--background"].includes(argument)) {
      const value = args[index + 1];
      if (value === undefined) throw new Error(`${argument} requires a value`);
      options[argument === "--frames" ? "frames" : "background"] = value;
      index += 1;
      continue;
    }
    throw new Error(`unknown option ${argument}`);
  }
  return options;
}

async function main(args) {
  const output = await renderMotionReview(parseArguments(args));
  console.log(`WROTE ${output}`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
