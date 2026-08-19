import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = await readFile(new URL("./assets/app.js", import.meta.url), "utf8");
const bootSource = await readFile(new URL("./assets/theme-boot.js", import.meta.url), "utf8").catch(() => "");

class FakeElement {
  constructor() {
    this.dataset = {};
    this.className = "";
    this._textContent = "";
    this.attributes = new Map();
    this.listeners = new Map();
    this.value = "";
  }

  get textContent() {
    return this._textContent;
  }

  set textContent(value) {
    this._textContent = String(value);
  }

  setAttribute(name, value) {
    this.attributes.set(name, String(value));
  }

  getAttribute(name) {
    return this.attributes.get(name);
  }

  addEventListener(type, listener) {
    const listeners = this.listeners.get(type) || [];
    listeners.push(listener);
    this.listeners.set(type, listeners);
  }

  dispatchEvent(type) {
    for (const listener of this.listeners.get(type) || []) {
      listener({ target: this });
    }
  }
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, reject, resolve };
}

function jsonResponse(body, ok = true) {
  return {
    ok,
    async json() {
      return body;
    },
  };
}

function createHarness({
  bootstrap = {
    version: "test",
    request_token: "token",
    event_cursor: 1,
    health: { healthy: true },
  },
  capabilities = {
    providers: { default_model: "gpt-desk" },
  },
  bootstrapError = false,
  capabilitiesError = false,
  activeSection = "tasks",
  storageSetError = false,
} = {}) {
  const elements = {
    "#rail-status": new FakeElement(),
    "#rail-status-dot": new FakeElement(),
    "#rail-connection": new FakeElement(),
    "#rail-model": new FakeElement(),
    "#desk-theme": Object.assign(new FakeElement(), { value: "system" }),
    ".desk-shell": Object.assign(new FakeElement(), {
      dataset: { activeSection },
    }),
  };
  const documentElement = new FakeElement();
  const stored = new Map();
  const mediaListeners = [];
  elements["#rail-connection"].textContent = "Connecting…";
  elements["#rail-model"].textContent = "—";
  elements["#rail-status"].dataset.connectionState = "connecting";
  elements["#rail-status-dot"].className = "status-dot is-connecting";

  const calls = [];
  const bootstrapGate = deferred();
  const capabilitiesGate = deferred();

  const fetch = async (path, options) => {
    calls.push({ path, options });
    if (path === "/api/v1/desk/bootstrap") {
      await bootstrapGate.promise;
      if (bootstrapError) {
        return jsonResponse({ message: "bootstrap unavailable" }, false);
      }
      return jsonResponse(bootstrap);
    }
    if (path === "/api/v1/desk/capabilities") {
      await capabilitiesGate.promise;
      if (capabilitiesError) {
        return jsonResponse({ message: "capabilities unavailable" }, false);
      }
      return jsonResponse(capabilities);
    }
    return jsonResponse({});
  };

  const document = {
    querySelector: (selector) => elements[selector] || null,
    documentElement,
  };
  const importMetaURL = "http://127.0.0.1/desk/assets/app.js?v=abc";

  // import.meta and dynamic import are not available in the vm harness.
  const rewritten = source
    .replaceAll("import.meta.url", JSON.stringify(importMetaURL))
    .replaceAll("void import(moduleURL.href)", "void Promise.resolve()");

  const context = vm.createContext({
    console,
    document,
    fetch,
    localStorage: {
      getItem: (key) => stored.get(key) ?? null,
      setItem: (key, value) => {
        if (storageSetError) {
          throw new Error("storage denied");
        }
        stored.set(key, String(value));
      },
    },
    window: {
      matchMedia: () => ({
        matches: false,
        addEventListener: (type, listener) => {
          if (type === "change") {
            mediaListeners.push(listener);
          }
        },
      }),
    },
    URL,
  });
  new vm.Script(rewritten, { filename: "app.js" }).runInContext(context);

  return {
    bootstrapGate,
    capabilitiesGate,
    calls,
    elements,
    documentElement,
    stored,
    changeSystemTheme: (matches) => {
      for (const listener of mediaListeners) {
        listener({ matches });
      }
    },
    rail: () => context.waffleDeskRail,
  };
}

