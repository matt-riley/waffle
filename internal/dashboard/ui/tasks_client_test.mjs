import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = await readFile(new URL("./assets/tasks.js", import.meta.url), "utf8");

class FakeElement {
  constructor(tagName = "div") {
    this.tagName = tagName.toUpperCase();
    this.children = [];
    this.listeners = new Map();
    this.dataset = {};
    this.attributes = new Map();
    this.className = "";
    this.hidden = false;
    this.value = "";
    this.checked = false;
    this.disabled = false;
    this._textContent = "";
  }

  get textContent() {
    return this._textContent + this.children.map((child) => child.textContent).join("");
  }

  set textContent(value) {
    this._textContent = String(value);
    this.children = [];
  }

  append(...children) {
    this.children.push(...children);
  }

  replaceChildren(...children) {
    this.children = [...children];
    this._textContent = "";
  }

  addEventListener(type, listener) {
    const listeners = this.listeners.get(type) || [];
    listeners.push(listener);
    this.listeners.set(type, listeners);
  }

  async emit(type) {
    const event = { preventDefault() {} };
    for (const listener of this.listeners.get(type) || []) {
      await listener(event);
    }
  }

  setAttribute(name, value) {
    this.attributes.set(name, String(value));
    if (name === "href") {
      this.href = String(value);
    }
  }

  getAttribute(name) {
    return this.attributes.get(name) || null;
  }
}

function jsonResponse(body, ok = true) {
  return {
    ok,
    async json() {
      return body;
    },
  };
}

function descendants(element) {
  return [element, ...element.children.flatMap(descendants)];
}

