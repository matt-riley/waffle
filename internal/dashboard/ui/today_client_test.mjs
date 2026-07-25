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
        if (force === undefined) {
          if (classes.has(name)) {
            classes.delete(name);
          } else {
            classes.add(name);
          }
        } else if (force) {
          classes.add(name);
        } else {
          classes.delete(name);
        }
        this.className = [...classes].join(" ");
      },
      add: (name) => {
        const classes = new Set(this.className.split(/\s+/).filter(Boolean));
        classes.add(name);
        this.className = [...classes].join(" ");
      },
      remove: (name) => {
        const classes = new Set(this.className.split(/\s+/).filter(Boolean));
        classes.delete(name);
        this.className = [...classes].join(" ");
      },
      contains: (name) => this.className.split(/\s+/).includes(name),
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

function defaultChatState(overrides = {}) {
  return {
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
    ...overrides,
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
  commandHandler,
  turnHandler,
  cancelHandler,
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
    "#desk-composer-status",
    "#desk-model",
    "#desk-model-status",
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
  elements["#desk-composer-status"].hidden = true;
  elements["#desk-model-status"].textContent = "Changes this conversation only.";
  elements["#desk-skill-status"].textContent = "Changes this conversation only.";

  const document = {
    body: { dataset: { requestToken: "stale-token" } },
    createElement: (tagName) => new FakeElement(tagName),
    createTextNode: (text) => new FakeTextNode(text),
    querySelector: (selector) => elements[selector] || null,
  };
  const calls = [];
  const timers = [];
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
        state: defaultChatState(),
      });
    }
    if (path === "/api/v1/desk/chat/command") {
      if (commandHandler) {
        return commandHandler({ path, options });
      }
      return jsonResponse({
        state: defaultChatState({
          skills: [{ name: "review", description: "Review changes", attached: true }],
        }),
      });
    }
    if (path === "/api/v1/desk/chat/turn") {
      if (turnHandler) {
        return turnHandler({ path, options });
      }
      return turnResponse.promise;
    }
    if (path === "/api/v1/desk/chat/cancel") {
      if (cancelHandler) {
        return cancelHandler({ path, options });
      }
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
      this.onopen = null;
      FakeEventSource.instances.push(this);
    }

    addEventListener(type, listener) {
      const listeners = this.listeners.get(type) || [];
      listeners.push(listener);
      this.listeners.set(type, listeners);
    }

    emit(type, envelope) {
      const event = {
        data: envelope === undefined ? "" : JSON.stringify(envelope),
      };
      for (const listener of this.listeners.get(type) || []) {
        listener(event);
      }
    }

    open() {
      if (typeof this.onopen === "function") {
        this.onopen();
      }
    }

    close() {
      this.closed = true;
    }
  }
  FakeEventSource.instances = [];

  const context = vm.createContext({
    console,
    crypto: { randomUUID: () => `key-${calls.length}` },
    document,
    EventSource: FakeEventSource,
    fetch,
    location: { href },
    URL,
    setTimeout: (fn, delay) => {
      const handle = { fn, delay, cleared: false };
      timers.push(handle);
      return handle;
    },
    clearTimeout: (handle) => {
      if (handle) {
        handle.cleared = true;
      }
    },
  });
  new vm.Script(source, { filename: "today.js" }).runInContext(context);

  return {
    calls,
    cancelResponse,
    elements,
    EventSource: FakeEventSource,
    timers,
    turnResponse,
    runTimers: async () => {
      const due = timers.splice(0, timers.length);
      for (const timer of due) {
        if (!timer.cleared) {
          timer.fn();
        }
      }
      await flush();
    },
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
  harness.EventSource.instances[0].emit("resync_required", {});
  const disconnectedMessage =
    "Live updates expired. Refresh to load canonical state.";
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

test("failed model change stays live and reports the error next to the control", async () => {
  const harness = createHarness({
    commandHandler: async () =>
      jsonResponse({ code: "model_not_found", message: "Model not found." }, false),
  });
  await flush();

  harness.elements["#desk-model"].value = "missing-model";
  await harness.elements["#desk-model"].listener("change")();
  await flush();

  assert.equal(harness.elements[".desk-shell"].dataset.phase, "idle");
  assert.equal(harness.elements["#desk-stale-status"].hidden, true);
  assert.equal(harness.elements["#desk-model"].disabled, false);
  assert.equal(harness.elements["#desk-message"].disabled, false);
  assert.equal(harness.elements["#desk-model-status"].textContent, "Model not found.");
  assert.equal(harness.elements["#desk-model-status"].classList.contains("is-error"), true);
  assert.equal(harness.EventSource.instances[0].closed, false);
});

test("failed skill toggle stays live and reports the error next to the control", async () => {
  const harness = createHarness({
    commandHandler: async () =>
      jsonResponse({ code: "skill_error", message: "Skill unavailable." }, false),
  });
  await flush();

  harness.elements["#desk-skill"].value = "review";
  await harness.elements["#desk-skill-toggle"].listener("click")();
  await flush();

  assert.equal(harness.elements[".desk-shell"].dataset.phase, "idle");
  assert.equal(harness.elements["#desk-stale-status"].hidden, true);
  assert.equal(harness.elements["#desk-skill-toggle"].disabled, false);
  assert.equal(harness.elements["#desk-skill-status"].textContent, "Skill unavailable.");
  assert.equal(harness.elements["#desk-skill-status"].classList.contains("is-error"), true);
  assert.equal(harness.EventSource.instances[0].closed, false);
});

test("rejected turn keeps composer text and reuses Idempotency-Key on retry", async () => {
  let turnCalls = 0;
  const harness = createHarness({
    turnHandler: async () => {
      turnCalls += 1;
      if (turnCalls === 1) {
        return jsonResponse(
          { code: "turn_rejected", message: "Turn rejected by policy." },
          false,
        );
      }
      return jsonResponse({});
    },
  });
  await flush();

  const message = harness.elements["#desk-message"];
  message.value = "Careful question";
  const submit = harness.elements["#desk-composer"].listener("submit");
  await submit({ preventDefault() {} });
  await flush();

  assert.equal(harness.elements[".desk-shell"].dataset.phase, "idle");
  assert.equal(message.value, "Careful question");
  assert.equal(
    harness.elements["#desk-composer-status"].textContent,
    "Turn rejected by policy.",
  );
  assert.equal(harness.elements["#desk-stale-status"].hidden, true);

  const firstKey = mutationCalls(harness, "/api/v1/desk/chat/turn")[0].options.headers[
    "Idempotency-Key"
  ];
  await submit({ preventDefault() {} });
  await flush();

  const turnPosts = mutationCalls(harness, "/api/v1/desk/chat/turn");
  assert.equal(turnPosts.length, 2);
  assert.equal(turnPosts[1].options.headers["Idempotency-Key"], firstKey);
  assert.deepEqual(JSON.parse(turnPosts[1].options.body), {
    client_id: "client-1",
    text: "Careful question",
  });
});

test("network failure after turn leaves is unrecoverable and names the cause", async () => {
  const harness = createHarness({
    turnHandler: async () => {
      throw new TypeError("Failed to fetch");
    },
  });
  await flush();

  const message = harness.elements["#desk-message"];
  message.value = "Maybe delivered";
  await harness.elements["#desk-composer"].listener("submit")({ preventDefault() {} });
  await flush();

  assert.equal(harness.elements[".desk-shell"].dataset.phase, "disconnected");
  assert.equal(harness.elements["#desk-stale-status"].hidden, false);
  assert.match(
    harness.elements["#desk-stale-message"].textContent,
    /could not reach Waffle|turn outcome is unknown/i,
  );
  assert.equal(message.value, "Maybe delivered");
});

test("dropped SSE reconnects automatically from the last cursor", async () => {
  const harness = createHarness();
  await flush();

  const first = harness.EventSource.instances[0];
  first.emit("state", {
    cursor: 50,
    resource: "chat",
    resource_id: "client-1",
    type: "state",
    data: {
      state: defaultChatState({ title: "Live", connection_mode: "Shared session" }),
    },
  });
  first.emit("error", {});
  await flush();

  assert.equal(harness.elements[".desk-shell"].dataset.phase, "idle");
  assert.equal(harness.elements["#desk-stale-status"].hidden, true);
  assert.equal(harness.elements["#desk-connection-text"].textContent, "Reconnecting");
  assert.equal(harness.elements["#desk-message"].disabled, false);
  assert.equal(first.closed, true);
  assert.equal(harness.timers.length, 1);

  await harness.runTimers();

  assert.equal(harness.EventSource.instances.length, 2);
  const second = harness.EventSource.instances[1];
  assert.equal(second.url, "/api/v1/desk/events?after=50");
  second.open();
  assert.equal(harness.elements["#desk-connection-text"].textContent, "Shared session");
  assert.equal(harness.elements[".desk-shell"].dataset.phase, "idle");
});

test("resync_required still surfaces the stale banner", async () => {
  const harness = createHarness();
  await flush();

  harness.EventSource.instances[0].emit("resync_required", {});
  await flush();

  assert.equal(harness.elements[".desk-shell"].dataset.phase, "disconnected");
  assert.equal(harness.elements["#desk-stale-status"].hidden, false);
  assert.equal(
    harness.elements["#desk-stale-message"].textContent,
    "Live updates expired. Refresh to load canonical state.",
  );
});

test("unparseable SSE frame does not tear down the desk", async () => {
  const harness = createHarness();
  await flush();

  const stream = harness.EventSource.instances[0];
  const listeners = stream.listeners.get("state") || [];
  for (const listener of listeners) {
    listener({ data: "{not-json" });
  }
  await flush();

  assert.equal(harness.elements[".desk-shell"].dataset.phase, "idle");
  assert.equal(harness.elements["#desk-stale-status"].hidden, true);
  assert.equal(stream.closed, false);
  assert.equal(harness.elements["#desk-message"].disabled, false);
});
