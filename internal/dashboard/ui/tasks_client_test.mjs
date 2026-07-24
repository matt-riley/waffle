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

function createHarness() {
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
  };
  const fetch = async (path, options = {}) => {
    calls.push({ path, options });
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
