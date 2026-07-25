import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = await readFile(
  new URL("./assets/posture.js", import.meta.url),
  "utf8",
);

class FakeElement {
  constructor(tag = "div") {
    this.tagName = tag.toUpperCase();
    this.childNodes = [];
    this.dataset = {};
    this.listeners = {};
    this.attributes = {};
    this.hidden = false;
    this.disabled = false;
    this.open = false;
    this.className = "";
    this.type = "";
    this._textContent = "";
    this.focused = false;
  }

  get textContent() {
    return this._textContent;
  }

  set textContent(value) {
    this._textContent = String(value);
    this.childNodes = [];
  }

  append(...nodes) {
    this.childNodes.push(...nodes);
  }

  appendChild(node) {
    this.childNodes.push(node);
    return node;
  }

  replaceChildren(...nodes) {
    this.childNodes = [...nodes];
  }

  setAttribute(name, value) {
    this.attributes[name] = String(value);
  }

  addEventListener(name, listener) {
    this.listeners[name] = listener;
  }

  showModal() {
    this.open = true;
  }

  close() {
    this.open = false;
  }

  focus() {
    this.focused = true;
  }

  // The delegated handler calls closest() on the click target.
  closest(selector) {
    if (selector === "[data-posture-open]" && "postureOpen" in this.dataset) {
      return this;
    }
    return null;
  }
}

function response(body, status = 200) {
  return {
    ok: status >= 200 && status < 300,
    status,
    async json() {
      return body;
    },
  };
}

function flatten(node) {
  if (!node) return "";
  if (node.childNodes.length === 0) return node.textContent;
  return node.childNodes.map(flatten).join(" ");
}

function createHarness(routes) {
  const selectors = [
    "#desk-posture-dialog",
    "#desk-posture-title",
    "#desk-posture-status",
    "#desk-posture-body",
    "#desk-posture-close",
  ];
  const elements = Object.fromEntries(
    selectors.map((selector) => [selector, new FakeElement()]),
  );
  const requests = [];
  let documentClick = null;
  const context = {
    console,
    document: {
      createElement: (tag) => new FakeElement(tag),
      querySelector: (selector) => elements[selector] || null,
      addEventListener: (name, listener) => {
        if (name === "click") documentClick = listener;
      },
    },
    fetch: async (path, options = {}) => {
      requests.push({ path, options });
      for (const [pattern, handler] of Object.entries(routes)) {
        if (path.includes(pattern)) return handler(path);
      }
      throw new Error(`unexpected fetch ${path}`);
    },
    setTimeout,
    clearTimeout,
    URL,
    URLSearchParams,
  };
  vm.runInNewContext(source, context, { filename: "posture.js" });
  return {
    elements,
    requests,
    click: (dataset) => {
      const trigger = new FakeElement("button");
      trigger.dataset = { postureOpen: "", ...dataset };
      return documentClick({ target: trigger, preventDefault() {} });
    },
  };
}

async function settle() {
  await new Promise((resolve) => setTimeout(resolve, 0));
  await new Promise((resolve) => setTimeout(resolve, 0));
}

const reviewerPosture = {
  profile: "reviewer",
  group: "main",
  known: true,
  system: { source: "file", path: "prompts/reviewer.md", text: "You review changes." },
  layers: [
    {
      name: "group", applied: true, sandbox_mode: "host",
      allow: ["bash", "read"], deny: [], deny_prefixes: ["rm -rf"],
      guidance: "Group guidance.",
    },
    {
      name: "profile", applied: true, sandbox_mode: "docker",
      allow: ["read"], deny: ["bash"], deny_prefixes: ["git push"],
    },
    { name: "repo", applied: false, allow: [], deny: [], deny_prefixes: [] },
  ],
  effective: {
    name: "effective", applied: true, sandbox_mode: "docker",
    allow: ["read"], deny: ["bash"], deny_prefixes: ["rm -rf", "git push"],
  },
  limits: {
    model: "primary", max_tokens: 4096, max_iterations: 12,
    allowed_children: ["reader"],
  },
};

