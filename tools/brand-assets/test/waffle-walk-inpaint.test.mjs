import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { test } from "node:test";

import { PNG } from "pngjs";

import { pointInPolygon } from "../build-waffle-standing-rig.mjs";
import { evaluateClip, renderRigFrameSnapshot } from "../rig-motion.mjs";

const REPO_ROOT = path.resolve(import.meta.dirname, "../../..");
const RIG_DIRECTORY = path.join(REPO_ROOT, "assets/brand/waffle/rigs/standing-v2");
const RIG_PATH = path.join(RIG_DIRECTORY, "rig.json");
const CLIP_PATH = path.join(RIG_DIRECTORY, "motions/walk-in-place.json");
const REPAIRS_PATH = path.join(RIG_DIRECTORY, "repairs.json");
const VARIANTS_PATH = path.join(RIG_DIRECTORY, "variants.json");
const SOURCE_PATH = path.join(REPO_ROOT, "assets/brand/waffle/poses/standing.png");
const SOLID_ALPHA = 250;
const CONTOUR_MEAN_LIMIT = 20;
const CONTOUR_P95_LIMIT = 60;
const CONTOUR_MAX_LIMIT = 100;
const SIDES = [{
  setId: "front-chain-left",
  anchor: "front-upper-left",
  underlay: "walk-socket-front-left",
  cover: "walk-cover-front-left",
  activeFrames: [1, 23],
  underlayBounds: { x: 450, y: 510, width: 340, height: 140 },
  coverBounds: { x: 598, y: 575, width: 49, height: 106 },
  coverBoundaryClearance: 3,
  maximumUnderlayArea: 44_000,
  maximumCoverArea: 4_500,
  minimumMemberOverlap: 450,
}, {
  setId: "front-chain-right",
  anchor: "front-upper-right",
  underlay: "walk-socket-front-right",
  cover: "walk-cover-front-right",
  activeFrames: [25, 46],
  underlayBounds: { x: 450, y: 510, width: 340, height: 140 },
  coverBounds: { x: 607, y: 580, width: 46, height: 101 },
  coverBoundaryClearance: 3,
  maximumUnderlayArea: 44_000,
  maximumCoverArea: 4_000,
  minimumMemberOverlap: 300,
}, {
  setId: "rear-chain-left",
  anchor: "rear-thigh-left",
  underlay: "walk-socket-rear-left",
  activeFrames: [25, 46],
  underlayBounds: { x: 735, y: 545, width: 210, height: 245 },
  underlayBoundaryClearance: 10,
  maximumUnderlayArea: 45_000,
}, {
  setId: "rear-chain-right",
  anchor: "rear-thigh-right",
  underlay: "walk-socket-rear-right",
  activeFrames: [1, 23],
  underlayBounds: { x: 900, y: 535, width: 220, height: 255 },
  underlayBoundaryClearance: 10,
  maximumUnderlayArea: 50_000,
}];
const PEAK_JOINT_COVERS = new Map([
  [12, ["walk-cover-front-left"]],
  [36, ["walk-cover-front-right"]],
]);
let productionSnapshotPromise;

function cloneRgba(image) {
  return PNG.sync.read(PNG.sync.write(image));
}

function rgbaEquals(left, right, offset) {
  return left[offset] === right[offset]
    && left[offset + 1] === right[offset + 1]
    && left[offset + 2] === right[offset + 2]
    && left[offset + 3] === right[offset + 3];
}

function solidComponents(image) {
  const visited = new Uint8Array(image.width * image.height);
  const components = [];
  for (let start = 0; start < visited.length; start += 1) {
    if (visited[start] || image.data[start * 4 + 3] < SOLID_ALPHA) continue;
    let size = 0;
    const pending = [start];
    visited[start] = 1;
    while (pending.length > 0) {
      const pixel = pending.pop();
      size += 1;
      const x = pixel % image.width;
      const y = Math.floor(pixel / image.width);
      for (let dy = -1; dy <= 1; dy += 1) {
        for (let dx = -1; dx <= 1; dx += 1) {
          if (dx === 0 && dy === 0) continue;
          const nextX = x + dx;
          const nextY = y + dy;
          if (nextX < 0 || nextY < 0 || nextX >= image.width || nextY >= image.height) continue;
          const next = nextY * image.width + nextX;
          if (visited[next] || image.data[next * 4 + 3] < SOLID_ALPHA) continue;
          visited[next] = 1;
          pending.push(next);
        }
      }
    }
    components.push(size);
  }
  return components.toSorted((left, right) => right - left);
}

