import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = await readFile(new URL("./assets/today.js", import.meta.url), "utf8");

class FakeElement {
  constructor(tagName = "div") {
    this.tagName = tagName.toUpperCase();
    this.childNodes = [];
    this.parentNode = null;
    this.listeners = new Map();
    this.dataset = {};
    this.className = "";
    this.hidden = false;
    this.disabled = false;
    this.value = "";
    this.selected = false;
    this._textContent = "";
    this.classList = {
      toggle: (name, force) => {
        const classes = new Set(this.className.split(/\s+/).filter(Boolean));
        if (force) {
          classes.add(name);
        } else {
          classes.delete(name);
        }
        this.className = [...classes].join(" ");
      },
    };
  }

  get textContent() {
    return this._textContent + this.childNodes.map((child) => child.textContent).join("");
  }

  set textContent(value) {
    this._textContent = String(value);
    this.childNodes = [];
  }

  append(...children) {
    for (const child of children) {
      this.appendChild(child);
    }
  }

  appendChild(child) {
    child.parentNode = this;
    this.childNodes.push(child);
    return child;
  }

  insertBefore(child, before) {
    const index = this.childNodes.indexOf(before);
    if (index === -1) {
      return this.appendChild(child);
    }
    child.parentNode = this;
    this.childNodes.splice(index, 0, child);
    return child;
  }

  remove() {
    if (!this.parentNode) {
      return;
    }
    const index = this.parentNode.childNodes.indexOf(this);
    if (index !== -1) {
      this.parentNode.childNodes.splice(index, 1);
    }
    this.parentNode = null;
  }

  hasChildNodes() {
    return this.childNodes.length > 0;
  }

  querySelector(selector) {
    if (selector === ".message-body") {
      return this.childNodes.find((child) => child.className === "message-body") || null;
    }
    return null;
  }

  addEventListener(type, listener) {
    const listeners = this.listeners.get(type) || [];
    listeners.push(listener);
    this.listeners.set(type, listeners);
  }

  listener(type) {
    return this.listeners.get(type)?.[0];
  }

  focus() {
    this.focused = true;
  }

  scrollIntoView() {}
}

