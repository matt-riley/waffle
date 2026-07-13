import assert from "node:assert/strict";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import pathModule from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import { validateSvg } from "../validate-svg.mjs";

const fixturePath = (name) =>
  fileURLToPath(new URL(`fixtures/${name}`, import.meta.url));

test("validates an editable SVG with the required pose layers", async () => {
  const path = fixturePath("valid.svg");

  const result = await validateSvg(path, {
    requiredIds: ["silhouette", "face", "markings", "shading"],
  });

  assert.equal(result.path, path);
  assert.deepEqual(result.ids, ["silhouette", "face", "markings", "shading"]);
  assert.equal(result.pathCount, 1);
  assert.equal(result.elementCount, 6);
});

test("rejects embedded raster image elements", async () => {
  await assert.rejects(
    validateSvg(fixturePath("embedded-raster.svg")),
    /embedded raster\/image elements are forbidden/,
  );
});

test("rejects external paint-server attribute values", async (t) => {
  const directory = await mkdtemp(pathModule.join(tmpdir(), "waffle-svg-"));
  t.after(() => rm(directory, { force: true, recursive: true }));
  const path = pathModule.join(directory, "external-paint.svg");
  await writeFile(
    path,
    '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><path fill="https://example.com/paint"/></svg>',
  );

  await assert.rejects(
    validateSvg(path),
    /external asset URLs are forbidden: fill/,
  );
});
