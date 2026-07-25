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
  catalogueResponse = response({ connection: "primary", models: [] }),
  connectionResponse = response([]),
  connectionResponses = null,
  bootstrapGenerations = ["process-old", "process-old", "process-new"],
  deferTimers = false,
  clock = false,
} = {}) {
  const selectors = [
    "#desk-capabilities",
    "#capability-status",
    "#capability-restart-status",
    "#capability-models",
    "#capability-skills",
    "#capability-connections",
    "#capability-provider-form",
    "#capability-provider-name",
    "#capability-provider-type",
    "#capability-provider-base-url",
    "#capability-provider-model-alias",
    "#capability-provider-model-id",
    "#capability-provider-credential",
    "#capability-default-form",
    "#capability-default-alias",
    "#capability-utility-form",
    "#capability-utility-alias",
    "#capability-model-form",
    "#capability-model-connection",
    "#capability-model-alias",
    "#capability-model-id",
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
  const providerSubmit = new FakeElement();
  providerSubmit.type = "submit";
  elements["#capability-provider-form"].controls = [
    elements["#capability-provider-name"],
    elements["#capability-provider-type"],
    elements["#capability-provider-base-url"],
    elements["#capability-provider-model-alias"],
    elements["#capability-provider-model-id"],
    elements["#capability-provider-credential"],
    providerSubmit,
  ];
  elements["#capability-default-form"].controls = [elements["#capability-default-alias"]];
  elements["#capability-utility-form"].controls = [elements["#capability-utility-alias"]];
  elements["#capability-model-form"].controls = [
    elements["#capability-model-connection"],
    elements["#capability-model-alias"],
    elements["#capability-model-id"],
  ];
  const calls = [];
  const timers = [];
  let bootstrapPolls = 0;
  let connectionFetches = 0;
  let nowMs = 0;
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

test("provider credential clears and polling waits for a changed process without replay", async () => {
  const harness = createHarness({
    providerResponse: response({
      restart_required: true,
      transaction_id: "txn-1",
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
      transaction_id: "txn-manual",
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
    harness.calls.some((call) => JSON.stringify(call).includes("txn-manual")),
    false,
  );
});

test("scheduled restart disables forms while waiting and reports timeout", async () => {
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
    clock: true,
    deferTimers: true,
  });
  await flush();

  const submitPromise = harness.elements["#capability-provider-form"].listener("submit")({
    preventDefault() {},
  });
  await flush();

  assert.equal(
    harness.elements["#capability-provider-form"].dataset.restartLocked,
    "true",
  );
  assert.equal(harness.providerSubmit.disabled, true);
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
  assert.ok(
    harness.calls.filter((call) => call.path === "/api/v1/desk/bootstrap").length > 1,
  );
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

  olderConnections.resolve([
    { name: "older-mcp", kind: "mcp", status: "configured" },
  ]);
  await flush();

  assert.equal(list.childNodes[0].childNodes[0].textContent, "newer-mcp");
  assert.equal(harness.elements["#capability-status"].textContent, "Capabilities are current.");
});
