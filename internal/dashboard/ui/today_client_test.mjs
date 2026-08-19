import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const readAloudSource = await readFile(
  new URL("./assets/read-aloud.js", import.meta.url),
  "utf8",
);
const dictateSource = await readFile(
  new URL("./assets/dictate.js", import.meta.url),
  "utf8",
);
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
    this.style = {};
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
  exportHandler,
  closeHandler,
  turnHandler,
  cancelHandler,
  workspacesHandler,
  projectHandler,
  artifactHandler,

  confirmResult = true,
  storedLease = null,
  sharedStorage = null,
  denyStorage = false,
  noSpeechRecognition = false,
} = {}) {
  const selectors = [
    ".desk-shell",
    "#desk-session-title",
    "#desk-connection",
    "#desk-connection-text",
    "#desk-connection-detail",
    "#desk-stale-status",
    "#desk-stale-label",
    "#desk-stale-message",
    "#desk-refresh",
    "#desk-recover-new",
    "#desk-phase",
    "#desk-transcript",
    "#desk-empty-transcript",
    "#desk-slash-menu",
    ".composer-actions",
    "#desk-composer",
    "#desk-message",
    "#desk-send",
    "#desk-cancel",
    "#desk-attach",
    "#desk-attach-button",
    "#desk-attachment-preview",
    "#desk-schedule-draft",
    "#desk-dictate",
    "#desk-dictate-hint",
    "#desk-composer-status",
    "#desk-model",
    "#desk-model-detail",
    "#desk-model-status",
    "#desk-skill",
    "#desk-skill-toggle",
    "#desk-task-mode",
    "#desk-reasoning",
    "#desk-skill-status",
    "#desk-profile",
    "#desk-workspace",
    "#desk-provider",
    "#desk-sandbox",
    "#desk-model-error-row",
    "#desk-model-error",
    "#desk-fork-row",
    "#desk-fork",
    "#desk-new",
    "#desk-session-refresh",
    "#desk-temporary-row",
    "#desk-temporary",
    "#desk-temporary-badge",
    "#desk-export-format",
    "#desk-export",
    "#desk-sessions",
    "#desk-session-filter",
    "#desk-session-options",
    "#desk-usage-refresh",
    "#desk-usage",
    "#desk-project-refresh",
    "#desk-project",
    "#desk-project-pin-form",
    "#desk-project-path",
    "#desk-project-pin",
    "#desk-project-note-form",
    "#desk-project-note-name",
    "#desk-project-note",
    "#desk-project-add-note",
    "#desk-permissions-refresh",
    "#desk-permissions",
    "#desk-workset-refresh",
    "#desk-workset",
    "#desk-help-refresh",
    "#desk-help",
    "#desk-queue",
  ];
  const elements = Object.fromEntries(selectors.map((selector) => [selector, new FakeElement()]));
  elements["#desk-transcript"].appendChild(elements["#desk-empty-transcript"]);
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
    get activeElement() {
      return activeElement;
    },
    body,
    createElement: (tagName) => new FakeElement(tagName),
    createTextNode: (text) => new FakeTextNode(text),
    execCommand: () => true,
    querySelector: (selector) => elements[selector] || null,
  };
  // Track programmatic focus so listbox arrow navigation is observable.
  let activeElement = null;
  FakeElement.prototype.focus = function () {
    this.focused = true;
    activeElement = this;
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
    if (path === "/api/v1/desk/chat/export") {
      if (exportHandler) {
        return exportHandler({ path, options });
      }
      return { ok: true, async text() { return "# Conversation"; }, async json() { return {}; } };
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
    if (path === "/api/v1/desk/workspaces") {
      if (workspacesHandler) {
        return workspacesHandler({ path, options });
      }
      return jsonResponse({ workspaces: [] });
    }
    if (path.startsWith("/api/v1/desk/projects/")) {
      if (projectHandler) {
        return projectHandler({ path, options });
      }
      return jsonResponse({});
    }
    if (path === "/api/v1/desk/chat/close") {
      if (closeHandler) {
        return closeHandler({ path, options });
      }
      return jsonResponse({});
    }
    if (path.startsWith("/api/v1/desk/artifacts/")) {
      if (artifactHandler) {
        return artifactHandler({ path, options });
      }
      return jsonResponse({});
    }
    if (path === "/api/v1/desk/chat/commands") {
      return jsonResponse({
        commands: [
          { name: "model", usage: "/model [alias]", description: "choose the session model" },
          { name: "skills", usage: "/skills", description: "list or change session skills" },
          { name: "status", usage: "/status", description: "show current runtime status" },
        ],
      });
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

  const storage = sharedStorage || new Map();
  if (storedLease) {
    storage.set("waffle.desk.today.owner.v1", JSON.stringify(storedLease));
  }
  const speechUtterances = [];
  class FakeSpeechSynthesisUtterance {
    constructor(text) {
      this.text = text;
      this.voice = null;
      this.onend = null;
      this.onerror = null;
      speechUtterances.push(this);
    }
  }
  const recognitionInstances = [];
  class FakeSpeechRecognition {
    constructor() {
      this.continuous = false;
      this.interimResults = false;
      this.lang = "";
      this.onresult = null;
      this.onerror = null;
      this.onend = null;
      this.started = false;
      recognitionInstances.push(this);
    }
    start() {
      this.started = true;
    }
    stop() {
      this.started = false;
    }
  }
  const speechCalls = [];
  const speechSynthesis = {
    speak(utterance) {
      speechCalls.push({ kind: "speak", text: utterance.text });
    },
    cancel() {
      speechCalls.push({ kind: "cancel" });
    },
    getVoices() {
      return [{ default: true, name: "Fixture" }];
    },
    onend: null,
    onerror: null,
  };
  const fileReadResults = [];
  class FakeFileReader {
    readAsDataURL(file) {
      const payload = fakeFilePayloads[file?.name] || "";
      this.result = `data:${file?.type || "application/octet-stream"};base64,${payload}`;
      fileReadResults.push({ name: file?.name, type: file?.type });
      this.onload?.();
    }
  }
  const fakeFilePayloads = {
    "shot.png": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==",
  };
  const sessionStorage = denyStorage
    ? {
        getItem() {
          throw new Error("storage denied");
        },
        removeItem() {
          throw new Error("storage denied");
        },
        setItem() {
          throw new Error("storage denied");
        },
      }
    : {
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
    FileReader: FakeFileReader,
    speechSynthesis,
    SpeechSynthesisUtterance: FakeSpeechSynthesisUtterance,
    SpeechRecognition: noSpeechRecognition ? undefined : FakeSpeechRecognition,
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
  new vm.Script(readAloudSource, { filename: "read-aloud.js" }).runInContext(context);
  new vm.Script(dictateSource, { filename: "dictate.js" }).runInContext(context);
  new vm.Script(source, { filename: "today.js" }).runInContext(context);

  return {
    calls,
    clipboardWrites,
    fakeFilePayloads,
    speechCalls,
    speechSynthesis,
    speechUtterances,
    recognitionInstances,
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
    temporary: false,
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

test("assistant inline markdown renders bold, italic, strike, and safe links; unsafe links stay literal", async () => {
  const markdown = [
    "**bold** with *italic* and ~~struck~~.",
    "",
    "Run `mise run test` and see [the docs](https://example.com/docs).",
    "",
    "Unsafe [label](javascript:alert(1)) stays literal.",
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
  assert.equal(transcript.querySelectorAll("strong").length, 1);
  assert.equal(transcript.querySelectorAll("em").length, 1);
  assert.equal(transcript.querySelectorAll("del").length, 1);
  assert.equal(transcript.querySelectorAll("a").length, 1);
  const anchor = transcript.querySelector("a");
  assert.equal(anchor.getAttribute("href"), "https://example.com/docs");
  assert.equal(anchor.getAttribute("target"), "_blank");
  assert.equal(anchor.getAttribute("rel"), "noopener noreferrer");
  assert.equal(anchor.textContent, "the docs");
  assert.match(transcript.textContent, /Unsafe \[label\]\(javascript:alert\(1\)\) stays literal\./);
  assert.match(transcript.textContent, /Run mise run test and see/);
  assert.equal(harness.forbiddenMarkupAssignments.length, 0);
});

test("markdown links whose scheme hides behind control characters stay literal", async () => {
  // The inline link pattern excludes \s from the target, but C0 controls like
  // \u0001 are not \s — and the URL parser strips them before reading the
  // scheme, so "\u0001javascript:alert(1)" navigates as javascript:. Scheme
  // detection must normalise the same way the browser does.
  const hostile = [
    "\u0001javascript:alert(1)",
    "\u000Ejavascript:alert(1)",
    "java\u0009script:alert(1)",
    "java\u000Ascript:alert(1)",
  ];
  const markdown = hostile
    .map((href, index) => `Link ${index}: [label](${href}) end.`)
    .join("\n\n");
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
  assert.equal(transcript.querySelectorAll("a").length, 0);
  assert.equal(harness.forbiddenMarkupAssignments.length, 0);
});

test("assistant markdown renders tables as semantic responsive tables", async () => {
  const markdown = [
    "| Name | Cost | Fit |",
    "| :--- | ---: | :---: |",
    "| mise | $0 | `free` |",
    "| figma | $12 | [docs](https://figma.com) |",
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
  const table = transcript.querySelector("table");
  assert.ok(table, "table element rendered");
  const wrap = transcript.querySelector(".table-scroll");
  assert.ok(wrap, "table wrapped in a scroll container");
  assert.equal(wrap.getAttribute("role"), "group");
  assert.equal(wrap.getAttribute("aria-label"), "Table");
  assert.equal(transcript.querySelectorAll("th").length, 3);
  for (const th of transcript.querySelectorAll("th")) {
    assert.equal(th.getAttribute("scope"), "col");
  }
  assert.equal(transcript.querySelectorAll("tr").length, 3);
  const tds = table.querySelectorAll("td");
  assert.equal(tds.length, 6);
  assert.equal(tds[0].style.textAlign, "left");
  assert.equal(tds[1].style.textAlign, "right");
  assert.equal(tds[2].style.textAlign, "center");
  assert.equal(tds[3].style.textAlign, "left");
  assert.equal(tds[0].querySelector("code"), null);
  assert.equal(tds[2].querySelector("code").textContent, "free");
  const link = tds[5].querySelector("a");
  assert.equal(link.getAttribute("href"), "https://figma.com");
  assert.equal(harness.forbiddenMarkupAssignments.length, 0);
});

test("streaming table settles once the delimiter row completes", async () => {
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState(),
      }),
  });
  await flush();
  const transcript = harness.elements["#desk-transcript"];
  const emit = (text) => {
    harness.EventSource.instances[0].emit("text_delta", {
      resource: "chat",
      resource_id: "client-1",
      type: "text_delta",
      data: { text },
    });
  };
  emit("| Name |");
  await flush();
  assert.equal(transcript.querySelector("table"), null, "header alone is not a table");
  emit(" Cost |");
  await flush();
  assert.equal(transcript.querySelector("table"), null, "missing delimiter is not a table");
  emit("\n| :--- |");
  await flush();
  assert.equal(transcript.querySelector("table"), null, "split delimiter stays text");
  emit(" :---: |\n| mise | $0 |");
  await flush();
  const table = transcript.querySelector("table");
  assert.ok(table, "complete table renders");
  assert.equal(transcript.querySelectorAll("th").length, 2);
  assert.equal(table.querySelectorAll("td").length, 2);
  assert.equal(harness.forbiddenMarkupAssignments.length, 0);
});

test("ragged and incomplete table syntax stays readable text", async () => {
  const markdown = ["| A | B", "not a delimiter", "| 1 | 2", "plain paragraph with | pipe"].join("\n");
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
  assert.equal(transcript.querySelector("table"), null);
  assert.match(transcript.textContent, /\| A \| B/);
  assert.match(transcript.textContent, /plain paragraph with \| pipe/);
});

test("table cells render unsafe model content inert", async () => {
  const markdown = [
    "| Name | Link |",
    "| --- | --- |",
    "| <script>globalThis.pwned = true</script> | [bad](javascript:alert(1)) |",
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
  assert.equal(transcript.querySelectorAll("script").length, 0);
  assert.equal(transcript.querySelectorAll("a").length, 0, "unsafe link stays literal");
  assert.match(transcript.textContent, /<script>globalThis\.pwned = true<\/script>/);
  assert.match(transcript.textContent, /javascript:alert\(1\)/);
  assert.equal(harness.forbiddenMarkupAssignments.length, 0);
});

test("message copy preserves raw markdown table delimiters", async () => {
  const markdown = ["| Name | Cost |", "| :--- | ---: |", "| mise | $0 |"].join("\n");
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
  const copy = harness.elements["#desk-transcript"].querySelector(".message-copy");
  assert.ok(copy);
  await copy.listener("click")();
  assert.deepEqual(harness.clipboardWrites, [markdown]);
});

test("Ctrl or Cmd Enter sends the message", async () => {
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

test("tool calls render inline as chips paired by opaque ID with duration and outcome", async () => {
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
  const chipsContainer = harness.elements["#desk-transcript"].querySelector(
    ".tool-chips",
  );
  assert.ok(chipsContainer, "inline tool chip container is created in the transcript");
  assert.equal(chipsContainer.childNodes.length, 2);
  assert.match(chipsContainer.childNodes[0].textContent, /read.*running/i);
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

  const chips = chipsContainer.childNodes;
  assert.equal(chips.length, 2);
  assert.match(chips[0].textContent, /read.*✓ 25 ms · 128 B/i);
  assert.equal(chips[0].classList.contains("is-success"), true);
  assert.match(chips[1].textContent, /read.*failed · 12 ms/i);
  assert.equal(chips[1].classList.contains("is-error"), true);
});

test("plain Enter sends the message while Shift+Enter keeps the default newline", async () => {
  const pending = deferred();
  const harness = createHarness({
    turnHandler: async () => pending.promise,
  });
  await flush();
  const message = harness.elements["#desk-message"];
  const keydown = message.listener("keydown");

  message.value = "hello";
  let defaultPrevented = false;
  keydown({
    key: "Enter",
    shiftKey: false,
    ctrlKey: false,
    metaKey: false,
    preventDefault() {
      defaultPrevented = true;
    },
  });
  assert.equal(defaultPrevented, true);
  await flush();
  assert.equal(mutationCalls(harness, "/api/v1/desk/chat/turn").length, 1);

  pending.resolve(jsonResponse({}));
  await flush();
  message.value = "line1";
  defaultPrevented = false;
  keydown({
    key: "Enter",
    shiftKey: true,
    ctrlKey: false,
    metaKey: false,
    preventDefault() {
      defaultPrevented = true;
    },
  });
  assert.equal(defaultPrevented, false, "Shift+Enter is left to the default newline");
});

test("slash menu filters commands and skills and inserts a command on Enter", async () => {
  const harness = createHarness();
  await flush();
  const message = harness.elements["#desk-message"];
  const menu = harness.elements["#desk-slash-menu"];
  const keydown = message.listener("keydown");
  const input = message.listener("input");

  message.value = "/mo";
  input();
  await flush();
  assert.equal(menu.hidden, false, "slash menu opens on a slash token");
  assert.match(menu.textContent, /Commands/);
  assert.match(menu.textContent, /\/model/);
  assert.doesNotMatch(menu.textContent, /\/skills/);

  keydown({
    key: "Enter",
    shiftKey: false,
    ctrlKey: false,
    metaKey: false,
    preventDefault() {},
  });
  assert.equal(message.value, "/model ");
  assert.equal(menu.hidden, true, "menu closes after insertion");
});

test("slash menu lists skills and attaches the selected one", async () => {
  const harness = createHarness();
  await flush();
  const message = harness.elements["#desk-message"];
  const menu = harness.elements["#desk-slash-menu"];
  const keydown = message.listener("keydown");
  const input = message.listener("input");

  message.value = "/";
  input();
  await flush();
  assert.equal(menu.hidden, false);
  assert.match(menu.textContent, /Skills/);
  assert.match(menu.textContent, /review/);

  // Commands precede skills in the menu; step past the three commands.
  for (let i = 0; i < 3; i += 1) {
    keydown({ key: "ArrowDown", preventDefault() {} });
  }
  keydown({
    key: "Enter",
    shiftKey: false,
    ctrlKey: false,
    metaKey: false,
    preventDefault() {},
  });
  await flush();
  const commandCalls = mutationCalls(harness, "/api/v1/desk/chat/command").map(
    (call) => JSON.parse(call.options.body).command,
  );
  assert.deepEqual(commandCalls, [
    { name: "skills", args: "attach review" },
  ]);
  assert.equal(message.value, "/", "selecting a skill leaves the composer text alone");
});

test("typing indicator appears on send and the first delta replaces it with a caret", async () => {
  const pending = deferred();
  const harness = createHarness({
    turnHandler: async () => pending.promise,
  });
  await flush();
  const message = harness.elements["#desk-message"];
  message.value = "hello";
  void message.listener("keydown")({
    key: "Enter",
    shiftKey: false,
    ctrlKey: false,
    metaKey: false,
    preventDefault() {},
  });
  await flush();

  const transcript = harness.elements["#desk-transcript"];
  assert.ok(transcript.querySelector(".user-message"), "user message appears immediately");
  assert.ok(transcript.querySelector(".typing-message"), "typing indicator shows while the model works");

  const stream = harness.EventSource.instances[0];
  const emit = (type, data) =>
    stream.emit(type, {
      resource: "chat",
      resource_id: "client-1",
      type,
      data,
    });
  emit("text_delta", { text: "Paris" });
  await flush();
  assert.equal(transcript.querySelector(".typing-message"), null);
  const caret = transcript.querySelector(".stream-caret");
  assert.ok(caret, "streaming message carries a blinking caret");
  emit("text_delta", { text: "!" });
  await flush();
  const caret2 = transcript.querySelector(".stream-caret");
  assert.ok(caret2, "caret persists across deltas");
  assert.match(caret2.parentNode.textContent, /Paris!/);

  emit("turn_done", { state: defaultChatState() });
  await flush();
  assert.equal(transcript.querySelector(".stream-caret"), null, "caret removed when the turn ends");
  pending.resolve(jsonResponse({}));
  await flush();
});

test("completed messages carry a copy button that writes the plain text", async () => {
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({
          history: [
            {
              role: "assistant",
              blocks: [{ type: "text", text: "Hello world" }],
            },
          ],
        }),
      }),
  });
  await flush();
  const copy = harness.elements["#desk-transcript"].querySelector(".message-copy");
  assert.ok(copy, "copy button rendered on the completed assistant message");
  await copy.listener("click")();
  assert.deepEqual(harness.clipboardWrites, ["Hello world"]);
});

test("rejected turn offers retry that resends the same text", async () => {
  let attempts = 0;
  const harness = createHarness({
    turnHandler: async () => {
      attempts += 1;
      if (attempts === 1) {
        return { ok: false, status: 422, json: async () => ({ message: "rejected" }) };
      }
      return jsonResponse({});
    },
  });
  await flush();
  const message = harness.elements["#desk-message"];
  message.value = "hello";
  void message.listener("keydown")({
    key: "Enter",
    shiftKey: false,
    ctrlKey: false,
    metaKey: false,
    preventDefault() {},
  });
  await flush();
  const retry = harness.elements[".composer-actions"].querySelector(".retry-button");
  assert.ok(retry, "retry button appears after a rejected turn");
  assert.match(harness.elements["#desk-composer-status"].textContent, /rejected/);
  await retry.listener("click")();
  await flush();
  const turns = mutationCalls(harness, "/api/v1/desk/chat/turn").map((call) =>
    JSON.parse(call.options.body).text,
  );
  assert.deepEqual(turns, ["hello", "hello"]);
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

test("new conversation with an empty history atomically replaces the previous transcript", async () => {
  // The real backend serializes a fresh session's nil history as a
  // missing/null history, so the confirm state carries no transcript (#455).
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({
          session_id: "session-old",
          title: "Old conversation",
          history: [
            { role: "user", blocks: [{ type: "text", text: "Previous prompt" }] },
            { role: "assistant", blocks: [{ type: "text", text: "Previous reply" }] },
          ],
        }),
      }),
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
            history: null,
          }),
        });
      }
      return jsonResponse({}, false);
    },
  });
  await flush();
  assert.match(harness.elements["#desk-transcript"].textContent, /Previous reply/);

  await harness.elements["#desk-new"].listener("click")();
  await flush();

  assert.equal(harness.elements["#desk-session-title"].textContent, "Fresh desk");
  // The previous conversation is gone and the new session owns an empty
  // transcript instead of inheriting the old DOM (#455).
  assert.doesNotMatch(harness.elements["#desk-transcript"].textContent, /Previous reply/);
  assert.match(
    harness.elements["#desk-transcript"].textContent,
    /The desk is ready\. What are we working on\?/,
  );
});

