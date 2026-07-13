import assert from "node:assert/strict";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import { PNG } from "pngjs";

import { renderSvg } from "../render-svg.mjs";

const fixturePath = fileURLToPath(
  new URL("fixtures/valid.svg", import.meta.url),
);

test("renders a square SVG to a transparent 256 pixel PNG", async (t) => {
  const directory = await mkdtemp(path.join(tmpdir(), "waffle-render-"));
  t.after(() => rm(directory, { force: true, recursive: true }));
  const outputPath = path.join(directory, "valid.png");

  await renderSvg(fixturePath, outputPath, 256);

  const png = PNG.sync.read(await readFile(outputPath));
  assert.equal(png.width, 256);
  assert.equal(png.height, 256);
  assert.equal(png.data[3], 0);
});
