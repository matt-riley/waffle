import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";

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

