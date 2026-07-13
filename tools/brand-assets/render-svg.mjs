import { readFile, writeFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";

import { Resvg } from "@resvg/resvg-js";

export async function renderSvg(inputPath, outputPath, width) {
  const svg = await readFile(inputPath, "utf8");
  const resvg = new Resvg(svg, {
    fitTo: { mode: "width", value: width },
  });
  const png = resvg.render().asPng();
  await writeFile(outputPath, png);
}

async function main([inputPath, outputPath, widthValue]) {
  if (!inputPath || !outputPath || !widthValue) {
    throw new Error(
      "usage: render-svg.mjs <input.svg> <output.png> <width>",
    );
  }

  const width = Number(widthValue);
  if (!Number.isInteger(width) || width <= 0) {
    throw new Error("width must be a positive integer");
  }
  await renderSvg(inputPath, outputPath, width);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