test("posture client is read-only and uses safe DOM APIs", () => {
  for (const required of ["/api/v1/desk/posture", "textContent", "createElement"]) {
    assert.ok(source.includes(required), `missing ${required}`);
  }
  // A read-only surface must never mutate: no POST, no token, no key.
  for (const forbidden of [
    "innerHTML",
    "insertAdjacentHTML",
    '"POST"',
    "X-Waffle-Desk-Token",
    "Idempotency-Key",
  ]) {
    assert.equal(source.includes(forbidden), false, `contains ${forbidden}`);
  }
});

test("posture renders the prompt, its source, and each policy tier", async () => {
  const harness = createHarness({
    "/posture/denials": () => response({ denials: [] }),
    "/api/v1/desk/posture": () => response(reviewerPosture),
  });
  await harness.click({ postureProfile: "reviewer", postureSession: "session-primary" });
  await settle();

  assert.equal(harness.elements["#desk-posture-dialog"].open, true);
  assert.equal(harness.elements["#desk-posture-title"].textContent, "Posture for reviewer");

  const rendered = flatten(harness.elements["#desk-posture-body"]);
  // AC1: the prompt and its labelled source.
  assert.ok(rendered.includes("Resolved from a file"), rendered);
  assert.ok(rendered.includes("prompts/reviewer.md"), rendered);
  assert.ok(rendered.includes("You review changes."), rendered);
  // AC2: each tier is named and shown separately from the effective result.
  for (const label of [
    "Agent group",
    "Profile narrowing",
    "Repo policy (WAFFLE.md)",
    "Effective",
  ]) {
    assert.ok(rendered.includes(label), `missing tier ${label}: ${rendered}`);
  }
  assert.ok(rendered.includes("No change"), "inert repo tier was not marked");
  assert.ok(rendered.includes("git push"), rendered);

  const posture = harness.requests.find((entry) =>
    entry.path.startsWith("/api/v1/desk/posture?"),
  );
  assert.equal(posture.options.method, "GET");
  assert.equal(posture.options.headers["X-Waffle-Desk-Token"], undefined);
  assert.ok(posture.path.includes("profile=reviewer"));
});

test("denials name the rule that refused the call", async () => {
  const harness = createHarness({
    "/posture/denials": () =>
      response({
        denials: [{
          at: "2026-07-25T12:00:00Z",
          tool: "bash",
          command: "git push --force",
          rule: "no-force-push",
          verdict: "deny",
          detail: "Force pushes are refused on shared branches.",
        }],
      }),
    "/api/v1/desk/posture": () => response(reviewerPosture),
  });
  await harness.click({ postureProfile: "reviewer", postureSession: "session-primary" });
  await settle();

  const rendered = flatten(harness.elements["#desk-posture-body"]);
  assert.ok(rendered.includes("no-force-push"), rendered);
  assert.ok(rendered.includes("git push --force"), rendered);
  assert.ok(rendered.includes("Force pushes are refused"), rendered);
});

test("a profile with no session skips the denial read entirely", async () => {
  const harness = createHarness({
    "/api/v1/desk/posture": () => response(reviewerPosture),
  });
  await harness.click({ postureProfile: "reviewer" });
  await settle();

  assert.equal(
    harness.requests.some((entry) => entry.path.includes("/denials")),
    false,
    "read denials without a session",
  );
  const rendered = flatten(harness.elements["#desk-posture-body"]);
  assert.ok(rendered.includes("No tool call has been refused"), rendered);
});

test("an unreadable posture reports the failure without blanking the dialog", async () => {
  const harness = createHarness({
    "/api/v1/desk/posture": () =>
      response({ message: "The posture could not be read." }, 503),
  });
  await harness.click({ postureProfile: "reviewer" });
  await settle();

  assert.equal(harness.elements["#desk-posture-dialog"].open, true);
  assert.equal(
    harness.elements["#desk-posture-status"].textContent,
    "The posture could not be read.",
  );
});

test("an unknown profile still shows the posture it would inherit", async () => {
  const harness = createHarness({
    "/api/v1/desk/posture": () =>
      response({ ...reviewerPosture, profile: "ghost", known: false }),
  });
  await harness.click({ postureProfile: "ghost" });
  await settle();

  const rendered = flatten(harness.elements["#desk-posture-body"]);
  assert.ok(rendered.includes("No profile named ghost is configured"), rendered);
  assert.ok(rendered.includes("Agent group"), rendered);
});
