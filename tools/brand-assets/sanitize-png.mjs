import { mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

import { PNG } from "pngjs";

export async function sanitizePng(input, output) {
  const decoded = PNG.sync.read(await readFile(input));
  const clean = new PNG({ width: decoded.width, height: decoded.height });
  clean.data = Buffer.from(decoded.data);
  await mkdir(path.dirname(output), { recursive: true });
  await writeFile(output, PNG.sync.write(clean));
  return { width: clean.width, height: clean.height };
}

async function main(args) {
  if (args.length !== 2) {
    throw new Error("usage: sanitize-png.mjs <input.png> <output.png>");
  }
  const result = await sanitizePng(args[0], args[1]);
  console.log(`WROTE ${args[1]} ${result.width}x${result.height}`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