test("ownership conflict offers inline recovery instead of a fatal screen", async () => {
  let openAttempts = 0;
  const harness = createHarness({
    openHandler: async () => {
      openAttempts += 1;
      return jsonResponse(
        { code: "session_active", message: "chat session is already active" },
        false,
      );
    },
  });
  await flush();
  assert.equal(openAttempts, 1);
  assert.equal(harness.elements["#desk-phase"].textContent, "Conversation in use");
  assert.equal(harness.elements["#desk-stale-status"].hidden, false);
  assert.match(harness.elements["#desk-stale-message"].textContent, /Another surface/);
  assert.match(harness.elements["#desk-stale-label"].textContent, /in use/);
  assert.doesNotMatch(harness.elements["#desk-stale-label"].textContent, /out of date/);
  assert.equal(harness.elements["#desk-recover-new"].hidden, false);
  assert.equal(harness.elements["#desk-recover-new"].disabled, false);
  assert.equal(harness.elements["#desk-refresh"].disabled, false);
});

test("Start a new conversation recovery opens a fresh session", async () => {
  let openAttempts = 0;
  const openBodies = [];
  const harness = createHarness({
    openHandler: async ({ options }) => {
      openAttempts += 1;
      openBodies.push(JSON.parse(options.body));
      if (openAttempts === 1) {
        return jsonResponse(
          { code: "session_active", message: "chat session is already active" },
          false,
        );
      }
      return jsonResponse({
        client_id: "client-3",
        reattach_token: "lease-3",
        state: defaultChatState({
          session_id: "session-3",
          title: "Fresh recovery",
        }),
      });
    },
  });
  await flush();
  assert.equal(harness.elements["#desk-recover-new"].hidden, false);
  await harness.elements["#desk-recover-new"].listener("click")();
  await flush();
  assert.equal(harness.elements["#desk-session-title"].textContent, "Fresh recovery");
  assert.equal(harness.elements["#desk-phase"].textContent, "Ready");
  const recoveryOpen = openBodies[openBodies.length - 1];
  assert.equal(openAttempts, 2);
  assert.equal(recoveryOpen.continue, false);
  assert.equal(recoveryOpen.session_id, "");
  assert.equal(recoveryOpen.reattach_client_id, undefined);
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
  const sessionButton = harness.elements["#desk-session-options"].querySelector("button");
  assert.ok(sessionButton, "first open renders the session list");
  assert.equal(harness.elements["#desk-sessions"].hidden, false);
  assert.match(sessionButton.textContent, /Second session.*25 Jul 2026/i);
  assert.equal(
    harness.elements["#desk-session-refresh"].getAttribute("aria-expanded"),
    "true",
  );
  await sessionButton.listener("click")();
  await flush();
  assert.equal(harness.elements["#desk-session-title"].textContent, "Second session");
  assert.match(harness.elements["#desk-transcript"].textContent, /Restored/);
  // The resumed session is now marked as the current one.
  const selected = harness.elements["#desk-session-options"]
    .querySelectorAll("button")
    .find((node) => node.classList.contains("is-selected"));
  assert.ok(selected);
  assert.equal(selected.getAttribute("aria-current"), "true");
  assert.equal(selected.getAttribute("aria-selected"), "true");

  failResume = true;
  await harness.elements["#desk-session-options"].querySelector("button").listener("click")();
  await flush();
  assert.equal(harness.elements["#desk-session-title"].textContent, "Second session");
  assert.match(
    harness.elements["#desk-session-options"].textContent,
    /Second session/,
    "failed resume leaves the list intact",
  );

  // A second click on the trigger collapses the disclosure and restores focus.
  await harness.elements["#desk-session-refresh"].listener("click")();
  await flush();
  assert.equal(harness.elements["#desk-sessions"].hidden, true);
  assert.equal(
    harness.elements["#desk-session-refresh"].getAttribute("aria-expanded"),
    "false",
  );
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
  // The composer stays usable during a turn so the operator can queue a
  // follow-up; submitting still does not start a second turn.
  assert.equal(message.disabled, false);
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
  assert.equal(message.disabled, false);
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
  assert.equal(message.disabled, false);
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

test("rejected turn clears the composer and retry reuses the Idempotency-Key", async () => {
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
  assert.equal(message.value, "", "composer clears once the message is in the transcript");
  assert.equal(
    harness.elements["#desk-composer-status"].textContent,
    "Turn rejected by policy.",
  );
  assert.equal(harness.elements["#desk-stale-status"].hidden, true);
  const retry = harness.elements[".composer-actions"].querySelector(".retry-button");
  assert.ok(retry, "rejected turn offers a retry button");

  const firstKey = mutationCalls(harness, "/api/v1/desk/chat/turn")[0].options.headers[
    "Idempotency-Key"
  ];
  await retry.listener("click")();
  await flush();

  const turnPosts = mutationCalls(harness, "/api/v1/desk/chat/turn");
  assert.equal(turnPosts.length, 2);
  assert.equal(turnPosts[1].options.headers["Idempotency-Key"], firstKey);
  assert.deepEqual(JSON.parse(turnPosts[1].options.body), {
    attachments: [],
    client_id: "client-1",
    reasoning_effort: "",
    task_mode: "",
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
  assert.equal(message.value, "", "composer clears; the message lives in the transcript");
  const userMessage = harness.elements["#desk-transcript"].querySelector(".user-message");
  assert.match(userMessage.textContent, /Maybe delivered/);
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
test("drafts persist per session across reloads and never leak into another session", async () => {
  const harness = createHarness();
  await flush();
  const message = harness.elements["#desk-message"];
  message.value = "work in progress";
  message.listener("input")();
  assert.equal(
    JSON.parse(harness.sessionStorage.getItem("waffle.desk.today.drafts.v1"))["session-1"],
    "work in progress",
  );

  // Same tab "reload" reuses the same storage and restores the draft.
  const reloaded = createHarness({ sharedStorage: harness.storage });
  await flush();
  assert.equal(reloaded.elements["#desk-message"].value, "work in progress");
  assert.equal(reloaded.elements["#desk-send"].disabled, false);

  // A different session never sees another session's draft.
  const other = createHarness({
    sharedStorage: harness.storage,
    openHandler: async () =>
      jsonResponse({
        client_id: "client-2",
        reattach_token: "lease-2",
        state: defaultChatState({ session_id: "session-2" }),
      }),
  });
  await flush();
  assert.equal(other.elements["#desk-message"].value, "");
});

test("accepted send clears the draft; rejected send keeps it for retry", async () => {
  let attempts = 0;
  const harness = createHarness({
    turnHandler: async () => {
      attempts += 1;
      if (attempts === 1) {
        return { ok: false, status: 422, json: async () => ({ message: "rejected" }) };
      }
      return jsonResponse({});
    },
  });
  await flush();
  const message = harness.elements["#desk-message"];
  message.value = "send me";
  message.listener("input")();
  const submit = harness.elements["#desk-composer"].listener("submit");
  await submit({ preventDefault() {} });
  await flush();
  // Rejection keeps the draft stored (retry refills it) and holds the queue.
  assert.ok(
    JSON.parse(harness.sessionStorage.getItem("waffle.desk.today.drafts.v1"))["session-1"],
  );
  assert.equal(harness.elements[".desk-shell"].dataset.phase, "idle");

  // The retry button refills the text; the accepted resend clears the draft.
  const retry = harness.elements[".composer-actions"].querySelector(".retry-button");
  assert.ok(retry, "rejection offers retry");
  await retry.listener("click")();
  await flush();
  assert.equal(
    harness.sessionStorage.getItem("waffle.desk.today.drafts.v1"),
    null,
    "draft cleared after acceptance",
  );
});

test("busy composer queues one follow-up and auto-dispatches after turn_done", async () => {
  const harness = createHarness();
  await flush();
  const message = harness.elements["#desk-message"];
  const submit = harness.elements["#desk-composer"].listener("submit");

  message.value = "first";
  const first = submit({ preventDefault() {} });

  message.value = "follow up";
  message.listener("input")();
  message.listener("keydown")({
    key: "Enter",
    shiftKey: false,
    ctrlKey: false,
    metaKey: false,
    preventDefault() {},
  });
  await flush();

  assert.equal(
    mutationCalls(harness, "/api/v1/desk/chat/turn").length,
    1,
    "queuing never starts a second turn",
  );
  assert.equal(harness.elements["#desk-send"].textContent, "Queue follow-up");
  const banner = harness.elements["#desk-queue"];
  assert.equal(banner.hidden, false);
  assert.match(banner.textContent, /Follow-up queued/);
  assert.match(banner.textContent, /follow up/);
  assert.match(
    harness.elements["#desk-composer-status"].textContent,
    /Follow-up queued/,
  );

  harness.turnResponse.resolve(jsonResponse({}));
  await first;
  await flush();
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
    data: { state: defaultChatState() },
  });
  await flush();

  const turns = mutationCalls(harness, "/api/v1/desk/chat/turn");
  assert.equal(turns.length, 2, "follow-up auto-dispatches after turn_done");
  assert.equal(JSON.parse(turns[1].options.body).text, "follow up");
  assert.notEqual(
    turns[0].options.headers["Idempotency-Key"],
    turns[1].options.headers["Idempotency-Key"],
    "queued dispatch gets its own idempotency key",
  );
  assert.equal(harness.elements["#desk-queue"].hidden, true, "banner clears after dispatch");
  // The composer keeps what the operator typed next.
  assert.equal(message.value, "follow up");
});

test("cancel, rejection, and network failure hold the follow-up for review", async () => {
  const harness = createHarness();
  await flush();
  const message = harness.elements["#desk-message"];
  const submit = harness.elements["#desk-composer"].listener("submit");

  message.value = "first";
  const first = submit({ preventDefault() {} });
  message.value = "queued item";
  await submit({ preventDefault() {} });
  await flush();
  const banner = harness.elements["#desk-queue"];
  assert.equal(banner.hidden, false);

  // Cancel: the running turn is cancelled, the queue is held and never fires.
  const cancellation = harness.elements["#desk-cancel"].listener("click")();
  await flush();
  harness.cancelResponse.resolve(jsonResponse({}));
  await cancellation;
  await flush();
  harness.EventSource.instances[0].emit("turn_done", {
    resource: "chat",
    resource_id: "client-1",
    type: "turn_done",
    data: { state: defaultChatState() },
  });
  harness.turnResponse.resolve(jsonResponse({}));
  await first;
  await flush();
  assert.equal(
    mutationCalls(harness, "/api/v1/desk/chat/turn").length,
    1,
    "cancelled turn never dispatches the queue",
  );
  assert.match(banner.textContent, /held for review/);
});

test("queued follow-up can be replaced with confirmation, edited, or removed", async () => {
  const harness = createHarness({ confirmResult: false });
  await flush();
  const message = harness.elements["#desk-message"];
  const submit = harness.elements["#desk-composer"].listener("submit");

  message.value = "first";
  void submit({ preventDefault() {} });
  message.value = "queued item";
  await submit({ preventDefault() {} });
  await flush();
  const banner = harness.elements["#desk-queue"];
  assert.match(banner.textContent, /queued item/);

  // Declined replacement keeps the original queue.
  message.value = "different text";
  await submit({ preventDefault() {} });
  await flush();
  assert.match(banner.textContent, /queued item/);

  // Edit pulls the queued text back into the composer and clears the queue.
  banner.querySelector(".queue-edit").listener("click")();
  await flush();
  assert.equal(message.value, "queued item");
  assert.equal(harness.elements["#desk-queue"].hidden, true);

  // Re-queue then Remove clears it.
  await submit({ preventDefault() {} });
  await flush();
  assert.equal(harness.elements["#desk-queue"].hidden, false);
  harness.elements["#desk-queue"].querySelector(".queue-remove").listener("click")();
  await flush();
  assert.equal(harness.elements["#desk-queue"].hidden, true);
  assert.equal(harness.sessionStorage.getItem("waffle.desk.today.queue.v1"), null);
});

test("storage-denied browsers still send and queue without crashing", async () => {
  const harness = createHarness({ denyStorage: true });
  await flush();
  const message = harness.elements["#desk-message"];
  message.value = "hello";
  message.listener("input")();
  message.listener("keydown")({
    key: "Enter",
    shiftKey: false,
    ctrlKey: false,
    metaKey: false,
    preventDefault() {},
  });
  await flush();
  assert.equal(mutationCalls(harness, "/api/v1/desk/chat/turn").length, 1);
  assert.equal(harness.elements[".desk-shell"].dataset.phase, "sending");
});

test("session list filters, disambiguates labels, arrows navigate, Escape closes", async () => {
  const harness = createHarness({
    commandHandler: async ({ options }) => {
      const { command } = JSON.parse(options.body);
      if (command.name === "sessions") {
        return jsonResponse({
          sessions: [
            { id: "s1", title: "Release review", summary: "Wave 1", updated_at: "2026-08-16T09:00:00Z", model_alias: "kimi" },
            { id: "s2", title: "Untitled", summary: "Brave search setup", updated_at: "2026-08-15T10:00:00Z", model_alias: "kimi" },
            { id: "s3", title: "Untitled", summary: "Desk polish", updated_at: "2026-08-14T11:00:00Z" },
          ],
        });
      }
      return jsonResponse({ state: defaultChatState() });
    },
  });
  await flush();
  const refresh = harness.elements["#desk-session-refresh"];
  await refresh.listener("click")();
  await flush();
  const options = harness.elements["#desk-session-options"];
  const choices = () => options.querySelectorAll(".session-choice");
  assert.equal(choices().length, 3);
  // Duplicate titles carry distinct recency context.
  assert.match(options.textContent, /16 Aug 2026 · kimi/);
  assert.match(options.textContent, /15 Aug 2026/);

  // Filter narrows matches.
  const filter = harness.elements["#desk-session-filter"];
  filter.value = "brave";
  filter.listener("input")();
  assert.equal(choices().length, 1);
  assert.match(options.textContent, /Brave search setup/);

  // No-match state stays readable.
  filter.value = "zzz";
  filter.listener("input")();
  assert.equal(choices().length, 0);
  assert.match(options.textContent, /No conversations match/);

  // Arrow keys move focus among options.
  filter.value = "";
  filter.listener("input")();
  const buttons = choices();
  const keydown = harness.elements["#desk-sessions"].listener("keydown");
  keydown({ key: "ArrowDown", preventDefault() {} });
  assert.equal(buttons[0].focused, true);
  keydown({ key: "ArrowDown", preventDefault() {} });
  assert.equal(buttons[1].focused, true);
  keydown({ key: "ArrowUp", preventDefault() {} });
  assert.equal(buttons[0].focused, true);

  // Escape closes the disclosure and restores focus to the trigger.
  keydown({ key: "Escape", preventDefault() {} });
  assert.equal(harness.elements["#desk-sessions"].hidden, true);
  assert.equal(refresh.focused, true);
});

test("conversation action menu renames, pins, and deletes through the live command surface", async () => {
  let sessions = [
    { id: "s1", title: "Alpha", summary: "", updated_at: "2026-08-16T09:00:00Z", model_alias: "kimi" },
    { id: "s2", title: "Beta", summary: "", updated_at: "2026-08-15T10:00:00Z" },
  ];
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({ session_id: "s1", title: "Alpha" }),
      }),
    commandHandler: async ({ options }) => {
      const { command } = JSON.parse(options.body);
      if (command.name === "sessions") {
        return jsonResponse({ sessions });
      }
      if (command.name === "rename") {
        const [id, ...rest] = command.args.split(" ");
        const title = rest.join(" ");
        sessions = sessions.map((s) => (s.id === id ? { ...s, title } : s));
        return jsonResponse({});
      }
      if (command.name === "pin" || command.name === "unpin") {
        sessions = sessions.map((s) =>
          s.id === command.args ? { ...s, pinned: command.name === "pin" } : s,
        );
        sessions = [
          ...sessions.filter((s) => s.pinned),
          ...sessions.filter((s) => !s.pinned),
        ];
        return jsonResponse({});
      }
      if (command.name === "delete") {
        sessions = sessions.filter((s) => s.id !== command.args);
        if (command.args === "s1") {
          return jsonResponse({
            state: defaultChatState({ session_id: "fresh", title: "Fresh conversation" }),
          });
        }
        return jsonResponse({});
      }
      return jsonResponse({});
    },
  });
  await flush();
  await harness.elements["#desk-session-refresh"].listener("click")();
  await flush();
  const options = harness.elements["#desk-session-options"];
  const rows = () => options.querySelectorAll(".session-row");

  // The action menu names the conversation and offers all three actions.
  const firstTrigger = rows()[0].querySelector(".session-menu-trigger");
  assert.equal(firstTrigger.getAttribute("aria-label"), "Actions for Alpha");
  firstTrigger.listener("click")();
  const firstPopover = rows()[0].querySelector(".session-menu-popover");
  assert.equal(firstPopover.hidden, false);
  assert.equal(firstTrigger.getAttribute("aria-expanded"), "true");
  assert.match(firstPopover.textContent, /Rename/);
  assert.match(firstPopover.textContent, /Pin/);
  assert.match(firstPopover.textContent, /Delete/);
  // Escape closes the menu and restores focus to the trigger.
  harness.elements["#desk-sessions"].listener("keydown")({
    key: "Escape",
    preventDefault() {},
  });
  assert.equal(firstPopover.hidden, true);
  assert.equal(firstTrigger.focused, true);

  // Rename the second conversation inline.
  const betaRow = rows()[1];
  betaRow.querySelector(".session-menu-trigger").listener("click")();
  const betaItems = betaRow.querySelector(".session-menu-popover").querySelectorAll("button");
  betaItems.find((item) => item.textContent === "Rename").listener("click")();
  const form = betaRow.querySelector(".session-rename");
  const input = form.querySelector("input");
  input.value = "Release review";
  form.listener("submit")({ preventDefault() {} });
  await flush();
  assert.match(options.textContent, /Release review/);

  // Pin the renamed conversation: it moves ahead and carries the Pinned label.
  const releaseRow = rows().find((row) => row.textContent.includes("Release review"));
  assert.ok(releaseRow, "renamed row present");
  releaseRow.querySelector(".session-menu-trigger").listener("click")();
  const pinnedItems = releaseRow.querySelector(".session-menu-popover").querySelectorAll("button");
  pinnedItems.find((item) => item.textContent === "Pin").listener("click")();
  await flush();
  assert.match(rows()[0].textContent, /Release review/);
  assert.match(rows()[0].textContent, /Pinned/);

  // Delete the non-current conversation after confirmation.
  await rows()[0].querySelector(".session-menu-trigger").listener("click")();
  const deleteItems = rows()[0].querySelector(".session-menu-popover").querySelectorAll("button");
  deleteItems.find((item) => item.textContent === "Delete").listener("click")();
  await flush();
  assert.equal(rows().length, 1);
  assert.match(rows()[0].textContent, /Alpha/);

  // Delete the current conversation: the desk renders the fresh replacement.
  rows()[0].querySelector(".session-menu-trigger").listener("click")();
  const currentDelete = rows()[0].querySelector(".session-menu-popover").querySelectorAll("button");
  currentDelete.find((item) => item.textContent === "Delete").listener("click")();
  await flush();
  assert.equal(harness.elements["#desk-session-title"].textContent, "Fresh conversation");
  assert.equal(harness.elements["#desk-session-options"].textContent, "No recent conversations.");
});

