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
    this.attributes = new Map();
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
    const matches = (node) => {
      if (selector.startsWith(".")) {
        return node.className.split(/\s+/).includes(selector.slice(1));
      }
      if (selector.startsWith("#")) {
        return node.getAttribute("id") === selector.slice(1);
      }
      return node.tagName === selector.toUpperCase();
    };
    for (const child of this.childNodes) {
      if (matches(child)) {
        return child;
      }
      const nested = child.querySelector?.(selector);
      if (nested) {
        return nested;
      }
    }
    return null;
  }

  querySelectorAll(selector) {
    const matches = [];
    const visit = (node) => {
      for (const child of node.childNodes) {
        if (
          (selector.startsWith(".") &&
            child.className.split(/\s+/).includes(selector.slice(1))) ||
          (!selector.startsWith(".") && child.tagName === selector.toUpperCase())
        ) {
          matches.push(child);
        }
        visit(child);
      }
    };
    visit(this);
    return matches;
  }

  setAttribute(name, value) {
    this.attributes.set(name, String(value));
  }

  getAttribute(name) {
    return this.attributes.get(name) ?? null;
  }

  removeAttribute(name) {
    this.attributes.delete(name);
  }

  replaceChildren(...children) {
    this.textContent = "";
    this.append(...children);
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

  select() {
    this.selectedText = true;
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
  openHandler,
  closeHandler,
  turnHandler,
  cancelHandler,
  confirmResult = true,
  storedLease = null,
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
    "#desk-sandbox",
    "#desk-model-error-row",
    "#desk-model-error",
    "#desk-new",
    "#desk-session-refresh",
    "#desk-sessions",
    "#desk-usage-refresh",
    "#desk-usage",
    "#desk-permissions-refresh",
    "#desk-permissions",
    "#desk-workset-refresh",
    "#desk-workset",
    "#desk-help-refresh",
    "#desk-help",
  ];
  const elements = Object.fromEntries(selectors.map((selector) => [selector, new FakeElement()]));
  elements["#desk-transcript"].appendChild(elements["#desk-empty-transcript"]);
  elements["#desk-tool-activity"].appendChild(elements["#desk-empty-activity"]);
  elements["#desk-composer-status"].hidden = true;
  elements["#desk-model-status"].textContent = "Changes this conversation only.";
  elements["#desk-skill-status"].textContent = "Changes this conversation only.";

  const body = new FakeElement("body");
  body.dataset.requestToken = "stale-token";
  const forbiddenMarkupAssignments = [];
  Object.defineProperty(FakeElement.prototype, "innerHTML", {
    configurable: true,
    get() {
      return "";
    },
    set(value) {
      forbiddenMarkupAssignments.push(String(value));
    },
  });
  Object.defineProperty(FakeElement.prototype, "outerHTML", {
    configurable: true,
    get() {
      return "";
    },
    set(value) {
      forbiddenMarkupAssignments.push(String(value));
    },
  });
  const document = {
    body,
    createElement: (tagName) => new FakeElement(tagName),
    createTextNode: (text) => new FakeTextNode(text),
    execCommand: () => true,
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
  const timers = [];
  const cancelResponse = deferred();
  const turnResponse = deferred();
  const fetch = async (path, options) => {
    calls.push({ path, options });
    if (path === "/api/v1/desk/bootstrap") {
      return jsonResponse(bootstrap);
    }
    if (path === "/api/v1/desk/chat/open") {
      if (openHandler) {
        return openHandler({ path, options });
      }
      return jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
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
    if (path === "/api/v1/desk/chat/close") {
      if (closeHandler) {
        return closeHandler({ path, options });
      }
      return jsonResponse({});
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

  const storage = new Map();
  if (storedLease) {
    storage.set("waffle.desk.today.owner.v1", JSON.stringify(storedLease));
  }
  const sessionStorage = {
    getItem: (key) => storage.get(key) ?? null,
    removeItem: (key) => storage.delete(key),
    setItem: (key, value) => storage.set(key, String(value)),
  };
  const lifecycleListeners = new Map();
  const clipboardWrites = [];
  const context = vm.createContext({
    console,
    crypto: { randomUUID: () => `key-${calls.length}` },
    document,
    EventSource: FakeEventSource,
    fetch,
    location: { href },
    navigator: {
      clipboard: {
        async writeText(value) {
          clipboardWrites.push(String(value));
        },
      },
    },
    sessionStorage,
    confirm: () => confirmResult,
    addEventListener: (type, listener) => {
      const listeners = lifecycleListeners.get(type) || [];
      listeners.push(listener);
      lifecycleListeners.set(type, listeners);
    },
    URL,
    waffleDeskRail,
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
    clipboardWrites,
    cancelResponse,
    elements,
    EventSource: FakeEventSource,
    forbiddenMarkupAssignments,
    lifecycleListeners,
    sessionStorage,
    storage,
    timers,
    railCalls,
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

test("rail stays live while SSE reconnects after a stream drop", async () => {
  const harness = createHarness();
  await flush();
  harness.railCalls.length = 0;

  harness.EventSource.instances[0].emit("error", {});
  await flush();

  // Recoverable drops reconnect; the desk is not torn down to disconnected.
  assert.equal(harness.elements[".desk-shell"].dataset.phase, "idle");
  assert.equal(harness.elements["#desk-connection-text"].textContent, "Reconnecting");
  assert.equal(harness.elements["#desk-stale-status"].hidden, true);
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

test("assistant markdown renders structured inert DOM and copies a fenced block", async () => {
  const markdown = [
    "## Build notes",
    "",
    "Use `mise run test`.",
    "",
    "- first",
    "- second <script>globalThis.pwned = true</script>",
    "",
    "```go",
    "fmt.Println(\"safe\")",
    "```",
  ].join("\n");
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({
          history: [{ role: "assistant", blocks: [{ type: "text", text: markdown }] }],
        }),
      }),
  });
  await flush();

  const transcript = harness.elements["#desk-transcript"];
  assert.equal(transcript.querySelectorAll("h2").length, 1);
  assert.equal(transcript.querySelectorAll("ul").length, 1);
  assert.equal(transcript.querySelectorAll("pre").length, 1);
  assert.equal(transcript.querySelectorAll("code").length, 2);
  assert.match(transcript.textContent, /<script>globalThis\.pwned = true<\/script>/);
  assert.equal(harness.forbiddenMarkupAssignments.length, 0);

  const copy = transcript.querySelector(".code-copy");
  assert.ok(copy);
  await copy.listener("click")();
  assert.deepEqual(harness.clipboardWrites, ['fmt.Println("safe")']);
  assert.equal(copy.textContent, "Copied");
});

test("Ctrl or Cmd Enter sends while plain Enter remains a newline", async () => {
  const harness = createHarness({
    turnHandler: async () => jsonResponse({}),
  });
  await flush();
  const message = harness.elements["#desk-message"];
  const keydown = message.listener("keydown");
  message.value = "line one";
  let prevented = 0;
  await keydown({
    key: "Enter",
    ctrlKey: false,
    metaKey: false,
    preventDefault() {
      prevented += 1;
    },
  });
  assert.equal(prevented, 0);
  assert.equal(mutationCalls(harness, "/api/v1/desk/chat/turn").length, 0);

  await keydown({
    key: "Enter",
    ctrlKey: true,
    metaKey: false,
    preventDefault() {
      prevented += 1;
    },
  });
  await flush();
  assert.equal(prevented, 1);
  assert.equal(mutationCalls(harness, "/api/v1/desk/chat/turn").length, 1);

  harness.EventSource.instances[0].emit("turn_done", {
    resource: "chat",
    resource_id: "client-1",
    type: "turn_done",
    data: { state: defaultChatState() },
  });
  await flush();
  message.value = "line two";
  await keydown({
    key: "Enter",
    ctrlKey: false,
    metaKey: true,
    preventDefault() {
      prevented += 1;
    },
  });
  await flush();
  assert.equal(prevented, 2);
  assert.equal(mutationCalls(harness, "/api/v1/desk/chat/turn").length, 2);
});

test("keyboard send ignores empty and non-idle composers", async () => {
  const pending = deferred();
  const harness = createHarness({
    turnHandler: async () => pending.promise,
  });
  await flush();
  const message = harness.elements["#desk-message"];
  const keydown = message.listener("keydown");
  const shortcut = () =>
    keydown({
      key: "Enter",
      ctrlKey: true,
      metaKey: false,
      preventDefault() {},
    });

  message.value = "   ";
  await shortcut();
  assert.equal(mutationCalls(harness, "/api/v1/desk/chat/turn").length, 0);
  message.value = "one";
  void shortcut();
  await flush();
  message.value = "two";
  await shortcut();
  assert.equal(mutationCalls(harness, "/api/v1/desk/chat/turn").length, 1);
  pending.resolve(jsonResponse({}));
});

test("tool ledger pairs concurrent calls by opaque ID with duration and outcome", async () => {
  const harness = createHarness();
  await flush();
  const stream = harness.EventSource.instances[0];
  const emit = (type, data) =>
    stream.emit(type, {
      resource: "chat",
      resource_id: "client-1",
      type,
      data,
    });
  emit("tool_started", { tool_name: "read", tool_call_id: "tool-1" });
  emit("tool_started", { tool_name: "read", tool_call_id: "tool-2" });
  emit("tool_finished", {
    tool_name: "read",
    tool_call_id: "tool-2",
    duration_ms: 12,
    byte_count: 0,
    is_error: true,
  });
  emit("tool_finished", {
    tool_name: "read",
    tool_call_id: "tool-1",
    duration_ms: 25,
    byte_count: 128,
    is_error: false,
  });
  emit("tool_finished", {
    tool_name: "read",
    tool_call_id: "tool-2",
    duration_ms: 12,
    byte_count: 0,
    is_error: true,
  });

  const ledger = harness.elements["#desk-tool-activity"];
  assert.equal(ledger.childNodes.length, 2);
  assert.match(ledger.childNodes[0].textContent, /read.*25 ms.*succeeded.*128 bytes/i);
  assert.equal(ledger.childNodes[0].classList.contains("is-success"), true);
  assert.match(ledger.childNodes[1].textContent, /read.*12 ms.*failed/i);
  assert.equal(ledger.childNodes[1].classList.contains("is-error"), true);
});

test("new conversation requires explicit confirmation then replaces canonical state", async () => {
  const harness = createHarness({
    commandHandler: async ({ options }) => {
      const { command } = JSON.parse(options.body);
      if (command.name === "new" && command.args === "") {
        return jsonResponse({ confirm: true, text: "Start over?" });
      }
      if (command.name === "new" && command.args === "confirm") {
        return jsonResponse({
          state: defaultChatState({
            session_id: "session-new",
            title: "Fresh desk",
            history: [{ role: "assistant", blocks: [{ type: "text", text: "Fresh start" }] }],
          }),
        });
      }
      return jsonResponse({}, false);
    },
  });
  await flush();
  await harness.elements["#desk-new"].listener("click")();
  await flush();

  const commands = mutationCalls(harness, "/api/v1/desk/chat/command").map(
    (call) => JSON.parse(call.options.body).command,
  );
  assert.deepEqual(commands, [
    { name: "new", args: "" },
    { name: "new", args: "confirm" },
  ]);
  assert.equal(harness.elements["#desk-session-title"].textContent, "Fresh desk");
  assert.match(harness.elements["#desk-transcript"].textContent, /Fresh start/);
  assert.equal(
    JSON.parse(harness.storage.get("waffle.desk.today.owner.v1")).session_id,
    "session-new",
  );
});

test("declining new conversation confirmation preserves the current session", async () => {
  const harness = createHarness({
    confirmResult: false,
    commandHandler: async () => jsonResponse({ confirm: true, text: "Start over?" }),
  });
  await flush();
  const before = harness.elements["#desk-session-title"].textContent;
  await harness.elements["#desk-new"].listener("click")();
  await flush();
  assert.equal(mutationCalls(harness, "/api/v1/desk/chat/command").length, 1);
  assert.equal(harness.elements["#desk-session-title"].textContent, before);
});

test("sessions list resumes selected history in place and failed resume leaves it intact", async () => {
  let failResume = false;
  const harness = createHarness({
    commandHandler: async ({ options }) => {
      const { command } = JSON.parse(options.body);
      if (command.name === "sessions") {
        return jsonResponse({
          sessions: [
            {
              id: "session-2",
              title: "Second session",
              summary: "Summary",
              updated_at: "2026-07-25T12:00:00Z",
            },
          ],
        });
      }
      if (command.name === "resume" && failResume) {
        return jsonResponse({ code: "resume_failed", message: "Could not resume." }, false);
      }
      return jsonResponse({
        state: defaultChatState({
          session_id: "session-2",
          title: "Second session",
          history: [{ role: "assistant", blocks: [{ type: "text", text: "Restored" }] }],
        }),
      });
    },
  });
  await flush();
  await harness.elements["#desk-session-refresh"].listener("click")();
  await flush();
  const sessionButton = harness.elements["#desk-sessions"].querySelector("button");
  assert.match(sessionButton.textContent, /Second session.*25 Jul 2026/i);
  await sessionButton.listener("click")();
  await flush();
  assert.equal(harness.elements["#desk-session-title"].textContent, "Second session");
  assert.match(harness.elements["#desk-transcript"].textContent, /Restored/);

  failResume = true;
  await harness.elements["#desk-session-refresh"].listener("click")();
  await flush();
  await harness.elements["#desk-sessions"].querySelector("button").listener("click")();
  await flush();
  assert.equal(harness.elements["#desk-session-title"].textContent, "Second session");
  assert.match(harness.elements["#desk-transcript"].textContent, /Restored/);
});

test("usage permissions workset and help commands render existing sanitized results", async () => {
  const harness = createHarness({
    commandHandler: async ({ options }) => {
      const { command } = JSON.parse(options.body);
      const results = {
        usage: {
          usage: [
            {
              period: "today",
              requests: 2,
              input_tokens: 30,
              output_tokens: 12,
              reserved_tokens: 4,
            },
          ],
        },
        permissions: {
          permissions: {
            sandbox_mode: "workspace-write",
            allow: ["read"],
            deny: ["bash"],
            deny_prefixes: ["secret."],
          },
        },
        workset: {
          workset: [{ id: "goal-1", text: "Ship the safe Desk" }],
        },
        help: {
          commands: [
            { name: "new", usage: "/new", description: "Start a conversation" },
          ],
        },
      };
      return jsonResponse(results[command.name]);
    },
  });
  await flush();
  for (const id of [
    "#desk-usage-refresh",
    "#desk-permissions-refresh",
    "#desk-workset-refresh",
    "#desk-help-refresh",
  ]) {
    await harness.elements[id].listener("click")();
    await flush();
  }
  assert.match(harness.elements["#desk-usage"].textContent, /2 requests.*30 in.*12 out.*4 reserved/i);
  assert.match(
    harness.elements["#desk-permissions"].textContent,
    /workspace-write.*Allow.*read.*Deny.*bash.*secret\./i,
  );
  assert.match(harness.elements["#desk-workset"].textContent, /goal-1.*Ship the safe Desk/i);
  assert.match(harness.elements["#desk-help"].textContent, /\/new.*Start a conversation/i);
});

test("sandbox mode and model error render from canonical state", async () => {
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({
          sandbox_mode: "workspace-write",
          model_error: "Pinned model is unavailable",
        }),
      }),
  });
  await flush();
  assert.equal(harness.elements["#desk-sandbox"].textContent, "workspace-write");
  assert.equal(harness.elements["#desk-model-error-row"].hidden, false);
  assert.equal(
    harness.elements["#desk-model-error"].textContent,
    "Pinned model is unavailable",
  );
  assert.equal(harness.elements["#desk-model-error"].classList.contains("is-error"), true);
});

test("reload reattaches with the stored proof and rotates persisted ownership", async () => {
  const storedLease = {
    client_id: "client-owned",
    reattach_token: "lease-old",
    session_id: "session-1",
  };
  const harness = createHarness({
    storedLease,
    openHandler: async ({ options }) => {
      const body = JSON.parse(options.body);
      assert.equal(body.reattach_client_id, "client-owned");
      assert.equal(body.reattach_token, "lease-old");
      return jsonResponse({
        client_id: "client-owned",
        reattach_token: "lease-new",
        state: defaultChatState(),
      });
    },
  });
  await flush();
  assert.equal(harness.elements[".desk-shell"].dataset.phase, "idle");
  assert.equal(harness.elements["#desk-message"].disabled, false);
  assert.deepEqual(JSON.parse(harness.storage.get("waffle.desk.today.owner.v1")), {
    client_id: "client-owned",
    reattach_token: "lease-new",
    session_id: "session-1",
  });

  for (const listener of harness.lifecycleListeners.get("pagehide") || []) {
    listener();
  }
  await flush();
  const close = mutationCalls(harness, "/api/v1/desk/chat/close").at(-1);
  assert.deepEqual(JSON.parse(close.options.body), {
    client_id: "client-owned",
    reattach_token: "lease-new",
  });
  assert.equal(close.options.keepalive, true);
});

test("expired stored proof clears and falls back to one fresh owner", async () => {
  let opens = 0;
  const harness = createHarness({
    storedLease: {
      client_id: "client-stale",
      reattach_token: "lease-stale",
      session_id: "session-1",
    },
    openHandler: async () => {
      opens += 1;
      if (opens === 1) {
        return jsonResponse(
          { code: "chat_client_not_found", message: "chat client was not found" },
          false,
        );
      }
      return jsonResponse({
        client_id: "client-fresh",
        reattach_token: "lease-fresh",
        state: defaultChatState(),
      });
    },
  });
  await flush();
  const openBodies = mutationCalls(harness, "/api/v1/desk/chat/open").map((call) =>
    JSON.parse(call.options.body)
  );
  assert.equal(openBodies.length, 2);
  assert.equal(openBodies[0].reattach_client_id, "client-stale");
  assert.equal("reattach_client_id" in openBodies[1], false);
  assert.equal(harness.elements[".desk-shell"].dataset.phase, "idle");
  assert.equal(
    JSON.parse(harness.storage.get("waffle.desk.today.owner.v1")).client_id,
    "client-fresh",
  );
});

test("an independent tab without a lease never claims an existing client", async () => {
  const harness = createHarness();
  await flush();
  const [open] = mutationCalls(harness, "/api/v1/desk/chat/open");
  const body = JSON.parse(open.options.body);
  assert.equal("reattach_client_id" in body, false);
  assert.equal("reattach_token" in body, false);
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
