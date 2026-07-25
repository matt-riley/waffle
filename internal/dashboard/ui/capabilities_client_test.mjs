import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import { getFragment, startFixture, stopFixture } from "./desk_fixture_client.mjs";

const template = fs.readFileSync(new URL("./capabilities.templ", import.meta.url), "utf8");
const fragments = fs.readFileSync(new URL("../fragments.go", import.meta.url), "utf8");
const bridge = fs.readFileSync(new URL("./assets/waffle-htmx.js", import.meta.url), "utf8");

test("Capabilities uses real fragment forms and safe catalogue/probe actions", () => {
  assert.match(template, /id="capability-models"[^>]+hx-get=/);
  assert.match(template, /id="capability-catalogue-form"[^>]+hx-post=/);
  assert.match(template, /id="capability-provider-test"[^>]+hx-post="\/api\/v1\/desk\/providers\/test"/);
  assert.match(fragments, /catalogue-add-/);
  assert.match(bridge, /catalogue-add-/);
  assert.doesNotMatch(template, /capabilities\.js/);
  assert.equal(fs.existsSync(new URL("./assets/capabilities.js", import.meta.url)), false);
});

test("Capabilities client receives an embedded HTML fragment from the real handler", async () => {
  const fixture = await startFixture();
  try {
    const fragment = await getFragment(fixture.url, "/api/v1/desk/capabilities?part=models");
    assert.equal(fragment.response.status, 200);
    assert.match(fragment.response.headers.get("content-type"), /text\/html/);
    assert.match(fragment.body, /data-waffle-fragment="true"/);
    assert.match(fragment.body, /hx-post="\/api\/v1\/desk\/models\/default"/);
  } finally {
    await stopFixture(fixture.child);
  }
});
