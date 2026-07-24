import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = await readFile(new URL("./assets/capabilities.js", import.meta.url), "utf8");

class FakeElement {
  constructor() {
    this.value = "";
    this.hidden = false;
    this.disabled = false;
    this.textContent = "";
    this.dataset = {};
    this.listeners = new Map();
    this.childNodes = [];
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

function createHarness({
  providerResponse = response({}, true, 202),
  catalogueResponse = response({ connection: "primary", models: [] }),
  bootstrapGenerations = ["process-old", "process-old", "process-new"],
} = {}) {
  const selectors = [
    "#desk-capabilities",
    "#capability-status",
    "#capability-restart-status",
    "#capability-models",
    "#capability-skills",
    "#capability-provider-form",
    "#capability-provider-name",
    "#capability-provider-type",
    "#capability-provider-base-url",
    "#capability-provider-model-alias",
    "#capability-provider-model-id",
    "#capability-provider-credential",
    "#capability-catalogue-form",
    "#capability-catalogue-connection",
    "#capability-catalogue-search",
    "#capability-catalogue-summary",
    "#capability-catalogue-results",
  ];
  const elements = Object.fromEntries(selectors.map((selector) => [selector, new FakeElement()]));
  elements["#capability-provider-name"].value = "openai";
  elements["#capability-provider-type"].value = "openai";
  elements["#capability-provider-model-alias"].value = "gpt";
  elements["#capability-provider-model-id"].value = "gpt-test";
  elements["#capability-provider-credential"].value = "sk-super-private";
  const calls = [];
  let bootstrapPolls = 0;
  const fetch = async (path, options = {}) => {
    calls.push({ path, options });
    if (path === "/api/v1/desk/capabilities") {
      return response({
        providers: {
          state: "ready",
          default_model: "gpt",
          utility_model: "gpt",
          providers: {},
          models: {},
        },
        skills: [],
      });
    }
    if (path === "/api/v1/desk/providers") {
      return providerResponse;
    }
    if (path === "/api/v1/desk/models/catalogue/refresh") {
      return catalogueResponse;
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
    createElement: () => new FakeElement(),
    querySelector: (selector) => elements[selector] || null,
  };
  const context = vm.createContext({
    console,
    crypto: { randomUUID: () => "idempotency-key" },
    document,
    fetch,
    setTimeout: (callback) => {
      void callback();
      return 1;
    },
  });
  new vm.Script(source, { filename: "capabilities.js" }).runInContext(context);
  return { calls, elements };
}

async function flush() {
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
}

test("provider credential clears and polling waits for a changed process without replay", async () => {
  const harness = createHarness({
    providerResponse: response({
      restart_required: true,
      transaction_id: "txn-1",
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

test("provider credential is cleared after failure and never appears in safe UI", async () => {
  const harness = createHarness({
    providerResponse: response({
      code: "capability_failed",
      message: "capability request could not be completed",
    }, false, 400),
  });
  await flush();

  await harness.elements["#capability-provider-form"].listener("submit")({
    preventDefault() {},
  });
  await flush();

  assert.equal(harness.elements["#capability-provider-credential"].value, "");
  assert.equal(
    harness.elements["#capability-status"].textContent,
    "capability request could not be completed",
  );
  assert.equal(
    harness.elements["#capability-status"].textContent.includes("sk-super-private"),
    false,
  );
});

test("catalogue refresh renders results and search filters without another request", async () => {
  const harness = createHarness({
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
