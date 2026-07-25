import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = await readFile(new URL("./assets/capabilities.js", import.meta.url), "utf8");

class FakeElement {
  constructor({ id = "", type = "", name = "", tagName = "DIV" } = {}) {
    this.id = id;
    this.type = type;
    this.name = name;
    this.tagName = tagName;
    this.value = "";
    this.hidden = false;
    this.disabled = false;
    this.textContent = "";
    this.dataset = {};
    this.listeners = new Map();
    this.childNodes = [];
    this.attributes = {};
    this.focused = false;
    this.controls = null;
  }

  set innerHTML(_value) {
    throw new Error("capabilities client must not use innerHTML");
  }

  addEventListener(type, callback) {
    this.listeners.set(type, callback);
  }

  listener(type) {
    return this.listeners.get(type);
  }

  appendChild(child) {
    this.childNodes.push(child);
    return child;
  }

  replaceChildren(...children) {
    this.childNodes = children;
  }

  setAttribute(name, value) {
    this.attributes[name] = String(value);
  }

  getAttribute(name) {
    return Object.prototype.hasOwnProperty.call(this.attributes, name)
      ? this.attributes[name]
      : null;
  }

  removeAttribute(name) {
    delete this.attributes[name];
  }

  focus() {
    this.focused = true;
  }

  querySelector(selector) {
    if (selector === 'button[type="submit"]') {
      if (Array.isArray(this.controls)) {
        return this.controls.find((control) => control && control.type === "submit") || null;
      }
      return null;
    }
    if (selector.startsWith("[name=")) {
      const name = selector.slice('[name="'.length, -2);
      if (Array.isArray(this.controls)) {
        return this.controls.find((control) => control && control.name === name) || null;
      }
      return null;
    }
    if (selector.startsWith("#")) {
      const id = selector.slice(1);
      if (Array.isArray(this.controls)) {
        return this.controls.find((control) => control && control.id === id) || null;
      }
      return null;
    }
    return null;
  }

  querySelectorAll(selector) {
    if (!Array.isArray(this.controls)) {
      return [];
    }
    if (selector === "input, button, select, textarea") {
      return this.controls.slice();
    }
    if (selector === "input, select, textarea") {
      return this.controls.filter(
        (control) => control && control.type !== "submit" && control.type !== "button",
      );
    }
    return [];
  }
}

function response(body, ok = true, status = 200) {
  return {
    ok,
    status,
    async json() {
      return body;
    },
  };
}

function deferredResponse() {
  let resolve;
  const promise = new Promise((release) => {
    resolve = release;
  });
  return {
    promise,
    resolve(body, ok = true, status = 200) {
      resolve(response(body, ok, status));
    },
  };
}