function distanceToTransparent(image, x, y, limit) {
  let nearest = Infinity;
  const radius = Math.ceil(limit);
  for (let sampleY = y - radius; sampleY <= y + radius; sampleY += 1) {
    for (let sampleX = x - radius; sampleX <= x + radius; sampleX += 1) {
      const distance = Math.hypot(sampleX - x, sampleY - y);
      if (distance >= nearest || distance > limit) continue;
      if (sampleX < 0 || sampleY < 0 || sampleX >= image.width || sampleY >= image.height
        || image.data[(sampleY * image.width + sampleX) * 4 + 3] === 0) {
        nearest = distance;
      }
    }
  }
  return nearest;
}

function assertSourceOwnedSupport(image, source, options) {
  const {
    boundaryReliefPolygons = [],
    bounds,
    id,
    maximumArea,
    minimumArea,
    minimumBoundaryClearance = 0,
  } = options;
  let nonzeroPixels = 0;
  for (let y = 0; y < image.height; y += 1) {
    for (let x = 0; x < image.width; x += 1) {
      const offset = (y * image.width + x) * 4;
      const alpha = image.data[offset + 3];
      if (alpha === 0) continue;
      nonzeroPixels += 1;
      assert.ok(
        x >= bounds.x
          && x < bounds.x + bounds.width
          && y >= bounds.y
          && y < bounds.y + bounds.height,
        `${id} has same-side support outside its proximal envelope at ${x},${y}`,
      );
      assert.ok(
        alpha <= source.data[offset + 3],
        `${id} alpha exceeds the approved source at ${x},${y}`,
      );
      assert.deepEqual(
        image.data.subarray(offset, offset + 3),
        source.data.subarray(offset, offset + 3),
        `${id} source-owned RGB drift at ${x},${y}`,
      );
      if (minimumBoundaryClearance > 0) {
        assert.ok(
          boundaryReliefPolygons.some((polygon) => pointInPolygon(x, y, polygon))
            || distanceToTransparent(source, x, y, minimumBoundaryClearance) >= minimumBoundaryClearance,
          `${id} reaches the full-source boundary at ${x},${y}; expected ${minimumBoundaryClearance}px clearance`,
        );
      }
    }
  }
  assert.ok(
    nonzeroPixels >= minimumArea,
    `${id} has no useful same-side proximal support (${nonzeroPixels} < ${minimumArea})`,
  );
  assert.ok(
    nonzeroPixels <= maximumArea,
    `${id} support area ${nonzeroPixels} exceeds ${maximumArea}`,
  );
  const components = solidComponents(image);
  assert.equal(
    components.length,
    1,
    `${id} solid support must form one 8-connected component; got ${components.join(", ")}`,
  );
}

function isolateOpaquePixel(image) {
  const mutated = cloneRgba(image);
  for (let y = 2; y < image.height - 2; y += 1) {
    for (let x = 2; x < image.width - 2; x += 1) {
      let surrounded = true;
      for (let dy = -2; dy <= 2 && surrounded; dy += 1) {
        for (let dx = -2; dx <= 2; dx += 1) {
          if (image.data[((y + dy) * image.width + x + dx) * 4 + 3] < SOLID_ALPHA) {
            surrounded = false;
            break;
          }
        }
      }
      if (!surrounded) continue;
      for (let dy = -1; dy <= 1; dy += 1) {
        for (let dx = -1; dx <= 1; dx += 1) {
          if (dx === 0 && dy === 0) continue;
          const offset = ((y + dy) * image.width + x + dx) * 4;
          mutated.data.fill(0, offset, offset + 4);
        }
      }
      return mutated;
    }
  }
  throw new Error("fixture has no opaque interior pixel for component mutation");
}

function variantMatrix(manifest, anchor, transform) {
  const pivotX = anchor.pivot.x * (manifest.canvas.width - 1);
  const pivotY = anchor.pivot.y * (manifest.canvas.height - 1);
  const radians = (transform.rotationDegrees * Math.PI) / 180;
  const cosine = Math.cos(radians);
  const sine = Math.sin(radians);
  const a = cosine * transform.scaleX;
  const b = sine * transform.scaleX;
  const c = -sine * transform.scaleY;
  const d = cosine * transform.scaleY;
  return [
    a,
    b,
    c,
    d,
    pivotX - a * pivotX - c * pivotY + transform.x * manifest.canvas.width,
    pivotY - b * pivotX - d * pivotY + transform.y * manifest.canvas.height,
  ];
}

