import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { createHash } from "node:crypto";
import {
  cp,
  lstat,
  mkdir,
  mkdtemp,
  readFile,
  readdir,
  realpath,
  rename,
  rm,
  symlink,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { test } from "node:test";

import { PNG } from "pngjs";

import { renderMotionReview } from "../render-waffle-rig-motion.mjs";

const execFileAsync = promisify(execFile);
const SCRIPT = path.resolve(import.meta.dirname, "../render-waffle-rig-motion.mjs");
const REPO_ROOT = path.resolve(import.meta.dirname, "../../..");
const WARM_WHITE = [250, 247, 240, 255];
const CHARCOAL = [35, 32, 30, 255];
let reviewSequence = 0;

function rgba(width, height) {
  return new PNG({ width, height });
}

function paintRectangle(png, left, top, width, height, color) {
  for (let y = top; y < top + height; y += 1) {
    for (let x = left; x < left + width; x += 1) {
      png.data.set(color, (y * png.width + x) * 4);
    }
  }
}

async function writePng(file, png) {
  await mkdir(path.dirname(file), { recursive: true });
  await writeFile(file, PNG.sync.write(png));
}

async function sha256(file) {
  return createHash("sha256").update(await readFile(file)).digest("hex");
}

async function workspace(t) {
  const root = await mkdtemp(path.join(tmpdir(), "waffle-motion-preview-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const waffle = path.join(root, "assets", "brand", "waffle");
  const rigDirectory = path.join(waffle, "rigs", "standing-v2");
  const sourceFile = path.join(waffle, "poses", "standing.png");
  const neutralFile = path.join(rigDirectory, "neutral-reference.png");
  const layerFile = path.join(rigDirectory, "layers", "cat.png");
  const manifestPath = path.join(rigDirectory, "rig.json");
  const clipPath = path.join(rigDirectory, "motions", "tiny.json");

  const source = rgba(1536, 1024);
  paintRectangle(source, 744, 480, 48, 64, [232, 137, 48, 255]);
  await Promise.all([
    writePng(sourceFile, source),
    writePng(neutralFile, source),
    writePng(layerFile, source),
  ]);

  const [sourceHash, neutralHash, layerHash] = await Promise.all([
    sha256(sourceFile),
    sha256(neutralFile),
    sha256(layerFile),
  ]);
  const manifest = {
    schemaVersion: 2,
    canvas: { width: 1536, height: 1024 },
    root: { id: "waffle-root", pivot: { x: 0.5, y: 0.75 } },
    source: { file: "../../poses/standing.png", sha256: sourceHash },
    neutralReference: { file: "neutral-reference.png", sha256: neutralHash },
    layers: [{
      id: "cat",
      file: "layers/cat.png",
      role: "visible",
      parent: "waffle-root",
      drawOrder: 1,
      visibleAtNeutral: true,
      blendMode: "normal",
      pivot: { x: 0.5, y: 0.5 },
      neutral: { x: 0, y: 0, rotationDegrees: 0, scaleX: 1, scaleY: 1 },
      limits: {},
      sha256: layerHash,
    }],
    variants: {},
    controls: {
      fade: {
        min: -1,
        max: 0,
        bindings: [{ layer: "cat", property: "opacity", factor: 1 }],
      },
    },
  };
  const clip = {
    schemaVersion: 1,
    id: "tiny",
    fps: 24,
    frameCount: 3,
    loop: false,
    requiredClosure: { firstFrame: 0, lastFrame: 2 },
    variants: {},
    controls: {
      fade: [
        { frame: 0, value: 0, interpolation: "hold" },
        { frame: 1, value: -1, interpolation: "hold" },
        { frame: 2, value: -0.5, interpolation: "hold" },
      ],
    },
  };
  await mkdir(path.dirname(clipPath), { recursive: true });
  await Promise.all([
    writeFile(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`),
    writeFile(clipPath, `${JSON.stringify(clip, null, 2)}\n`),
  ]);
  return { clipPath, layerFile, manifestPath, root };
}

function pixel(png, x, y) {
  const offset = (y * png.width + x) * 4;
  return [...png.data.subarray(offset, offset + 4)];
}

async function decoded(file) {
  return PNG.sync.read(await readFile(file));
}

async function entries(directory) {
  return (await readdir(directory)).toSorted();
}

function repoReviewPath(t, name) {
  reviewSequence += 1;
  const output = path.join(
    REPO_ROOT,
    ".superpowers",
    "tests",
    `task-6-${process.pid}-${reviewSequence}-${name}`,
  );
  t.after(async () => {
    await Promise.all([
      rm(output, { recursive: true, force: true }),
      rm(`${output}.building-${process.pid}`, { recursive: true, force: true }),
      rm(`${output}.previous-${process.pid}`, { recursive: true, force: true }),
      rm(`${output}.promotion.json`, { force: true }),
      rm(`${output}.promotion.json.building-${process.pid}`, { force: true }),
    ]);
  });
  return output;
}

test("repository anchoring is invariant across repo root, asset subdirectory, and unrelated cwd", async (t) => {
  const fixture = await workspace(t);
  const fromRoot = repoReviewPath(t, "root");
  const fromAssets = repoReviewPath(t, "assets-subdir");
  const fromOutside = repoReviewPath(t, "outside");
  const common = {
    manifestPath: fixture.manifestPath,
    clipPath: fixture.clipPath,
    frames: "0",
    background: "warm-white",
    contactSheet: false,
  };

  await renderMotionReview({
    ...common,
    outputDirectory: path.relative(REPO_ROOT, fromRoot),
    cwd: REPO_ROOT,
  });
  await renderMotionReview({
    ...common,
    outputDirectory: path.relative(path.join(REPO_ROOT, "assets"), fromAssets),
    cwd: path.join(REPO_ROOT, "assets"),
  });
  await renderMotionReview({
    ...common,
    outputDirectory: fromOutside,
    cwd: fixture.root,
  });
  for (const output of [fromRoot, fromAssets, fromOutside]) {
    assert.deepEqual(await entries(output), ["frame-0000.png", "review.json"]);
  }

  const invalidClip = JSON.parse(await readFile(fixture.clipPath, "utf8"));
  invalidClip.frameCount = 0;
  await writeFile(fixture.clipPath, JSON.stringify(invalidClip));
  const forbidden = path.join(REPO_ROOT, "assets", ".superpowers", `task-6-${process.pid}`);
  t.after(() => rm(forbidden, { recursive: true, force: true }));
  await assert.rejects(renderMotionReview({
    ...common,
    outputDirectory: `.superpowers/task-6-${process.pid}`,
    cwd: path.join(REPO_ROOT, "assets"),
  }), /refusing to write inside assets/);
  await assert.rejects(lstat(forbidden), { code: "ENOENT" });
});

test("CLI renders every selected frame and row-major 320/160 contact sheets deterministically", async (t) => {
  const fixture = await workspace(t);
  const output = repoReviewPath(t, "tiny-warm");

  await execFileAsync(process.execPath, [
    SCRIPT,
    fixture.manifestPath,
    fixture.clipPath,
    output,
    "--frames", "all",
    "--background", "warm-white",
    "--contact-sheet",
  ], { cwd: fixture.root });

  assert.deepEqual(await entries(output), [
    "contact-sheet-160.png",
    "contact-sheet-320.png",
    "frame-0000.png",
    "frame-0001.png",
    "frame-0002.png",
    "review.json",
  ]);
  const frames = await Promise.all([0, 1, 2].map((frame) => (
    decoded(path.join(output, `frame-${String(frame).padStart(4, "0")}.png`))
  )));
  for (const frame of frames) {
    assert.equal(frame.width, 1536);
    assert.equal(frame.height, 1024);
    assert.equal(frame.colorType, 6);
    assert.deepEqual(pixel(frame, 0, 0), WARM_WHITE);
  }
  assert.deepEqual(pixel(frames[0], 768, 512), [232, 137, 48, 255]);
  assert.deepEqual(pixel(frames[1], 768, 512), WARM_WHITE);
  assert.deepEqual(pixel(frames[2], 768, 512), [241, 192, 144, 255]);

  for (const size of [320, 160]) {
    const sheet = await decoded(path.join(output, `contact-sheet-${size}.png`));
    const cellHeight = Math.round(size * 1024 / 1536);
    assert.equal(sheet.width, size * 3);
    assert.equal(sheet.height, cellHeight);
    assert.deepEqual(pixel(sheet, Math.floor(size * 0.5), Math.floor(cellHeight * 0.5)), [232, 137, 48, 255]);
    assert.deepEqual(pixel(sheet, size + Math.floor(size * 0.5), Math.floor(cellHeight * 0.5)), WARM_WHITE);
    assert.deepEqual(pixel(sheet, size * 2 + Math.floor(size * 0.5), Math.floor(cellHeight * 0.5)), [241, 192, 144, 255]);
  }

  const review = JSON.parse(await readFile(path.join(output, "review.json"), "utf8"));
  assert.deepEqual(review.frames, [0, 1, 2]);
  assert.equal(review.background, "warm-white");
  assert.deepEqual(
    Object.fromEntries(review.files.map((file) => [file.file, file.sha256])),
    {
      "contact-sheet-160.png": "34f475e7ba9517fa4c8beecc7386a2bd3c6ba888f8e10f7d1663b9a5a4b15d23",
      "contact-sheet-320.png": "8e97b3e8bc7188fae9a7ca8e5d0b7a746f7d68092dc448ff384a6d64751cb0da",
      "frame-0000.png": "f92284ecb3d8a35c56289aab4972cc04dce2c8bc9bc5e44d758a77134c1399c5",
      "frame-0001.png": "bd763700a9a6c0cb3f2330e5a4749cda71bdd5808033c634b49aa3776c221f7f",
      "frame-0002.png": "40e28fce2b25186e1a0eff1755077b54ee167e43aa33e62591d7ec5236d9fd11",
    },
  );
});

test("renderer requires explicit supported backgrounds and preserves selected frame order", async (t) => {
  const fixture = await workspace(t);
  const base = repoReviewPath(t, "backgrounds");
  for (const [background, expected] of [
    ["charcoal", CHARCOAL],
    ["checkerboard", [238, 235, 228, 255]],
  ]) {
    const output = path.join(base, background);
    await renderMotionReview({
      manifestPath: fixture.manifestPath,
      clipPath: fixture.clipPath,
      outputDirectory: output,
      frames: "2,0",
      background,
      contactSheet: false,
      cwd: fixture.root,
    });
    assert.deepEqual(await entries(output), ["frame-0000.png", "frame-0002.png", "review.json"]);
    assert.deepEqual(pixel(await decoded(path.join(output, "frame-0000.png")), 0, 0), expected);
    const review = JSON.parse(await readFile(path.join(output, "review.json"), "utf8"));
    assert.deepEqual(review.frames, [0, 2]);
  }
  const checker = await decoded(path.join(base, "checkerboard", "frame-0000.png"));
  assert.deepEqual(pixel(checker, 32, 0), [207, 202, 193, 255]);

  await assert.rejects(renderMotionReview({
    manifestPath: fixture.manifestPath,
    clipPath: fixture.clipPath,
    outputDirectory: path.join(base, "missing-background"),
    frames: "all",
    cwd: fixture.root,
  }), /background must be one of warm-white, charcoal, checkerboard/);
});

test("renderer validates the rig and clip before creating temporary output", async (t) => {
  const fixture = await workspace(t);
  const output = repoReviewPath(t, "invalid");
  const clip = JSON.parse(await readFile(fixture.clipPath, "utf8"));
  clip.frameCount = 0;
  await writeFile(fixture.clipPath, JSON.stringify(clip));

  await assert.rejects(renderMotionReview({
    manifestPath: fixture.manifestPath,
    clipPath: fixture.clipPath,
    outputDirectory: output,
    frames: "all",
    background: "warm-white",
    contactSheet: true,
    cwd: fixture.root,
  }), /frameCount must be a positive integer/);

  await assert.rejects(lstat(output), { code: "ENOENT" });
  await assert.rejects(lstat(`${output}.building-${process.pid}`), { code: "ENOENT" });
});

test("renderer preflights every non-loop clip frame before creating output", async (t) => {
  const fixture = await workspace(t);
  const output = repoReviewPath(t, "uncovered-frame");
  const clip = JSON.parse(await readFile(fixture.clipPath, "utf8"));
  clip.controls.fade.pop();
  await writeFile(fixture.clipPath, JSON.stringify(clip));

  await assert.rejects(renderMotionReview({
    manifestPath: fixture.manifestPath,
    clipPath: fixture.clipPath,
    outputDirectory: output,
    frames: "0",
    background: "warm-white",
    contactSheet: false,
    cwd: fixture.root,
  }), /frame 2 is outside keyframe range 0\.\.1/);

  await assert.rejects(lstat(output), { code: "ENOENT" });
  await assert.rejects(lstat(`${output}.building-${process.pid}`), { code: "ENOENT" });
});

test("renderer uses one validated manifest, clip, and raster snapshot after live inputs mutate", async (t) => {
  const fixture = await workspace(t);
  const output = repoReviewPath(t, "immutable-snapshot");
  const [manifestBytes, clipBytes, layerBytes] = await Promise.all([
    readFile(fixture.manifestPath),
    readFile(fixture.clipPath),
    readFile(fixture.layerFile),
  ]);
  let snapshotHookCalls = 0;

  await renderMotionReview({
    manifestPath: fixture.manifestPath,
    clipPath: fixture.clipPath,
    outputDirectory: output,
    frames: "0",
    background: "warm-white",
    contactSheet: false,
    cwd: fixture.root,
  }, {
    afterSnapshot: async () => {
      snapshotHookCalls += 1;
      const changedLayer = rgba(1536, 1024);
      paintRectangle(changedLayer, 744, 480, 48, 64, [20, 60, 220, 255]);
      const changedClip = JSON.parse(clipBytes.toString("utf8"));
      changedClip.controls.fade[0].value = -1;
      const changedManifest = JSON.parse(manifestBytes.toString("utf8"));
      changedManifest.root.pivot.x = 0.49;
      await Promise.all([
        writePng(fixture.layerFile, changedLayer),
        writeFile(fixture.clipPath, `${JSON.stringify(changedClip, null, 2)}\n`),
        writeFile(fixture.manifestPath, `${JSON.stringify(changedManifest, null, 2)}\n`),
      ]);
    },
  });

  assert.equal(snapshotHookCalls, 1);
  const frame = await decoded(path.join(output, "frame-0000.png"));
  assert.deepEqual(pixel(frame, 768, 512), [232, 137, 48, 255]);
  const review = JSON.parse(await readFile(path.join(output, "review.json"), "utf8"));
  assert.equal(review.inputs.manifest.sha256, createHash("sha256").update(manifestBytes).digest("hex"));
  assert.equal(review.inputs.clip.sha256, createHash("sha256").update(clipBytes).digest("hex"));
  assert.equal(review.inputs.manifest.scope, "external");
  assert.equal(review.inputs.clip.scope, "external");
  assert.equal(review.inputs.manifest.path.includes(fixture.root), false);
  assert.equal(review.inputs.clip.path.includes(fixture.root), false);
  const layer = review.inputs.rasters.find((entry) => entry.declaredPath === "layers/cat.png");
  assert.equal(layer.sha256, createHash("sha256").update(layerBytes).digest("hex"));
  assert.equal(layer.scope, "external");
  assert.equal(layer.path.includes(fixture.root), false);
});

test("renderer refuses assets, traversal, and symlinked output before writing", async (t) => {
  const fixture = await workspace(t);
  const common = {
    manifestPath: fixture.manifestPath,
    clipPath: fixture.clipPath,
    frames: "all",
    background: "warm-white",
    contactSheet: true,
    cwd: fixture.root,
  };
  await assert.rejects(renderMotionReview({
    ...common,
    outputDirectory: path.join(REPO_ROOT, "assets", "brand", "waffle", "review"),
  }), /refusing to write inside assets/);
  await assert.rejects(renderMotionReview({
    ...common,
    outputDirectory: path.join(fixture.root, ".superpowers", "reviews", "..", "..", "assets"),
  }), /refusing to write inside assets|output must be lexically inside repository \.superpowers/);

  const outside = path.join(fixture.root, "outside");
  const link = repoReviewPath(t, "linked");
  await Promise.all([mkdir(outside), mkdir(path.dirname(link), { recursive: true })]);
  await symlink(outside, link);
  await assert.rejects(renderMotionReview({
    ...common,
    outputDirectory: path.join(link, "escape"),
  }), /output path must not use symlinks|output must stay inside \.superpowers/);
  assert.deepEqual(await readdir(outside), []);

  const allowedTarget = repoReviewPath(t, "allowed-symlink-target");
  const outsideLink = path.join(fixture.root, "link-into-repo-reviews");
  await mkdir(allowedTarget);
  await symlink(allowedTarget, outsideLink);
  const invalidClip = JSON.parse(await readFile(fixture.clipPath, "utf8"));
  invalidClip.frameCount = 0;
  await writeFile(fixture.clipPath, JSON.stringify(invalidClip));
  await assert.rejects(renderMotionReview({
    ...common,
    outputDirectory: path.join(outsideLink, "must-not-write"),
  }), /output must be lexically inside repository \.superpowers/);
  assert.deepEqual(await readdir(allowedTarget), []);
});

test("injected promotion failure restores the previous complete review output", async (t) => {
  const fixture = await workspace(t);
  const output = repoReviewPath(t, "recoverable");
  const common = {
    manifestPath: fixture.manifestPath,
    clipPath: fixture.clipPath,
    outputDirectory: output,
    frames: "0",
    contactSheet: true,
    cwd: fixture.root,
  };
  await renderMotionReview({ ...common, background: "warm-white" });
  const originalReview = await readFile(path.join(output, "review.json"));
  const originalFrame = await readFile(path.join(output, "frame-0000.png"));
  const canonicalOutput = await realpath(output);
  const temporary = `${canonicalOutput}.building-${process.pid}`;
  let failed = false;

  await assert.rejects(renderMotionReview({ ...common, background: "charcoal" }, {
    renamePath: async (from, to) => {
      if (!failed && from === temporary && to === canonicalOutput) {
        failed = true;
        throw new Error("injected review promotion failure");
      }
      return rename(from, to);
    },
  }), /injected review promotion failure/);

  assert.deepEqual(await readFile(path.join(output, "review.json")), originalReview);
  assert.deepEqual(await readFile(path.join(output, "frame-0000.png")), originalFrame);
  const siblings = await readdir(path.dirname(output));
  assert.equal(siblings.some((name) => name.startsWith("recoverable.building-")), false);
  assert.equal(siblings.some((name) => name.startsWith("recoverable.previous-")), false);
  await assert.rejects(lstat(`${output}.promotion.json`), { code: "ENOENT" });
});

test("post-rename validation failure immediately restores the previous complete review", async (t) => {
  const fixture = await workspace(t);
  const output = repoReviewPath(t, "post-rename-validation");
  const common = {
    manifestPath: fixture.manifestPath,
    clipPath: fixture.clipPath,
    outputDirectory: output,
    frames: "0",
    contactSheet: false,
    cwd: fixture.root,
  };
  await renderMotionReview({ ...common, background: "warm-white" });
  const [originalReview, originalFrame] = await Promise.all([
    readFile(path.join(output, "review.json")),
    readFile(path.join(output, "frame-0000.png")),
  ]);
  let hookCalls = 0;

  await assert.rejects(renderMotionReview({ ...common, background: "charcoal" }, {
    afterPromote: async (promotedDirectory) => {
      hookCalls += 1;
      await writeFile(path.join(promotedDirectory, "frame-0000.png"), "corrupt promoted frame");
    },
  }), /review output hash mismatch|file is not a PNG/);

  assert.equal(hookCalls, 1);
  assert.deepEqual(await readFile(path.join(output, "review.json")), originalReview);
  assert.deepEqual(await readFile(path.join(output, "frame-0000.png")), originalFrame);
  const siblings = await readdir(path.dirname(output));
  assert.equal(siblings.some((name) => name.startsWith(`${path.basename(output)}.previous-`)), false);
  await assert.rejects(lstat(`${output}.promotion.json`), { code: "ENOENT" });
});

test("startup restores a valid previous review over a corrupt promoted destination", async (t) => {
  const fixture = await workspace(t);
  const output = repoReviewPath(t, "recover-corrupt-destination");
  const previous = `${output}.previous-424242`;
  t.after(() => rm(previous, { recursive: true, force: true }));
  const options = {
    manifestPath: fixture.manifestPath,
    clipPath: fixture.clipPath,
    outputDirectory: output,
    frames: "0",
    background: "warm-white",
    contactSheet: false,
    cwd: fixture.root,
  };
  await renderMotionReview(options);
  const [originalReview, originalFrame] = await Promise.all([
    readFile(path.join(output, "review.json")),
    readFile(path.join(output, "frame-0000.png")),
  ]);
  await rename(output, previous);
  await mkdir(output);
  await writeFile(path.join(output, "review.json"), "{\"corrupt\":true}\n");
  await writeFile(`${output}.promotion.json`, `${JSON.stringify({
    schemaVersion: 1,
    outputDirectory: path.basename(output),
    previousDirectory: path.basename(previous),
  }, null, 2)}\n`);

  await renderMotionReview(options);

  assert.deepEqual(await readFile(path.join(output, "review.json")), originalReview);
  assert.deepEqual(await readFile(path.join(output, "frame-0000.png")), originalFrame);
  await assert.rejects(lstat(previous), { code: "ENOENT" });
  await assert.rejects(lstat(`${output}.promotion.json`), { code: "ENOENT" });
});

test("recovery refuses a malformed marker without deleting target or evidence", async (t) => {
  const fixture = await workspace(t);
  const output = repoReviewPath(t, "malformed-marker");
  const options = {
    manifestPath: fixture.manifestPath,
    clipPath: fixture.clipPath,
    outputDirectory: output,
    frames: "0",
    background: "warm-white",
    contactSheet: false,
    cwd: fixture.root,
  };
  await renderMotionReview(options);
  const originalReview = await readFile(path.join(output, "review.json"));
  await writeFile(`${output}.promotion.json`, "not-json\n");

  await assert.rejects(renderMotionReview(options), /ambiguous review promotion recovery state/);

  assert.deepEqual(await readFile(path.join(output, "review.json")), originalReview);
  assert.equal(await readFile(`${output}.promotion.json`, "utf8"), "not-json\n");
});

test("recovery rejects a marker whose previous directory lacks a numeric promotion suffix", async (t) => {
  const fixture = await workspace(t);
  const output = repoReviewPath(t, "malformed-previous-name");
  const malformedPrevious = `${output}.previous-not-a-pid`;
  t.after(() => rm(malformedPrevious, { recursive: true, force: true }));
  const options = {
    manifestPath: fixture.manifestPath,
    clipPath: fixture.clipPath,
    outputDirectory: output,
    frames: "0",
    background: "warm-white",
    contactSheet: false,
    cwd: fixture.root,
  };
  await renderMotionReview(options);
  await cp(output, malformedPrevious, { recursive: true });
  await writeFile(`${output}.promotion.json`, `${JSON.stringify({
    schemaVersion: 1,
    outputDirectory: path.basename(output),
    previousDirectory: path.basename(malformedPrevious),
  })}\n`);

  await assert.rejects(renderMotionReview(options), /ambiguous review promotion recovery state/);

  assert.deepEqual(await entries(output), ["frame-0000.png", "review.json"]);
  assert.deepEqual(await entries(malformedPrevious), ["frame-0000.png", "review.json"]);
  assert.equal((await lstat(`${output}.promotion.json`)).isFile(), true);
});

test("recovery refuses multiple stale previous directories and preserves every artifact", async (t) => {
  const fixture = await workspace(t);
  const output = repoReviewPath(t, "multiple-previous");
  const previousOne = `${output}.previous-111111`;
  const previousTwo = `${output}.previous-222222`;
  t.after(() => Promise.all([
    rm(previousOne, { recursive: true, force: true }),
    rm(previousTwo, { recursive: true, force: true }),
  ]));
  const options = {
    manifestPath: fixture.manifestPath,
    clipPath: fixture.clipPath,
    outputDirectory: output,
    frames: "0",
    background: "warm-white",
    contactSheet: false,
    cwd: fixture.root,
  };
  await renderMotionReview(options);
  await Promise.all([
    cp(output, previousOne, { recursive: true }),
    cp(output, previousTwo, { recursive: true }),
  ]);

  await assert.rejects(renderMotionReview(options), /ambiguous review promotion recovery state/);

  for (const directory of [output, previousOne, previousTwo]) {
    assert.deepEqual(await entries(directory), ["frame-0000.png", "review.json"]);
  }
});

test("recovery restores the sole valid previous review when a marker has no destination", async (t) => {
  const fixture = await workspace(t);
  const output = repoReviewPath(t, "marker-missing-destination");
  const previous = `${output}.previous-333333`;
  t.after(() => rm(previous, { recursive: true, force: true }));
  const options = {
    manifestPath: fixture.manifestPath,
    clipPath: fixture.clipPath,
    outputDirectory: output,
    frames: "0",
    background: "warm-white",
    contactSheet: false,
    cwd: fixture.root,
  };
  await renderMotionReview(options);
  const originalFrame = await readFile(path.join(output, "frame-0000.png"));
  await rename(output, previous);
  await writeFile(`${output}.promotion.json`, `${JSON.stringify({
    schemaVersion: 1,
    outputDirectory: path.basename(output),
    previousDirectory: path.basename(previous),
  })}\n`);

  await renderMotionReview(options);

  assert.deepEqual(await readFile(path.join(output, "frame-0000.png")), originalFrame);
  await assert.rejects(lstat(previous), { code: "ENOENT" });
  await assert.rejects(lstat(`${output}.promotion.json`), { code: "ENOENT" });
});

test("recovery cleans an abandoned build only beside a complete destination", async (t) => {
  const fixture = await workspace(t);
  const output = repoReviewPath(t, "clean-abandoned-build");
  const building = `${output}.building-444444`;
  t.after(() => rm(building, { recursive: true, force: true }));
  const options = {
    manifestPath: fixture.manifestPath,
    clipPath: fixture.clipPath,
    outputDirectory: output,
    frames: "0",
    background: "warm-white",
    contactSheet: false,
    cwd: fixture.root,
  };
  await renderMotionReview(options);
  await cp(output, building, { recursive: true });

  await renderMotionReview(options);

  await assert.rejects(lstat(building), { code: "ENOENT" });
  assert.deepEqual(await entries(output), ["frame-0000.png", "review.json"]);
});

test("recovery preserves an abandoned build when no destination proves which state won", async (t) => {
  const fixture = await workspace(t);
  const seedOutput = repoReviewPath(t, "abandoned-seed");
  const output = repoReviewPath(t, "ambiguous-abandoned-build");
  const building = `${output}.building-555555`;
  t.after(() => rm(building, { recursive: true, force: true }));
  const common = {
    manifestPath: fixture.manifestPath,
    clipPath: fixture.clipPath,
    frames: "0",
    background: "warm-white",
    contactSheet: false,
    cwd: fixture.root,
  };
  await renderMotionReview({ ...common, outputDirectory: seedOutput });
  await rename(seedOutput, building);

  await assert.rejects(renderMotionReview({
    ...common,
    outputDirectory: output,
  }), /ambiguous review promotion recovery state/);

  assert.deepEqual(await entries(building), ["frame-0000.png", "review.json"]);
  await assert.rejects(lstat(output), { code: "ENOENT" });
});

test("renderer refuses to replace an existing review whose declared inventory is incomplete", async (t) => {
  const fixture = await workspace(t);
  const output = repoReviewPath(t, "incomplete");
  const common = {
    manifestPath: fixture.manifestPath,
    clipPath: fixture.clipPath,
    outputDirectory: output,
    frames: "0",
    background: "warm-white",
    contactSheet: false,
    cwd: fixture.root,
  };
  await renderMotionReview(common);
  const reviewFile = path.join(output, "review.json");
  const review = JSON.parse(await readFile(reviewFile, "utf8"));
  review.contactSheets = [320, 160];
  await writeFile(reviewFile, `${JSON.stringify(review, null, 2)}\n`);
  const corruptedReview = await readFile(reviewFile);

  await assert.rejects(renderMotionReview(common), /review output declared inventory is incomplete/);

  assert.deepEqual(await readFile(reviewFile), corruptedReview);
  assert.deepEqual(await entries(output), ["frame-0000.png", "review.json"]);
});

test("package exposes the review-only motion renderer script", async () => {
  const packageJson = JSON.parse(await readFile(path.resolve(import.meta.dirname, "../package.json"), "utf8"));
  assert.equal(packageJson.scripts["render:rig-motion"], "node render-waffle-rig-motion.mjs");
});