function runThemeBoot({ stored, prefersDark, storageGetError = false }) {
  const attributes = new Map();
  const storage = {
    getItem: () => {
      if (storageGetError) {
        throw new Error("storage denied");
      }
      return stored;
    },
  };
  const context = vm.createContext({
    document: {
      documentElement: {
        setAttribute: (name, value) => attributes.set(name, value),
      },
    },
    localStorage: storage,
    window: {
      matchMedia: () => ({ matches: prefersDark }),
    },
  });
  new vm.Script(bootSource, { filename: "theme-boot.js" }).runInContext(context);
  return Object.fromEntries(attributes);
}

test("theme boot resolves system and invalid preferences before paint", () => {
  assert.deepEqual(runThemeBoot({ stored: "system", prefersDark: true }), {
    "data-theme": "dark",
    "data-theme-preference": "system",
  });
  assert.deepEqual(runThemeBoot({ stored: "dark", prefersDark: false }), {
    "data-theme": "dark",
    "data-theme-preference": "dark",
  });
  for (const stored of ["not-a-theme", "toString", "constructor", "__proto__"]) {
    assert.deepEqual(runThemeBoot({ stored, prefersDark: true }), {
      "data-theme": "dark",
      "data-theme-preference": "system",
    });
  }
});

test("theme control persists the preference and updates document attributes", () => {
  const harness = createHarness();

  harness.elements["#desk-theme"].value = "dark";
  harness.elements["#desk-theme"].dispatchEvent("change");

  assert.equal(harness.stored.get("waffle.desk.theme"), "dark");
  assert.equal(harness.documentElement.getAttribute("data-theme"), "dark");
  assert.equal(harness.documentElement.getAttribute("data-theme-preference"), "dark");
});

test("theme boot falls back to system when storage reads throw", () => {
  assert.deepEqual(runThemeBoot({ storageGetError: true, prefersDark: true }), {
    "data-theme": "dark",
    "data-theme-preference": "system",
  });
});

test("theme control applies in memory when storage writes throw", () => {
  const harness = createHarness({ storageSetError: true });

  harness.elements["#desk-theme"].value = "dark";
  harness.elements["#desk-theme"].dispatchEvent("change");

  assert.equal(harness.stored.has("waffle.desk.theme"), false);
  assert.equal(harness.documentElement.getAttribute("data-theme"), "dark");
  assert.equal(harness.documentElement.getAttribute("data-theme-preference"), "dark");
});

test("system theme changes update the document only while system is selected", () => {
  const harness = createHarness();
  const theme = harness.elements["#desk-theme"];

  theme.value = "system";
  theme.dispatchEvent("change");
  harness.changeSystemTheme(true);
  assert.equal(harness.documentElement.getAttribute("data-theme"), "dark");

  theme.value = "light";
  theme.dispatchEvent("change");
  harness.changeSystemTheme(true);
  assert.equal(harness.documentElement.getAttribute("data-theme"), "light");
});

async function flush() {
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
}

test("rail hydrates connected health and waffle-wide default model", async () => {
  const harness = createHarness();
  assert.equal(harness.elements["#rail-connection"].textContent, "Connecting…");

  harness.bootstrapGate.resolve();
  await flush();
  assert.equal(harness.elements["#rail-connection"].textContent, "Connected");
  assert.equal(harness.elements["#rail-status"].dataset.connectionState, "connected");
  assert.equal(harness.elements["#rail-status-dot"].className, "status-dot is-connected");
  assert.match(
    harness.elements["#rail-status"].getAttribute("aria-label"),
    /Connected/,
  );

  harness.capabilitiesGate.resolve();
  await flush();
  assert.equal(harness.elements["#rail-model"].textContent, "gpt-desk · waffle-wide");
  assert.equal(harness.elements["#rail-model"].dataset.modelScope, "waffle-wide");
  assert.match(
    harness.elements["#rail-status"].getAttribute("aria-label"),
    /Waffle-wide default/,
  );
});