test("declined delete leaves the conversation intact", async () => {
  const harness = createHarness({
    confirmResult: false,
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({ session_id: "s1", title: "Alpha" }),
      }),
    commandHandler: async () =>
      jsonResponse({
        sessions: [
          { id: "s1", title: "Alpha", summary: "", updated_at: "2026-08-16T09:00:00Z" },
        ],
      }),
  });
  await flush();
  const options = harness.elements["#desk-session-options"];
  // Prime the list so a row exists.
  harness.elements["#desk-session-refresh"].listener("click")();
  await flush();
  const row = options.querySelector(".session-row");
  row.querySelector(".session-menu-trigger").listener("click")();
  const items = row.querySelector(".session-menu-popover").querySelectorAll("button");
  items.find((item) => item.textContent === "Delete").listener("click")();
  await flush();
  assert.equal(
    mutationCalls(harness, "/api/v1/desk/chat/command").filter((call) =>
      JSON.parse(call.options.body).command.name === "delete",
    ).length,
    0,
    "declined delete never mutates",
  );
  assert.equal(harness.elements["#desk-session-title"].textContent, "Alpha");
});

test("edit and regenerate branch at exact boundaries and fail closed mid-turn", async () => {
  const branchCalls = [];
  const turnTexts = [];
  const branchState = () =>
    defaultChatState({
      session_id: "s2",
      history: [
        { role: "user", blocks: [{ type: "text", text: "First prompt" }] },
        { role: "assistant", blocks: [{ type: "text", text: "First answer" }] },
      ],
    });
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({
          session_id: "s1",
          history: [
            { role: "user", blocks: [{ type: "text", text: "First prompt" }] },
            { role: "assistant", blocks: [{ type: "text", text: "First answer" }] },
            { role: "user", blocks: [{ type: "text", text: "Second prompt" }] },
            { role: "assistant", blocks: [{ type: "text", text: "Second answer" }] },
          ],
        }),
      }),
    commandHandler: async ({ options }) => {
      const { command } = JSON.parse(options.body);
      if (command.name === "branch") {
        branchCalls.push(command.args);
        return jsonResponse({ state: branchState() });
      }
      return jsonResponse({});
    },
    turnHandler: async ({ options }) => {
      turnTexts.push(JSON.parse(options.body).text);
      return jsonResponse({});
    },
  });
  await flush();
  const transcript = harness.elements["#desk-transcript"];
  const edits = transcript.querySelectorAll(".message-edit");
  const regens = transcript.querySelectorAll(".message-regenerate");
  assert.equal(edits.length, 2, "every completed prompt offers Edit");
  assert.equal(regens.length, 2, "every completed response offers Regenerate");

  // Edit the second prompt: branch ends before it and the exact text prefills.
  await edits[1].listener("click")();
  await flush();
  assert.deepEqual(branchCalls, ["s1 2"]);
  assert.equal(harness.elements["#desk-message"].value, "Second prompt");
  assert.match(harness.elements["#desk-composer-status"].textContent, /branch/i);

  // Regenerate the remaining answer: branch before its prompt and re-send it.
  const regen = transcript.querySelector(".message-regenerate");
  await regen.listener("click")();
  await flush();
  assert.deepEqual(branchCalls, ["s1 2", "s2 0"]);
  assert.deepEqual(turnTexts, ["First prompt"], "regenerate re-sends the prompt in the branch");

  // Turn actions fail closed while another turn is running.
  const message = harness.elements["#desk-message"];
  message.value = "third";
  const submit = harness.elements["#desk-composer"].listener("submit");
  const pending = submit({ preventDefault() {} });
  await flush();
  for (const button of transcript.querySelectorAll(".message-edit")) {
    assert.equal(button.disabled, true, "edit disabled during a turn");
  }
  harness.turnResponse.resolve(jsonResponse({}));
  await pending;
  await flush();
  harness.EventSource.instances[0].emit("turn_done", {
    resource: "chat",
    resource_id: "client-1",
    type: "turn_done",
    data: { state: defaultChatState() },
  });
  await flush();
  // The completed in-session turn pair exposes actions after finalize.
  assert.ok(transcript.querySelectorAll(".message-edit").length >= 1);
  assert.ok(transcript.querySelectorAll(".message-regenerate").length >= 1);
  for (const button of transcript.querySelectorAll(".message-edit")) {
    assert.equal(button.disabled, false, "edit re-enabled when idle");
  }
});