class FakeTextNode extends FakeElement {
  constructor(text) {
    super("#text");
    this._textContent = String(text);
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
  href = "http://127.0.0.1/desk/?section=today",
  bootstrap = {
    version: "test-version",
    request_token: "fresh-token",
    event_cursor: 42,
    health: {},
    status: {},
  },
} = {}) {
  const selectors = [
    ".desk-shell",
    "#desk-session-title",
    "#desk-connection",
    "#desk-connection-text",
    "#desk-connection-detail",
    "#desk-stale-status",
    "#desk-stale-message",
    "#desk-refresh",
    "#desk-phase",
    "#desk-transcript",
    "#desk-empty-transcript",
    "#desk-tool-activity",
    "#desk-empty-activity",
    "#desk-composer",
    "#desk-message",
    "#desk-send",
    "#desk-cancel",
    "#desk-model",
    "#desk-skill",
    "#desk-skill-toggle",
    "#desk-skill-status",
    "#desk-profile",
    "#desk-workspace",
    "#desk-provider",
  ];
  const elements = Object.fromEntries(selectors.map((selector) => [selector, new FakeElement()]));
  elements["#desk-transcript"].appendChild(elements["#desk-empty-transcript"]);
  elements["#desk-tool-activity"].appendChild(elements["#desk-empty-activity"]);

  const document = {
    body: { dataset: { requestToken: "stale-token" } },
    createElement: (tagName) => new FakeElement(tagName),
    createTextNode: (text) => new FakeTextNode(text),
    querySelector: (selector) => elements[selector] || null,
  };
  const railCalls = [];
  const waffleDeskRail = {
    connectionStates: {
      connecting: "connecting",
      connected: "connected",
      degraded: "degraded",
      disconnected: "disconnected",
    },
    modelScopes: {
      session: "session",
      waffleWide: "waffle-wide",
    },
    setConnection(state) {
      railCalls.push({ kind: "connection", state });
    },
    setModel(alias, scope) {
      railCalls.push({ kind: "model", alias, scope });
    },
  };
  const calls = [];
  const cancelResponse = deferred();
  const turnResponse = deferred();
  const fetch = async (path, options) => {
    calls.push({ path, options });
    if (path === "/api/v1/desk/bootstrap") {
      return jsonResponse(bootstrap);
    }
    if (path === "/api/v1/desk/chat/open") {
      return jsonResponse({
        client_id: "client-1",
        state: {
          session_id: "session-1",
          title: "",
          connection_mode: "Shared session",
          profile: "default",
          workspace: "No workspace",
          provider_label: "Test provider",
          model_alias: "old-model",
          models: [{ alias: "old-model", provider: "test", current: true }],
          skills: [{ name: "review", description: "Review changes", attached: false }],
          history: [],
        },
      });
    }
    if (path === "/api/v1/desk/chat/command") {
      return jsonResponse({
        state: {
          session_id: "session-1",
          title: "",
          connection_mode: "Shared session",
          profile: "default",
          workspace: "No workspace",
          provider_label: "Test provider",
          model_alias: "old-model",
          models: [{ alias: "old-model", provider: "test", current: true }],
          skills: [{ name: "review", description: "Review changes", attached: true }],
        },
      });
    }
    if (path === "/api/v1/desk/chat/turn") {
      return turnResponse.promise;
    }
    if (path === "/api/v1/desk/chat/cancel") {
      return cancelResponse.promise;
    }
    return jsonResponse({});
  };

  class FakeEventSource {
    static instances = [];

    constructor(url) {
      this.url = url;
      this.listeners = new Map();
      this.closed = false;
      FakeEventSource.instances.push(this);
    }

    addEventListener(type, listener) {
      const listeners = this.listeners.get(type) || [];
      listeners.push(listener);
      this.listeners.set(type, listeners);
    }

    emit(type, envelope) {
      const event = { data: JSON.stringify(envelope) };
      for (const listener of this.listeners.get(type) || []) {
        listener(event);
      }
    }

    close() {
      this.closed = true;
    }
  }

  const context = vm.createContext({
    console,
    crypto: { randomUUID: () => `key-${calls.length}` },
    document,
    EventSource: FakeEventSource,
    fetch,
    location: { href },
    URL,
    waffleDeskRail,
  });
  new vm.Script(source, { filename: "today.js" }).runInContext(context);

  return {
    calls,
    cancelResponse,
    elements,
    EventSource: FakeEventSource,
    railCalls,
    turnResponse,
  };
}

async function flush() {
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
}

function mutationCalls(harness, path) {
  return harness.calls.filter((call) => call.path === path);
}

test("bootstrap replaces stale in-memory authority and seeds the native event cursor", async () => {
  const harness = createHarness();
  await flush();

  const [open] = mutationCalls(harness, "/api/v1/desk/chat/open");
  assert.equal(open.options.headers["X-Waffle-Desk-Token"], "fresh-token");
  assert.equal(harness.EventSource.instances.length, 1);
  assert.equal(harness.EventSource.instances[0].url, "/api/v1/desk/events?after=42");
  assert.deepEqual(
    harness.railCalls.filter((call) => call.kind === "model"),
    [{ kind: "model", alias: "old-model", scope: "session" }],
  );
  assert.equal(
    harness.railCalls.some(
      (call) => call.kind === "connection" && call.state === "connected",
    ),
    true,
  );
});

test("rail receives disconnected when the live stream closes", async () => {
  const harness = createHarness();
  await flush();
  harness.railCalls.length = 0;

  harness.EventSource.instances[0].emit("error", {});
  await flush();

  assert.equal(harness.elements[".desk-shell"].dataset.phase, "disconnected");
  assert.deepEqual(harness.railCalls.at(-1), {
    kind: "connection",
    state: "disconnected",
  });
});