test("rail reflects degraded process health from bootstrap", async () => {
  const harness = createHarness({
    bootstrap: {
      version: "test",
      request_token: "token",
      event_cursor: 1,
      health: { healthy: false },
    },
  });
  harness.bootstrapGate.resolve();
  harness.capabilitiesGate.resolve();
  await flush();

  assert.equal(harness.elements["#rail-connection"].textContent, "Degraded");
  assert.equal(harness.elements["#rail-status"].dataset.connectionState, "degraded");
  assert.equal(harness.elements["#rail-status-dot"].className, "status-dot is-degraded");
  assert.match(
    harness.elements["#rail-status"].getAttribute("aria-label"),
    /Degraded/,
  );
});

test("rail updates when connection state changes after hydration", async () => {
  const harness = createHarness();
  harness.bootstrapGate.resolve();
  harness.capabilitiesGate.resolve();
  await flush();

  const rail = harness.rail();
  assert.ok(rail, "shared rail API must be published");

  rail.setConnection(rail.connectionStates.disconnected);
  assert.equal(harness.elements["#rail-connection"].textContent, "Disconnected");
  assert.equal(harness.elements["#rail-status"].dataset.connectionState, "disconnected");
  assert.equal(harness.elements["#rail-status-dot"].className, "status-dot is-disconnected");
  assert.match(
    harness.elements["#rail-status"].getAttribute("aria-label"),
    /Disconnected/,
  );

  rail.setConnection(rail.connectionStates.connected);
  assert.equal(harness.elements["#rail-connection"].textContent, "Connected");
  assert.equal(harness.elements["#rail-status"].dataset.connectionState, "connected");
  assert.equal(harness.elements["#rail-status-dot"].className, "status-dot is-connected");
});

test("session model wins over waffle-wide default", async () => {
  const harness = createHarness();
  harness.bootstrapGate.resolve();
  harness.capabilitiesGate.resolve();
  await flush();

  const rail = harness.rail();
  rail.setModel("session-claude", rail.modelScopes.session);
  assert.equal(harness.elements["#rail-model"].textContent, "session-claude · session");
  assert.equal(harness.elements["#rail-model"].dataset.modelScope, "session");
  assert.match(
    harness.elements["#rail-status"].getAttribute("aria-label"),
    /this conversation/,
  );

  rail.setModel("gpt-desk", rail.modelScopes.waffleWide);
  assert.equal(
    harness.elements["#rail-model"].textContent,
    "session-claude · session",
    "waffle-wide default must not overwrite an active session model",
  );
});

test("failed bootstrap marks the rail disconnected", async () => {
  const harness = createHarness({ bootstrapError: true });
  harness.bootstrapGate.resolve();
  harness.capabilitiesGate.resolve();
  await flush();

  assert.equal(harness.elements["#rail-connection"].textContent, "Disconnected");
  assert.equal(harness.elements["#rail-status"].dataset.connectionState, "disconnected");
  assert.equal(harness.elements["#rail-status-dot"].className, "status-dot is-disconnected");
  assert.equal(
    harness.calls.some((call) => call.path === "/api/v1/desk/capabilities"),
    false,
    "capabilities fetch is skipped after bootstrap failure",
  );
});

test("Today palette keeps Ctrl/Cmd+K global and offers a conversation-filter focus command", () => {
  assert.match(source, /Find a conversation/);
  assert.match(source, /waffle:find-conversation/);
  assert.match(source, /ctrlKey|metaKey/);
});

test("Today presentation loads before today.js with the shared asset version", () => {
  assert.match(source, /session-presentation\.mjs/);
  assert.match(source, /presentationURL\.searchParams\.set\("v", version\)/);
  assert.match(
    source,
    /import\(presentationURL\.href\)\.then\(\(presentation\) => \{[\s\S]*globalThis\.waffleSessionPresentation = presentation;[\s\S]*return import\(moduleURL\.href\);/,
  );
});