function inversePoint(matrix, x, y) {
  const [a, b, c, d, e, f] = matrix;
  const determinant = a * d - b * c;
  assert.notEqual(determinant, 0, "variant overlap transform must be invertible");
  const translatedX = x - e;
  const translatedY = y - f;
  return [
    (d * translatedX - c * translatedY) / determinant,
    (-b * translatedX + a * translatedY) / determinant,
  ];
}

function bilinearAlpha(image, x, y) {
  if (x < -0.5 || y < -0.5 || x > image.width - 0.5 || y > image.height - 0.5) return 0;
  const clampedX = Math.max(0, Math.min(image.width - 1, x));
  const clampedY = Math.max(0, Math.min(image.height - 1, y));
  const x0 = Math.floor(clampedX);
  const y0 = Math.floor(clampedY);
  const x1 = Math.min(image.width - 1, x0 + 1);
  const y1 = Math.min(image.height - 1, y0 + 1);
  const fractionX = clampedX - x0;
  const fractionY = clampedY - y0;
  return image.data[(y0 * image.width + x0) * 4 + 3] * (1 - fractionX) * (1 - fractionY)
    + image.data[(y0 * image.width + x1) * 4 + 3] * fractionX * (1 - fractionY)
    + image.data[(y1 * image.width + x0) * 4 + 3] * (1 - fractionX) * fractionY
    + image.data[(y1 * image.width + x1) * 4 + 3] * fractionX * fractionY;
}

function swingMember(evaluated, setId) {
  return evaluated.variants.get(setId);
}

function swingActive(evaluated, setId) {
  return Number(swingMember(evaluated, setId) !== "neutral");
}

function solidCoverMemberOverlap(snapshot, side, frame) {
  const evaluated = evaluateClip(snapshot.manifest, snapshot.clip, frame);
  const selectedMember = swingMember(evaluated, side.setId);
  assert.notEqual(selectedMember, "neutral", `${side.setId} frame ${frame} must select an active member`);
  const member = snapshot.manifest.variants[side.setId].members.find(({ id }) => id === selectedMember);
  const anchor = snapshot.manifest.layers.find(({ id }) => id === side.anchor);
  const coverLayer = snapshot.manifest.layers.find(({ id }) => id === side.cover);
  const memberRaster = snapshot.rasters.get(member.file);
  const coverRaster = snapshot.rasters.get(coverLayer.file);
  const transform = selectedMember === "low-lift"
    ? evaluated.variantTransforms.get(side.setId).transform
    : { x: 0, y: 0, rotationDegrees: 0, scaleX: 1, scaleY: 1 };
  const matrix = variantMatrix(snapshot.manifest, anchor, transform);
  let overlap = 0;
  for (let y = side.coverBounds.y; y < side.coverBounds.y + side.coverBounds.height; y += 1) {
    for (let x = side.coverBounds.x; x < side.coverBounds.x + side.coverBounds.width; x += 1) {
      if (coverRaster.data[(y * coverRaster.width + x) * 4 + 3] < SOLID_ALPHA) continue;
      const [sourceX, sourceY] = inversePoint(matrix, x, y);
      if (bilinearAlpha(memberRaster, sourceX, sourceY) >= SOLID_ALPHA) overlap += 1;
    }
  }
  return overlap;
}

function assertMinimumCoverMemberOverlap(snapshot, side, frame) {
  const overlap = solidCoverMemberOverlap(snapshot, side, frame);
  assert.ok(
    overlap >= side.minimumMemberOverlap,
    `${side.cover} overlaps ${side.setId}/low-lift by ${overlap} solid pixels at frame ${frame}; `
      + `expected at least ${side.minimumMemberOverlap}; a blank or undersized cover exposes the member boundary`,
  );
  return overlap;
}

