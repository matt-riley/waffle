import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = await readFile(
  new URL("./assets/setup.js", import.meta.url),
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
    this.value = "";
    this.className = "";
    this.type = "";
    this._textContent = "";
    this.focused = false;
    this.scrolled = false;
    this.classList = {
      names: new Set(),
      toggle(name, force) {
        if (force === undefined) {
          if (this.names.has(name)) this.names.delete(name);
          else this.names.add(name);
          return;
        }
        if (force) this.names.add(name);
        else this.names.delete(name);
      },
    };
  }

  closest() {
    return null;
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

  replaceChildren(...nodes) {
    this.childNodes = [...nodes];
  }

  setAttribute(name, value) {
    this.attributes[name] = String(value);
  }

  addEventListener(name, listener) {
    this.listeners[name] = listener;
  }

  async dispatch(name) {
    return this.listeners[name]?.({ preventDefault() {}, currentTarget: this, target: this });
  }

  focus() {
    this.focused = true;
  }

  scrollIntoView() {
    this.scrolled = true;
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

const SELECTORS = [
  "#setup-checklist", "#setup-steps", "#setup-status",
  "#desk-setup-banner", "#desk-setup-banner-message",
  // Controls the checklist points at, all owned by other sections.
  "#capability-provider-name", "#capability-default-alias",
  "#profile-name", "#profile-system", "#profile-form-title",
];

function createHarness(routes, { present = SELECTORS } = {}) {
  const elements = Object.fromEntries(
    present.map((selector) => [selector, new FakeElement()]),
  );
  elements["#setup-checklist"].dataset.starterSystem =
    "You are the owner's personal assistant.";
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
    location: { hash: "" },
    setTimeout,
    clearTimeout,
    URL,
    URLSearchParams,
  };
  vm.runInNewContext(source, context, { filename: "setup.js" });
  return { elements, requests };
}

async function settle() {
  await new Promise((resolve) => setTimeout(resolve, 0));
  await new Promise((resolve) => setTimeout(resolve, 0));
}

const freshInstall = {
  complete: false,
  steps: [
    {
      id: "identity",
      title: "Secret-store identity",
      state: "missing",
      detail: "Provider credentials cannot be stored until an identity exists.",
      action: "create-identity",
      action_label: "Create identity",
    },
    {
      id: "provider",
      title: "Provider connection",
      state: "missing",
      detail: "No provider connection is configured.",
      action: "enroll-provider",
      action_label: "Enroll a provider",
    },
    {
      id: "profile",
      title: "Owner profile (agent.profile.main)",
      state: "missing",
      detail: "No [agent.profile.main] is configured.",
      action: "create-profile",
      action_label: "Create a starter profile",
    },
    {
      id: "dashboard",
      title: "Desk enabled",
      state: "configured",
      detail: "Desk is serving this page.",
      command: "waffle setup",
    },
  ],
};

test("the setup client never invents state or handles key material", () => {
  for (const required of [
    "/api/v1/desk/setup",
    '"X-Waffle-Desk-Token"',
    '"Idempotency-Key"',
    "textContent",
  ]) {
    assert.ok(source.includes(required), `missing ${required}`);
  }
  // AC4: no key material path exists in the client at all — it neither reads
  // an identity from a response nor renders one. Unsafe DOM sinks would be a
  // way to smuggle one in, so they are barred too.
  for (const forbidden of [
    "innerHTML",
    "insertAdjacentHTML",
    "AGE-SECRET-KEY",
    "identity_value",
    "private_key",
    "api_key",
  ]) {
    assert.equal(source.includes(forbidden), false, `contains ${forbidden}`);
  }
});

test("a fresh install lists every prerequisite with its state", async () => {
  const { elements } = createHarness({
    "/api/v1/desk/setup": () => response(freshInstall),
  });
  await settle();

  const steps = elements["#setup-steps"].childNodes;
  assert.equal(steps.length, 4);
  assert.deepEqual(
    steps.map((step) => step.dataset.step),
    ["identity", "provider", "profile", "dashboard"],
  );
  assert.deepEqual(
    steps.map((step) => step.dataset.state),
    ["missing", "missing", "missing", "configured"],
  );
  assert.match(elements["#setup-status"].textContent, /3 prerequisites are outstanding/);
});

test("a step Desk cannot satisfy states the exact command instead", async () => {
  const { elements } = createHarness({
    "/api/v1/desk/setup": () => response(freshInstall),
  });
  await settle();

  const dashboardStep = elements["#setup-steps"].childNodes.at(-1);
  assert.equal(findAction(dashboardStep, "create-identity"), null);
  assert.match(flatten(dashboardStep), /waffle setup/);
});

test("creating the identity posts a guarded mutation and reloads", async () => {
  let reads = 0;
  const { elements, requests } = createHarness({
    "/api/v1/desk/setup/identity": () => response({ restart_required: true }),
    "/api/v1/desk/setup": () => {
      reads += 1;
      return response(reads === 1 ? freshInstall : { complete: true, steps: [] });
    },
  });
  await settle();

  const button = findAction(elements["#setup-steps"], "create-identity");
  assert.ok(button, "identity action is offered");
  await button.dispatch("click");
  await settle();

  const mutation = requests.find((request) => request.path.includes("/setup/identity"));
  assert.equal(mutation.options.method, "POST");
  assert.equal(mutation.options.headers["X-Waffle-Desk-Token"], "desk-token");
  assert.ok(mutation.options.headers["Idempotency-Key"]);
  // The action re-reads the projection rather than assuming the step flipped.
  assert.equal(reads, 2);
  assert.equal(elements["#setup-status"].textContent, "Waffle is fully set up.");
});

test("in-Desk actions point at the existing controls rather than duplicating them", async () => {
  const { elements, requests } = createHarness({
    "/api/v1/desk/setup": () => response(freshInstall),
  });
  await settle();

  await findAction(elements["#setup-steps"], "enroll-provider").dispatch("click");
  await settle();
  assert.equal(elements["#capability-provider-name"].focused, true);
  // AC4: routing to the enrollment form must not itself be a credential
  // channel — no request is made at all.
  assert.equal(requests.filter((request) => request.options.method === "POST").length, 0);

  await findAction(elements["#setup-steps"], "create-profile").dispatch("click");
  await settle();
  assert.equal(elements["#profile-name"].value, "main");
  assert.equal(
    elements["#profile-system"].value,
    "You are the owner's personal assistant.",
  );
});

test("a starter profile never overwrites an edit already in progress", async () => {
  const { elements } = createHarness({
    "/api/v1/desk/setup": () => response(freshInstall),
  });
  await settle();
  elements["#profile-name"].value = "reviewer";
  elements["#profile-system"].value = "You review changes.";

  await findAction(elements["#setup-steps"], "create-profile").dispatch("click");
  await settle();

  assert.equal(elements["#profile-name"].value, "reviewer");
  assert.equal(elements["#profile-system"].value, "You review changes.");
});

test("Today shows the banner only while a prerequisite is outstanding", async () => {
  const outstanding = createHarness({
    "/api/v1/desk/setup": () => response(freshInstall),
  });
  await settle();
  assert.equal(outstanding.elements["#desk-setup-banner"].hidden, false);
  assert.match(
    outstanding.elements["#desk-setup-banner-message"].textContent,
    /Secret-store identity/,
  );

  const ready = createHarness({
    "/api/v1/desk/setup": () =>
      response({ complete: true, steps: freshInstall.steps.map((step) => ({ ...step, state: "configured" })) }),
  });
  await settle();
  assert.equal(ready.elements["#desk-setup-banner"].hidden, true);
});

test("an unavailable setup surface degrades instead of blocking the section", async () => {
  const { elements } = createHarness({
    "/api/v1/desk/setup": () =>
      response({ code: "setup_unavailable", message: "setup state is unavailable" }, 503),
  });
  await settle();

  assert.equal(elements["#setup-status"].textContent, "setup state is unavailable");
  assert.equal(elements["#desk-setup-banner"].hidden, true);
  assert.equal(elements["#setup-steps"].childNodes.length, 0);
});

test("the failed identity action reports the server message and stays usable", async () => {
  const { elements } = createHarness({
    "/api/v1/desk/setup/identity": () =>
      response({ code: "identity_unavailable", message: "run `waffle secret init --print`" }, 503),
    "/api/v1/desk/setup": () => response(freshInstall),
  });
  await settle();

  const button = findAction(elements["#setup-steps"], "create-identity");
  await button.dispatch("click");
  await settle();

  assert.equal(button.disabled, false);
  const step = elements["#setup-steps"].childNodes[0];
  assert.match(flatten(step), /waffle secret init --print/);
});
