import { readFile } from "node:fs/promises";
import pathModule from "node:path";
import { pathToFileURL } from "node:url";

import { XMLParser, XMLValidator } from "fast-xml-parser";

const SVG_NAMESPACE = "http://www.w3.org/2000/svg";
const MAX_ELEMENTS = 500;
const MAX_PATHS = 240;
const POSE_IDS = ["silhouette", "face", "markings", "shading"];
const PAINT_SERVER_ATTRIBUTES = new Set([
  "fill",
  "stroke",
  "filter",
  "clip-path",
  "mask",
  "marker",
  "marker-start",
  "marker-mid",
  "marker-end",
]);

const MODEL_VIEWS = ["front", "three-quarter", "profile", "rear-top"];
const MODEL_IDS = MODEL_VIEWS.flatMap((view) => [
  `view-${view}`,
  ...POSE_IDS.map((layer) => `${view}-${layer}`),
]);
const EXPRESSION_IDS = [
  "neutral",
  "curious",
  "pleased",
  "focused",
  "startled",
  "sleepy",
];
const MOTION_MASTER_IDS = [
  "tail",
  "torso",
  "rear-leg-left",
  "rear-leg-right",
  "front-leg-left",
  "front-leg-right",
  "head",
  "ear-left",
  "ear-right",
  "eye-left",
  "eye-right",
  "eyelids",
  "muzzle",
  "mouth",
  "whiskers",
  "markings",
  "shading",
];

const FILE_REQUIREMENTS = new Map([
  ["model-sheet.svg", MODEL_IDS],
  ["expression-sheet.svg", EXPRESSION_IDS],
  ["waffle-motion-master.svg", MOTION_MASTER_IDS],
  ["standing.svg", POSE_IDS],
  ["sitting.svg", POSE_IDS],
  ["curled.svg", POSE_IDS],
]);

const parser = new XMLParser({
  attributeNamePrefix: "",
  ignoreAttributes: false,
  preserveOrder: true,
});

const isMetadataKey = (key) =>
  key === ":@" || key.startsWith("#") || key.startsWith("?");

function elementEntry(node) {
  return Object.entries(node).find(([key]) => !isMetadataKey(key));
}

function isExternalReference(value) {
  return value !== "" && !value.startsWith("#");
}

function validateReferences(attribute, value) {
  const lowerAttribute = attribute.toLowerCase();
  const stringValue = String(value).trim();
  const lowerValue = stringValue.toLowerCase();

  if (lowerValue.includes("data:image/")) {
    throw new Error("embedded raster/image elements are forbidden");
  }
  if (lowerValue.includes("javascript:")) {
    throw new Error("javascript references are forbidden");
  }
  if (lowerAttribute.startsWith("on")) {
    throw new Error(`inline event handlers are forbidden: ${attribute}`);
  }
  if (
    (lowerAttribute === "href" || lowerAttribute === "xlink:href") &&
    isExternalReference(stringValue)
  ) {
    throw new Error(`external asset URLs are forbidden: ${attribute}`);
  }
  if (
    PAINT_SERVER_ATTRIBUTES.has(lowerAttribute) &&
    (/^[a-z][a-z\d+.-]*:/iu.test(stringValue) || stringValue.startsWith("//"))
  ) {
    throw new Error(`external asset URLs are forbidden: ${attribute}`);
  }

  for (const match of stringValue.matchAll(/url\(\s*(['"]?)(.*?)\1\s*\)/giu)) {
    if (isExternalReference(match[2].trim())) {
      throw new Error(`external asset URLs are forbidden: ${attribute}`);
    }
  }
}

function inspectTree(nodes) {
  let elementCount = 0;
  let pathCount = 0;
  const ids = [];
  const seenIds = new Set();

  function visit(items) {
    for (const node of items) {
      const entry = elementEntry(node);
      if (!entry) {
        const text = node["#text"];
        if (typeof text === "string") {
          validateReferences("text", text);
        }
        continue;
      }

      const [tagName, children] = entry;
      const localName = tagName.split(":").at(-1).toLowerCase();
      elementCount += 1;
      if (localName === "path") pathCount += 1;
      if (localName === "image") {
        throw new Error("embedded raster/image elements are forbidden");
      }
      if (localName === "script") {
        throw new Error("script elements are forbidden");
      }

      const attributes = node[":@"] ?? {};
      for (const [attribute, value] of Object.entries(attributes)) {
        if (attribute === "id") {
          if (seenIds.has(value)) throw new Error(`duplicate id: ${value}`);
          seenIds.add(value);
          ids.push(value);
        }
        validateReferences(attribute, value);
      }

      if (Array.isArray(children)) visit(children);
    }
  }

  visit(nodes);
  return { elementCount, pathCount, ids };
}

export async function validateSvg(path, requirements = {}) {
  const markup = await readFile(path, "utf8");
  const validation = XMLValidator.validate(markup);
  if (validation !== true) {
    throw new Error(`invalid SVG XML: ${validation.err.msg}`);
  }

  const document = parser.parse(markup);
  const rootNode = document.find((node) => elementEntry(node));
  const rootEntry = rootNode && elementEntry(rootNode);
  if (!rootEntry || rootEntry[0] !== "svg") {
    throw new Error("root element must be svg");
  }

  const rootAttributes = rootNode[":@"] ?? {};
  if (rootAttributes.xmlns !== SVG_NAMESPACE) {
    throw new Error(`root xmlns must be ${SVG_NAMESPACE}`);
  }
  if (!rootAttributes.viewBox) throw new Error("viewBox is required");

  const result = inspectTree([rootNode]);
  if (result.elementCount > MAX_ELEMENTS) {
    throw new Error(`total elements exceed ${MAX_ELEMENTS}`);
  }
  if (result.pathCount > MAX_PATHS) {
    throw new Error(`paths exceed ${MAX_PATHS}`);
  }

  const requiredIds = requirements.requiredIds ?? [];
  const missingIds = requiredIds.filter((id) => !result.ids.includes(id));
  if (missingIds.length > 0) {
    throw new Error(`required IDs are absent: ${missingIds.join(", ")}`);
  }

  return { path, ...result };
}

async function main(paths) {
  if (paths.length === 0) {
    throw new Error("usage: validate-svg.mjs <paths...>");
  }

  for (const path of paths) {
    const requiredIds = FILE_REQUIREMENTS.get(pathModule.basename(path)) ?? [];
    try {
      const result = await validateSvg(path, { requiredIds });
      console.log(
        `PASS ${path} elements=${result.elementCount} paths=${result.pathCount}`,
      );
    } catch (error) {
      console.error(`FAIL ${path}: ${error.message}`);
      process.exitCode = 1;
      return;
    }
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
