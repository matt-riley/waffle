import { mkdir } from "node:fs/promises";
import path from "node:path";

import { renderSvg } from "./render-svg.mjs";

const outputDirectory = "assets/brand/waffle/exports/png";
const renders = [
  ["assets/brand/waffle/canon/model-sheet.svg", "model-sheet.png", 1600],
  [
    "assets/brand/waffle/canon/expression-sheet.svg",
    "expression-sheet.png",
    1600,
  ],
  ["assets/brand/waffle/poses/standing.svg", "standing.png", 800],
  ["assets/brand/waffle/poses/sitting.svg", "sitting.png", 800],
  ["assets/brand/waffle/poses/curled.svg", "curled.png", 800],
];

await mkdir(outputDirectory, { recursive: true });
for (const [inputPath, outputName, width] of renders) {
  const outputPath = path.join(outputDirectory, outputName);
  await renderSvg(inputPath, outputPath, width);
  console.log(`RENDER ${inputPath} -> ${outputPath} width=${width}`);
}