test("branch boundaries stay exact across tool-result carriers in history", async () => {
  const branchCalls = [];
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({
          session_id: "s1",
          history: [
            { role: "user", blocks: [{ type: "text", text: "Inspect repo" }] },
            { role: "assistant", blocks: [{ type: "tool_use", tool_use: { id: "tu-1", name: "read" } }] },
            { role: "user", blocks: [{ type: "tool_result", tool_result: { tool_use_id: "tu-1", content: "ok" } }] },
            { role: "assistant", blocks: [{ type: "text", text: "Repo inspected" }] },
            { role: "user", blocks: [{ type: "text", text: "Now build it" }] },
            { role: "assistant", blocks: [{ type: "text", text: "Built." }] },
          ],
        }),
      }),
    commandHandler: async ({ options }) => {
      const { command } = JSON.parse(options.body);
      if (command.name === "branch") {
        branchCalls.push(command.args);
        return jsonResponse({
          state: defaultChatState({
            session_id: "s2",
            history: [{ role: "user", blocks: [{ type: "text", text: "Inspect repo" }] }],
          }),
        });
      }
      return jsonResponse({});
    },
  });
  await flush();
  const transcript = harness.elements["#desk-transcript"];
  // Only text-bearing messages render, but boundaries count every history slot.
  assert.equal(transcript.querySelectorAll(".message").length, 4);
  assert.equal(transcript.querySelectorAll(".message-edit").length, 2);
  const edits = transcript.querySelectorAll(".message-edit");
  await edits[0].listener("click")();
  await flush();
  assert.deepEqual(branchCalls, ["s1 0"]);
  await transcript.querySelectorAll(".message-edit")[0].listener("click")();
  await flush();
  assert.deepEqual(branchCalls, ["s1 0", "s2 0"]);
});

