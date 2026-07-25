import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = await readFile(
  new URL("./assets/workspaces.js", import.meta.url),
  "utf8",
);

class FakeElement {
  constructor(tag = "div") {
    this.tagName = tag.toUpperCase();
    this.childNodes = [];
    this.dataset = {};
    this.listeners = {};
    this.attributes = {};
    this.hidden = false;
    this.disabled = false;
    this.open = false;
    this.value = "";
    this.className = "";
    this._textContent = "";
    this.focused = false;
  }

  get children() {
    return this.childNodes;
  }

  get textContent() {
    return this._textContent;
  }

  set textContent(value) {
    this._textContent = String(value);
    this.childNodes = [];
  }

  append(...nodes) {
    this.childNodes.push(...nodes);
  }

  replaceChildren(...nodes) {
    this.childNodes = [...nodes];
  }

  setAttribute(name, value) {
    this.attributes[name] = String(value);
  }

  getAttribute(name) {
    return this.attributes[name];
  }

  addEventListener(name, listener) {
    this.listeners[name] = listener;
  }

  async dispatch(name, extra = {}) {
    return this.listeners[name]?.({
      preventDefault() {},
      currentTarget: this,
      target: this,
      ...extra,
    });
  }

  showModal() {
    this.open = true;
  }

  close() {
    this.open = false;
  }

  focus() {
    this.focused = true;
  }
}

function response(body, status = 200) {
  return {
    ok: status >= 200 && status < 300,
    status,
    async json() {
      return body;
    },
  };
}

function findAction(node, action) {
  if (node?.dataset?.action === action) {
    return node;
  }
  for (const child of node?.childNodes || []) {
    const match = findAction(child, action);
    if (match) return match;
  }
  return null;
}

// createHarness drives workspaces.js against a stub fetch. Routed paths are
// matched first so the read-only git and connections polls do not have to be
// interleaved into every test's ordered response queue; anything unrouted
// falls back to fetchResponses in call order.
function createHarness(fetchResponses, routes = {}) {
  const routed = {
    "/git": () =>
      response({
        workspace: "ws-1",
        available: false,
        reason: "The workspace is idle. Resume it to read git status.",
      }),
    "/api/v1/desk/connections": () => response([]),
    ...routes,
  };
  const selectors = [
    ".workspaces",
    "#workspaces-list",
    "#workspaces-errors",
    "#workspaces-empty",
    "#workspace-open-button",
    "#workspace-open-dialog",
    "#workspace-open-form",
    "#workspace-repository",
    "#workspace-profile",
    "#workspace-open-cancel",
    "#workspace-open-status",
    "#workspace-open-prerequisite",
    "#workspace-close-dialog",
    "#workspace-close-title",
    "#workspace-close-dirty",
    "#workspace-close-unpushed",
    "#workspace-close-status",
    "#workspace-close-cancel",
    "#workspace-close-confirm",
  ];
  const elements = Object.fromEntries(
    selectors.map((selector) => [selector, new FakeElement()]),
  );
  elements[".workspaces"].dataset = {};
  const requests = [];
  const mutations = [];
  const navigations = [];
  let uuid = 0;
  const context = {
    console,
    crypto: { randomUUID: () => `uuid-${++uuid}` },
    document: {
      body: { dataset: { requestToken: "desk-token" } },
      createElement: (tag) => new FakeElement(tag),
      querySelector: (selector) => elements[selector] || null,
    },
    fetch: async (path, options = {}) => {
      requests.push({ path, options });
      if ((options.method || "GET") === "POST") {
        mutations.push({ path, options });
      }
      for (const [pattern, handler] of Object.entries(routed)) {
        if (!path.includes(pattern)) continue;
        const next = Array.isArray(handler) ? handler.shift() : handler(path);
        if (!next) throw new Error(`unexpected fetch ${path}`);
        return next;
      }
      const next = fetchResponses.shift();
      if (!next) throw new Error(`unexpected fetch ${path}`);
      return next;
    },
    window: {
      location: {
        assign: (target) => navigations.push(target),
      },
    },
    URL,
    URLSearchParams,
    setTimeout,
    clearTimeout,
  };
  vm.runInNewContext(source, context, { filename: "workspaces.js" });
  return { elements, requests, mutations, navigations };
}

async function settle() {
  await new Promise((resolve) => setTimeout(resolve, 0));
  await new Promise((resolve) => setTimeout(resolve, 0));
}

