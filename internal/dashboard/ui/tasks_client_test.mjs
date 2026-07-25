import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import { getFragment, startFixture, stopFixture } from "./desk_fixture_client.mjs";

const template = fs.readFileSync(new URL("./tasks.templ", import.meta.url), "utf8");
const bridge = fs.readFileSync(new URL("./assets/waffle-htmx.js", import.meta.url), "utf8");

test("Tasks keeps strict create/update forms and server-owned filter state", () => {
  assert.match(template, /id="task-schedule-form"[^>]+data-waffle-json-kind="task-schedule"/);
  assert.match(template, /id="task-filter-scheduled"[^>]+hx-get="\/api\/v1\/desk\/tasks\?filter=scheduled"/);
  assert.match(template, /id="task-schedule-submit"/);
  assert.match(bridge, /taskScheduleBody/);
  assert.match(bridge, /Save schedule/);
  assert.equal(fs.existsSync(new URL("./assets/tasks.js", import.meta.url)), false);
});

test("Tasks client receives an embedded HTML fragment from the real handler", async () => {
  const fixture = await startFixture();
  try {
    const fragment = await getFragment(fixture.url, "/api/v1/desk/tasks?filter=all");
    assert.equal(fragment.response.status, 200);
    assert.match(fragment.response.headers.get("content-type"), /text\/html/);
    assert.match(fragment.body, /data-waffle-fragment="true"/);
    assert.match(fragment.body, /id="tasks-list"/);
  } finally {
    await stopFixture(fixture.child);
  }
});