async function productionSnapshot() {
  productionSnapshotPromise ??= (async () => {
    const [manifest, clip, repairs, variants, source] = await Promise.all([
      readFile(RIG_PATH, "utf8").then(JSON.parse),
      readFile(CLIP_PATH, "utf8").then(JSON.parse),
      readFile(REPAIRS_PATH, "utf8").then(JSON.parse),
      readFile(VARIANTS_PATH, "utf8").then(JSON.parse),
      readFile(SOURCE_PATH).then(PNG.sync.read),
    ]);
    const rasterFiles = new Set([
      ...manifest.layers.map(({ file }) => file),
      ...Object.values(manifest.variants).flatMap(({ members }) => members.map(({ file }) => file)),
    ]);
    const rasters = new Map(await Promise.all([...rasterFiles].map(async (file) => [
      file,
      PNG.sync.read(await readFile(path.join(RIG_DIRECTORY, file))),
    ])));
    return { clip, manifest, rasters, repairs, source, variants };
  })();
  return productionSnapshotPromise;
}

function alphaAt(image, x, y) {
  return image.data[(y * image.width + x) * 4 + 3];
}

function horizontalRgbJumpCount(image, { endX, startX, threshold, y }) {
  let jumps = 0;
  for (let x = startX; x <= endX; x += 1) {
    const above = ((y - 1) * image.width + x) * 4;
    const below = (y * image.width + x) * 4;
    const distance = Math.abs(image.data[above] - image.data[below])
      + Math.abs(image.data[above + 1] - image.data[below + 1])
      + Math.abs(image.data[above + 2] - image.data[below + 2]);
    jumps += Number(distance > threshold);
  }
  return jumps;
}

function polygonPerimeterSamples(polygon) {
  const samples = new Set();
  for (let index = 0; index < polygon.length; index += 1) {
    const [startX, startY] = polygon[index];
    const [endX, endY] = polygon[(index + 1) % polygon.length];
    const steps = Math.max(Math.abs(endX - startX), Math.abs(endY - startY));
    for (let step = 0; step <= steps; step += 1) {
      const progress = steps === 0 ? 0 : step / steps;
      samples.add(`${Math.round(startX + (endX - startX) * progress)},${Math.round(startY + (endY - startY) * progress)}`);
    }
  }
  return [...samples].map((sample) => sample.split(",").map(Number));
}

function alphaComponents(image, threshold = 0) {
  const active = new Set();
  for (let pixel = 0; pixel < image.width * image.height; pixel += 1) {
    if (image.data[pixel * 4 + 3] > threshold) active.add(pixel);
  }
  const components = [];
  while (active.size > 0) {
    const first = active.values().next().value;
    const pending = [first];
    active.delete(first);
    let size = 0;
    while (pending.length > 0) {
      const pixel = pending.pop();
      size += 1;
      const x = pixel % image.width;
      const neighbours = [pixel - image.width, pixel + image.width];
      if (x > 0) neighbours.push(pixel - 1);
      if (x + 1 < image.width) neighbours.push(pixel + 1);
      for (const neighbour of neighbours) {
        if (active.delete(neighbour)) pending.push(neighbour);
      }
    }
    components.push(size);
  }
  return components.toSorted((left, right) => right - left);
}

function replaceCapRasters(snapshot, capIds, mutate) {
  const rasters = new Map(snapshot.rasters);
  for (const id of capIds) {
    const layer = snapshot.manifest.layers.find((candidate) => candidate.id === id);
    assert.ok(layer, `missing cap layer ${id}`);
    rasters.set(layer.file, mutate(cloneRgba(snapshot.rasters.get(layer.file))));
  }
  return { ...snapshot, rasters };
}

function blankRgba(image) {
  image.data.fill(0);
  return image;
}

function neonRgba(image) {
  for (let offset = 0; offset < image.data.length; offset += 4) {
    if (image.data[offset + 3] === 0) continue;
    image.data[offset] = 255;
    image.data[offset + 1] = 0;
    image.data[offset + 2] = 255;
  }
  return image;
}

function percentile(sorted, fraction) {
  return sorted[Math.min(sorted.length - 1, Math.floor(sorted.length * fraction))];
}

