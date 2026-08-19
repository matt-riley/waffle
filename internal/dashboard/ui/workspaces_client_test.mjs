import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import { getFragment, startFixture, stopFixture } from "./desk_fixture_client.mjs";

const template = fs.readFileSync(new URL("./workspaces.templ", import.meta.url), "utf8");
const fragments = fs.readFileSync(new URL("../fragments.go", import.meta.url), "utf8");
const fragmentTemplate = fs.readFileSync(new URL("./fragments.templ", import.meta.url), "utf8");

test("Workspaces is fragment-driven and retains guarded mutation targets", () => {
  assert.match(template, /id="workspaces-list"[^>]+hx-get="\/api\/v1\/desk\/workspaces"/);
  assert.match(template, /id="workspaces-list"[^>]+aria-busy="true"/);
  assert.match(template, /id="workspaces-list"[\s\S]*Loading workspaces…/);
  assert.doesNotMatch(template, /id="workspaces-empty"/);
  assert.equal((template.match(/id="workspace-open-button"/g) || []).length, 1);
  assert.match(template, /id="workspace-open-dialog"/);
  assert.match(fragments, /workspacesFragment/);
  assert.match(fragments, /workspaceGitFragment/);
  assert.equal(fs.existsSync(new URL("./assets/workspaces.js", import.meta.url)), false);
});

test("Workspaces declares a bounded action hierarchy and native More actions disclosure", () => {
  assert.match(fragments, /PrimaryActions/);
  assert.match(fragments, /MoreActions/);
  assert.match(fragmentTemplate, /workspace-more-actions/);
  assert.match(fragmentTemplate, /More actions/);
});

test("Workspaces client receives an embedded HTML fragment from the real handler", async () => {
  const fixture = await startFixture();
  try {
    const fragment = await getFragment(fixture.url, "/api/v1/desk/workspaces");
    assert.equal(fragment.response.status, 200);
    assert.match(fragment.response.headers.get("content-type"), /text\/html/);
    assert.match(fragment.body, /data-waffle-fragment="true"/);
    assert.match(fragment.body, /id="workspaces-list"/);
  } finally {
    await stopFixture(fixture.child);
  }
});
