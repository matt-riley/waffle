import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtemp, mkdir, readFile, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import { PNG } from "pngjs";

const tool = (name) => fileURLToPath(new URL(`../${name}`, import.meta.url));
const run = (name, ...args) =>
  spawnSync(process.execPath, [tool(name), ...args], { encoding: "utf8" });

async function workspace(t) {
  const directory = await mkdtemp(path.join(tmpdir(), "waffle-sprite-"));
  t.after(() => rm(directory, { force: true, recursive: true }));
  return directory;
}

async function writePng(target, width, height, pixel) {
  const png = new PNG({ width, height });
  for (let y = 0; y < height; y += 1) {
    for (let x = 0; x < width; x += 1) {
      const rgba = pixel(x, y);
      const offset = (y * width + x) * 4;
      png.data.set(rgba, offset);
    }
  }
  await mkdir(path.dirname(target), { recursive: true });
  await writeFile(target, PNG.sync.write(png));
  return target;
}

const decode = async (target) => PNG.sync.read(await readFile(target));

function crc32(buffer) {
  let crc = 0xffffffff;
  for (const byte of buffer) {
    crc ^= byte;
    for (let bit = 0; bit < 8; bit += 1) {
      crc = (crc >>> 1) ^ (crc & 1 ? 0xedb88320 : 0);
    }
  }
  return (crc ^ 0xffffffff) >>> 0;
}

function insertChunkBeforeIend(buffer, type, data) {
  const marker = buffer.lastIndexOf(Buffer.from("IEND")) - 4;
  const typeBuffer = Buffer.from(type);
  const chunk = Buffer.alloc(12 + data.length);
  chunk.writeUInt32BE(data.length, 0);
  typeBuffer.copy(chunk, 4);
  data.copy(chunk, 8);
  chunk.writeUInt32BE(crc32(Buffer.concat([typeBuffer, data])), 8 + data.length);
  return Buffer.concat([buffer.subarray(0, marker), chunk, buffer.subarray(marker)]);
}

test("sanitize decodes and re-encodes identical RGBA pixels", async (t) => {
  const directory = await workspace(t);
  const source = await writePng(path.join(directory, "source.png"), 3, 2, (x, y) => [x * 30, y * 50, 90, x === y ? 0 : 255]);
  const output = path.join(directory, "clean.png");

  const result = run("sanitize-png.mjs", source, output);

  assert.equal(result.status, 0, result.stderr);
  const before = await decode(source);
  const after = await decode(output);
  assert.equal(after.width, before.width);
  assert.equal(after.height, before.height);
  assert.deepEqual(after.data, before.data);
});

test("sanitize drops PNG textual metadata chunks", async (t) => {
  const directory = await workspace(t);
  const source = await writePng(path.join(directory, "source.png"), 2, 2, () => [240, 120, 20, 255]);
  const withText = insertChunkBeforeIend(await readFile(source), "tEXt", Buffer.from("author\0private"));
  await writeFile(source, withText);
  const output = path.join(directory, "clean.png");

  const result = run("sanitize-png.mjs", source, output);

  assert.equal(result.status, 0, result.stderr);
  assert.ok(withText.includes(Buffer.from("tEXt")));
  assert.ok(!(await readFile(output)).includes(Buffer.from("tEXt")));
});

test("bilinear resize is deterministic and preserves transparent RGBA output", async (t) => {
  const directory = await workspace(t);
  const source = await writePng(path.join(directory, "source.png"), 2, 2, (x, y) => [x * 200, y * 200, 40, x === 0 && y === 0 ? 0 : 255]);
  const first = path.join(directory, "first.png");
  const second = path.join(directory, "second.png");

  let result = run("resize-raster.mjs", source, first, "5", "3");
  assert.equal(result.status, 0, result.stderr);
  result = run("resize-raster.mjs", source, second, "5", "3");
  assert.equal(result.status, 0, result.stderr);

  assert.deepEqual(await readFile(first), await readFile(second));
  const resized = await decode(first);
  assert.equal(resized.width, 5);
  assert.equal(resized.height, 3);
  assert.ok([...resized.data].some((value, index) => index % 4 === 3 && value < 255));
});