async function internalContourMetric(snapshot, frame, capIds, referenceSnapshot = snapshot) {
  const [rendered, referenceRendered, withoutCaps] = await Promise.all([
    renderRigFrameSnapshot(snapshot, frame),
    renderRigFrameSnapshot(referenceSnapshot, frame),
    renderRigFrameSnapshot(replaceCapRasters(referenceSnapshot, capIds, blankRgba), frame),
  ]);
  const affected = new Uint8Array(rendered.width * rendered.height);
  let affectedPixels = 0;
  for (let pixel = 0; pixel < affected.length; pixel += 1) {
    const offset = pixel * 4;
    if (rgbaEquals(referenceRendered.data, withoutCaps.data, offset)) continue;
    affected[pixel] = 1;
    affectedPixels += 1;
  }

  const distances = [];
  for (let pixel = 0; pixel < affected.length; pixel += 1) {
    if (!affected[pixel]) continue;
    const x = pixel % rendered.width;
    const y = Math.floor(pixel / rendered.width);
    const offset = pixel * 4;
    if (rendered.data[offset + 3] < SOLID_ALPHA || snapshot.source.data[offset + 3] < SOLID_ALPHA) continue;
    for (const [dx, dy] of [[-1, 0], [1, 0], [0, -1], [0, 1]]) {
      const nextX = x + dx;
      const nextY = y + dy;
      if (nextX < 0 || nextY < 0 || nextX >= rendered.width || nextY >= rendered.height) continue;
      const next = nextY * rendered.width + nextX;
      const nextOffset = next * 4;
      if (affected[next]
        || rendered.data[nextOffset + 3] < SOLID_ALPHA
        || withoutCaps.data[nextOffset + 3] < SOLID_ALPHA
        || snapshot.source.data[nextOffset + 3] < SOLID_ALPHA) continue;
      distances.push(Math.hypot(
        rendered.data[offset] - rendered.data[nextOffset],
        rendered.data[offset + 1] - rendered.data[nextOffset + 1],
        rendered.data[offset + 2] - rendered.data[nextOffset + 2],
      ));
    }
  }
  distances.sort((left, right) => left - right);
  assert.ok(affectedPixels >= 100, `frame ${frame} cap contribution is not measurable (${affectedPixels} pixels)`);
  assert.ok(distances.length >= 40, `frame ${frame} internal cap contour is not measurable (${distances.length} samples)`);
  return {
    affectedPixels,
    max: distances.at(-1),
    mean: distances.reduce((total, value) => total + value, 0) / distances.length,
    p95: percentile(distances, 0.95),
    samples: distances.length,
  };
}

async function assertContinuousInternalContours(snapshot, frame, capIds, referenceSnapshot = snapshot) {
  const metric = await internalContourMetric(snapshot, frame, capIds, referenceSnapshot);
  assert.ok(
    metric.mean <= CONTOUR_MEAN_LIMIT
      && metric.p95 <= CONTOUR_P95_LIMIT
      && metric.max <= CONTOUR_MAX_LIMIT,
    `frame ${frame} has a hard chromatic contour at the joint-cover boundary: `
      + `mean ${metric.mean.toFixed(2)} (max ${CONTOUR_MEAN_LIMIT}), `
      + `p95 ${metric.p95.toFixed(2)} (max ${CONTOUR_P95_LIMIT}), `
      + `outlier ${metric.max.toFixed(2)} (max ${CONTOUR_MAX_LIMIT}), `
      + `${metric.samples} samples`,
  );
  return metric;
}

test("every side has a bounded source-owned underlay and every configured front joint has an interior cover", async () => {
  const snapshot = await productionSnapshot();
  for (const side of SIDES) {
    const underlayLayer = snapshot.manifest.layers.find(({ id }) => id === side.underlay);
    const underlaySpecification = snapshot.repairs.repairs.find(({ id }) => id === side.underlay);
    assert.ok(underlayLayer && underlaySpecification, `missing proximal underlay for ${side.setId}`);
    assert.deepEqual(underlaySpecification.input.crop, side.underlayBounds, `${side.underlay} authored bounds drift`);
    assertSourceOwnedSupport(snapshot.rasters.get(underlayLayer.file), snapshot.source, {
      boundaryReliefPolygons: underlaySpecification.input.boundaryReliefPolygons,
      bounds: side.underlayBounds,
      id: side.underlay,
      maximumArea: side.maximumUnderlayArea,
      minimumArea: underlaySpecification.input.outputBounds.minNonzeroPixels,
      minimumBoundaryClearance: side.underlayBoundaryClearance ?? 0,
    });
    if (side.cover) {
      const coverLayer = snapshot.manifest.layers.find(({ id }) => id === side.cover);
      const coverSpecification = snapshot.repairs.repairs.find(({ id }) => id === side.cover);
      assert.ok(coverLayer && coverSpecification, `missing interior cover for ${side.setId}`);
      assert.deepEqual(coverSpecification.input.crop, side.coverBounds, `${side.cover} authored bounds drift`);
      assert.equal(
        coverSpecification.input.internalOnlyDistancePixels,
        side.coverBoundaryClearance,
        `${side.cover} full-source clearance contract`,
      );
      assertSourceOwnedSupport(snapshot.rasters.get(coverLayer.file), snapshot.source, {
        bounds: side.coverBounds,
        id: side.cover,
        maximumArea: side.maximumCoverArea,
        minimumArea: 100,
        minimumBoundaryClearance: side.coverBoundaryClearance,
      });
    }
  }
});

