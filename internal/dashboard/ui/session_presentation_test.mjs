import assert from "node:assert/strict";
import test from "node:test";

import {
  filterSessions,
  presentSessions,
  sessionAccessibleLabel,
  sessionTitle,
} from "./assets/session-presentation.mjs";

const now = new Date(2026, 7, 19, 12, 0, 0);

function session(id, updated_at, overrides = {}) {
  return {
    id,
    title: id,
    summary: `${id} summary`,
    model_alias: "primary",
    updated_at,
    ...overrides,
  };
}

test("presentSessions groups recency by owner-local midnight and keeps pinned rows unique", () => {
  const result = presentSessions([
    session("pinned", new Date(2026, 7, 1, 9), { pinned: true }),
    session("today-old", new Date(2026, 7, 19, 8)),
    session("today-new", new Date(2026, 7, 19, 11)),
    session("yesterday", new Date(2026, 7, 18, 23)),
    session("week", new Date(2026, 7, 13, 12)),
    session("older", new Date(2026, 7, 10, 12)),
  ], { now });

  assert.deepEqual(result.map((group) => group.key), [
    "pinned",
    "today",
    "yesterday",
    "week",
    "older",
  ]);
  assert.deepEqual(result.map((group) => group.items.map((item) => item.id)), [
    ["pinned"],
    ["today-new", "today-old"],
    ["yesterday"],
    ["week"],
    ["older"],
  ]);
  assert.equal(result.flatMap((group) => group.items).filter((item) => item.id === "pinned").length, 1);
});

test("presentSessions handles local day and seven-day boundaries without UTC drift", () => {
  const result = presentSessions([
    session("at-today-midnight", new Date(2026, 7, 19, 0, 0)),
    session("at-yesterday-midnight", new Date(2026, 7, 18, 0, 0)),
    session("at-week-boundary", new Date(2026, 7, 12, 0, 0)),
    session("before-week", new Date(2026, 7, 11, 23, 59)),
  ], { now });

  assert.deepEqual(
    Object.fromEntries(result.map((group) => [group.key, group.items.map((item) => item.id)])),
    {
      today: ["at-today-midnight"],
      yesterday: ["at-yesterday-midnight"],
      week: ["at-week-boundary"],
      older: ["before-week"],
    },
  );
});

test("presentSessions keeps invalid timestamps stable and after valid rows", () => {
  const result = presentSessions([
    session("invalid-first", "not-a-date"),
    session("valid", new Date(2026, 7, 19, 10)),
    session("missing"),
    session("invalid-second", "also-not-a-date"),
  ], { now });

  assert.deepEqual(result.map((group) => [group.key, group.items.map((item) => item.id)]), [
    ["today", ["valid"]],
    ["older", ["invalid-first", "missing", "invalid-second"]],
  ]);
});

test("filterSessions matches normalized title, summary, id, and model fields", () => {
  const sessions = [
    session("session-title", new Date(2026, 7, 19), {
      title: "Release Review",
      summary: "Queue notes",
      model_alias: "flash-0731",
    }),
    session("other", new Date(2026, 7, 19), {
      title: "Untitled",
      summary: "Workspace policy",
      model_alias: "primary",
    }),
  ];

  for (const query of [" release ", "QUEUE", "SESSION-TITLE", "flash-0731", "workspace policy"]) {
    assert.equal(filterSessions(sessions, query).length, 1, query);
  }
  assert.equal(filterSessions(sessions, "no match").length, 0);
});

test("sessionTitle chooses a trimmed title, label, or stable fallback", () => {
  assert.equal(sessionTitle({ title: "  Launch notes  " }), "Launch notes");
  assert.equal(sessionTitle({ title: "  ", label: "  Label fallback  " }), "Label fallback");
  assert.equal(sessionTitle({ title: "", label: "   " }), "Untitled conversation");
});

test("sessionAccessibleLabel adds bounded disambiguation details", () => {
  const summary = "A".repeat(100);
  const id = "session-1234567890abcdef";
  const label = sessionAccessibleLabel({
    title: "Release review",
    summary,
    model_alias: "primary-large-model",
    id,
  });

  assert.match(label, /^Release review · A{71}… · model primary-large-model · id 90abcdef$/);
  assert.ok(!label.includes(summary));
  assert.ok(!label.includes(id));
});

test("presentSessions preserves equal-timestamp input order", () => {
  const result = presentSessions([
    session("first", new Date(2026, 7, 19, 10)),
    session("second", new Date(2026, 7, 19, 10)),
  ], { now });

  assert.deepEqual(result[0].items.map((item) => item.id), ["first", "second"]);
});