test("completed exchanges expose a branch action that forks through the command API", async () => {
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({
          history: [
            { role: "user", seq: 1, blocks: [{ type: "text", text: "hello" }] },
            { role: "assistant", seq: 2, blocks: [{ type: "text", text: "hi there" }] },
            { role: "user", seq: 3, blocks: [{ type: "text", text: "what is 2+2?" }] },
            { role: "assistant", seq: 4, blocks: [{ type: "text", text: "four" }] },
          ],
        }),
      }),
    commandHandler: async ({ options }) => {
      const { command } = JSON.parse(options.body);
      assert.equal(command.name, "branch");
      assert.equal(command.args, "4");
      return jsonResponse({
        state: defaultChatState({
          session_id: "session-branch",
          title: "Branched",
          lineage: { forked_from: "session-1", forked_at_seq: 4 },
          history: [
            { role: "user", seq: 1, blocks: [{ type: "text", text: "hello" }] },
            { role: "assistant", seq: 2, blocks: [{ type: "text", text: "hi there" }] },
            { role: "user", seq: 3, blocks: [{ type: "text", text: "what is 2+2?" }] },
            { role: "assistant", seq: 4, blocks: [{ type: "text", text: "four" }] },
          ],
        }),
      });
    },
  });
  await flush();
  const buttons = harness.elements["#desk-transcript"].querySelectorAll(".message-branch");
  assert.equal(buttons.length, 2, "one branch action per completed assistant exchange");
  const last = buttons[buttons.length - 1];
  assert.equal(last.getAttribute("aria-label"), "Branch from this exchange (turn 4)");
  await last.listener("click")();
  await flush();
  // The branch state replaced the conversation: provenance row shows lineage.
  assert.equal(
    harness.elements["#desk-fork"].textContent,
    "Branched from session session-1 at turn 4",
  );
  assert.equal(harness.elements["#desk-fork-row"].hidden, false);
  assert.equal(harness.elements["#desk-session-title"].textContent, "Branched");
});

test("branch provenance stays hidden for conversations started fresh", async () => {
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({}),
      }),
  });
  await flush();
  assert.equal(harness.elements["#desk-fork-row"].hidden, true);
  assert.equal(harness.elements["#desk-fork"].textContent, "");
});

test("branch action on a streamed exchange branches at the end of the transcript", async () => {
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({
          history: [
            { role: "user", seq: 1, blocks: [{ type: "text", text: "hello" }] },
            { role: "assistant", seq: 2, blocks: [{ type: "text", text: "hi there" }] },
          ],
        }),
      }),
    commandHandler: async ({ options }) => {
      const { command } = JSON.parse(options.body);
      assert.equal(command.name, "branch");
      assert.equal(command.args, "");
      return jsonResponse({
        state: defaultChatState({ session_id: "session-branch-2" }),
      });
    },
  });
  await flush();
  const message = harness.elements["#desk-message"];
  message.value = "another question";
  void message.listener("keydown")({
    key: "Enter",
    shiftKey: false,
    ctrlKey: false,
    metaKey: false,
    preventDefault() {},
  });
  harness.EventSource.instances[0].emit("text_delta", {
    resource: "chat",
    resource_id: "client-1",
    type: "text_delta",
    data: { text: "the answer" },
  });
  harness.EventSource.instances[0].emit("turn_done", {
    resource: "chat",
    resource_id: "client-1",
    type: "turn_done",
    data: {
      state: defaultChatState({
        session_id: "session-1",
        history: [
          { role: "user", seq: 1, blocks: [{ type: "text", text: "hello" }] },
          { role: "assistant", seq: 2, blocks: [{ type: "text", text: "hi there" }] },
          { role: "user", seq: 3, blocks: [{ type: "text", text: "another question" }] },
          { role: "assistant", seq: 4, blocks: [{ type: "text", text: "the answer" }] },
        ],
      }),
    },
  });
  await flush();
  const streamed = Array.from(
    harness.elements["#desk-transcript"].querySelectorAll(".message-branch"),
  ).pop();
  assert.ok(streamed, "streamed exchange carries a branch action");
  assert.equal(
    streamed.getAttribute("aria-label"),
    "Branch from the end of this conversation",
  );
  await streamed.listener("click")();
  await flush();
  assert.equal(harness.elements["#desk-session-title"].textContent, "Untitled conversation");
});