test("joint supports fade to transparent at every authored perimeter instead of revealing crop rectangles", async () => {
  const snapshot = await productionSnapshot();
  for (const side of SIDES) {
    for (const id of [side.underlay, side.cover].filter(Boolean)) {
      const layer = snapshot.manifest.layers.find((candidate) => candidate.id === id);
      const specification = snapshot.repairs.repairs.find((candidate) => candidate.id === id);
      const image = snapshot.rasters.get(layer.file);
      for (const polygon of specification.polygons) {
        const visiblePerimeter = polygonPerimeterSamples(polygon)
          .filter(([x, y]) => x >= 0 && y >= 0 && x < image.width && y < image.height)
          .filter(([x, y]) => alphaAt(image, x, y) > 0);
        assert.equal(
          visiblePerimeter.length,
          0,
          `${id} exposes ${visiblePerimeter.length} pixels on its authored crop perimeter`,
        );
      }
    }
  }
});

test("every coherent low-lift painting is one connected alpha component", async () => {
  const snapshot = await productionSnapshot();
  for (const set of snapshot.variants.sets.filter(({ id }) => id.endsWith("chain-left") || id.endsWith("chain-right"))) {
    const member = set.members.find(({ id }) => id === "low-lift");
    if (!member) continue;
    const manifestMember = snapshot.manifest.variants[set.id].members.find(({ id }) => id === "low-lift");
    const components = alphaComponents(snapshot.rasters.get(manifestMember.file));
    assert.equal(
      components.length,
      1,
      `${set.id}/low-lift has disconnected alpha artifacts: ${components.join(", ")}`,
    );
  }
});

test("every landing painting has one coherent component and exact neutral distal pixels below its concealed seam", async () => {
  const snapshot = await productionSnapshot();
  for (const side of SIDES) {
    const set = snapshot.variants.sets.find(({ id }) => id === side.setId);
    const specification = set.members.find(({ id }) => id === "landing");
    const member = snapshot.manifest.variants[side.setId].members.find(({ id }) => id === "landing");
    assert.ok(specification && member, `${side.setId} must declare a landing member`);
    const image = snapshot.rasters.get(member.file);
    assert.equal(
      alphaComponents(image, 16).length,
      1,
      `${side.setId}/landing must have one coherent alpha component above the source antialias noise floor`,
    );
    const supportBottom = side.cover
      ? side.coverBounds.y + side.coverBounds.height
      : side.underlayBounds.y + side.underlayBounds.height;
    assert.equal(specification.input.seamY, supportBottom, `${side.setId} landing seam must finish beneath its fixed support`);
    assert.ok(
      specification.input.transitionStartY < specification.input.seamY,
      `${side.setId} landing transition must have a concealed overlap band`,
    );
    let distalMismatches = 0;
    let firstMismatch;
    for (let y = specification.input.seamY; y < image.height; y += 1) {
      for (let x = 0; x < image.width; x += 1) {
        const offset = (y * image.width + x) * 4;
        const sourceOwned = specification.input.neutralDistalPolygons.some((polygon) => pointInPolygon(x, y, polygon));
        const expected = sourceOwned ? snapshot.source : null;
        const matches = expected
          ? (image.data[offset + 3] === 0 && expected.data[offset + 3] === 0)
            || rgbaEquals(image.data, expected.data, offset)
          : image.data[offset] === 0 && image.data[offset + 1] === 0
            && image.data[offset + 2] === 0 && image.data[offset + 3] === 0;
        if (!matches) {
          distalMismatches += 1;
          firstMismatch ??= { x, y };
        }
      }
    }
    assert.equal(
      distalMismatches,
      0,
      `${side.setId}/landing has ${distalMismatches} neutral distal mismatches; first at ${firstMismatch?.x},${firstMismatch?.y}`,
    );
  }
});

