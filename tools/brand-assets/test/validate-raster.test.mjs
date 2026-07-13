import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import {
  appendFile,
  mkdtemp,
  mkdir,
  readFile,
  rm,
  symlink,
  truncate,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import { PNG } from "pngjs";

const validator = fileURLToPath(new URL("../validate-raster.mjs", import.meta.url));

function run(...args) {
  return spawnSync(process.execPath, [validator, ...args], { encoding: "utf8" });
}

const crcTable = Array.from({ length: 256 }, (_, value) => {
  let crc = value;
  for (let bit = 0; bit < 8; bit += 1) crc = (crc >>> 1) ^ (crc & 1 ? 0xedb88320 : 0);
  return crc >>> 0;
});

function crc32(buffer) {
  let crc = 0xffffffff;
  for (const byte of buffer) crc = (crc >>> 8) ^ crcTable[(crc ^ byte) & 0xff];
  return (crc ^ 0xffffffff) >>> 0;
}

function insertChunkBeforeIend(buffer, type, data) {
  const marker = buffer.lastIndexOf(Buffer.from("IEND")) - 4;
  const typeBuffer = Buffer.from(type);
  const chunk = Buffer.allocUnsafe(12 + data.length);
  chunk.writeUInt32BE(data.length, 0);
  typeBuffer.copy(chunk, 4);
  data.copy(chunk, 8);
  chunk.writeUInt32BE(crc32(Buffer.concat([typeBuffer, data])), 8 + data.length);
  return Buffer.concat([buffer.subarray(0, marker), chunk, buffer.subarray(marker)]);
}

async function padPngToSize(target, size) {
  const source = await readFile(target);
  const payloadBytes = size - source.length - 12;
  assert.ok(payloadBytes >= 0, "target size must fit a padding chunk");
  await writeFile(target, insertChunkBeforeIend(source, "raNd", Buffer.alloc(payloadBytes)));
}

async function workspace(t) {
  const directory = await mkdtemp(path.join(tmpdir(), "waffle-raster-"));
  t.after(() => rm(directory, { force: true, recursive: true }));
  return directory;
}

async function pngFile(directory, name, options = {}) {
  const width = options.width ?? 4;
  const height = options.height ?? 4;
  const png = new PNG({ width, height });
  const alpha = options.alpha ?? 255;
  for (let i = 0; i < png.data.length; i += 4) {
    png.data[i] = options.red ?? 220;
    png.data[i + 1] = options.green ?? 130;
    png.data[i + 2] = options.blue ?? 40;
    png.data[i + 3] = alpha;
  }
  if (options.transparentCorners) {
    for (const [x, y] of [[0, 0], [width - 1, 0], [0, height - 1], [width - 1, height - 1]]) {
      png.data[(width * y + x) * 4 + 3] = 0;
    }
  }
  const target = path.join(directory, name);
  await mkdir(path.dirname(target), { recursive: true });
  await writeFile(
    target,
    PNG.sync.write(png, options.rgb ? { colorType: 2, inputColorType: 6 } : {}),
  );
  return target;
}

async function manifestFile(directory, assets, extra = {}) {
  const target = path.join(directory, "manifest.json");
  await writeFile(target, JSON.stringify({ schemaVersion: 1, assets, ...extra }));
  return target;
}

function asset(file = "cat.png", overrides = {}) {
  return {
    id: "cat",
    file,
    role: "pose",
    width: 4,
    height: 4,
    alphaPolicy: "opaque",
    provenance: "Approved raster character study.",
    ...overrides,
  };
}

test("accepts a schema-v1 raster asset manifest and RGBA PNG", async (t) => {
  const directory = await workspace(t);
  await pngFile(directory, "cat.png");
  const manifest = await manifestFile(directory, [asset()]);

  const result = run(manifest);

  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /PASS .*manifest\.json assets=1 bytes=/);
});

test("requires every declared asset file to exist", async (t) => {
  const directory = await workspace(t);
  const manifest = await manifestFile(directory, [asset("missing.png")]);

  const result = run(manifest);

  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /asset file does not exist: missing\.png/);
});

test("rejects a static asset symlink that resolves outside the manifest directory", async (t) => {
  const directory = await workspace(t);
  const outside = await workspace(t);
  const external = await pngFile(outside, "external.png");
  await symlink(external, path.join(directory, "cat.png"));
  const manifest = await manifestFile(directory, [asset()]);

  const result = run(manifest);

  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /asset cat file must resolve inside the manifest directory/);
});

test("requires asset-manifest schema version 1", async (t) => {
  const directory = await workspace(t);
  await pngFile(directory, "cat.png");
  const manifest = await manifestFile(directory, [asset()], { schemaVersion: 2 });

  const result = run(manifest);

  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /schemaVersion must be 1/);
});

test("requires all asset fields and unique IDs", async (t) => {
  const directory = await workspace(t);
  await pngFile(directory, "cat.png");
  const incomplete = asset("cat.png");
  delete incomplete.provenance;
  let manifest = await manifestFile(directory, [incomplete]);

  let result = run(manifest);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /asset cat is missing provenance/);

  manifest = await manifestFile(directory, [asset(), asset("cat.png")]);
  result = run(manifest);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /duplicate asset id: cat/);
});