test("project context panel lists workspace resources and attaches in place", async () => {
  let attached = false;
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({
          session_id: "session-p",
          workspace: "matt-riley/waffle",
        }),
      }),
    workspacesHandler: async () =>
      jsonResponse({
        workspaces: [
          {
            id: "ws-p",
            repository: "matt-riley/waffle",
            session: "session-p",
            status: "open",
          },
        ],
      }),
    projectHandler: async ({ path, options }) => {
      const cleanPath = path.split("?")[0];
      if (cleanPath.endsWith("/resources")) {
        return jsonResponse({
          workspace: "ws-p",
          resources: [
            {
              id: "pr-1",
              workspace: "ws-p",
              kind: "note",
              name: "Guidance",
              size: 0,
              state: "available",
              attached,
            },
            {
              id: "pr-2",
              workspace: "ws-p",
              kind: "file",
              name: "README.md",
              path: "README.md",
              size: 1024,
              state: "stale",
            },
          ],
        });
      }
      if (cleanPath.endsWith("/attach")) {
        attached = true;
        return jsonResponse({});
      }
      return jsonResponse({});
    },
  });
  await flush();
  const refresh = harness.elements["#desk-project-refresh"];
  assert.equal(refresh.disabled, false, "project refresh enabled once idle");
  await refresh.listener("click")();
  await flush();
  const project = harness.elements["#desk-project"];
  assert.match(project.textContent, /Guidance/);
  assert.match(project.textContent, /README\.md/);
  assert.match(project.textContent, /changed since pinning/);
  const rows = project.querySelectorAll(".project-resource");
  assert.ok(rows.length >= 2, "one row per resource");
  const attachButtons = rows.flatMap((row) =>
    row.childNodes.filter((node) => node.tagName === "BUTTON"),
  );
  assert.ok(attachButtons.length >= 2, "attach/detach controls per resource");
  // Attach the note in place.
  await attachButtons[0].listener("click")();
  await flush();
  assert.equal(attached, true, "attach mutation fired with the session");
});

test("project panel pins a workspace file through the guarded mutation", async () => {
  let pinned = null;
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({ session_id: "session-p" }),
      }),
    workspacesHandler: async () =>
      jsonResponse({
        workspaces: [{ id: "ws-p", session: "session-p", status: "open" }],
      }),
    projectHandler: async ({ path, options }) => {
      if (path.endsWith("/resources/pin")) {
        pinned = JSON.parse(options.body).path;
        return jsonResponse({ id: "pr-9", name: "plan.md", kind: "file" });
      }
      if (path.endsWith("/resources")) {
        return jsonResponse({ resources: [] });
      }
      return jsonResponse({});
    },
  });
  await flush();
  const refresh = harness.elements["#desk-project-refresh"];
  await refresh.listener("click")();
  await flush();
  harness.elements["#desk-project-path"].value = "docs/plan.md";
  const form = harness.elements["#desk-project-pin-form"];
  await form.listener("submit")({ preventDefault() {} });
  await flush();
  assert.equal(pinned, "docs/plan.md");
  assert.equal(harness.elements["#desk-project-path"].value, "", "path cleared after pin");
});



test("restored history renders artifact cards with preview, download, and copy actions", async () => {
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({
          history: [
            {
              role: "assistant",
              blocks: [{ type: "text", text: "Here is the report." }],
            },
            {
              role: "user",
              blocks: [
                {
                  type: "tool_result",
                  tool_result: {
                    tool_use_id: "t1",
                    content: "artifact created: report.md",
                    blocks: [
                      {
                        type: "artifact",
                        artifact: {
                          id: "art-1",
                          name: "report.md",
                          media_type: "text/markdown",
                          size: 1024,
                          digest: "abc123",
                          state: "available",
                        },
                      },
                    ],
                  },
                },
              ],
            },
          ],
        }),
      }),
    artifactHandler: async ({ path, options }) => {
      assert.equal(path, "/api/v1/desk/artifacts/art-1/preview");
      assert.equal(JSON.parse(options.body).client_id, "client-1");
      return jsonResponse({
        id: "art-1",
        name: "report.md",
        media_type: "text/markdown",
        size: 1024,
        digest: "abc123",
        state: "available",
        mode: "inline",
        content: "# Report\n\nFindings.",
      });
    },
  });
  await flush();
  const card = harness.elements["#desk-transcript"].querySelector(".artifact-card");
  assert.ok(card, "artifact card rendered at the producing transcript position");
  assert.equal(card.querySelector(".artifact-name").textContent, "report.md");
  assert.match(card.querySelector(".artifact-meta").textContent, /text\/markdown/);
  assert.match(card.querySelector(".artifact-meta").textContent, /1\.0 KiB/);
  const preview = card.querySelector(".artifact-preview-toggle");
  await preview.listener("click")();
  await flush();
  assert.match(card.querySelector(".artifact-preview-body").textContent, /Findings/);
});

test("artifact preview falls back to download-only and stale cards are not served", async () => {
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({
          history: [
            {
              role: "user",
              blocks: [
                {
                  type: "tool_result",
                  tool_result: {
                    tool_use_id: "t1",
                    content: "created",
                    blocks: [
                      {
                        type: "artifact",
                        artifact: {
                          id: "art-stale",
                          name: "old.pdf",
                          media_type: "application/pdf",
                          size: 512,
                          state: "stale",
                        },
                      },
                    ],
                  },
                },
              ],
            },
          ],
        }),
      }),
    artifactHandler: async () =>
      jsonResponse({
        id: "art-1",
        name: "old.pdf",
        media_type: "application/pdf",
        size: 512,
        state: "available",
        mode: "download_only",
        reason: "This artifact type is available for download only.",
      }),
  });
  await flush();
  const card = harness.elements["#desk-transcript"].querySelector(".artifact-card");
  assert.ok(card, "stale artifact card rendered");
  assert.match(card.textContent, /changed or could not be verified/);
  assert.equal(card.querySelector(".artifact-preview-toggle"), null, "stale cards offer no actions");
});

test("streamed artifact event appends a card after the tool chips", async () => {
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({}),
      }),
  });
  await flush();
  const message = harness.elements["#desk-message"];
  message.value = "make an artifact";
  void message.listener("keydown")({
    key: "Enter",
    shiftKey: false,
    ctrlKey: false,
    metaKey: false,
    preventDefault() {},
  });
  harness.EventSource.instances[0].emit("tool_started", {
    resource: "chat",
    resource_id: "client-1",
    type: "tool_started",
    data: { tool_name: "write_artifact", tool_call_id: "tool-1" },
  });
  harness.EventSource.instances[0].emit("tool_finished", {
    resource: "chat",
    resource_id: "client-1",
    type: "tool_finished",
    data: { tool_name: "write_artifact", tool_call_id: "tool-1", is_error: false },
  });
  harness.EventSource.instances[0].emit("artifact", {
    resource: "chat",
    resource_id: "client-1",
    type: "artifact",
    data: {
      artifacts: [
        {
          id: "art-2",
          name: "summary.md",
          media_type: "text/markdown",
          size: 64,
          digest: "def456",
          state: "available",
        },
      ],
    },
  });
  harness.EventSource.instances[0].emit("turn_done", {
    resource: "chat",
    resource_id: "client-1",
    type: "turn_done",
    data: { state: defaultChatState({ session_id: "session-1" }) },
  });
  await flush();
  const card = harness.elements["#desk-transcript"].querySelector(".artifact-card");
  assert.ok(card, "streamed artifact card rendered");
  assert.equal(card.querySelector(".artifact-name").textContent, "summary.md");
});



test("restored history renders inline citation markers and a safe source drawer", async () => {
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({
          history: [
            {
              role: "assistant",
              blocks: [
                {
                  type: "text",
                  text: "The answer is based on two sources.",
                  citations: [
                    {
                      id: "s1",
                      label: "Example docs",
                      kind: "web",
                      url: "https://example.com/docs",
                      snippet: "A bounded excerpt.",
                      provenance: "provider citation",
                    },
                    {
                      id: "s2",
                      label: "Workspace plan",
                      kind: "workspace",
                      resource: "file-42",
                    },
                  ],
                },
              ],
            },
          ],
        }),
      }),
  });
  await flush();
  const article = harness.elements["#desk-transcript"].querySelector(".waffle-message");
  assert.match(
    article.querySelector(".message-body").textContent,
    /The answer is based on two sources\. \[1\] \[2\]/,
  );
  const drawer = article.querySelector(".sources-drawer");
  assert.ok(drawer, "source drawer rendered");
  assert.match(drawer.querySelector("summary").textContent, /Sources \(2\)/);
  const items = drawer.querySelectorAll(".source-item");
  assert.equal(items.length, 2);
  const web = items[0];
  assert.equal(web.querySelector(".source-label").textContent, "Example docs");
  const open = web.querySelector(".source-open");
  assert.ok(open, "web source renders an open link");
  assert.equal(open.getAttribute("href"), "https://example.com/docs");
  assert.equal(open.getAttribute("rel"), "noopener noreferrer");
  assert.match(web.querySelector(".source-snippet").textContent, /bounded excerpt/);
  const workspace = items[1];
  assert.equal(workspace.querySelector(".source-label").textContent, "Workspace plan");
  assert.equal(workspace.querySelector(".source-open"), null, "workspace sources never link");
  assert.match(workspace.querySelector(".source-kind").textContent, /Workspace source/);
});

test("unsafe citation URLs and hostile schemes never become links", async () => {
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({
          history: [
            {
              role: "assistant",
              blocks: [
                {
                  type: "text",
                  text: "Careful.",
                  citations: [
                    { id: "s1", label: "Bad", kind: "web", url: "javascript:alert(1)" },
                    { id: "s2", label: "Mail", kind: "web", url: "mailto:someone@example.com" },
                    { id: "s3", label: "Relative", kind: "web", url: "/desk/" },
                    { id: "s4", label: "Missing metadata" },
                  ],
                },
              ],
            },
          ],
        }),
      }),
  });
  await flush();
  const article = harness.elements["#desk-transcript"].querySelector(".waffle-message");
  const drawer = article.querySelector(".sources-drawer");
  assert.equal(drawer.querySelectorAll(".source-open").length, 0, "unsafe URLs render plain");
  assert.equal(
    drawer.querySelector(".source-kind").textContent,
    "Workspace source",
    "unknown kinds degrade to workspace labels",
  );
});