function createHarness({
  providerResponse = response({}, true, 202),
  providerTestResponse = response({ outcome: "success" }),
  providerProspectiveTestResponse = response({ outcome: "success" }),
  defaultResponse = response({ restart_required: false }),
  utilityResponse = response({ restart_required: false }),
  modelResponse = response({ restart_required: false }),
  modelRemovalPreviewResponse = response({ kind: "model", target: "gpt", references: [], replacement_required: false, preview_token: "model-preview", expires_at: "2026-07-25T00:01:00Z" }),
  modelRemovalResponse = response({ restart_required: false }),
  catalogueResponse = response({ connection: "primary", models: [] }),
  stageResponse = response({}, true, 200),
  installResponse = response({ restart_required: false }),
  skillLifecycleResponse = response({ restart_required: false }),
  capabilitiesResponse = null,
  connectionResponse = response([]),
  providerRemovalPreviewResponse = response({ kind: "provider", target: "openai", references: [], replacement_required: false, preview_token: "provider-preview", expires_at: "2026-07-25T00:01:00Z" }),
  providerRemovalResponse = response({ restart_required: false }),
  connectionResponses = null,
  bootstrapGenerations = ["process-old", "process-old", "process-new"],
  skills = [],
  deferTimers = false,
  clock = false,
  uuidSequence = null,
} = {}) {
  const selectors = [
    "#desk-capabilities",
    "#capability-status",
    "#capability-restart-status",
    "#capability-models",
    "#capability-skills",
    "#capability-connections",
    "#capability-provider-form",
    "#capability-provider-status",
    "#capability-provider-name",
    "#capability-provider-type",
    "#capability-provider-base-url",
    "#capability-provider-base-url-guidance",
    "#capability-provider-model-alias",
    "#capability-provider-model-id",
    "#capability-provider-max-tokens",
    "#capability-provider-default",
    "#capability-provider-utility",
    "#capability-provider-credential",
    "#capability-provider-test",
    "#capability-default-form",
    "#capability-default-status",
    "#capability-default-alias",
    "#capability-default-empty",
    "#capability-utility-form",
    "#capability-utility-status",
    "#capability-utility-alias",
    "#capability-utility-empty",
    "#capability-model-form",
    "#capability-model-status",
    "#capability-model-connection",
    "#capability-model-connection-empty",
    "#capability-model-alias",
    "#capability-model-id",
    "#capability-catalogue-form",
    "#capability-catalogue-status",
    "#capability-catalogue-connection",
    "#capability-catalogue-empty",
    "#capability-catalogue-search",
    "#capability-catalogue-summary",
    "#capability-catalogue-results",
    "#capability-skill-stage-form",
    "#capability-skill-stage-status",
    "#capability-skill-stage-prerequisite",
    "#capability-skill-local-help",
    "#capability-skill-git-help",
    "#capability-skill-local-path",
    "#capability-skill-git-url",
    "#capability-skill-commit",
    "#capability-skill-review",
    "#capability-skill-review-expires",
    "#capability-skill-preview",
    "#capability-skill-install",
    "#capability-skill-install-status",
  ];
  const elements = Object.fromEntries(
    selectors.map((selector) => {
      const id = selector.startsWith("#") ? selector.slice(1) : "";
      return [selector, new FakeElement({ id })];
    }),
  );

  const wireField = (selector, name) => {
    const el = elements[selector];
    el.name = name;
    el.tagName = "INPUT";
    el.type = selector.includes("credential") ? "password" : "text";
    return el;
  };

  elements["#capability-provider-name"].value = "openai";
  wireField("#capability-provider-name", "connection_name");
  elements["#capability-provider-type"].value = "openai";
  wireField("#capability-provider-type", "type");
  elements["#capability-provider-type"].tagName = "SELECT";
  wireField("#capability-provider-base-url", "base_url");
  elements["#capability-provider-model-alias"].value = "gpt";
  wireField("#capability-provider-model-alias", "model_alias");
  elements["#capability-provider-model-id"].value = "gpt-test";
  wireField("#capability-provider-model-id", "model_id");
  wireField("#capability-provider-max-tokens", "max_tokens");
  elements["#capability-provider-max-tokens"].type = "number";
  wireField("#capability-provider-default", "make_default");
  elements["#capability-provider-default"].type = "checkbox";
  wireField("#capability-provider-utility", "make_utility");
  elements["#capability-provider-utility"].type = "checkbox";
  elements["#capability-provider-credential"].value = "sk-super-private";
  wireField("#capability-provider-credential", "api_key");

  wireField("#capability-default-alias", "alias");
  wireField("#capability-utility-alias", "alias");
  wireField("#capability-model-connection", "connection_name");
  wireField("#capability-model-alias", "alias");
  wireField("#capability-model-id", "upstream_model");
  wireField("#capability-catalogue-connection", "connection");
  wireField("#capability-skill-local-path", "local_path");
  wireField("#capability-skill-git-url", "git_url");
  wireField("#capability-skill-commit", "commit");

  elements["#capability-provider-form"].setAttribute("aria-describedby", "capability-provider-status");
  elements["#capability-default-form"].setAttribute("aria-describedby", "capability-default-status");
  elements["#capability-utility-form"].setAttribute("aria-describedby", "capability-utility-status");
  elements["#capability-model-form"].setAttribute("aria-describedby", "capability-model-status");
  elements["#capability-catalogue-form"].setAttribute("aria-describedby", "capability-catalogue-status");
  elements["#capability-skill-stage-form"].setAttribute("aria-describedby", "capability-skill-stage-status");

  const providerSubmit = new FakeElement({ type: "submit", tagName: "BUTTON" });
  providerSubmit.textContent = "Enroll provider";
  const providerTest = elements["#capability-provider-test"];
  providerTest.type = "button";
  providerTest.textContent = "Test connection";
  elements["#capability-provider-form"].controls = [
    elements["#capability-provider-name"],
    elements["#capability-provider-type"],
    elements["#capability-provider-base-url"],
    elements["#capability-provider-model-alias"],
    elements["#capability-provider-model-id"],
    elements["#capability-provider-max-tokens"],
    elements["#capability-provider-default"],
    elements["#capability-provider-utility"],
    elements["#capability-provider-credential"],
    providerSubmit,
    providerTest,
  ];

  const defaultSubmit = new FakeElement({ type: "submit", tagName: "BUTTON" });
  defaultSubmit.textContent = "Set default";
  elements["#capability-default-form"].controls = [
    elements["#capability-default-alias"],
    defaultSubmit,
  ];

  const utilitySubmit = new FakeElement({ type: "submit", tagName: "BUTTON" });
  utilitySubmit.textContent = "Set utility model";
  elements["#capability-utility-form"].controls = [
    elements["#capability-utility-alias"],
    utilitySubmit,
  ];

  const modelSubmit = new FakeElement({ type: "submit", tagName: "BUTTON" });
  modelSubmit.textContent = "Add model";
  elements["#capability-model-form"].controls = [
    elements["#capability-model-connection"],
    elements["#capability-model-alias"],
    elements["#capability-model-id"],
    modelSubmit,
  ];

  const catalogueSubmit = new FakeElement({ type: "submit", tagName: "BUTTON" });
  catalogueSubmit.textContent = "Refresh catalogue";
  elements["#capability-catalogue-form"].controls = [
    elements["#capability-catalogue-connection"],
    catalogueSubmit,
  ];

  const stageSubmit = new FakeElement({ type: "submit", tagName: "BUTTON" });
  stageSubmit.textContent = "Stage review";
  elements["#capability-skill-stage-form"].controls = [
    elements["#capability-skill-local-path"],
    elements["#capability-skill-git-url"],
    elements["#capability-skill-commit"],
    stageSubmit,
  ];

  elements["#capability-skill-install"].type = "button";
  elements["#capability-skill-install"].textContent = "Install inactive";
  elements["#capability-skill-install"].setAttribute(
    "aria-describedby",
    "capability-skill-install-status",
  );

  const calls = [];
  const timers = [];
  let bootstrapPolls = 0;
  let connectionFetches = 0;
  let nowMs = 0;
  let uuidIndex = 0;
  const fetch = async (path, options = {}) => {
    calls.push({ path, options });
    if (path === "/api/v1/desk/capabilities") {
      return capabilitiesResponse || response({
        providers: {
          state: "ready",
          default_model: "gpt",
          utility_model: "gpt",
          providers: {},
          models: {},
        },
        provider_presets: [
          { name: "openai", runtime_type: "openai", requires_base_url: false },
          { name: "anthropic", runtime_type: "anthropic", requires_base_url: false },
          { name: "openai-compatible", runtime_type: "openai", requires_base_url: true },
        ],
        skills,
      });
    }
    if (path === "/api/v1/desk/connections") {
      if (connectionResponses) {
        const index = Math.min(connectionFetches, connectionResponses.length - 1);
        connectionFetches += 1;
        return connectionResponses[index];
      }
      return connectionResponse;
    }
    if (path === "/api/v1/desk/providers") {
      return providerResponse;
    }
    if (path === "/api/v1/desk/providers/test") {
      return providerProspectiveTestResponse;
    }
    if (path.startsWith("/api/v1/desk/providers/") && path.endsWith("/test")) {
      return providerTestResponse;
    }
    if (path === "/api/v1/desk/models/default") {
      return defaultResponse;
    }
    if (path === "/api/v1/desk/models/utility") {
      return utilityResponse;
    }
    if (path === "/api/v1/desk/models") {
      return modelResponse;
    }
    if (path.startsWith("/api/v1/desk/models/") && path.endsWith("/removal-preview")) {
      return modelRemovalPreviewResponse;
    }
    if (path.startsWith("/api/v1/desk/models/") && path.endsWith("/remove")) {
      return modelRemovalResponse;
    }
    if (path === "/api/v1/desk/models/catalogue/refresh") {
      return catalogueResponse;
    }
    if (path === "/api/v1/desk/skills/stage") {
      return stageResponse;
    }
    if (path === "/api/v1/desk/skills/install") {
      return installResponse;
    }
    if (path.startsWith("/api/v1/desk/skills/") &&
      (path.endsWith("/activate") || path.endsWith("/deactivate") || path.endsWith("/uninstall"))) {
      return skillLifecycleResponse;
    }
    if (path.startsWith("/api/v1/desk/providers/") && path.endsWith("/removal-preview")) {
      return providerRemovalPreviewResponse;
    }
    if (path.startsWith("/api/v1/desk/providers/") && path.endsWith("/remove")) {
      return providerRemovalResponse;
    }
    if (path === "/api/v1/desk/bootstrap") {
      const generation =
        bootstrapGenerations[Math.min(bootstrapPolls, bootstrapGenerations.length - 1)];
      bootstrapPolls += 1;
      return response({
        request_token: generation === "process-new" ? "fresh-token" : "token",
        process_generation: generation,
      });
    }
    throw new Error(`unexpected fetch ${path}`);
  };
  const document = {
    body: { dataset: { requestToken: "token" } },
    createElement: (tag) => {
      const element = new FakeElement({ tagName: String(tag).toUpperCase() });
      // Real elements expose tagName as a getter-only property, so assigning to
      // it from a strict-mode module throws. Match that here to keep the client
      // from relying on a writable tagName that only exists in this harness.
      Object.defineProperty(element, "tagName", { value: element.tagName, writable: false });
      return element;
    },
    querySelector: (selector) => elements[selector] || null,
  };
  const context = vm.createContext({
    console,
    crypto: {
      randomUUID: () => {
        if (Array.isArray(uuidSequence) && uuidSequence.length > 0) {
          const value = uuidSequence[Math.min(uuidIndex, uuidSequence.length - 1)];
          uuidIndex += 1;
          return value;
        }
        return "idempotency-key";
      },
    },
    document,
    fetch,
    Date: clock
      ? class extends Date {
          static now() {
            return nowMs;
          }
        }
      : Date,
    setTimeout: (callback, ms = 0) => {
      if (deferTimers) {
        timers.push({ callback, ms: Number(ms) || 0 });
        return timers.length;
      }
      if (clock) {
        nowMs += Number(ms) || 0;
      }
      void callback();
      return 1;
    },
  });
  new vm.Script(source, { filename: "capabilities.js" }).runInContext(context);
  return {
    calls,
    elements,
    providerSubmit,
    defaultSubmit,
    catalogueSubmit,
    timers,
    async runTimersUntilIdle(maxSteps = 100) {
      let steps = 0;
      while (timers.length > 0 && steps < maxSteps) {
        const timer = timers.shift();
        if (clock) {
          nowMs += timer.ms;
        }
        timer.callback();
        await flush();
        steps += 1;
      }
    },
  };
}

