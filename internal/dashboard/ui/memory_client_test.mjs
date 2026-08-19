import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import { getFragment, startFixture, stopFixture } from "./desk_fixture_client.mjs";

const template = fs.readFileSync(new URL("./memory.templ", import.meta.url), "utf8");
const fragments = fs.readFileSync(new URL("../fragments.go", import.meta.url), "utf8");
const memoryClient = fs.readFileSync(new URL("./assets/memory.js", import.meta.url), "utf8");

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