test("requires asset IDs to be non-empty strings", async (t) => {
  const directory = await workspace(t);
  await pngFile(directory, "cat.png");
  const manifest = await manifestFile(directory, [asset("cat.png", { id: null })]);

  const result = run(manifest);

  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /asset 1 id must be a non-empty string/);
});

test("rejects declared dimensions that differ from the PNG", async (t) => {
  const directory = await workspace(t);
  await pngFile(directory, "cat.png", { width: 5, height: 4 });
  const manifest = await manifestFile(directory, [asset()]);

  const result = run(manifest);

  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /declared dimensions 4x4 do not match PNG 5x4/);
});

test("requires PNG colour type RGBA", async (t) => {
  const directory = await workspace(t);
  await pngFile(directory, "cat.png", { rgb: true });
  const manifest = await manifestFile(directory, [asset()]);

  const result = run(manifest);

  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /PNG colour type must be RGBA/);
});

test("enforces configurable transparent-corner and opaque alpha policies", async (t) => {
  const directory = await workspace(t);
  await pngFile(directory, "cat.png");
  let manifest = await manifestFile(directory, [asset("cat.png", { alphaPolicy: "transparent-corners" })]);

  let result = run(manifest);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /all four corner pixels must be transparent/);

  await pngFile(directory, "cat.png", { transparentCorners: true });
  result = run(manifest);
  assert.equal(result.status, 0, result.stderr);

  manifest = await manifestFile(directory, [asset("cat.png", { alphaPolicy: "opaque" })]);
  result = run(manifest);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /all pixels must be opaque/);

  manifest = await manifestFile(directory, [asset("cat.png", { alphaPolicy: "any" })]);
  result = run(manifest);
  assert.equal(result.status, 0, result.stderr);
});

test("rejects PNG text, EXIF, and embedded-profile chunks", async (t) => {
  const forbidden = ["tEXt", "zTXt", "iTXt", "eXIf", "iCCP"];
  for (const chunk of forbidden) {
    await t.test(chunk, async () => {
      const directory = await workspace(t);
      const target = await pngFile(directory, "cat.png");
      await appendFile(target, Buffer.concat([Buffer.from([0, 0, 0, 0]), Buffer.from(chunk), Buffer.alloc(4)]));
      const manifest = await manifestFile(directory, [asset()]);

      const result = run(manifest);

      assert.notEqual(result.status, 0);
      assert.match(result.stderr, new RegExp(`forbidden PNG chunk ${chunk}`));
    });
  }
});

test("enforces the 10 MB per-file budget", async (t) => {
  const directory = await workspace(t);
  const target = await pngFile(directory, "cat.png");
  await truncate(target, 10 * 1024 * 1024 + 1);
  const manifest = await manifestFile(directory, [asset()]);

  const result = run(manifest);

  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /asset exceeds 10485760-byte budget/);
});

test("enforces the 60 MB complete-milestone budget", async (t) => {
  const directory = await workspace(t);
  const assets = [];
  for (let index = 0; index < 7; index += 1) {
    const name = `cat-${index}.png`;
    const target = await pngFile(directory, name);
    await padPngToSize(target, 9 * 1024 * 1024);
    assets.push(asset(name, { id: `cat-${index}` }));
  }
  const manifest = await manifestFile(directory, assets);

  const result = run(manifest);

  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /manifest assets exceed 62914560-byte budget/);
});

async function combinedManifestFixture(t, idleExtraBytes) {
  const directory = await workspace(t);
  const assets = [];
  for (let index = 0; index < 6; index += 1) {
    const name = `asset-${index}.png`;
    const target = await pngFile(directory, name, { transparentCorners: true });
    await padPngToSize(target, 9 * 1024 * 1024);
    assets.push(asset(name, {
      id: `asset-${index}`,
      alphaPolicy: "transparent-corners",
    }));
  }
  const staticManifest = await manifestFile(directory, assets);
  const extra = await pngFile(directory, "idle-extra.png", { transparentCorners: true });
  await padPngToSize(extra, idleExtraBytes);
  const idleManifest = path.join(directory, "idle.json");
  await writeFile(idleManifest, JSON.stringify({
    schemaVersion: 1,
    canvas: { width: 4, height: 4 },
    anchor: { x: 2, y: 3 },
    loop: true,
    seed: "asset-0.png",
    frames: [
      { file: "asset-0.png", durationMs: 100 },
      { file: "idle-extra.png", durationMs: 100 },
    ],
  }));
  return { idleManifest, staticManifest };
}

test("combined budget deduplicates shared PNGs and accepts a milestone below 60 MB", async (t) => {
  const fixture = await combinedManifestFixture(t, 5 * 1024 * 1024);

  const result = run(fixture.staticManifest, fixture.idleManifest);

  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /TOTAL uniquePngBytes=61865984/);
});

test("combined budget rejects static and idle PNGs above 60 MB", async (t) => {
  const fixture = await combinedManifestFixture(t, 8 * 1024 * 1024);

  const result = run(fixture.staticManifest, fixture.idleManifest);

  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /combined production PNGs exceed 62914560-byte budget/);
});

test("optional mode skips manifests that do not exist", () => {
  const result = run("--optional", "/definitely/missing/manifest.json");
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /SKIP .*manifest\.json \(not present\)/);
});