test("open at desk selects exactly one requested persisted session", async () => {
  const harness = createHarness({
    href: "http://127.0.0.1/desk/?section=today&session_id=session-live",
  });
  await flush();

  const [open] = mutationCalls(harness, "/api/v1/desk/chat/open");
  assert.deepEqual(JSON.parse(open.options.body), {
    capabilities: [],
    continue: false,
    profile: "",
    session_id: "session-live",
  });
});

test("invalid bootstrap request token prevents authenticated mutations", async () => {
  const harness = createHarness({
    bootstrap: {
      version: "test-version",
      request_token: "",
      event_cursor: 42,
      health: {},
      status: {},
    },
  });
  await flush();

  assert.equal(mutationCalls(harness, "/api/v1/desk/chat/open").length, 0);
  assert.equal(harness.elements[".desk-shell"].dataset.phase, "disconnected");
});

test("invalid bootstrap event cursor prevents opening an unresumable stream", async () => {
  const harness = createHarness({
    bootstrap: {
      version: "test-version",
      request_token: "fresh-token",
      event_cursor: -1,
      health: {},
      status: {},
    },
  });
  await flush();

  assert.equal(mutationCalls(harness, "/api/v1/desk/chat/open").length, 0);
  assert.equal(harness.EventSource.instances.length, 0);
  assert.equal(harness.elements[".desk-shell"].dataset.phase, "disconnected");
});

test("session skill control attaches through the live chat command", async () => {
  const harness = createHarness();
  await flush();

  assert.equal(harness.elements["#desk-skill"].childNodes[0].value, "review");
  assert.equal(harness.elements["#desk-skill-toggle"].textContent, "Attach skill");

  harness.elements["#desk-skill"].value = "review";
  await harness.elements["#desk-skill-toggle"].listener("click")();
  await flush();

  const [command] = mutationCalls(harness, "/api/v1/desk/chat/command");
  assert.deepEqual(
    JSON.parse(command.options.body),
    {
      client_id: "client-1",
      command: { name: "skills", args: "attach review" },
    },
  );
  assert.equal(harness.elements["#desk-skill-toggle"].textContent, "Detach skill");
  assert.equal(harness.elements["#desk-skill-status"].textContent, "Attached to this conversation.");
});

test("turn remains locked until its POST and turn_done settle, then applies canonical metadata", async () => {
  const harness = createHarness();
  await flush();

  const form = harness.elements["#desk-composer"];
  const message = harness.elements["#desk-message"];
  message.value = "Question";
  const submit = form.listener("submit");
  const firstSubmission = submit({ preventDefault() {} });
  await flush();
  assert.equal(mutationCalls(harness, "/api/v1/desk/chat/turn").length, 1);

  const stream = harness.EventSource.instances[0];
  stream.emit("text_delta", {
    resource: "chat",
    resource_id: "client-1",
    type: "text_delta",
    data: { text: "Answer" },
  });
  stream.emit("turn_done", {
    resource: "chat",
    resource_id: "client-1",
    type: "turn_done",
    data: {
      state: {
        title: "Question",
        connection_mode: "Shared session",
        profile: "coding",
        workspace: "waffle",
        provider_label: "New provider",
        model_alias: "new-model",
        models: [{ alias: "new-model", provider: "test", current: true }],
        history: [
          { role: "user", blocks: [{ type: "text", text: "Server copy" }] },
        ],
      },
    },
  });

  assert.notEqual(harness.elements[".desk-shell"].dataset.phase, "idle");
  assert.equal(message.disabled, true);
  void submit({ preventDefault() {} });
  assert.equal(mutationCalls(harness, "/api/v1/desk/chat/turn").length, 1);

  harness.turnResponse.resolve(jsonResponse({}));
  await firstSubmission;
  await flush();

  assert.equal(harness.elements[".desk-shell"].dataset.phase, "idle");
  assert.equal(harness.elements["#desk-session-title"].textContent, "Question");
  assert.equal(harness.elements["#desk-profile"].textContent, "coding");
  assert.equal(harness.elements["#desk-workspace"].textContent, "waffle");
  assert.equal(harness.elements["#desk-provider"].textContent, "New provider");
  assert.equal(harness.elements["#desk-model"].childNodes[0].value, "new-model");
  assert.deepEqual(
    harness.elements["#desk-transcript"].childNodes.map((node) =>
      node.querySelector(".message-body").textContent
    ),
    ["Question", "Answer"],
  );
});