test("builds a square chroma-key edit canvas with the seed in slot one", async (t) => {
  const directory = await workspace(t);
  const seed = await writePng(path.join(directory, "seed.png"), 4, 4, () => [240, 120, 20, 255]);
  const output = path.join(directory, "canvas.png");

  const result = run("build-sprite-edit-canvas.mjs", seed, output, "3", "4", "16");

  assert.equal(result.status, 0, result.stderr);
  const canvas = await decode(output);
  assert.equal(canvas.width, 16);
  assert.equal(canvas.height, 16);
  const blue = (x, y) => [...canvas.data.subarray((y * 16 + x) * 4, (y * 16 + x) * 4 + 4)];
  assert.deepEqual(blue(0, 0), [0, 0, 255, 255]);
  assert.deepEqual(blue(2, 7), [240, 120, 20, 255]);
  assert.deepEqual(blue(6, 7), [0, 0, 255, 255]);
});

test("edit canvas composites transparent and partial-alpha seed pixels over opaque blue", async (t) => {
  const directory = await workspace(t);
  const seed = await writePng(path.join(directory, "seed.png"), 2, 2, (x) =>
    x === 0 ? [240, 120, 20, 0] : [240, 120, 20, 128]);
  const output = path.join(directory, "canvas.png");

  const result = run("build-sprite-edit-canvas.mjs", seed, output, "1", "2", "4");

  assert.equal(result.status, 0, result.stderr);
  const canvas = await decode(output);
  const pixel = (x, y) => [...canvas.data.subarray((y * 4 + x) * 4, (y * 4 + x) * 4 + 4)];
  assert.deepEqual(pixel(1, 1), [0, 0, 255, 255]);
  assert.deepEqual(pixel(2, 1), [120, 60, 137, 255]);
});

test("normalizes strip frames with one shared scale and bottom-centre anchor", async (t) => {
  const directory = await workspace(t);
  const strip = await writePng(path.join(directory, "strip.png"), 12, 12, (x, y) => {
    const slotX = x % 4;
    const visible = y >= 4 && y <= 9 && slotX >= 1 && slotX <= (x < 4 ? 2 : 3);
    return visible ? [230, 130, 30, 255] : [0, 0, 0, 0];
  });
  const seed = await writePng(path.join(directory, "seed.png"), 4, 4, (x, y) =>
    x === 2 && y >= 1 ? [230, 130, 30, 255] : [0, 0, 0, 0]);
  const output = path.join(directory, "frames");

  const result = run("normalize-sprite-strip.mjs", strip, output, seed, "3", "4", "2", "3");

  assert.equal(result.status, 0, result.stderr);
  const frame1 = await readFile(path.join(output, "frame-01.png"));
  assert.deepEqual(frame1, await readFile(seed), "frame 01 must lock back byte-for-byte to seed");
  for (let index = 1; index <= 3; index += 1) {
    const frame = await decode(path.join(output, `frame-0${index}.png`));
    assert.equal(frame.width, 4);
    assert.equal(frame.height, 4);
    if (index > 1) {
      const opaque = [];
      for (let y = 0; y < 4; y += 1) for (let x = 0; x < 4; x += 1) {
        if (frame.data[(y * 4 + x) * 4 + 3] > 0) opaque.push([x, y]);
      }
      assert.ok(opaque.length > 0);
      assert.equal(Math.max(...opaque.map(([, y]) => y)), 2);
      const xs = opaque.map(([x]) => x);
      assert.ok((Math.min(...xs) + Math.max(...xs)) / 2 >= 1 && (Math.min(...xs) + Math.max(...xs)) / 2 <= 2);
    }
  }
});