async function settle() {
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

function createHarness({ snapshotOverrides = {}, fetchOverride } = {}) {
  const selectors = [
    ".tasks",
    "#tasks-attention-count",
    "#tasks-list",
    "#tasks-errors",
    "#tasks-empty",
    "#task-schedule-form",
    "#task-schedule-id",
    "#task-schedule-name",
    "#task-schedule-cron",
    "#task-schedule-prompt",
    "#task-schedule-deliver",
    "#task-schedule-profile",
    "#task-schedule-enabled",
    "#task-schedule-enabled-row",
    "#task-schedule-cancel",
    "#task-schedule-submit",
    "#task-schedule-status",
  ];
  const elements = Object.fromEntries(selectors.map((selector) => [selector, new FakeElement()]));
  const filters = ["all", "active", "scheduled", "completed", "attention"].map((value) => {
    const button = new FakeElement("button");
    button.dataset.taskFilter = value;
    return button;
  });
  const calls = [];
  const snapshots = {
    all: {
      filter: "all",
      attention_count: 1,
      errors: [],
      tasks: [
        {
          id: "run-live",
          kind: "active",
          source: "cron",
          phase: "agent",
          session: "session-live",
          elapsed_ms: 1250,
          usage: { input_tokens: 20, output_tokens: 10 },
          evidence_label: "Running now",
          attention: false,
          open_at_desk: true,
        },
        {
          id: "run-failed",
          kind: "recent",
          source: "cron",
          outcome: "failed",
          runtime_ms: 800,
          usage: { input_tokens: 5, output_tokens: 2 },
          evidence_label: "Run needs attention",
          attention: true,
        },
      ],
    },
    attention: {
      filter: "attention",
      attention_count: 1,
      errors: [],
      tasks: [
        {
          id: "run-failed",
          kind: "recent",
          source: "cron",
          outcome: "failed",
          usage: { input_tokens: 5, output_tokens: 2 },
          evidence_label: "Run needs attention",
          attention: true,
        },
      ],
    },
    ...snapshotOverrides,
  };
  const fetch = async (path, options = {}) => {
    calls.push({ path, options });
    if (fetchOverride) {
      return fetchOverride(path, options);
    }
    if (options.method === "POST") {
      return jsonResponse({ task: { id: "job-new" } });
    }
    const filter = new URL(path, "http://desk.test").searchParams.get("filter") || "all";
    return jsonResponse(snapshots[filter] || { filter, attention_count: 0, errors: [], tasks: [] });
  };
  const document = {
    body: { dataset: { requestToken: "request-token" } },
    createElement: (tagName) => new FakeElement(tagName),
    querySelector: (selector) => elements[selector] || null,
    querySelectorAll: (selector) => selector === "[data-task-filter]" ? filters : [],
  };
  let key = 0;
  const context = vm.createContext({
    console,
    crypto: { randomUUID: () => `intent-${++key}` },
    AbortController,
    document,
    fetch,
    URL,
  });
  new vm.Script(source, { filename: "tasks.js" }).runInContext(context);
  return { calls, elements, filters };
}

test("renders attention evidence and only eligible Open at Desk links", async () => {
  const harness = createHarness();
  await settle();

  assert.equal(harness.calls[0].path, "/api/v1/desk/tasks?filter=all");
  assert.equal(harness.elements["#tasks-attention-count"].textContent, "1 needs attention");
  assert.equal(harness.elements["#tasks-list"].children.length, 2);
  const rendered = descendants(harness.elements["#tasks-list"]);
  const openLinks = rendered.filter((element) => element.dataset.action === "open-at-desk");
  assert.equal(openLinks.length, 1);
  assert.equal(openLinks[0].href, "/desk/?section=today&session_id=session-live");
  assert.match(harness.elements["#tasks-list"].textContent, /Run needs attention/);
});

test("filters tasks and sends protected create mutations with a fresh intent key", async () => {
  const harness = createHarness();
  await settle();
  const attention = harness.filters.find((button) => button.dataset.taskFilter === "attention");
  await attention.emit("click");

  assert.equal(harness.calls.at(-1).path, "/api/v1/desk/tasks?filter=attention");
  assert.equal(attention.getAttribute("aria-pressed"), "true");
  assert.equal(harness.elements["#tasks-list"].children.length, 1);

  harness.elements["#task-schedule-name"].value = "Morning brief";
  harness.elements["#task-schedule-cron"].value = "0 9 * * *";
  harness.elements["#task-schedule-prompt"].value = "Summarize";
  harness.elements["#task-schedule-deliver"].value = "telegram:900";
  harness.elements["#task-schedule-profile"].value = "researcher";
  await harness.elements["#task-schedule-form"].emit("submit");

  const mutation = harness.calls.find((call) => call.options.method === "POST");
  assert.equal(mutation.path, "/api/v1/desk/tasks/schedules");
  assert.equal(mutation.options.headers["X-Waffle-Desk-Token"], "request-token");
  assert.equal(mutation.options.headers["Idempotency-Key"], "intent-1");
  assert.deepEqual(
    JSON.parse(mutation.options.body),
    {
      name: "Morning brief",
      cron: "0 9 * * *",
      prompt: "Summarize",
      deliver: "telegram:900",
      profile: "researcher",
    },
  );
  assert.equal(harness.elements["#task-schedule-status"].textContent, "Schedule created.");
  assert.equal(harness.calls.at(-1).path, "/api/v1/desk/tasks?filter=attention");
});

test("redacted editable fields require re-entry and are never posted as placeholders", async () => {
  const harness = createHarness({
    snapshotOverrides: {
      all: {
        filter: "all",
        attention_count: 0,
        errors: [],
        tasks: [{
          id: "job-sensitive",
          kind: "schedule",
          name: "Sensitive",
          source: "schedule",
          cron: "0 9 * * *",
          prompt: "Summarize [redacted]",
          deliver: "telegram:[redacted]",
          enabled: true,
          redacted_fields: ["prompt", "deliver"],
          usage: { input_tokens: 0, output_tokens: 0 },
          retry: { attempt: 0, max_attempts: 1 },
          evidence_label: "Not run yet",
        }],
      },
    },
  });
  await settle();
  const edit = descendants(harness.elements["#tasks-list"])
    .find((element) => element.dataset.action === "edit-schedule");
  await edit.emit("click");

  assert.equal(harness.elements["#task-schedule-prompt"].value, "");
  assert.equal(harness.elements["#task-schedule-deliver"].value, "");
  harness.elements["#task-schedule-name"].value = "Renamed";
  await harness.elements["#task-schedule-form"].emit("submit");
  assert.equal(harness.calls.filter((call) => call.options.method === "POST").length, 0);
  assert.match(harness.elements["#task-schedule-status"].textContent, /re-enter/i);
  assert.doesNotMatch(
    JSON.stringify(harness.calls),
    /\[redacted\]/,
  );
});

test("newer task filters abort and supersede late responses", async () => {
  const all = deferred();
  const attention = deferred();
  const harness = createHarness({
    fetchOverride(path) {
      const filter = new URL(path, "http://desk.test").searchParams.get("filter");
      return filter === "attention" ? attention.promise : all.promise;
    },
  });
  await settle();
  const attentionButton = harness.filters
    .find((button) => button.dataset.taskFilter === "attention");
  const click = attentionButton.emit("click");
  await settle();

  assert.equal(harness.calls.length, 2);
  assert.equal(harness.calls[0].options.signal.aborted, true);
  attention.resolve(jsonResponse({
    filter: "attention",
    attention_count: 1,
    errors: [],
    tasks: [{
      id: "new-filter-result",
      kind: "recent",
      source: "cron",
      outcome: "failed",
      attention: true,
      usage: { input_tokens: 1, output_tokens: 1 },
      retry: {},
      evidence_label: "Needs attention",
    }],
  }));
  await click;
  assert.equal(harness.elements["#tasks-list"].children[0].dataset.taskId, "new-filter-result");

  all.resolve(jsonResponse({
    filter: "all",
    attention_count: 0,
    errors: [],
    tasks: [{
      id: "stale-all-result",
      kind: "recent",
      source: "cron",
      outcome: "ok",
      usage: { input_tokens: 0, output_tokens: 0 },
      retry: {},
      evidence_label: "Completed",
    }],
  }));
  await settle();
  assert.equal(harness.elements["#tasks-list"].children[0].dataset.taskId, "new-filter-result");
  assert.equal(attentionButton.getAttribute("aria-pressed"), "true");
});

test("a failed replacement filter clears cards from the prior filter", async () => {
  const attention = deferred();
  const harness = createHarness({
    fetchOverride(path) {
      const filter = new URL(path, "http://desk.test").searchParams.get("filter");
      if (filter === "attention") {
        return attention.promise;
      }
      return jsonResponse({
        filter: "all",
        attention_count: 0,
        errors: [],
        tasks: [{
          id: "old-all-result",
          kind: "recent",
          source: "cron",
          outcome: "ok",
          usage: { input_tokens: 0, output_tokens: 0 },
          retry: {},
          evidence_label: "Completed",
        }],
      });
    },
  });
  await settle();
  assert.equal(harness.elements["#tasks-list"].children.length, 1);

  const attentionButton = harness.filters
    .find((button) => button.dataset.taskFilter === "attention");
  const click = attentionButton.emit("click");
  await settle();
  assert.equal(harness.elements["#tasks-list"].children.length, 0);
  const failure = new Error("request_failed");
  failure.safeMessage = "Attention evidence is unavailable.";
  attention.reject(failure);
  await click;

  assert.equal(harness.elements["#tasks-list"].children.length, 0);
  assert.equal(harness.elements["#tasks-errors"].hidden, false);
  assert.equal(
    harness.elements["#tasks-errors"].textContent,
    "Attention evidence is unavailable.",
  );
});