test("cancel keeps the turn locked until its mutation and turn_done settle", async () => {
  const harness = createHarness();
  await flush();

  const form = harness.elements["#desk-composer"];
  const message = harness.elements["#desk-message"];
  const submit = form.listener("submit");
  message.value = "Question";
  const submission = submit({ preventDefault() {} });
  harness.turnResponse.resolve(jsonResponse({}));
  await submission;
  await flush();

  const cancellation = harness.elements["#desk-cancel"].listener("click")();
  await flush();
  harness.EventSource.instances[0].emit("turn_done", {
    resource: "chat",
    resource_id: "client-1",
    type: "turn_done",
    data: { state: { title: "Question" } },
  });

  assert.equal(harness.elements[".desk-shell"].dataset.phase, "cancelling");
  assert.equal(message.disabled, true);
  message.value = "Must stay blocked";
  void submit({ preventDefault() {} });
  assert.equal(mutationCalls(harness, "/api/v1/desk/chat/turn").length, 1);

  harness.cancelResponse.resolve(jsonResponse({}));
  await cancellation;
  await flush();

  assert.equal(harness.elements[".desk-shell"].dataset.phase, "idle");
  assert.equal(message.disabled, false);
  assert.equal(mutationCalls(harness, "/api/v1/desk/chat/cancel").length, 1);
});

test("cancel response before turn_done keeps the composer locked", async () => {
  const harness = createHarness();
  await flush();

  const form = harness.elements["#desk-composer"];
  const message = harness.elements["#desk-message"];
  const send = harness.elements["#desk-send"];
  const submit = form.listener("submit");
  message.value = "Question";
  const submission = submit({ preventDefault() {} });
  harness.turnResponse.resolve(jsonResponse({}));
  await submission;
  await flush();

  const cancellation = harness.elements["#desk-cancel"].listener("click")();
  await flush();
  harness.cancelResponse.resolve(jsonResponse({}));
  await cancellation;
  await flush();

  assert.equal(harness.elements[".desk-shell"].dataset.phase, "cancelling");
  assert.equal(message.disabled, true);
  assert.equal(send.disabled, true);
  message.value = "Must stay blocked";
  await submit({ preventDefault() {} });
  assert.equal(mutationCalls(harness, "/api/v1/desk/chat/turn").length, 1);

  harness.EventSource.instances[0].emit("turn_done", {
    resource: "chat",
    resource_id: "client-1",
    type: "turn_done",
    data: { state: { title: "Question" } },
  });
  await flush();

  assert.equal(harness.elements[".desk-shell"].dataset.phase, "idle");
  assert.equal(message.disabled, false);
  assert.equal(send.disabled, false);
  assert.equal(mutationCalls(harness, "/api/v1/desk/chat/cancel").length, 1);
});

test("late cancel rejection cannot overwrite a disconnected turn", async () => {
  const harness = createHarness();
  await flush();

  const message = harness.elements["#desk-message"];
  message.value = "Question";
  const submission = harness.elements["#desk-composer"].listener("submit")({
    preventDefault() {},
  });
  harness.turnResponse.resolve(jsonResponse({}));
  await submission;
  await flush();

  const cancellation = harness.elements["#desk-cancel"].listener("click")();
  await flush();
  harness.EventSource.instances[0].emit("error", {});
  const disconnectedMessage =
    "The live connection closed. Refresh before sending again.";
  assert.equal(
    harness.elements["#desk-stale-message"].textContent,
    disconnectedMessage,
  );

  harness.cancelResponse.reject(new Error("late cancel failure"));
  await cancellation;
  await flush();

  assert.equal(harness.elements[".desk-shell"].dataset.phase, "disconnected");
  assert.equal(
    harness.elements["#desk-stale-message"].textContent,
    disconnectedMessage,
  );
  assert.equal(mutationCalls(harness, "/api/v1/desk/chat/turn").length, 1);
});