test("front-right rendered landing handoffs have no rectangular shoulder or belly seam", async () => {
  const snapshot = await productionSnapshot();
  const guards = [
    { y: 640, startX: 630, endX: 805, threshold: 60, maximumJumps: 20, label: "shoulder transition" },
    { y: 681, startX: 630, endX: 805, threshold: 60, maximumJumps: 20, label: "belly seam" },
  ];
  for (const frame of [25, 46]) {
    const rendered = await renderRigFrameSnapshot(snapshot, frame);
    for (const guard of guards) {
      const jumps = horizontalRgbJumpCount(rendered, guard);
      assert.ok(
        jumps <= guard.maximumJumps,
        `frame ${frame} ${guard.label} has ${jumps} hard RGB jumps across its native rendered handoff; expected at most ${guard.maximumJumps}`,
      );
    }
  }
});

test("rear low-lift paintings have no solid neutral underbelly panel above the leg root", async () => {
  const snapshot = await productionSnapshot();
  for (const setId of ["rear-chain-left", "rear-chain-right"]) {
    const member = snapshot.manifest.variants[setId].members.find(({ id }) => id === "low-lift");
    const image = snapshot.rasters.get(member.file);
    let forbiddenPixels = 0;
    for (let y = 0; y < 710; y += 1) {
      for (let x = 700; x < 1140; x += 1) {
        forbiddenPixels += Number(alphaAt(image, x, y) >= SOLID_ALPHA);
      }
    }
    assert.equal(
      forbiddenPixels,
      0,
      `${setId}/low-lift carries ${forbiddenPixels} solid pixels above the authored rear-leg root`,
    );
  }
});

test("rear low-lift proximal feathers stay inside their stationary socket support", async () => {
  const snapshot = await productionSnapshot();
  const cases = [{
    setId: "rear-chain-right",
    socketId: "walk-socket-rear-right",
    shift: { x: -9, y: 13 },
    region: { x: 880, y: 700, width: 125, height: 72 },
  }, {
    setId: "rear-chain-left",
    socketId: "walk-socket-rear-left",
    shift: { x: 0, y: 0 },
    region: { x: 880, y: 690, width: 50, height: 69 },
  }];

  for (const { setId, socketId, shift, region } of cases) {
    const member = snapshot.manifest.variants[setId].members.find(({ id }) => id === "low-lift");
    const socket = snapshot.manifest.layers.find(({ id }) => id === socketId);
    const memberRaster = snapshot.rasters.get(member.file);
    const socketRaster = snapshot.rasters.get(socket.file);
    let unsupportedFeatherPixels = 0;
    for (let y = region.y; y < region.y + region.height; y += 1) {
      for (let x = region.x; x < region.x + region.width; x += 1) {
        const memberX = x - shift.x;
        const memberY = y - shift.y;
        const memberAlpha = alphaAt(memberRaster, memberX, memberY);
        if (memberAlpha > 8 && memberAlpha < 240 && alphaAt(socketRaster, x, y) === 0) {
          unsupportedFeatherPixels += 1;
        }
      }
    }
    assert.ok(
      unsupportedFeatherPixels <= 8,
      `${setId}/low-lift exposes ${unsupportedFeatherPixels} unsupported proximal feather pixels`,
    );
  }
});

test("rear-left socket preserves the approved solid hip contour vacated by the lifted member", async () => {
  const snapshot = await productionSnapshot();
  const member = snapshot.manifest.variants["rear-chain-left"].members.find(({ id }) => id === "low-lift");
  const socket = snapshot.manifest.layers.find(({ id }) => id === "walk-socket-rear-left");
  const memberRaster = snapshot.rasters.get(member.file);
  const socketRaster = snapshot.rasters.get(socket.file);
  let missingPixels = 0;
  for (let y = 705; y < 715; y += 1) {
    for (let x = 883; x < 891; x += 1) {
      if (alphaAt(snapshot.source, x, y) < SOLID_ALPHA) continue;
      if (Math.max(alphaAt(memberRaster, x, y), alphaAt(socketRaster, x, y)) < SOLID_ALPHA) {
        missingPixels += 1;
      }
    }
  }
  assert.equal(missingPixels, 0, `rear-chain-left opens a ${missingPixels}-pixel bite in the approved hip contour`);
});