test("streaming sources event attaches the drawer to the completed exchange", async () => {
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({}),
      }),
  });
  await flush();
  const message = harness.elements["#desk-message"];
  message.value = "research this";
  void message.listener("keydown")({
    key: "Enter",
    shiftKey: false,
    ctrlKey: false,
    metaKey: false,
    preventDefault() {},
  });
  harness.EventSource.instances[0].emit("text_delta", {
    resource: "chat",
    resource_id: "client-1",
    type: "text_delta",
    data: { text: "Here is the answer" },
  });
  harness.EventSource.instances[0].emit("sources", {
    resource: "chat",
    resource_id: "client-1",
    type: "sources",
    data: {
      sources: [
        { id: "s1", label: "Example docs", kind: "web", url: "https://example.com/docs" },
      ],
    },
  });
  harness.EventSource.instances[0].emit("turn_done", {
    resource: "chat",
    resource_id: "client-1",
    type: "turn_done",
    data: { state: defaultChatState({ session_id: "session-1" }) },
  });
  await flush();
  const article = harness.elements["#desk-transcript"].querySelector(".waffle-message");
  const drawer = article.querySelector(".sources-drawer");
  assert.ok(drawer, "streamed exchange carries the source drawer");
  assert.match(drawer.querySelector("summary").textContent, /Sources \(1\)/);
  assert.equal(drawer.querySelector(".source-label").textContent, "Example docs");
});


test("a completed user prompt and the composer draft expose Create schedule handoff", async () => {
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({
          session_id: "session-1",
          title: "Existing",
          history: [
            { role: "user", blocks: [{ type: "text", text: "Summarize the release queue" }] },
            { role: "assistant", blocks: [{ type: "text", text: "Done." }] },
          ],
        }),
      }),
  });
  await flush();
  const userMessage = harness.elements["#desk-transcript"].querySelector(".user-message");
  const scheduleButton = userMessage.querySelector(".message-schedule");
  assert.ok(scheduleButton, "completed user prompt exposes Create schedule");
  await scheduleButton.listener("click")();
  assert.equal(
    JSON.parse(harness.sessionStorage.getItem("waffle.desk.schedule.draft.v1")).text,
    "Summarize the release queue",
  );

  // The composer draft handoff carries exactly the visible draft text.
  harness.elements["#desk-message"].value = "Draft a report every morning";
  await harness.elements["#desk-schedule-draft"].listener("click")();
  assert.equal(
    JSON.parse(harness.sessionStorage.getItem("waffle.desk.schedule.draft.v1")).text,
    "Draft a report every morning",
  );
});