test("workspace client uses exact guarded endpoints and safe DOM APIs", () => {
  for (const required of [
    "/api/v1/desk/workspaces",
    "/close-preview",
    '"X-Waffle-Desk-Token"',
    '"Idempotency-Key"',
    "crypto.randomUUID()",
    "textContent",
    "createElement",
  ]) {
    assert.ok(source.includes(required), `missing ${required}`);
  }
  for (const forbidden of ["innerHTML", "insertAdjacentHTML", "force"]) {
    assert.equal(source.includes(forbidden), false, `contains ${forbidden}`);
  }
});

test("workspace client renders state actions and selects the exact Today session", async () => {
  const harness = createHarness([
    response({
      workspaces: [{
        id: "ws-1",
        repository: "matt-riley/waffle",
        session: "session-workspace",
        status: "open",
        profile: "reviewer",
        image: "bookworm",
        egress: "No network egress",
      }],
    }),
    response({
      workspace: {
        id: "ws-1",
        repository: "matt-riley/waffle",
        session: "session-workspace",
        status: "open",
        profile: "reviewer",
        image: "bookworm",
        egress: "No network egress",
      },
      today_url: "/desk/?section=today&session_id=session-workspace",
    }),
  ]);
  await settle();

  const card = harness.elements["#workspaces-list"].childNodes[0];
  assert.ok(card, "workspace card was not rendered");
  const select = findAction(card, "select");
  assert.ok(select, "open workspace did not render Select");
  assert.ok(findAction(card, "idle"), "open workspace did not render Idle");
  assert.ok(findAction(card, "close-preview"), "workspace did not render Close");
  await select.dispatch("click");
  await settle();

  assert.equal(harness.mutations[0].path, "/api/v1/desk/workspaces/ws-1/select");
  assert.equal(harness.mutations[0].options.method, "POST");
  assert.equal(harness.mutations[0].options.body, "{}");
  assert.deepEqual(harness.navigations, [
    "/desk/?section=today&session_id=session-workspace",
  ]);
});

test("dirty close preview shows evidence and cannot confirm", async () => {
  const harness = createHarness([
    response({
      workspaces: [{
        id: "ws-1",
        repository: "matt-riley/waffle",
        session: "session-workspace",
        status: "idle",
        image: "bookworm",
        egress: "No network egress",
      }],
    }),
    response({
      workspace: {
        id: "ws-1",
        repository: "matt-riley/waffle",
        session: "session-workspace",
        status: "idle",
        image: "bookworm",
        egress: "No network egress",
      },
      preview_token: "preview-token",
      expires_in_seconds: 60,
      eligible: false,
      dirty: "M main.go",
      unpushed: "abc123 local commit",
    }),
  ]);
  await settle();

  const card = harness.elements["#workspaces-list"].childNodes[0];
  assert.ok(findAction(card, "resume"), "idle workspace did not render Resume");
  const close = findAction(card, "close-preview");
  await close.dispatch("click");
  await settle();

  assert.equal(harness.mutations[0].path, "/api/v1/desk/workspaces/ws-1/close-preview");
  assert.equal(harness.mutations[0].options.body, "{}");
  assert.equal(harness.elements["#workspace-close-dialog"].open, true);
  assert.equal(harness.elements["#workspace-close-dirty"].textContent, "M main.go");
  assert.equal(harness.elements["#workspace-close-unpushed"].textContent, "abc123 local commit");
  assert.equal(harness.elements["#workspace-close-confirm"].disabled, true);
  await harness.elements["#workspace-close-cancel"].dispatch("click");
  assert.equal(close.focused, true, "Cancel did not restore focus to its opener");
});

test("open forwards owner/repo and profile with one fresh idempotency key", async () => {
  const harness = createHarness([
    response({ workspaces: [] }),
    response({
      workspace: {
        id: "ws-1",
        repository: "matt-riley/waffle",
        session: "session-workspace",
        status: "open",
        profile: "reviewer",
        image: "bookworm",
        egress: "No network egress",
      },
    }, 201),
    response({ workspaces: [] }),
  ]);
  await settle();

  await harness.elements["#workspace-open-button"].dispatch("click");
  harness.elements["#workspace-repository"].value = "matt-riley/waffle";
  harness.elements["#workspace-profile"].value = "reviewer";
  await harness.elements["#workspace-open-form"].dispatch("submit");
  await settle();

  const request = harness.mutations[0];
  assert.equal(request.path, "/api/v1/desk/workspaces/open");
  assert.deepEqual(JSON.parse(request.options.body), {
    repository: "matt-riley/waffle",
    profile: "reviewer",
  });
  assert.equal(request.options.headers["X-Waffle-Desk-Token"], "desk-token");
  assert.equal(request.options.headers["Idempotency-Key"], "uuid-1");
  assert.equal(harness.elements["#workspace-open-dialog"].open, false);
});

