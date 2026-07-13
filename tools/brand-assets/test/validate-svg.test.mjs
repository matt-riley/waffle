import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import pathModule from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import { validateSvg } from "../validate-svg.mjs";

const fixturePath = (name) =>
  fileURLToPath(new URL(`fixtures/${name}`, import.meta.url));
const validatorPath = fileURLToPath(
  new URL("../validate-svg.mjs", import.meta.url),
);
const svg = (
  body,
  attributes = 'xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"',
) =>
  `<svg ${attributes}>${body}</svg>`;

async function temporarySvg(t, markup, filename = "fixture.svg") {
  const directory = await mkdtemp(pathModule.join(tmpdir(), "waffle-svg-"));
  t.after(() => rm(directory, { force: true, recursive: true }));
  const path = pathModule.join(directory, filename);
  await writeFile(path, markup);
  return path;
}

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

const rejectionCases = [
  [
    "script elements",
    svg("<script>alert(1)</script>"),
    /script elements are forbidden/,
  ],
  [
    "inline event handlers",
    svg('<path onclick="alert(1)"/>'),
    /inline event handlers are forbidden: onclick/,
  ],
  [
    "external href values",
    svg('<use href="https://example.com/cat.svg#cat"/>'),
    /external asset URLs are forbidden: href/,
  ],
  [
    "external xlink:href values",
    svg(
      '<use xlink:href="https://example.com/cat.svg#cat"/>',
      'xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" viewBox="0 0 64 64"',
    ),
    /external asset URLs are forbidden: xlink:href/,
  ],
  [
    "external src values",
    svg('<g src="https://example.com/photo.png"/>'),
    /external asset URLs are forbidden: src/,
  ],
  [
    "external CSS url values",
    svg('<path style="fill:url(https://example.com/paint)"/>'),
    /external asset URLs are forbidden: style/,
  ],
  [
    "external paint-server values",
    svg('<path fill="https://example.com/paint"/>'),
    /external asset URLs are forbidden: fill/,
  ],
  [
    "namespace mismatch",
    svg("<path/>", 'xmlns="https://example.com/svg" viewBox="0 0 64 64"'),
    /root xmlns must be http:\/\/www\.w3\.org\/2000\/svg/,
  ],
  [
    "duplicate IDs",
    svg('<g id="same"><path id="same"/></g>'),
    /duplicate id: same/,
  ],
  [
    "missing required IDs",
    svg('<g id="silhouette"/>'),
    /required IDs are absent: face/,
  ],
  ["element limit", svg("<g/>".repeat(500)), /total elements exceed 500/],
  ["path limit", svg("<path/>".repeat(241)), /paths exceed 240/],
  [
    "foreignObject embedded content",
    svg('<foreignObject><img src="https://example.com/photo.png"/></foreignObject>'),
    /non-vector embedding elements are forbidden: foreignObject/,
  ],
  [
    "other non-vector embedding elements",
    svg('<feImage href="#paint"/>'),
    /non-vector embedding elements are forbidden: feImage/,
  ],
];

for (const [name, markup, expected] of rejectionCases) {
  test(`rejects ${name}`, async (t) => {
    const path = await temporarySvg(t, markup);
    const requirements =
      name === "missing required IDs" ? { requiredIds: ["face"] } : {};
    await assert.rejects(validateSvg(path, requirements), expected);
  });
}

for (const filename of [
  "model-sheet.svg",
  "expression-sheet.svg",
  "waffle-motion-master.svg",
  "standing.svg",
  "sitting.svg",
  "curled.svg",
]) {
  test(`enforces the ${filename} filename policy`, async (t) => {
    const path = await temporarySvg(t, svg("<g/>"), filename);
    const result = spawnSync(process.execPath, [validatorPath, path], {
      encoding: "utf8",
    });

    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /required IDs are absent:/);
  });
}

test("allows the exact SVG namespace and internal fragment references", async (t) => {
  const markup = svg(
    '<defs><linearGradient id="paint"/></defs>' +
      '<path fill="url(#paint)"/><use href="#paint"/>',
  );
  const path = await temporarySvg(t, markup);

  await validateSvg(path);
});