test("each underlay, member, and interior cover has correct torso-space order and phase-linked opacity", async () => {
  const snapshot = await productionSnapshot();
  for (const side of SIDES) {
    const underlay = snapshot.manifest.layers.find(({ id }) => id === side.underlay);
    const anchor = snapshot.manifest.layers.find(({ id }) => id === side.anchor);
    const member = snapshot.manifest.variants[side.setId].members.find(({ id }) => id === "low-lift");
    assert.ok(underlay && anchor && member, `missing ordering input for ${side.setId}`);
    assert.equal(underlay.parent, "torso", `${side.underlay} must share torso space`);
    assert.equal(member.parentOverride, "torso", `${side.setId}/low-lift must share torso space`);
    assert.ok(underlay.drawOrder < anchor.drawOrder, `${side.underlay} must render below ${side.anchor}`);
    const cover = side.cover ? snapshot.manifest.layers.find(({ id }) => id === side.cover) : null;
    if (cover) {
      assert.equal(cover.parent, "torso", `${side.cover} must share torso space`);
      assert.ok(anchor.drawOrder < cover.drawOrder, `${side.cover} must render above ${side.anchor}`);
    }
    for (let frame = 0; frame < snapshot.clip.frameCount; frame += 1) {
      const evaluated = evaluateClip(snapshot.manifest, snapshot.clip, frame);
      const active = swingActive(evaluated, side.setId);
      assert.equal(evaluated.layers.get(side.underlay).opacity, active, `${side.underlay} phase mismatch at frame ${frame}`);
      if (side.cover) assert.equal(evaluated.layers.get(side.cover).opacity, active, `${side.cover} phase mismatch at frame ${frame}`);
    }
  }
});

test("every active frame keeps a solid interior-cover overlap with its moving member", async () => {
  const snapshot = await productionSnapshot();
  for (const side of SIDES.filter(({ cover }) => cover)) {
    for (let frame = side.activeFrames[0]; frame <= side.activeFrames[1]; frame += 1) {
      assertMinimumCoverMemberOverlap(snapshot, side, frame);
    }
  }
});

test("support acceptance rejects source colour drift and a disconnected opaque island", async () => {
  const snapshot = await productionSnapshot();
  const side = SIDES.find(({ setId }) => setId === "front-chain-right");
  const layer = snapshot.manifest.layers.find(({ id }) => id === side.cover);
  const image = snapshot.rasters.get(layer.file);
  const options = {
    bounds: side.coverBounds,
    id: side.cover,
    maximumArea: side.maximumCoverArea,
    minimumArea: 100,
    minimumBoundaryClearance: 0,
  };

  const colourDrift = cloneRgba(image);
  const opaqueAlphaOffset = colourDrift.data.findIndex((value, offset) => offset % 4 === 3 && value === 255);
  colourDrift.data[opaqueAlphaOffset - 3] = (colourDrift.data[opaqueAlphaOffset - 3] + 1) % 256;
  assert.throws(
    () => assertSourceOwnedSupport(colourDrift, snapshot.source, options),
    /source-owned RGB drift/,
  );
  assert.throws(
    () => assertSourceOwnedSupport(isolateOpaquePixel(image), snapshot.source, options),
    /solid support must form one 8-connected component/,
  );
});

test("visible joint-cover contours stay chromatically continuous at both diagonal peaks", async () => {
  const snapshot = await productionSnapshot();
  for (const [frame, capIds] of PEAK_JOINT_COVERS) {
    await assertContinuousInternalContours(snapshot, frame, capIds);
  }
});

test("internal-contour acceptance rejects a neon seam injected into the active caps", async () => {
  const snapshot = await productionSnapshot();
  for (const [frame, capIds] of PEAK_JOINT_COVERS) {
    const corrupted = replaceCapRasters(snapshot, capIds, neonRgba);
    await assert.rejects(
      () => assertContinuousInternalContours(corrupted, frame, capIds, snapshot),
      new RegExp(`frame ${frame} has a hard chromatic contour`),
    );
  }
});

test("member-boundary acceptance rejects blank active covers", async () => {
  const snapshot = await productionSnapshot();
  for (const [frame, coverIds] of PEAK_JOINT_COVERS) {
    for (const coverId of coverIds) {
      const side = SIDES.find(({ cover }) => cover === coverId);
      const blanked = replaceCapRasters(snapshot, [coverId], blankRgba);
      assert.throws(
        () => assertMinimumCoverMemberOverlap(blanked, side, frame),
        /a blank or undersized cover exposes the member boundary/,
      );
    }
  }
});