async function flush() {
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
}

function findSkillActivateButton(skillsRoot) {
  for (const card of skillsRoot.childNodes) {
    for (const child of card.childNodes || []) {
      if (child.dataset && child.dataset.skillActivate === "true") {
        return child;
      }
    }
  }
  return null;
}

function findSkillButton(skillsRoot, dataKey) {
  for (const card of skillsRoot.childNodes) {
    for (const child of card.childNodes || []) {
      if (child.dataset && child.dataset[dataKey] === "true") {
        return child;
      }
    }
  }
  return null;
}

function nodeText(node) {
  if (!node) return "";
  return `${node.textContent || ""}${(node.childNodes || []).map(nodeText).join("")}`;
}

test("skill review renders safe audit, provenance, files, and previews without raw JSON", async () => {
  const hostilePreview = `<img src=x onerror="steal()">`;
  const harness = createHarness({
    clock: true,
    deferTimers: true,
    capabilitiesResponse: response({
      providers: { state: "ready", providers: {}, models: {} },
      skills: [],
      skill_sources: { local_roots: ["reviewed-skills"], git_hosts: [] },
    }),
    stageResponse: response({
      name: "reviewer",
      description: "Reviews changes.",
      source_ref: "local:reviewer",
      content_digest: "sha256:reviewer",
      expires_at: "1970-01-01T00:01:01.000Z",
      audit: { passed: false, flags: ["code:review.py", "network-reference:guide.txt"] },
      files: [{ path: "SKILL.md", size: 12, sha256: "abc123", preview: hostilePreview }],
      stage_id: "stage-review",
    }),
  });
  await flush();
  harness.elements["#capability-skill-local-path"].value = "/imports/reviewer";
  await harness.elements["#capability-skill-stage-form"].listener("submit")({ preventDefault() {} });
  await flush();

  const review = harness.elements["#capability-skill-preview"];
  assert.match(nodeText(review), /local:reviewer/);
  assert.match(nodeText(review), /sha256:reviewer/);
  assert.match(nodeText(review), /code:review\.py/);
  assert.match(nodeText(review), /network-reference:guide\.txt/);
  assert.match(nodeText(review), /SKILL\.md/);
  assert.match(nodeText(review), /<img src=x onerror="steal\(\)">/);
  assert.equal(harness.elements["#capability-skill-install"].hidden, true);
  assert.equal(source.includes("innerHTML"), false);
  assert.equal(source.includes("JSON.stringify(state.staged"), false);
});

test("expired skill review disables install and tells the user to re-stage", async () => {
  const harness = createHarness({
    clock: true,
    deferTimers: true,
    capabilitiesResponse: response({
      providers: { state: "ready", providers: {}, models: {} },
      skills: [],
      skill_sources: { local_roots: ["reviewed-skills"], git_hosts: [] },
    }),
    stageResponse: response({
      name: "reviewer",
      source_ref: "local:reviewer",
      content_digest: "sha256:reviewer",
      expires_at: "1970-01-01T00:00:01.000Z",
      audit: { passed: true, flags: [] },
      files: [],
      stage_id: "stage-expiring",
    }),
  });
  await flush();
  harness.elements["#capability-skill-local-path"].value = "/imports/reviewer";
  await harness.elements["#capability-skill-stage-form"].listener("submit")({ preventDefault() {} });
  await flush();
  await harness.runTimersUntilIdle();

  assert.equal(harness.elements["#capability-skill-install"].disabled, true);
  assert.match(harness.elements["#capability-skill-review-expires"].textContent, /re-stage/i);
  assert.equal(harness.calls.filter((call) => call.path === "/api/v1/desk/skills/install").length, 0);
});

test("skill source choices explain the deny-by-default state and validate Git input", async () => {
  const empty = createHarness({
    capabilitiesResponse: response({
      providers: { state: "ready", providers: {}, models: {} },
      skills: [],
      skill_sources: { local_roots: [], git_hosts: [] },
    }),
  });
  await flush();
  assert.equal(empty.elements["#capability-skill-stage-form"].hidden, true);
  assert.equal(empty.elements["#capability-skill-stage-form"].dataset.disabled, "true");
  assert.equal(empty.elements["#capability-skill-local-path"].disabled, true);
  assert.equal(empty.elements["#capability-skill-stage-prerequisite"].hidden, false);
  assert.match(empty.elements["#capability-skill-stage-prerequisite"].textContent, /skill_import_roots/);
  assert.match(empty.elements["#capability-skill-stage-prerequisite"].textContent, /skill_git_hosts/);

  const configured = createHarness({
    capabilitiesResponse: response({
      providers: { state: "ready", providers: {}, models: {} },
      skills: [],
      skill_sources: { local_roots: ["reviewed-skills"], git_hosts: ["github.com"] },
    }),
  });
  await flush();
  assert.match(configured.elements["#capability-skill-local-help"].textContent, /reviewed-skills/);
  assert.match(configured.elements["#capability-skill-git-help"].textContent, /github\.com/);

  configured.elements["#capability-skill-git-url"].value = "https://github.com/org/reviewer";
  configured.elements["#capability-skill-commit"].value = "not-a-commit";
  await configured.elements["#capability-skill-stage-form"].listener("submit")({ preventDefault() {} });
  await flush();
  assert.match(configured.elements["#capability-skill-stage-status"].textContent, /40-hex/i);
  assert.equal(configured.calls.filter((call) => call.path === "/api/v1/desk/skills/stage").length, 0);
});