async function idleFixture(t, overrides = {}) {
  const directory = await workspace(t);
  const framesDirectory = path.join(directory, "frames");
  await mkdir(framesDirectory);
  const seed = await writePng(path.join(directory, "seed.png"), 4, 4, (x, y) =>
    x === 2 && y > 0 ? [220, 120, 20, 255] : [0, 0, 0, 0]);
  const frames = [];
  for (let index = 1; index <= 3; index += 1) {
    const name = `frames/frame-0${index}.png`;
    if (index === 1) {
      await writeFile(path.join(directory, name), await readFile(seed));
    } else {
      await writePng(path.join(directory, name), 4, 4, (x, y) =>
        x === index - 1 && y > 0 ? [220, 120, 20, 255] : [0, 0, 0, 0]);
    }
    frames.push({ file: name, durationMs: index * 100 });
  }
  const manifest = {
    schemaVersion: 1,
    canvas: { width: 4, height: 4 },
    anchor: { x: 2, y: 3 },
    loop: true,
    seed: "seed.png",
    frames,
    ...overrides,
  };
  const manifestPath = path.join(directory, "idle-manifest.json");
  await writeFile(manifestPath, JSON.stringify(manifest));
  return { directory, manifest, manifestPath };
}

test("validates idle canvas, anchor, ordered frames, durations, and seed lock", async (t) => {
  const { manifestPath } = await idleFixture(t);
  const result = run("validate-raster.mjs", manifestPath);
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /PASS .*idle-manifest\.json frames=3 durationMs=600/);
});

for (const [name, mutate, expected] of [
  ["schema", (value) => { value.schemaVersion = 2; }, /schemaVersion must be 1/],
  ["numeric canvas", (value) => { value.canvas.width = "4"; }, /canvas width and height must be positive numbers/],
  ["numeric anchor", (value) => { value.anchor.x = "2"; }, /anchor x and y must be numbers/],
  ["boolean loop", (value) => { value.loop = "true"; }, /loop must be boolean/],
  ["positive duration", (value) => { value.frames[1].durationMs = 0; }, /frame 2 durationMs must be a positive number/],
  ["ordered files", (value) => { value.frames[1].file = "frames/frame-03.png"; }, /frame files must be ordered and unique/],
]) {
  test(`rejects invalid idle ${name}`, async (t) => {
    const fixture = await idleFixture(t);
    mutate(fixture.manifest);
    await writeFile(fixture.manifestPath, JSON.stringify(fixture.manifest));
    const result = run("validate-raster.mjs", fixture.manifestPath);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, expected);
  });
}

test("rejects idle frames with dimensions different from the canvas", async (t) => {
  const fixture = await idleFixture(t);
  await writePng(path.join(fixture.directory, "frames/frame-02.png"), 5, 4, () => [220, 120, 20, 255]);
  const result = run("validate-raster.mjs", fixture.manifestPath);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /frame 2 dimensions 5x4 do not match canvas 4x4/);
});

test("rejects an idle frame symlink that resolves outside the manifest directory", async (t) => {
  const fixture = await idleFixture(t);
  const outside = await workspace(t);
  const external = await writePng(path.join(outside, "external.png"), 4, 4, (x, y) =>
    x === 1 && y > 0 ? [220, 120, 20, 255] : [0, 0, 0, 0]);
  await rm(path.join(fixture.directory, "frames/frame-02.png"));
  await symlink(external, path.join(fixture.directory, "frames/frame-02.png"));

  const result = run("validate-raster.mjs", fixture.manifestPath);

  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /frame 2 file must resolve inside the manifest directory/);
});

test("rejects an idle seed symlink that resolves outside the manifest directory", async (t) => {
  const fixture = await idleFixture(t);
  const outside = await workspace(t);
  const external = await writePng(path.join(outside, "external.png"), 4, 4, (x, y) =>
    x === 2 && y > 0 ? [220, 120, 20, 255] : [0, 0, 0, 0]);
  await rm(path.join(fixture.directory, "seed.png"));
  await symlink(external, path.join(fixture.directory, "seed.png"));

  const result = run("validate-raster.mjs", fixture.manifestPath);

  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /seed must resolve inside the manifest directory/);
});

test("rejects an idle frame 01 that does not match the seed pixels", async (t) => {
  const fixture = await idleFixture(t);
  await writePng(path.join(fixture.directory, "frames/frame-01.png"), 4, 4, () => [1, 2, 3, 0]);
  const result = run("validate-raster.mjs", fixture.manifestPath);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /frame 01 pixels do not match seed/);
});