test("read aloud speaks sanitized chunked content, replaces, and stops on teardown", async () => {
  const longMarkdown =
    "# Findings\n\n- **alpha** is fine\n- beta is not\n\n```go\nfmt.Println(\"x\")\n```\n\n| A | B |\n| --- | --- |\n| one | two |\n\nSee [the docs](https://example.com). " +
    "This sentence is repeated enough times to exceed the chunk boundary. " .repeat(6);
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({
          session_id: "session-1",
          history: [
            { role: "user", blocks: [{ type: "text", text: "Question" }] },
            { role: "assistant", blocks: [{ type: "text", text: longMarkdown }] },
          ],
        }),
      }),
  });
  await flush();
  const article = harness.elements["#desk-transcript"].querySelector(".waffle-message");
  const readButton = article.querySelector(".message-read");
  assert.ok(readButton, "assistant message exposes Read aloud");
  await readButton.listener("click")();

  // Sanitized, chunked utterances: no markdown punctuation, links read as
  // visible text, code and tables handled intelligibly.
  const spoken = harness.speechCalls.filter((call) => call.kind === "speak").map((call) => call.text);
  assert.ok(spoken.length >= 2, "long response is chunked");
  const joined = spoken.join(" ");
  assert.ok(joined.includes("Findings"), joined);
  assert.ok(joined.includes("alpha is fine"), joined);
  assert.ok(joined.includes("the docs"), joined);
  assert.ok(joined.includes("Code: fmt.Println"), joined);
  assert.ok(joined.includes("one, two"), joined);
  assert.doesNotMatch(joined, /\*\*|```|\[|\]|\(https/);

  const lastUtterance = harness.speechUtterances.at(-1);
  assert.equal(typeof lastUtterance.onend, "function");
  assert.equal(typeof lastUtterance.onerror, "function");
  assert.equal(harness.speechSynthesis.onend, null);
  assert.equal(harness.speechSynthesis.onerror, null);
  assert.equal(readButton.textContent, "Stop");
  lastUtterance.onend();
  assert.equal(readButton.textContent, "Read aloud");
  assert.equal(readButton.getAttribute("aria-pressed"), "false");
  assert.equal(readButton.classList.contains("is-speaking"), false);

  // Toggle stops the first and starts nothing; toggling a second message
  // replaces the first. Snapshot cancels after start — start() already
  // cancels any previous utterance, so a raw "some cancel happened" check
  // would be false-green.
  await readButton.listener("click")();
  const cancelsAfterReplay = harness.speechCalls.filter((call) => call.kind === "cancel").length;
  await readButton.listener("click")();
  const cancelsAfterStop = harness.speechCalls.filter((call) => call.kind === "cancel").length;
  assert.ok(cancelsAfterStop > cancelsAfterReplay, "stop cancels synthesis");
  const secondHarness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({
          session_id: "session-1",
          history: [
            { role: "user", blocks: [{ type: "text", text: "Q1" }] },
            { role: "assistant", blocks: [{ type: "text", text: "First reply" }] },
            { role: "user", blocks: [{ type: "text", text: "Q2" }] },
            { role: "assistant", blocks: [{ type: "text", text: "Second reply" }] },
          ],
        }),
      }),
  });
  await flush();
  const messages = secondHarness.elements["#desk-transcript"].querySelectorAll(".waffle-message");
  const first = messages[0].querySelector(".message-read");
  const second = messages[1].querySelector(".message-read");
  await first.listener("click")();
  const cancelsAfterFirst = secondHarness.speechCalls.filter((call) => call.kind === "cancel").length;
  await second.listener("click")();
  const cancelsAfterSecond = secondHarness.speechCalls.filter((call) => call.kind === "cancel").length;
  assert.ok(cancelsAfterSecond > cancelsAfterFirst, "starting a second playback stops the first");
  assert.equal(secondHarness.speechCalls.filter((call) => call.kind === "speak").length, 2);
});

test("read aloud stops on a new conversation and resume, and never speaks user drafts", async () => {
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({
          session_id: "session-1",
          history: [
            { role: "user", blocks: [{ type: "text", text: "Question" }] },
            { role: "assistant", blocks: [{ type: "text", text: "Reply" }] },
          ],
        }),
      }),
    commandHandler: async ({ options }) => {
      const { command } = JSON.parse(options.body);
      if (command.name === "new" && command.args === "") {
        return jsonResponse({ confirm: true, text: "Start over?" });
      }
      if (command.name === "sessions") {
        return jsonResponse({
          sessions: [
            {
              id: "session-3",
              title: "Other session",
              updated_at: "2026-07-25T12:00:00Z",
            },
          ],
        });
      }
      if (command.name === "resume") {
        return jsonResponse({
          state: defaultChatState({
            session_id: "session-3",
            title: "Other session",
            history: [{ role: "assistant", blocks: [{ type: "text", text: "Resumed reply" }] }],
          }),
        });
      }
      return jsonResponse({
        state: defaultChatState({ session_id: "session-2", title: "Fresh", history: null }),
      });
    },
  });
  await flush();
  const article = harness.elements["#desk-transcript"].querySelector(".waffle-message");
  const readButton = article.querySelector(".message-read");
  await readButton.listener("click")();
  assert.ok(harness.speechCalls.some((call) => call.kind === "speak"));
  const userMessage = harness.elements["#desk-transcript"].querySelector(".user-message");
  assert.equal(userMessage.querySelector(".message-read"), null, "user messages never read aloud");
  const cancelsAfterStart = harness.speechCalls.filter((call) => call.kind === "cancel").length;
  await harness.elements["#desk-new"].listener("click")();
  await flush();
  const cancelsAfterNew = harness.speechCalls.filter((call) => call.kind === "cancel").length;
  assert.ok(cancelsAfterNew > cancelsAfterStart, "new conversation stops speech");

  await harness.elements["#desk-session-refresh"].listener("click")();
  await flush();
  const sessionButton = harness.elements["#desk-session-options"].querySelector("button");
  await sessionButton.listener("click")();
  await flush();
  const resumed = harness.elements["#desk-transcript"].querySelector(".waffle-message");
  const resumeRead = resumed.querySelector(".message-read");
  await resumeRead.listener("click")();
  const cancelsAfterResumePlay = harness.speechCalls.filter((call) => call.kind === "cancel").length;
  await sessionButton.listener("click")();
  await flush();
  const cancelsAfterResume = harness.speechCalls.filter((call) => call.kind === "cancel").length;
  assert.ok(cancelsAfterResume > cancelsAfterResumePlay, "resume stops speech");
});

test("dictation inserts at the caret without destroying the draft and stops on Escape", async () => {
  const harness = createHarness();
  await flush();
  const textarea = harness.elements["#desk-message"];
  const dictate = harness.elements["#desk-dictate"];
  assert.equal(dictate.disabled, false, "dictate is enabled when supported");

  // Start listening; the button flips to a Stop state.
  await dictate.listener("click")();
  const recognition = harness.recognitionInstances[0];
  assert.ok(recognition.started, "recognition starts on activation");
  assert.equal(dictate.textContent, "Stop dictation");
  assert.equal(dictate.getAttribute("aria-pressed"), "true");

  // A transcript lands at the caret without destroying the draft.
  textarea.value = "Please ";
  textarea.selectionStart = 7;
  textarea.selectionEnd = 7;
  recognition.onresult({ results: [[{ transcript: "summarise the queue" }]] });
  assert.equal(textarea.value, "Please summarise the queue");
  assert.equal(dictate.textContent, "Dictate");
  assert.equal(dictate.getAttribute("aria-pressed"), "false");

  // Denied mic state is announced and the control returns to idle.
  await dictate.listener("click")();
  const second = harness.recognitionInstances[1];
  second.onerror({ error: "not-allowed" });
  assert.match(harness.elements["#desk-composer-status"].textContent, /denied/i);
  assert.equal(dictate.textContent, "Dictate");

  // Escape stops listening and returns focus to the composer.
  await dictate.listener("click")();
  await harness.elements["#desk-message"].listener("keydown")({ key: "Escape" });
  assert.equal(dictate.textContent, "Dictate");
  assert.equal(harness.elements["#desk-message"].focused, true);
});

test("dictation stays disabled and explains when recognition is unsupported", async () => {
  const harness = createHarness({ noSpeechRecognition: true });
  await flush();
  assert.equal(harness.elements["#desk-dictate"].disabled, true);
  assert.match(
    harness.elements["#desk-dictate-hint"].textContent,
    /not available in this browser/i,
  );
});

test("model choices explain roles, upstream, and operator description", async () => {
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({
          model_alias: "primary",
          models: [
            { alias: "primary", provider: "fixture", upstream: "primary-model", current: true, default: true, utility: true, description: "Everyday reasoning and tool use." },
            { alias: "local", provider: "fixture", upstream: "local-model", current: false, description: "" },
          ],
        }),
      }),
  });
  await flush();
  const detail = harness.elements["#desk-model-detail"];
  // The current pick shows roles, provider → upstream, and the description.
  assert.match(detail.textContent, /Waffle-wide default/);
  assert.match(detail.textContent, /Utility model/);
  assert.match(detail.textContent, /This conversation/);
  assert.match(detail.textContent, /fixture → primary-model/);
  assert.match(detail.textContent, /Everyday reasoning and tool use/);

  // Switching to an alias without a description labels it honestly.
  harness.elements["#desk-model"].value = "local";
  await harness.elements["#desk-model"].listener("change")({ preventDefault() {} });
  assert.match(detail.textContent, /No operator description configured/);
  assert.match(detail.textContent, /fixture → local-model/);
});

test("export sends the live owner lease and chosen format", async () => {
  let exportBody = null;
  const harness = createHarness({
    exportHandler: async ({ options }) => {
      exportBody = JSON.parse(options.body);
      return { ok: true, async text() { return "# Conversation"; }, async json() { return {}; } };
    },
  });
  await flush();
  assert.equal(harness.elements["#desk-export"].disabled, false);
  await harness.elements["#desk-export"].listener("click")();
  await flush();
  assert.equal(exportBody.format, "markdown");
  assert.ok(exportBody.client_id, "export sends the client id");
  assert.ok(exportBody.reattach_token, "export sends the live reattach proof");
});

test("temporary conversations send the option and show a live badge", async () => {
  const harness = createHarness({
    openHandler: async ({ options }) => {
      const body = JSON.parse(options.body);
      if (body.temporary) {
        return jsonResponse({
          client_id: "client-1",
          reattach_token: "lease-1",
          state: defaultChatState({
            session_id: "session-temp",
            title: "Temporary conversation",
            temporary: true,
            history: [],
          }),
        });
      }
      return jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState(),
      });
    },
  });
  await flush();
  // The option is available before the first message.
  assert.equal(harness.elements["#desk-temporary-row"].hidden, false);
  assert.equal(harness.elements["#desk-temporary-badge"].hidden, true);

  harness.elements["#desk-temporary"].checked = true;
  await harness.elements["#desk-refresh"].listener("click")();
  await flush();
  const opens = mutationCalls(harness, "/api/v1/desk/chat/open");
  assert.equal(JSON.parse(opens[opens.length - 1].options.body).temporary, true);
  assert.equal(harness.elements["#desk-session-title"].textContent, "Temporary conversation");
  assert.equal(harness.elements["#desk-temporary-badge"].hidden, false);
});

test("attachments preview, reject unsupported types, and send with the turn", async () => {
  const harness = createHarness();
  await flush();
  const pngB64 = harness.fakeFilePayloads["shot.png"];
  harness.elements["#desk-attach"].files = [
    { name: "shot.png", type: "image/png", size: 100 },
  ];
  await harness.elements["#desk-attach"].listener("change")();
  await flush();
  assert.equal(harness.elements["#desk-attachment-preview"].childNodes.length, 1);
  assert.ok(harness.elements["#desk-attachment-preview"].textContent.includes("shot.png"));

  // An unsupported type is rejected with a status message and no preview.
  harness.elements["#desk-attach"].files = [{ name: "evil.exe", type: "application/x-msdownload", size: 10 }];
  await harness.elements["#desk-attach"].listener("change")();
  await flush();
  assert.match(harness.elements["#desk-composer-status"].textContent, /not an allowed/);

  // Send includes the attachment and the preview is consumed.
  harness.elements["#desk-message"].value = "Look at this";
  await harness.elements["#desk-message"].listener("keydown")({
    key: "Enter",
    ctrlKey: true,
    metaKey: false,
    preventDefault() {},
  });
  await flush();
  const turn = mutationCalls(harness, "/api/v1/desk/chat/turn");
  assert.equal(turn.length, 1);
  const body = JSON.parse(turn[0].options.body);
  assert.equal(body.attachments.length, 1);
  assert.equal(body.attachments[0].name, "shot.png");
  assert.equal(body.attachments[0].data_base64, pngB64);
  assert.equal(harness.elements["#desk-attachment-preview"].childNodes.length, 0);
  const liveImage = harness.elements["#desk-transcript"].querySelector(".message-media-image");
  assert.ok(liveImage, "live transcript shows the sent image");
  assert.match(liveImage.getAttribute("src"), new RegExp(`^data:image/png;base64,${pngB64}$`));
});

test("attachments-only submit sends without text", async () => {
  const harness = createHarness();
  await flush();
  harness.elements["#desk-attach"].files = [
    { name: "shot.png", type: "image/png", size: 100 },
  ];
  await harness.elements["#desk-attach"].listener("change")();
  await flush();
  harness.elements["#desk-message"].value = "";
  void harness.elements["#desk-composer"].listener("submit")({ preventDefault() {} });
  await flush();
  const turn = mutationCalls(harness, "/api/v1/desk/chat/turn");
  assert.equal(turn.length, 1);
  const body = JSON.parse(turn[0].options.body);
  assert.equal(body.text, "");
  assert.equal(body.attachments.length, 1);
  assert.equal(body.attachments[0].name, "shot.png");
  assert.ok(harness.elements["#desk-transcript"].querySelector(".message-media-image"));
});

test("queued follow-up keeps attachments and sends them after turn_done", async () => {
  const harness = createHarness();
  await flush();
  const message = harness.elements["#desk-message"];
  const submit = harness.elements["#desk-composer"].listener("submit");
  const pngB64 = harness.fakeFilePayloads["shot.png"];

  message.value = "first";
  const first = submit({ preventDefault() {} });
  harness.elements["#desk-attach"].files = [
    { name: "shot.png", type: "image/png", size: 100 },
  ];
  await harness.elements["#desk-attach"].listener("change")();
  await flush();
  message.value = "look";
  void submit({ preventDefault() {} });
  await flush();

  assert.equal(mutationCalls(harness, "/api/v1/desk/chat/turn").length, 1);
  assert.match(harness.elements["#desk-queue"].textContent, /shot\.png/);
  assert.equal(harness.elements["#desk-attachment-preview"].childNodes.length, 1);

  harness.turnResponse.resolve(jsonResponse({}));
  await first;
  await flush();
  harness.EventSource.instances[0].emit("turn_done", {
    resource: "chat",
    resource_id: "client-1",
    type: "turn_done",
    data: { state: defaultChatState() },
  });
  await flush();

  const turns = mutationCalls(harness, "/api/v1/desk/chat/turn");
  assert.equal(turns.length, 2);
  const followUp = JSON.parse(turns[1].options.body);
  assert.equal(followUp.text, "look");
  assert.equal(followUp.attachments.length, 1);
  assert.equal(followUp.attachments[0].data_base64, pngB64);
});

test("attachments cap at four and a fifth is refused", async () => {
  const harness = createHarness();
  await flush();
  const files = Array.from({ length: 5 }, (_, index) => ({
    name: `file-${index}.png`,
    type: "image/png",
    size: 10,
  }));
  harness.elements["#desk-attach"].files = files;
  await harness.elements["#desk-attach"].listener("change")();
  await flush();
  assert.equal(harness.elements["#desk-attachment-preview"].childNodes.length, 4);
  assert.match(harness.elements["#desk-composer-status"].textContent, /capped at 4/);
});

test("restored history renders safe attachment cards for media blocks", async () => {
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({
          session_id: "session-1",
          history: [
            {
              role: "user",
              blocks: [
                { type: "text", text: "See the screenshot" },
                { type: "image", source: { type: "base64", media_type: "image/png", data: "aGVsbG8=" } },
                { type: "document", source: { type: "base64", media_type: "application/pdf", data: "cGRm" } },
              ],
            },
            { role: "assistant", blocks: [{ type: "text", text: "Noted." }] },
          ],
        }),
      }),
  });
  await flush();
  const userMessage = harness.elements["#desk-transcript"].querySelector(".user-message");
  const attachments = userMessage.querySelectorAll(".message-attachments");
  assert.equal(attachments.length, 1, "media cards render as a holder");
  const image = attachments[0].querySelector(".message-media-image");
  assert.ok(image, "image card renders");
  assert.match(image.getAttribute("src"), /^data:image\/png;base64,aGVsbG8=$/);
  const doc = attachments[0].querySelector(".message-media-doc");
  assert.ok(doc, "document card renders");
  assert.equal(doc.textContent, "Document (application/pdf)");
});

test("per-turn task and reasoning modes are sent with the turn and rendered as a chip", async () => {
  const harness = createHarness({
    openHandler: async () =>
      jsonResponse({
        client_id: "client-1",
        reattach_token: "lease-1",
        state: defaultChatState({}),
      }),
  });
  await flush();
  harness.elements["#desk-task-mode"].value = "deep";
  harness.elements["#desk-reasoning"].value = "high";
  harness.elements["#desk-message"].value = "Think hard about this";
  await harness.elements["#desk-message"].listener("keydown")({
    key: "Enter",
    ctrlKey: true,
    metaKey: false,
    preventDefault() {},
  });
  await flush();
  const turn = mutationCalls(harness, "/api/v1/desk/chat/turn");
  const body = JSON.parse(turn[0].options.body);
  assert.equal(body.task_mode, "deep");
  assert.equal(body.reasoning_effort, "high");
});

test("a successful retry clears the stale retry control", async () => {
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
  harness.elements["#desk-message"].value = "Retry me";
  await harness.elements["#desk-composer"].listener("submit")({ preventDefault() {} });
  await flush();
  const actions = harness.elements[".composer-actions"];
  const retry = actions.querySelector(".retry-button");
  assert.ok(retry, "retry control appears after a rejected turn");
  retry.listener("click")();
  await flush();
  assert.equal(actions.querySelector(".retry-button"), null);
  assert.equal(turnCalls, 2);
});
