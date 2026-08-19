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

test("skill-import disclosure is removed while imports are disabled", () => {
  assert.match(shim, /capability-skill-import-disclosure/);
  assert.match(shim, /disclosure\.hidden = !showDisclosure/);
  assert.match(shim, /aria-disabled/);
  assert.match(shim, /waffleSourceAvailable/);
});

test("editing a schedule restores profile after options reload", () => {
  assert.match(shim, /loadScheduleOptions\(form\)\.then/);
  assert.match(shim, /profileSelect\.value = profile/);
});

test("schedule dialog Escape and Cancel share an opener-aware dismissal contract", () => {
  assert.match(shim, /function dismissTaskScheduleDialog\(/);
  assert.match(shim, /addEventListener\(\s*["']cancel["']/);
  assert.match(shim, /event\.preventDefault\(\)/);
  assert.match(shim, /taskScheduleOpener/);
  assert.match(shim, /\.isConnected/);
  assert.match(shim, /queueMicrotask\(/);
  assert.match(shim, /taskScheduleAdvanced|task-schedule-advanced/);
  assert.match(shim, /advanced\.open\s*=\s*false/);
  assert.match(shim, /openTaskScheduleDialog\(scheduleOpen\)/);
  assert.match(shim, /openTaskScheduleDialog\(button\)/);
  assert.doesNotMatch(shim, /hx-on:|oncancel\s*=/);
});

test("guided time and cadence write cron and reveal day controls", () => {
  assert.match(shim, /if \(id === "task-schedule-time"\) updateScheduleGuide\(form\)/);
  assert.match(shim, /syncGuidedVisibility\(form\)/);
  assert.match(shim, /dowRow\.hidden = cadence !== "weekly"/);
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