function textOf(node) {
  if (!node) return "";
  if (node.childNodes.length === 0) return node.textContent;
  return node.childNodes.map(textOf).join(" ");
}

function openWorkspace(status = "open") {
  return {
    id: "ws-1",
    repository: "matt-riley/waffle",
    session: "session-workspace",
    status,
    image: "bookworm",
    egress: "No network egress",
  };
}

test("workspace card reads git status with a GET that carries no token or key", async () => {
  const harness = createHarness(
    [response({ workspaces: [openWorkspace()] })],
    {
      "/git": () =>
        response({
          workspace: "ws-1",
          available: true,
          branch: "topic",
          dirty: true,
          dirty_files: 3,
          tracking: true,
          ahead: 2,
          behind: 5,
          commit: "1a2b3c4",
          subject: "feat: land it",
        }),
    },
  );
  await settle();

  const card = harness.elements["#workspaces-list"].childNodes[0];
  const rendered = textOf(card);
  for (const expected of [
    "topic",
    "3 uncommitted files",
    "2 ahead · 5 behind",
    "1a2b3c4 feat: land it",
  ]) {
    assert.ok(rendered.includes(expected), `card missing ${expected}: ${rendered}`);
  }

  const git = harness.requests.find((entry) => entry.path.endsWith("/git"));
  assert.ok(git, "git status was never requested");
  assert.equal(git.options.method, "GET");
  assert.equal(git.options.headers["X-Waffle-Desk-Token"], undefined);
  assert.equal(git.options.headers["Idempotency-Key"], undefined);
  assert.equal(harness.mutations.length, 0, "reading git status mutated something");
});

test("unavailable git status shows the reason instead of inventing state", async () => {
  const harness = createHarness(
    [response({ workspaces: [openWorkspace("idle")] })],
    {
      "/git": () =>
        response({
          workspace: "ws-1",
          available: false,
          reason: "The workspace is idle. Resume it to read git status.",
        }),
    },
  );
  await settle();

  const rendered = textOf(harness.elements["#workspaces-list"].childNodes[0]);
  assert.ok(
    rendered.includes("The workspace is idle. Resume it to read git status."),
    `card missing the unavailable reason: ${rendered}`,
  );
  assert.equal(rendered.includes("ahead"), false, "unavailable card invented tracking state");
});

test("a clean untracked branch reads as clean with no upstream", async () => {
  const harness = createHarness(
    [response({ workspaces: [openWorkspace()] })],
    {
      "/git": () =>
        response({
          workspace: "ws-1",
          available: true,
          branch: "solo",
          dirty: false,
          dirty_files: 0,
          tracking: false,
          commit: "deadbee",
          subject: "initial commit",
        }),
    },
  );
  await settle();

  const rendered = textOf(harness.elements["#workspaces-list"].childNodes[0]);
  assert.ok(rendered.includes("Clean"), `card missing Clean: ${rendered}`);
  assert.ok(
    rendered.includes("No upstream branch"),
    `card missing the untracked label: ${rendered}`,
  );
});

test("open dialog names the GitHub prerequisite before submit", async () => {
  const harness = createHarness(
    [response({ workspaces: [] })],
    {
      "/api/v1/desk/connections": () =>
        response([{
          name: "github",
          kind: "github",
          status: "unconfigured",
          guidance: "Configure [github.app] to give workspaces git access.",
        }]),
    },
  );
  await settle();

  const prerequisite = harness.elements["#workspace-open-prerequisite"];
  assert.equal(prerequisite.hidden, false, "prerequisite stayed hidden");
  assert.ok(
    prerequisite.textContent.includes("GitHub App credentials are not configured"),
    `prerequisite text = ${prerequisite.textContent}`,
  );
  assert.ok(
    prerequisite.textContent.includes("Configure [github.app]"),
    `prerequisite dropped its guidance: ${prerequisite.textContent}`,
  );
});

test("a healthy GitHub connection leaves the open dialog unqualified", async () => {
  const harness = createHarness(
    [response({ workspaces: [] })],
    {
      "/api/v1/desk/connections": () =>
        response([{ name: "github", kind: "github", status: "healthy" }]),
    },
  );
  await settle();

  const prerequisite = harness.elements["#workspace-open-prerequisite"];
  assert.equal(prerequisite.hidden, true);
  assert.equal(prerequisite.textContent, "");
});
