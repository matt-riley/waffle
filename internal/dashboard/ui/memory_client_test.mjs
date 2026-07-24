import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const source = await readFile(new URL("./assets/memory.js", import.meta.url), "utf8");

test("Memory client uses safe DOM APIs and fresh mutation keys", () => {
  assert.match(source, /textContent/);
  assert.match(source, /replaceChildren/);
  assert.match(source, /crypto\.randomUUID\(\)/);
  assert.match(source, /X-Waffle-Desk-Token/);
  assert.match(source, /Idempotency-Key/);
  assert.doesNotMatch(source, /innerHTML|localStorage|sessionStorage|console\./);
});

test("Memory client confirms forget with a cancel-first native dialog and canonical refresh", () => {
  assert.match(source, /showModal\(\)/);
  assert.match(source, /memory-forget-cancel/);
  assert.match(source, /memory-forget-confirm/);
  assert.match(source, /await loadMemory\(\)/);
  assert.doesNotMatch(source, /\bUndo\b|provider\/delete|backup\/delete/);
});
