import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = await readFile(new URL("./assets/memory.js", import.meta.url), "utf8");

test("Memory client uses safe DOM APIs and fresh mutation keys", () => {
  assert.match(source, /textContent/);
  assert.match(source, /replaceChildren/);
  assert.match(source, /crypto\.randomUUID\(\)/);
  assert.match(source, /X-Waffle-Desk-Token/);
  assert.match(source, /Idempotency-Key/);
  assert.doesNotMatch(source, /innerHTML|localStorage|sessionStorage|console\./);
});

test("Memory client confirms forget with a cancel-first native dialog and canonical refresh", () => {
  assert.match(source, /showModal\(\)/);
  assert.match(source, /memory-forget-cancel/);
  assert.match(source, /memory-forget-confirm/);
  assert.match(source, /await loadMemory\(\)/);
  assert.doesNotMatch(source, /\bUndo\b|provider\/delete|backup\/delete/);
});

class FakeElement {
  constructor(tagName = "div") {
    this.tagName = tagName.toUpperCase();
    this.childNodes = [];
    this.listeners = new Map();
    this.dataset = {};
    this.className = "";
    this.hidden = false;
    this.disabled = false;
    this.value = "";
    this._textContent = "";
  }

  get textContent() {
    return this._textContent + this.childNodes.map((child) => child.textContent).join("");
  }

  set textContent(value) {
    this._textContent = String(value);
    this.childNodes = [];
  }

  append(...children) {
    this.childNodes.push(...children);
  }

  replaceChildren(...children) {
    this._textContent = "";
    this.childNodes = [...children];
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

  close() {
    this.open = false;
  }

  showModal() {
    this.open = true;
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

function createSearchHarness() {
  const selectors = [
    ".memory",
    "#memory-search-form",
    "#memory-query",
    "#memory-session-id",
    "#memory-results",
    "#memory-status",
    "#memory-attach-status",
    "#memory-forget-dialog",
    "#memory-forget-note",
    "#memory-forget-scope",
    "#memory-forget-exclusions",
    "#memory-forget-cancel",
    "#memory-forget-confirm",
  ];
  const elements = Object.fromEntries(selectors.map((selector) => [selector, new FakeElement()]));
  const document = {
    body: { dataset: { requestToken: "test-token" } },
    createElement: (tagName) => new FakeElement(tagName),
    querySelector: (selector) => elements[selector] || null,
  };
  const searchRequests = [];
  const fetch = (path, options) => {
    if (String(path).startsWith("/api/v1/desk/memory?")) {
      const response = deferred();
      searchRequests.push({ options, path, response });
      return response.promise;
    }
    return Promise.resolve(jsonResponse({}));
  };
  let nextTimer = 0;
  const timers = new Map();
  const setTimeout = (callback) => {
    nextTimer += 1;
    timers.set(nextTimer, callback);
    return nextTimer;
  };
  const clearTimeout = (timer) => {
    timers.delete(timer);
  };

  class FakeAbortController {
    constructor() {
      this.signal = { aborted: false };
    }

    abort() {
      this.signal.aborted = true;
    }
  }

  const context = vm.createContext({
    AbortController: FakeAbortController,
    clearTimeout,
    crypto: { randomUUID: () => "key-1" },
    document,
    fetch,
    setTimeout,
  });
  new vm.Script(source, { filename: "memory.js" }).runInContext(context);

  return {
    elements,
    runLatestTimer() {
      const ids = [...timers.keys()];
      const latest = Math.max(...ids);
      const callback = timers.get(latest);
      timers.delete(latest);
      callback();
    },
    searchRequests,
  };
}

async function flush() {
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
}

function startDebouncedSearch(harness, query) {
  harness.elements["#memory-query"].value = query;
  harness.elements["#memory-query"].listener("input")();
  harness.runLatestTimer();
}

test("superseded debounced search success cannot overwrite newer results", async () => {
  const harness = createSearchHarness();
  startDebouncedSearch(harness, "older");
  startDebouncedSearch(harness, "newer");
  assert.equal(harness.searchRequests.length, 2);

  harness.searchRequests[1].response.resolve(jsonResponse({
    hits: [{ source: "note", source_id: "new", excerpt: "newer result" }],
  }));
  await flush();
  assert.match(harness.elements["#memory-results"].textContent, /newer result/);

  harness.searchRequests[0].response.resolve(jsonResponse({
    hits: [{ source: "note", source_id: "old", excerpt: "older result" }],
  }));
  await flush();
  assert.match(harness.elements["#memory-results"].textContent, /newer result/);
  assert.doesNotMatch(harness.elements["#memory-results"].textContent, /older result/);
  assert.equal(harness.searchRequests[0].options.signal?.aborted, true);
});

test("superseded debounced search failure cannot clear newer results or status", async () => {
  const harness = createSearchHarness();
  startDebouncedSearch(harness, "older");
  startDebouncedSearch(harness, "newer");

  harness.searchRequests[1].response.resolve(jsonResponse({
    hits: [{ source: "summary", source_id: "new", excerpt: "newer result" }],
  }));
  await flush();
  harness.searchRequests[0].response.resolve(jsonResponse({ message: "older failed" }, false));
  await flush();

  assert.match(harness.elements["#memory-results"].textContent, /newer result/);
  assert.equal(harness.elements["#memory-status"].textContent, "1 attributed result.");
});
