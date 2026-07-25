import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";

const shim = fs.readFileSync(new URL("./assets/waffle-htmx.js", import.meta.url), "utf8");

const templates = Object.fromEntries(
  await Promise.all(
    ["capabilities", "tasks", "workspaces", "memory", "today"].map(async (name) => [
      name,
      await fs.promises.readFile(new URL(`./${name}.templ`, import.meta.url), "utf8"),
    ]),
  ),
);

test("Waffle htmx bridge is external, CSP-safe, and disables eval features", () => {
  assert.match(shim, /htmx\.config\.allowEval\s*=\s*false/);
  assert.match(shim, /htmx\.config\.allowScriptTags\s*=\s*false/);
  assert.match(shim, /htmx:configRequest/);
  assert.match(shim, /htmx:beforeSend/);
  assert.match(shim, /X-Waffle-Desk-Token/);
  assert.match(shim, /Idempotency-Key/);
  assert.match(shim, /Content-Type.*application\/json/);
  assert.match(shim, /inFlight/);
  assert.doesNotMatch(shim, /hx-on:|js:/);
});

test("Waffle htmx bridge retains unchanged retry identity and rotates after success", () => {
  assert.match(shim, /intents\.get\(identity\)/);
  assert.match(shim, /intents\.delete\(identity\)/);
  assert.match(shim, /candidate\.key === intent\.key/);
  assert.match(shim, /JSON\.stringify\(body\)/);
  assert.match(shim, /catalogue-add-/);
  assert.match(shim, /textContent = "Enrolled"/);
});

test("the four migrated sections declare server fragments and Today stays bespoke", () => {
  for (const section of ["capabilities", "tasks", "workspaces", "memory"]) {
    assert.match(templates[section], /hx-(get|post)=/);
  }
  assert.doesNotMatch(templates.today, /hx-(get|post)=|hx-trigger=/);
  for (const asset of ["capabilities.js", "tasks.js", "workspaces.js", "memory.js"]) {
    assert.equal(fs.existsSync(new URL(`./assets/${asset}`, import.meta.url)), false);
  }
});