test("skill lifecycle controls preserve active gates and use inverse routes", async () => {
  const active = createHarness({
    capabilitiesResponse: response({
      providers: { state: "ready", providers: {}, models: {} },
      skills: [{ name: "reviewer", active: true, attached: false }],
      skill_sources: { local_roots: [], git_hosts: [] },
    }),
  });
  await flush();
  const deactivate = findSkillButton(active.elements["#capability-skills"], "skillDeactivate");
  assert.ok(deactivate);
  await deactivate.listener("click")();
  await flush();
  assert.equal(active.calls.filter((call) => call.path === "/api/v1/desk/skills/reviewer/deactivate").length, 1);

  const attached = createHarness({
    capabilitiesResponse: response({
      providers: { state: "ready", providers: {}, models: {} },
      skills: [{ name: "reviewer", active: false, attached: true }],
      skill_sources: { local_roots: [], git_hosts: [] },
    }),
  });
  await flush();
  const uninstall = findSkillButton(attached.elements["#capability-skills"], "skillUninstall");
  assert.ok(uninstall);
  assert.equal(uninstall.disabled, true);
  assert.equal(attached.calls.filter((call) => call.path.endsWith("/uninstall")).length, 0);
});

test("skill uninstall attachment conflicts clear the intent before retry", async () => {
  let uninstallHits = 0;
  const lifecycleResponses = {
    then(onFulfilled) {
      uninstallHits += 1;
      const result = uninstallHits === 1
        ? response({ code: "skill_attached", message: "skill is attached" }, false, 409)
        : response({ restart_required: false });
      return Promise.resolve(result).then(onFulfilled);
    },
  };
  const harness = createHarness({
    capabilitiesResponse: response({
      providers: { state: "ready", providers: {}, models: {} },
      skills: [{ name: "reviewer", active: false, attached: false }],
      skill_sources: { local_roots: [], git_hosts: [] },
    }),
    skillLifecycleResponse: lifecycleResponses,
    uuidSequence: ["uninstall-conflict", "uninstall-retry"],
  });
  await flush();

  const uninstall = findSkillButton(harness.elements["#capability-skills"], "skillUninstall");
  assert.ok(uninstall);
  await uninstall.listener("click")();
  await flush();
  await uninstall.listener("click")();
  await flush();

  const calls = harness.calls.filter((call) => call.path.endsWith("/uninstall"));
  assert.equal(calls.length, 2);
  assert.equal(calls[0].options.headers["Idempotency-Key"], "uninstall-conflict");
  assert.equal(
    calls[1].options.headers["Idempotency-Key"],
    "uninstall-retry",
    "attachment conflicts must not replay the stale uninstall intent",
  );
});

test("attached missing skills remain visibly actionable without an activate action", async () => {
  const harness = createHarness({
    capabilitiesResponse: response({
      providers: { state: "ready", providers: {}, models: {} },
      skills: [{ name: "reviewer", active: false, attached: true, missing: true }],
      skill_sources: { local_roots: [], git_hosts: [] },
    }),
  });
  await flush();
  const card = harness.elements["#capability-skills"].childNodes[0];
  assert.match(nodeText(card), /Missing from the library/);
  assert.equal(findSkillActivateButton(harness.elements["#capability-skills"]), null);
});

function findButton(root, label) {
  const visit = (node) => {
    if (!node) return null;
    if (node.tagName === "BUTTON" && node.textContent === label) return node;
    for (const child of node.childNodes || []) {
      const found = visit(child);
      if (found) return found;
    }
    return null;
  };
  return visit(root);
}

function findElementByTag(root, tagName) {
  const visit = (node) => {
    if (!node) return null;
    if (node.tagName === tagName) return node;
    for (const child of node.childNodes || []) {
      const found = visit(child);
      if (found) return found;
    }
    return null;
  };
  return visit(root);
}

function allText(root) {
  return [root.textContent || "", ...(root.childNodes || []).map(allText)].join(" ");
}

test("provider credential clears and polling waits for a changed process without replay", async () => {
  const harness = createHarness({
    providerResponse: response({
      restart_required: true,
      restart: {
        scheduled: true,
        code: "restart_scheduled",
        message: "Waffle restart was scheduled.",
      },
    }, true, 202),
  });
  await flush();

  await harness.elements["#capability-provider-form"].listener("submit")({
    preventDefault() {},
  });
  await flush();

  assert.equal(harness.elements["#capability-provider-credential"].value, "");
  assert.equal(
    harness.calls.filter((call) => call.path === "/api/v1/desk/providers").length,
    1,
  );
  assert.ok(
    harness.calls.filter((call) => call.path === "/api/v1/desk/bootstrap").length >= 3,
  );
  assert.equal(harness.elements["#capability-restart-status"].hidden, true);
});

test("manual restart outcome stops polling and shows a terminal message", async () => {
  const harness = createHarness({
    providerResponse: response({
      restart_required: true,
      restart: {
        scheduled: false,
        code: "manual_restart_required",
        message: "Change committed; restart waffle serve to apply.",
      },
    }, true, 202),
    bootstrapGenerations: ["process-old"],
  });
  await flush();

  const bootstrapBefore = harness.calls.filter((call) => call.path === "/api/v1/desk/bootstrap").length;

  await harness.elements["#capability-provider-form"].listener("submit")({
    preventDefault() {},
  });
  await flush();

  const bootstrapAfter = harness.calls.filter((call) => call.path === "/api/v1/desk/bootstrap").length;
  assert.equal(bootstrapAfter, bootstrapBefore, "manual restart must not poll bootstrap");
  assert.equal(
    harness.elements["#capability-status"].textContent,
    "Change committed; restart waffle serve to apply.",
  );
  assert.equal(
    harness.elements["#capability-provider-status"].textContent,
    "Provider enrolled.",
  );
  assert.equal(harness.elements["#capability-restart-status"].hidden, false);
  assert.match(
    harness.elements["#capability-restart-status"].childNodes.map((n) => n.textContent).join(" "),
    /restart waffle serve/i,
  );
  assert.equal(
    harness.elements["#capability-provider-form"].dataset.restartLocked,
    "false",
  );
  assert.equal(harness.providerSubmit.disabled, false);
  assert.equal(
    harness.calls.some((call) => JSON.stringify(call).includes("transaction_id")),
    false,
  );
});

