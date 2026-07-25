import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";

const template = fs.readFileSync(new URL("./workspaces.templ", import.meta.url), "utf8");
const fragments = fs.readFileSync(new URL("../fragments.go", import.meta.url), "utf8");

test("Workspaces is fragment-driven and retains guarded mutation targets", () => {
  assert.match(template, /id="workspaces-list"[^>]+hx-get="\/api\/v1\/desk\/workspaces"/);
  assert.match(template, /id="workspace-open-dialog"/);
  assert.match(fragments, /workspacesFragment/);
  assert.match(fragments, /workspaceGitFragment/);
  assert.equal(fs.existsSync(new URL("./assets/workspaces.js", import.meta.url)), false);
});

