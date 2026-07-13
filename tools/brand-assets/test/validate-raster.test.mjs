import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import {
  appendFile,
  mkdtemp,
  mkdir,
  rm,
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
    await truncate(target, 9 * 1024 * 1024);
    assets.push(asset(name, { id: `cat-${index}` }));
  }
  const manifest = await manifestFile(directory, assets);

  const result = run(manifest);

  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /manifest assets exceed 62914560-byte budget/);
});

test("optional mode skips manifests that do not exist", () => {
  const result = run("--optional", "/definitely/missing/manifest.json");
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /SKIP .*manifest\.json \(not present\)/);
});