test("scheduled restart disables forms and skill activate while waiting and reports timeout", async () => {
  const harness = createHarness({
    providerResponse: response({
      restart_required: true,
      restart: {
        scheduled: true,
        code: "restart_scheduled",
        message: "Waffle restart was scheduled.",
      },
    }, true, 202),
    bootstrapGenerations: ["process-old"],
    skills: [{ name: "review-me", active: false }],
    clock: true,
    deferTimers: true,
  });
  await flush();

  const activate = findSkillActivateButton(harness.elements["#capability-skills"]);
  assert.ok(activate, "expected skill activate button after load");
  assert.equal(activate.disabled, false);

  const submitPromise = harness.elements["#capability-provider-form"].listener("submit")({
    preventDefault() {},
  });
  await flush();

  assert.equal(
    harness.elements["#capability-provider-form"].dataset.restartLocked,
    "true",
  );
  assert.equal(harness.providerSubmit.disabled, true);
  assert.equal(activate.disabled, true, "skill activate must lock during restart wait");
  assert.equal(harness.elements["#capability-restart-status"].hidden, false);
  assert.match(
    harness.elements["#capability-restart-status"].childNodes.map((n) => n.textContent).join(" "),
    /Waiting for a new Waffle process/i,
  );

  await harness.runTimersUntilIdle(70);
  await submitPromise;
  await flush();

  assert.match(
    harness.elements["#capability-status"].textContent,
    /did not complete in time/i,
  );
  assert.equal(harness.elements["#capability-restart-status"].hidden, false);
  assert.equal(
    harness.elements["#capability-provider-form"].dataset.restartLocked,
    "false",
  );
  assert.equal(harness.providerSubmit.disabled, false);
  assert.equal(activate.disabled, false, "skill activate re-enables after wait ends");
  assert.ok(
    harness.calls.filter((call) => call.path === "/api/v1/desk/bootstrap").length > 1,
  );
});

test("scheduled restart disables removal buttons through a NodeList-like childNodes tree", async () => {
  const harness = createHarness({
    capabilitiesResponse: response({
      providers: {
        state: "ready",
        default_model: "gpt",
        utility_model: "gpt",
        providers: { openai: { type: "openai" } },
        models: { gpt: { provider: "openai", model: "gpt-test" } },
      },
      provider_presets: [],
      skills: [],
    }),
    providerResponse: response({
      restart_required: true,
      restart: {
        scheduled: true,
        code: "restart_scheduled",
        message: "Waffle restart was scheduled.",
      },
    }, true, 202),
    bootstrapGenerations: ["process-old"],
    clock: true,
    deferTimers: true,
  });
  await flush();

  const modelCard = harness.elements["#capability-models"].childNodes[0];
  const remove = findButton(modelCard, "Remove alias");
  assert.ok(remove, "expected model removal button after capabilities load");
  const children = harness.elements["#capability-models"].childNodes;
  harness.elements["#capability-models"].childNodes = {
    0: children[0],
    length: children.length,
  };

  const submitPromise = harness.elements["#capability-provider-form"].listener("submit")({
    preventDefault() {},
  });
  await flush();

  assert.equal(remove.disabled, true, "removal button must lock during restart wait");

  await harness.runTimersUntilIdle(70);
  await submitPromise;
  await flush();
  assert.equal(remove.disabled, false, "removal button must unlock after restart wait ends");
});

test("provider credential is cleared after failure and never appears in safe UI", async () => {
  const harness = createHarness({
    providerResponse: response({
      code: "capability_failed",
      message: "capability request could not be completed",
    }, false, 400),
    uuidSequence: ["enrollment-a", "enrollment-b"],
  });
  await flush();

  await harness.elements["#capability-provider-form"].listener("submit")({
    preventDefault() {},
  });
  await flush();

  assert.equal(harness.elements["#capability-provider-credential"].value, "");
  assert.equal(
    harness.elements["#capability-provider-status"].textContent,
    "capability request could not be completed",
  );
  assert.equal(
    harness.elements["#capability-provider-status"].textContent.includes("sk-super-private"),
    false,
  );
  assert.equal(
    harness.elements["#capability-default-status"].textContent.includes("capability request could not be completed"),
    false,
    "failure must not leak into another form status",
  );

  await harness.elements["#capability-provider-form"].listener("submit")({
    preventDefault() {},
  });
  await flush();
  const enrollments = harness.calls.filter((call) => call.path === "/api/v1/desk/providers");
  assert.equal(enrollments.length, 2);
  assert.equal(enrollments[0].options.headers["Idempotency-Key"], "enrollment-a");
  assert.equal(
    enrollments[1].options.headers["Idempotency-Key"],
    "enrollment-b",
    "failed sensitive enrollment intents must not retain their idempotency key",
  );
});

test("prospective provider test sends entered inputs and clears its sensitive intent after failure", async () => {
  const prospectiveFailure = {
    then(onFulfilled) {
      return Promise.resolve(
        response({ code: "capability_failed", message: "connection test failed" }, false, 400),
      ).then(onFulfilled);
    },
  };
  const harness = createHarness({
    providerProspectiveTestResponse: prospectiveFailure,
    uuidSequence: ["prospective-a", "prospective-b"],
  });
  await flush();

  const testButton = harness.elements["#capability-provider-form"].controls.find(
    (control) => control.textContent === "Test connection",
  );
  await testButton.listener("click")();
  await flush();

  const tests = harness.calls.filter((call) => call.path === "/api/v1/desk/providers/test");
  assert.equal(tests.length, 1);
  assert.deepEqual(JSON.parse(tests[0].options.body), {
    connection_name: "openai",
    type: "openai",
    base_url: "",
    max_tokens: 0,
    model: "gpt-test",
    api_key: "sk-super-private",
  });
  assert.equal(tests[0].options.headers["Idempotency-Key"], "prospective-a");
  assert.equal(harness.elements["#capability-provider-credential"].value, "");

  await testButton.listener("click")();
  await flush();
  const repeatedTests = harness.calls.filter((call) => call.path === "/api/v1/desk/providers/test");
  assert.equal(repeatedTests.length, 2);
  assert.equal(
    repeatedTests[1].options.headers["Idempotency-Key"],
    "prospective-b",
    "failed sensitive test intents must not retain their idempotency key",
  );
});

