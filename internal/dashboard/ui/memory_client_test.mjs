import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import { getFragment, startFixture, stopFixture } from "./desk_fixture_client.mjs";
import { sessionAccessibleLabels, sessionPresentationURL } from "./assets/memory.js";

const template = fs.readFileSync(new URL("./memory.templ", import.meta.url), "utf8");
const fragments = fs.readFileSync(new URL("../fragments.go", import.meta.url), "utf8");
const memoryClient = fs.readFileSync(new URL("./assets/memory.js", import.meta.url), "utf8");
const appClient = fs.readFileSync(new URL("./assets/app.js", import.meta.url), "utf8");

test("Memory cache-busts its shared session presentation module with the shell asset version", () => {
  const memoryURL = new URL("https://desk.example/desk/assets/memory.js?v=HASH");
  assert.equal(
    sessionPresentationURL(memoryURL).href,
    "https://desk.example/desk/assets/session-presentation.mjs?v=HASH",
  );
  assert.equal(
    sessionPresentationURL("https://desk.example/desk/assets/memory.js").href,
    "https://desk.example/desk/assets/session-presentation.mjs",
  );
  assert.match(memoryClient, /const presentationURL = sessionPresentationURL\(import\.meta\.url\);/);
  assert.match(memoryClient, /await import\(presentationURL\.href\)/);
  assert.match(appClient, /presentationURL\.searchParams\.set\("v", version\);/);
});

test("Memory picker disambiguates identical accessible labels without exposing full IDs", () => {
  const sessions = [
    {
      id: "session-alpha-12345678",
      title: "Release review",
      summary: "Same bounded summary",
      model_alias: "primary",
    },
    {
      id: "session-beta-12345678",
      title: "Release review",
      summary: "Same bounded summary",
      model_alias: "primary",
    },
  ];

  const labels = sessionAccessibleLabels(sessions);
  assert.equal(new Set(labels).size, sessions.length);
  assert.match(labels[0], / · conversation 1 of 2$/);
  assert.match(labels[1], / · conversation 2 of 2$/);
  for (const session of sessions) {
    assert.ok(labels.every((label) => !label.includes(session.id)));
  }
});

test("Memory search and attachment use server fragments with bounded targets", () => {
  assert.match(template, /id="memory-search-form"[^>]+hx-get="\/api\/v1\/desk\/memory"/);
  assert.match(template, /id="memory-results"/);
  assert.match(template, /memory\.js/);
  assert.match(fragments, /MemorySearchResponse/);
  assert.match(fragments, /MemoryAttachResponse/);
  assert.match(memoryClient, /initMemorySessionPicker/);
});

test("Memory client receives an embedded HTML fragment from the real handler", async () => {
  const fixture = await startFixture();
  try {
    const fragment = await getFragment(fixture.url, "/api/v1/desk/memory?query=release");
    assert.equal(fragment.response.status, 200);
    assert.match(fragment.response.headers.get("content-type"), /text\/html/);
    assert.match(fragment.body, /data-waffle-fragment="true"/);
    assert.match(fragment.body, /id="memory-results"/);
  } finally {
    await stopFixture(fixture.child);
  }
});
