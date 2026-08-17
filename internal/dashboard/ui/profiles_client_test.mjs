import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = await readFile(
  new URL("./assets/profiles.js", import.meta.url),
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
    this.value = "";
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

  getAttribute(name) {
    return this.attributes[name];
  }

  removeAttribute(name) {
    delete this.attributes[name];
  }

  addEventListener(name, listener) {
    this.listeners[name] = listener;
  }

  async dispatch(name, extra = {}) {
    return this.listeners[name]?.({
      preventDefault() {},
      currentTarget: this,
      target: this,
      ...extra,
    });
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

function findAction(node, action) {
  if (node?.dataset?.action === action) return node;
  for (const child of node?.childNodes || []) {
    const match = findAction(child, action);
    if (match) return match;
  }
  return null;
}

function flatten(node) {
  if (!node) return "";
  if (node.childNodes.length === 0) return node.textContent;
  return node.childNodes.map(flatten).join(" ");
}

const reviewerProfile = {
  name: "reviewer",
  system: "You review changes.",
  model: "primary",
  sandbox: "docker",
  allow: ["read_file"],
  deny: [],
  deny_prefixes: ["git push"],
  guidance: "",
  max_tokens: 4096,
  max_iterations: 0,
  allowed_children: [],
};

function createHarness(routes) {
  const selectors = [
    "#profile-editor", "#profile-list", "#profile-errors", "#profile-new",
    "#profile-form", "#profile-form-title", "#profile-name", "#profile-system",
    "#profile-model", "#profile-sandbox", "#profile-allow", "#profile-deny",
    "#profile-deny-prefixes", "#profile-guidance", "#profile-max-tokens",
    "#profile-max-iterations", "#profile-allowed-children",
    "#profile-form-status", "#profile-review-dialog", "#profile-review-title",
    "#profile-review-body", "#profile-review-status", "#profile-review-cancel",
    "#profile-review-confirm",
  ];
  const elements = Object.fromEntries(
    selectors.map((selector) => [selector, new FakeElement()]),
  );
  const requests = [];
  let uuid = 0;
  const context = {
    console,
    crypto: { randomUUID: () => `uuid-${++uuid}` },
    document: {
      body: { dataset: { requestToken: "desk-token" } },
      createElement: (tag) => new FakeElement(tag),
      querySelector: (selector) => elements[selector] || null,
    },
    fetch: async (path, options = {}) => {
      requests.push({ path, options });
      for (const [pattern, handler] of Object.entries(routes)) {
        if (path.includes(pattern)) return handler(path, options);
      }
      throw new Error(`unexpected fetch ${path}`);
    },
    setTimeout,
    clearTimeout,
    URL,
    URLSearchParams,
  };
  vm.runInNewContext(source, context, { filename: "profiles.js" });
  return { elements, requests };
}

async function settle() {
  await new Promise((resolve) => setTimeout(resolve, 0));
  await new Promise((resolve) => setTimeout(resolve, 0));
}

test("profile editor is structured and never handles raw TOML", () => {
  for (const required of [
    "/api/v1/desk/profiles",
    '"X-Waffle-Desk-Token"',
    '"Idempotency-Key"',
    "preview_token",
    "textContent",
  ]) {
    assert.ok(source.includes(required), `missing ${required}`);
  }
  // AC1: no unsafe DOM, and no TOML handling — a config table header, a
  // .toml file reference, or a raw-config field would each mean this editor
  // had stopped being structured. (The prose comments may say "TOML"; what
  // must not exist is code that touches it.)
  for (const forbidden of [
    "innerHTML",
    "insertAdjacentHTML",
    "[agent.",
    ".toml",
    "raw_toml",
    "rawToml",
  ]) {
    assert.equal(source.includes(forbidden), false, `contains ${forbidden}`);
  }
  // Every control the form reads maps to exactly one typed config key.
  for (const field of [
    "name", "system", "model", "sandbox", "allow", "deny",
    "deny_prefixes", "guidance", "max_tokens", "max_iterations",
    "allowed_children",
  ]) {
    assert.ok(source.includes(`${field}:`) || source.includes(`"${field}"`), `missing field ${field}`);
  }
});

test("saving requires a review first and sends the issued token", async () => {
  let previewBody = null;
  let saveBody = null;
  const harness = createHarness({
    "/api/v1/desk/profiles/preview": (_path, options) => {
      previewBody = JSON.parse(options.body);
      return response({
        profile: "auditor",
        exists: false,
        preview_token: "token-1",
        expires_in_seconds: 120,
        before: { system: { text: "" }, effective: { allow: [], deny: [], deny_prefixes: [] } },
        after: {
          system: { text: "You audit." },
          effective: { sandbox_mode: "docker", allow: ["read_file"], deny: ["bash"], deny_prefixes: ["git push"] },
        },
      });
    },
    "/api/v1/desk/profiles": (path, options) => {
      if (options.method === "POST") {
        saveBody = JSON.parse(options.body);
        return response({ profile: "auditor", restart_required: true });
      }
      return response({ profiles: [reviewerProfile], groups: ["main"] });
    },
  });
  await settle();

  harness.elements["#profile-name"].value = "auditor";
  harness.elements["#profile-system"].value = "You audit.";
  harness.elements["#profile-sandbox"].value = "docker";
  harness.elements["#profile-allow"].value = "read_file\nsearch";
  harness.elements["#profile-deny"].value = "bash";
  harness.elements["#profile-deny-prefixes"].value = "git push\nrm -rf";
  harness.elements["#profile-max-tokens"].value = "2048";
  await harness.elements["#profile-form"].dispatch("submit");
  await settle();

  // Typed fields, parsed into lists. Prefixes keep their spaces.
  assert.deepEqual(previewBody.allow, ["read_file", "search"]);
  assert.deepEqual(previewBody.deny, ["bash"]);
  assert.deepEqual(previewBody.deny_prefixes, ["git push", "rm -rf"]);
  assert.equal(previewBody.max_tokens, 2048);
  assert.equal(saveBody, null, "preview wrote before confirmation");

  // AC3: the review dialog opens with before and after.
  assert.equal(harness.elements["#profile-review-dialog"].open, true);
  const review = flatten(harness.elements["#profile-review-body"]);
  assert.ok(review.includes("You audit."), review);
  assert.ok(review.includes("System prompt before"), review);
  assert.ok(review.includes("Effective policy after"), review);

  await harness.elements["#profile-review-confirm"].dispatch("click");
  await settle();

  assert.equal(saveBody.preview_token, "token-1");
  assert.equal(saveBody.name, "auditor");
  assert.equal(harness.elements["#profile-review-dialog"].open, false);
  assert.match(harness.elements["#profile-form-status"].textContent, /Restart Waffle/);
});

test("a widening refusal points at the offending field", async () => {
  const harness = createHarness({
    "/api/v1/desk/profiles/preview": () =>
      response({
        code: "profile_widens_group",
        message: "This change would widen the agent group's policy: the group does not allow bash.",
        field: "tools.allow",
      }, 422),
    "/api/v1/desk/profiles": () => response({ profiles: [], groups: [] }),
  });
  await settle();

  harness.elements["#profile-name"].value = "escape";
  harness.elements["#profile-allow"].value = "bash";
  await harness.elements["#profile-form"].dispatch("submit");
  await settle();

  assert.equal(harness.elements["#profile-review-dialog"].open, false);
  assert.match(
    harness.elements["#profile-form-status"].textContent,
    /would widen the agent group/,
  );
  // AC2: the named field is marked invalid and focused.
  assert.equal(harness.elements["#profile-allow"].getAttribute("aria-invalid"), "true");
  assert.equal(harness.elements["#profile-allow"].focused, true);
});

test("a referenced profile cannot be confirmed for deletion", async () => {
  const harness = createHarness({
    "/delete-preview": () =>
      response({
        profile: "reviewer",
        eligible: false,
        references: ["scheduled job Daily review", "workspace matt-riley/waffle"],
      }),
    "/api/v1/desk/profiles": () => response({ profiles: [reviewerProfile], groups: ["main"] }),
  });
  await settle();

  const card = harness.elements["#profile-list"].childNodes[0];
  await findAction(card, "delete").dispatch("click");
  await settle();

  const body = flatten(harness.elements["#profile-review-body"]);
  // AC4: the blocking references are named.
  assert.ok(body.includes("scheduled job Daily review"), body);
  assert.ok(body.includes("workspace matt-riley/waffle"), body);
  assert.equal(harness.elements["#profile-review-confirm"].disabled, true);
});

test("an unreferenced profile deletes after explicit confirmation", async () => {
  let deleted = null;
  const harness = createHarness({
    "/delete-preview": () =>
      response({ profile: "reviewer", eligible: true, references: [], preview_token: "token-del" }),
    "/delete": (_path, options) => {
      deleted = JSON.parse(options.body);
      return response({ profile: "reviewer", restart_required: true });
    },
    "/api/v1/desk/profiles": () => response({ profiles: [reviewerProfile], groups: ["main"] }),
  });
  await settle();

  const card = harness.elements["#profile-list"].childNodes[0];
  await findAction(card, "delete").dispatch("click");
  await settle();
  assert.equal(harness.elements["#profile-review-confirm"].disabled, false);
  assert.equal(deleted, null, "delete ran before confirmation");

  await harness.elements["#profile-review-confirm"].dispatch("click");
  await settle();
  assert.deepEqual(deleted, { preview_token: "token-del" });
});

test("copy prefills the form without a name and writes nothing", async () => {
  const harness = createHarness({
    "/api/v1/desk/profiles": () => response({ profiles: [reviewerProfile], groups: ["main"] }),
  });
  await settle();

  const card = harness.elements["#profile-list"].childNodes[0];
  await findAction(card, "copy").dispatch("click");
  await settle();

  assert.equal(harness.elements["#profile-name"].value, "");
  assert.equal(harness.elements["#profile-system"].value, "You review changes.");
  assert.equal(harness.elements["#profile-allow"].value, "read_file");
  assert.equal(harness.elements["#profile-deny-prefixes"].value, "git push");
  assert.match(harness.elements["#profile-form-title"].textContent, /New profile from reviewer/);
  // A copy is only a prefill: nothing is written until it is reviewed.
  assert.equal(
    harness.requests.some((entry) => entry.options.method === "POST"),
    false,
  );
});

test("review flags narrowing and widening directions explicitly", async () => {
  const harness = createHarness({
    "/api/v1/desk/profiles/preview": () =>
      response({
        profile: "reviewer",
        exists: true,
        preview_token: "token-review",
        before: {
          system: { text: "Before." },
          effective: {
            sandbox_mode: "host",
            allow: ["read_file", "search"],
            deny: ["bash"],
            deny_prefixes: ["git push"],
          },
        },
        after: {
          system: { text: "After." },
          effective: {
            sandbox_mode: "docker",
            allow: ["read_file"],
            deny: ["bash", "curl"],
            deny_prefixes: ["git push"],
          },
        },
      }),
    "/api/v1/desk/profiles": () => response({ profiles: [reviewerProfile], groups: ["main"] }),
  });
  await settle();
  harness.elements["#profile-name"].value = "reviewer";
  await harness.elements["#profile-form"].dispatch("submit");
  await settle();

  const review = flatten(harness.elements["#profile-review-body"]);
  // Narrowing rows are flagged in both directions of the diff.
  assert.match(review, /narrows/);
  assert.ok(review.includes("Sandbox (narrowed)"), review);
  assert.ok(review.includes("Allow (narrowed)"), review);
  assert.ok(review.includes("Deny (narrowed)"), review);
  assert.ok(review.includes("Deny prefixes git push"), review);
  assert.doesNotMatch(review, /widened/);
});

test("tool fields read one structured entry per line", async () => {
  let body = null;
  const harness = createHarness({
    "/api/v1/desk/profiles/preview": (_path, options) => {
      body = JSON.parse(options.body);
      return response({
        profile: "auditor",
        exists: false,
        preview_token: "token-1",
        before: { system: { text: "" }, effective: {} },
        after: { system: { text: "You audit." }, effective: {} },
      });
    },
    "/api/v1/desk/profiles": () => response({ profiles: [], groups: ["main"] }),
  });
  await settle();
  harness.elements["#profile-name"].value = "auditor";
  harness.elements["#profile-allow"].value = "read_file\nsearch";
  harness.elements["#profile-deny"].value = "bash\ncurl";
  harness.elements["#profile-allowed-children"].value = "reviewer";
  await harness.elements["#profile-form"].dispatch("submit");
  await settle();
  assert.deepEqual(body.allow, ["read_file", "search"]);
  assert.deepEqual(body.deny, ["bash", "curl"]);
  assert.deepEqual(body.allowed_children, ["reviewer"]);
});