test("catalogue refresh renders results and search filters without another request", async () => {
  const harness = createHarness({
    capabilitiesResponse: response({
      providers: { state: "ready", providers: { primary: { type: "openai" } }, models: {} },
      provider_presets: [{ name: "openai", runtime_type: "openai", requires_base_url: false }],
      skills: [],
    }),
    catalogueResponse: response({
      connection: "primary",
      models: [
        { id: "vendor/alpha", display_name: "Alpha", owner: "Vendor" },
        { id: "vendor/beta", display_name: "Beta", owner: "Vendor" },
      ],
    }),
  });
  harness.elements["#capability-catalogue-connection"].value = "primary";
  await flush();

  await harness.elements["#capability-catalogue-form"].listener("submit")({
    preventDefault() {},
  });
  await flush();

  const results = harness.elements["#capability-catalogue-results"];
  assert.equal(results.childNodes.length, 2);
  assert.equal(
    harness.elements["#capability-catalogue-summary"].textContent,
    "2 models from primary.",
  );
  assert.equal(
    harness.elements["#capability-catalogue-status"].textContent,
    "Catalogue refreshed.",
  );
  assert.equal(
    harness.calls.filter((call) => call.path === "/api/v1/desk/models/catalogue/refresh").length,
    1,
  );
  const refreshCall = harness.calls.find(
    (call) => call.path === "/api/v1/desk/models/catalogue/refresh",
  );
  assert.deepEqual(JSON.parse(refreshCall.options.body), { connection: "primary" });

  harness.elements["#capability-catalogue-search"].value = "beta";
  harness.elements["#capability-catalogue-search"].listener("input")();

  assert.equal(results.childNodes.length, 1);
  assert.equal(results.childNodes[0].childNodes[0].textContent, "Beta");
  assert.equal(
    harness.calls.filter((call) => call.path === "/api/v1/desk/models/catalogue/refresh").length,
    1,
  );
});

test("connections load read-only and render every allowlisted field as text", async () => {
  const hostileName = `<img src=x onerror="steal()">`;
  const harness = createHarness({
    connectionResponse: response([
      {
        name: hostileName,
        kind: "mcp",
        status: "configured",
        profile: "review",
        sandbox_mode: "docker",
        egress: "restricted",
        guidance: "Runs in a sandbox.",
        ignored_private_field: "sk-never-render",
      },
    ]),
  });
  await flush();

  const requests = harness.calls.filter((call) => call.path === "/api/v1/desk/connections");
  assert.equal(requests.length, 1);
  assert.equal(requests[0].options.method, "GET");
  assert.equal(requests[0].options.credentials, "same-origin");
  assert.equal(requests[0].options.cache, "no-store");

  const list = harness.elements["#capability-connections"];
  assert.equal(list.childNodes.length, 1);
  const card = list.childNodes[0];
  assert.equal(card.childNodes[0].textContent, hostileName);
  assert.equal(
    card.childNodes[1].textContent,
    "MCP · Configured · Profile review · Docker sandbox · Restricted egress",
  );
  assert.equal(card.childNodes[2].textContent, "Runs in a sandbox.");
  assert.equal(
    card.childNodes.some((node) => node.textContent.includes("sk-never-render")),
    false,
  );
});

test("github and intake connections render their allowlisted labels only", async () => {
  const harness = createHarness({
    connectionResponse: response([
      {
        name: "github",
        kind: "github",
        status: "unconfigured",
        guidance: "Configure [github.app] to give workspaces git access.",
      },
      { name: "github", kind: "github", status: "stale" },
      { name: "github", kind: "github", status: "healthy" },
      {
        name: "owner/board",
        kind: "intake",
        status: "configured",
        label: "waffle",
        concurrency: 2,
        guidance: "Issues matching this label are picked up by the issue profile.",
      },
    ]),
  });
  await flush();

  const cards = harness.elements["#capability-connections"].childNodes;
  assert.equal(cards.length, 4);
  assert.equal(cards[0].childNodes[1].textContent, "GitHub · Not configured");
  assert.equal(
    cards[0].childNodes[2].textContent,
    "Configure [github.app] to give workspaces git access.",
  );
  assert.equal(cards[1].childNodes[1].textContent, "GitHub · Stale");
  assert.equal(cards[2].childNodes[1].textContent, "GitHub · Healthy");
  assert.equal(cards[3].childNodes[0].textContent, "owner/board");
  assert.equal(
    cards[3].childNodes[1].textContent,
    "Issue intake · Configured · Label waffle · Concurrency 2",
  );
});

test("model removal previews references, requires a replacement, and posts the opaque token safely", async () => {
  const hostileReference = `<script>steal()</script>`;
  const harness = createHarness({
    capabilitiesResponse: response({
      providers: { state: "ready", default_model: "gpt", utility_model: "small", providers: { openai: { type: "openai" } }, models: {
        gpt: { provider: "openai", model: "gpt" },
        small: { provider: "openai", model: "small" },
      } },
      provider_presets: [],
      skills: [],
    }),
    modelRemovalPreviewResponse: response({
      kind: "model",
      target: "gpt",
      references: [{ kind: "default", name: "default" }, { kind: "session", name: hostileReference }],
      replacement_required: true,
      preview_token: "opaque-model-preview",
      expires_at: "2026-07-25T00:01:00Z",
    }),
    modelRemovalResponse: response({ restart_required: false }),
  });
  await flush();

  const modelCard = harness.elements["#capability-models"].childNodes.find(
    (card) => card.childNodes[0]?.textContent === "gpt",
  );
  assert.ok(modelCard, "expected gpt card");
  const remove = findButton(modelCard, "Remove alias");
  assert.ok(remove, "expected remove button");
  await remove.listener("click")();
  await flush();

  assert.equal(
    harness.calls.filter((call) => call.path === "/api/v1/desk/models/gpt/removal-preview").length,
    1,
  );
  assert.match(allText(modelCard), /default/);
  assert.match(allText(modelCard), /steal/);
  assert.equal(allText(modelCard).includes("opaque-model-preview"), false);

  const replacement = findElementByTag(modelCard, "SELECT");
  assert.ok(replacement, "expected explicit replacement picker");
  replacement.value = "small";
  const confirm = findButton(modelCard, "Remove model alias");
  assert.ok(confirm, "expected confirmation button");
  await confirm.listener("click")();
  await flush();

  const removal = harness.calls.find((call) => call.path === "/api/v1/desk/models/gpt/remove");
  assert.ok(removal, "expected removal request");
  assert.deepEqual(JSON.parse(removal.options.body), {
    preview_token: "opaque-model-preview",
    replacement: "small",
  });
});

test("failed model removal confirmation re-enables the original trigger", async () => {
  const harness = createHarness({
    capabilitiesResponse: response({
      providers: { state: "ready", default_model: "gpt", utility_model: "small", providers: { openai: { type: "openai" } }, models: {
        gpt: { provider: "openai", model: "gpt" },
        small: { provider: "openai", model: "small" },
      } },
      provider_presets: [],
      skills: [],
    }),
    modelRemovalPreviewResponse: response({
      kind: "model",
      target: "gpt",
      references: [],
      replacement_required: false,
      preview_token: "stale-model-preview",
      expires_at: "2026-07-25T00:01:00Z",
    }),
    modelRemovalResponse: response({ code: "preview_invalid" }, false, 409),
  });
  await flush();

  const modelCard = harness.elements["#capability-models"].childNodes.find(
    (card) => card.childNodes[0]?.textContent === "gpt",
  );
  const remove = findButton(modelCard, "Remove alias");
  await remove.listener("click")();
  await flush();
  const confirm = findButton(modelCard, "Remove model alias");
  await confirm.listener("click")();
  await flush();

  assert.equal(confirm.disabled, false);
  assert.equal(remove.disabled, false);
});

test("provider removal preview gives safe alias guidance and no destructive action", async () => {
  const harness = createHarness({
    connectionResponse: response([{ name: "openai", kind: "provider", status: "configured" }]),
    providerRemovalPreviewResponse: response({
      kind: "provider",
      target: "openai",
      references: [{ kind: "model_alias", name: "gpt" }],
      replacement_required: false,
      preview_token: "opaque-provider-preview",
      expires_at: "2026-07-25T00:01:00Z",
    }),
  });
  await flush();

  const card = harness.elements["#capability-connections"].childNodes[0];
  const remove = findButton(card, "Remove connection");
  assert.ok(remove, "expected provider removal button");
  await remove.listener("click")();
  await flush();

  assert.match(allText(card), /gpt/);
  assert.match(allText(card), /still referenced/i);
  assert.equal(findButton(card, "Remove provider connection"), null);
  assert.equal(
    harness.calls.some((call) => call.path.endsWith("/remove") && call.options.method === "POST"),
    false,
  );
});

test("connections use a stable accessible empty state", async () => {
  const harness = createHarness({ connectionResponse: response(null) });
  await flush();

  assert.equal(
    harness.elements["#capability-connections"].textContent,
    "No tools or connections are configured.",
  );
});

test("an older overlapping load cannot overwrite newer post-mutation connections", async () => {
  const olderConnections = deferredResponse();
  const harness = createHarness({
    providerResponse: response({ restart_required: false }),
    connectionResponses: [
      olderConnections.promise,
      response([{ name: "newer-mcp", kind: "mcp", status: "configured" }]),
    ],
  });
  await flush();

  await harness.elements["#capability-provider-form"].listener("submit")({
    preventDefault() {},
  });
  await flush();

  const list = harness.elements["#capability-connections"];
  assert.equal(list.childNodes[0].childNodes[0].textContent, "newer-mcp");
  assert.equal(harness.elements["#capability-status"].textContent, "Capabilities are current.");
  assert.equal(harness.elements["#capability-provider-status"].textContent, "Provider enrolled.");

  olderConnections.resolve([
    { name: "older-mcp", kind: "mcp", status: "configured" },
  ]);
  await flush();

  assert.equal(list.childNodes[0].childNodes[0].textContent, "newer-mcp");
  assert.equal(harness.elements["#capability-status"].textContent, "Capabilities are current.");
});

test("form failures stay on the form that caused them (AC1)", async () => {
  const harness = createHarness({
    capabilitiesResponse: response({
      providers: { state: "ready", providers: { primary: { type: "openai" } }, models: { gpt: { provider: "primary", model: "gpt-test" } } },
      provider_presets: [{ name: "openai", runtime_type: "openai", requires_base_url: false }],
      skills: [],
    }),
    providerResponse: response({
      code: "capability_failed",
      message: "provider enrollment failed",
    }, false, 400),
  });
  await flush();

  await harness.elements["#capability-provider-form"].listener("submit")({
    preventDefault() {},
  });
  await flush();

  assert.equal(
    harness.elements["#capability-provider-status"].textContent,
    "provider enrollment failed",
  );
  assert.equal(harness.elements["#capability-default-status"].textContent.includes("provider enrollment failed"), false);
  assert.equal(harness.elements["#capability-catalogue-status"].textContent.includes("provider enrollment failed"), false);

  harness.elements["#capability-default-alias"].value = "gpt";
  await harness.elements["#capability-default-form"].listener("submit")({
    preventDefault() {},
  });
  await flush();

  assert.equal(
    harness.elements["#capability-provider-status"].textContent,
    "provider enrollment failed",
    "activity in another form must not erase the first form status",
  );
  assert.equal(
    harness.elements["#capability-default-status"].textContent,
    "Waffle-wide default changed.",
  );
});

test("double submit produces exactly one in-flight request (AC5)", async () => {
  const deferred = deferredResponse();
  const harness = createHarness({
    capabilitiesResponse: response({
      providers: { state: "ready", providers: { primary: { type: "openai" } }, models: { gpt: { provider: "primary", model: "gpt-test" } } },
      provider_presets: [{ name: "openai", runtime_type: "openai", requires_base_url: false }],
      skills: [],
    }),
    providerResponse: deferred.promise,
  });
  await flush();

  const submit = harness.elements["#capability-provider-form"].listener("submit");
  const first = submit({ preventDefault() {} });
  const second = submit({ preventDefault() {} });
  await flush();

  const inFlight = harness.calls.filter((call) => call.path === "/api/v1/desk/providers");
  assert.equal(inFlight.length, 1, "double submit must not start a second request");
  assert.equal(harness.providerSubmit.disabled, true);
  assert.equal(harness.providerSubmit.textContent, "Working…");
  assert.equal(harness.elements["#capability-provider-status"].textContent, "Working…");

  deferred.resolve({ restart_required: false });
  await Promise.all([first, second]);
  await flush();

  assert.equal(
    harness.calls.filter((call) => call.path === "/api/v1/desk/providers").length,
    1,
  );
  assert.equal(harness.providerSubmit.disabled, false);
  assert.equal(harness.providerSubmit.textContent, "Enroll provider");
});

test("unchanged resubmit reuses Idempotency-Key until success (AC3)", async () => {
  let defaultHits = 0;
  const defaultResponse = {
    then(onFulfilled) {
      defaultHits += 1;
      if (defaultHits === 1) {
        return Promise.resolve(
          response({ code: "capability_failed", message: "temporary failure" }, false, 503),
        ).then(onFulfilled);
      }
      return Promise.resolve(response({ restart_required: false })).then(onFulfilled);
    },
  };
  const harness = createHarness({
    capabilitiesResponse: response({
      providers: { state: "ready", providers: { router: { type: "openai" } }, models: { gpt: { provider: "router", model: "gpt-test" } } },
      provider_presets: [{ name: "openai", runtime_type: "openai", requires_base_url: false }],
      skills: [],
    }),
    defaultResponse,
    uuidSequence: ["key-a", "key-b", "key-c"],
  });
  harness.elements["#capability-default-alias"].value = "gpt";
  await flush();

  await harness.elements["#capability-default-form"].listener("submit")({
    preventDefault() {},
  });
  await flush();

  await harness.elements["#capability-default-form"].listener("submit")({
    preventDefault() {},
  });
  await flush();

  const defaultPosts = harness.calls.filter((call) => call.path === "/api/v1/desk/models/default");
  assert.equal(defaultPosts.length, 2);
  assert.equal(defaultPosts[0].options.headers["Idempotency-Key"], "key-a");
  assert.equal(
    defaultPosts[1].options.headers["Idempotency-Key"],
    "key-a",
    "same body must reuse the prior key after failure",
  );

  // Successful path cleared the key; a later identical submit mints a new one.
  await harness.elements["#capability-default-form"].listener("submit")({
    preventDefault() {},
  });
  await flush();

  const afterSuccess = harness.calls.filter((call) => call.path === "/api/v1/desk/models/default");
  assert.equal(afterSuccess.length, 3);
  assert.equal(afterSuccess[2].options.headers["Idempotency-Key"], "key-b");
});

test("empty required field sets aria-invalid and focuses it (AC4)", async () => {
  const harness = createHarness();
  await flush();

  harness.elements["#capability-default-alias"].value = "";
  await harness.elements["#capability-default-form"].listener("submit")({
    preventDefault() {},
  });
  await flush();

  assert.equal(
    harness.elements["#capability-default-alias"].getAttribute("aria-invalid"),
    "true",
  );
  assert.equal(harness.elements["#capability-default-alias"].focused, true);
  assert.equal(
    harness.elements["#capability-default-status"].textContent,
    "Fill in the required field.",
  );
  assert.equal(
    harness.elements["#capability-default-form"].getAttribute("aria-describedby"),
    "capability-default-status",
  );
  assert.equal(
    harness.calls.filter((call) => call.path === "/api/v1/desk/models/default").length,
    0,
  );
});

test("catalogue card add keeps the exact upstream ID and marks enrolled aliases", async () => {
  const harness = createHarness({
    capabilitiesResponse: response({
      providers: { state: "ready", providers: { router: { type: "openai" } }, models: {} },
      provider_presets: [{ name: "openai", runtime_type: "openai", requires_base_url: false }],
      skills: [],
    }),
    catalogueResponse: response({
      connection: "router",
      models: [
        { id: "anthropic/claude-sonnet-4-6", alias_suggestion: "claude", enrolled_alias: "claude" },
        { id: "accounts/fireworks/models/deepseek-v3", alias_suggestion: "deepseek-v3" },
      ],
    }),
  });
  harness.elements["#capability-catalogue-connection"].value = "router";
  await flush();

  await harness.elements["#capability-catalogue-form"].listener("submit")({ preventDefault() {} });
  await flush();

  const cards = harness.elements["#capability-catalogue-results"].childNodes;
  assert.match(cards[0].childNodes.map((node) => node.textContent).join(" "), /Enrolled as claude/);
  const alias = cards[1].childNodes.find((node) => node.tagName === "INPUT");
  assert.equal(alias.value, "deepseek-v3");
  const add = cards[1].childNodes.find((node) => node.textContent === "Add as alias");
  assert.ok(add, "new catalogue model needs an add action");
  await add.listener("click")();
  await flush();

  const addCall = harness.calls.find((call) => call.path === "/api/v1/desk/models");
  assert.deepEqual(JSON.parse(addCall.options.body), {
    connection_name: "router",
    alias: "deepseek-v3",
    upstream_model: "accounts/fireworks/models/deepseek-v3",
    default: false,
    utility: false,
  });
});

test("model role and connection pickers use the latest capability snapshot", async () => {
  const harness = createHarness({
    capabilitiesResponse: response({
      providers: {
        state: "ready",
        default_model: "main",
        utility_model: "fast",
        providers: { primary: { type: "openai" }, backup: { type: "anthropic" } },
        models: {
          main: { provider: "primary", model: "gpt-main" },
          fast: { provider: "backup", model: "claude-fast" },
        },
      },
      skills: [],
    }),
  });
  await flush();

  for (const selector of [
    "#capability-default-alias",
    "#capability-utility-alias",
  ]) {
    assert.deepEqual(
      harness.elements[selector].childNodes.map((option) => option.value),
      ["fast", "main"],
      `${selector} options must come from snapshot models`,
    );
  }
  for (const selector of [
    "#capability-catalogue-connection",
    "#capability-model-connection",
  ]) {
    assert.deepEqual(
      harness.elements[selector].childNodes.map((option) => option.value),
      ["backup", "primary"],
      `${selector} options must come from snapshot providers`,
    );
  }

  const cards = harness.elements["#capability-models"].childNodes;
  const fastCard = cards.find((card) => card.childNodes[0].textContent === "fast");
  const makeDefault = fastCard.childNodes.find((node) => node.textContent === "Make default");
  await makeDefault.listener("click")();
  await flush();
  const defaultCall = harness.calls.find((call) => call.path === "/api/v1/desk/models/default");
  assert.deepEqual(JSON.parse(defaultCall.options.body), { alias: "fast" });
});

test("provider enrollment sends explicit roles and reports a redacted connection test", async () => {
  const harness = createHarness({
    providerResponse: response({ restart_required: false }),
    providerProspectiveTestResponse: response({ outcome: "authentication_failed" }),
  });
  harness.elements["#capability-provider-default"].checked = false;
  harness.elements["#capability-provider-utility"].checked = true;
  await flush();

  await harness.elements["#capability-provider-form"].listener("submit")({ preventDefault() {} });
  await flush();
  const enrollment = harness.calls.find((call) => call.path === "/api/v1/desk/providers");
  assert.deepEqual(JSON.parse(enrollment.options.body), {
    connection_name: "openai",
    type: "openai",
    base_url: "",
    max_tokens: 0,
    api_key: "sk-super-private",
    models: { gpt: { model: "gpt-test" } },
    default_model: "",
    utility_model: "gpt",
  });
  assert.equal(harness.elements["#capability-provider-credential"].value, "");

  const testButton = harness.elements["#capability-provider-form"].controls.find(
    (control) => control.textContent === "Test connection",
  );
  assert.ok(testButton, "enrollment exposes a protected connection test");
  await testButton.listener("click")();
  await flush();
  assert.equal(
    harness.calls.some((call) => call.path === "/api/v1/desk/providers/test"),
    true,
  );
  assert.match(harness.elements["#capability-provider-status"].textContent, /authentication/i);
  assert.equal(harness.elements["#capability-provider-status"].textContent.includes("sk-super-private"), false);
});
