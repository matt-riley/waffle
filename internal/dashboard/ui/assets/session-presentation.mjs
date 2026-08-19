const groupOrder = Object.freeze([
  ["pinned", "Pinned"],
  ["today", "Today"],
  ["yesterday", "Yesterday"],
  ["week", "Previous 7 days"],
  ["older", "Older"],
]);

function normalizedText(value) {
  return typeof value === "string" ? value.trim().toLowerCase() : "";
}

function sessionTitle(session) {
  for (const value of [session?.title, session?.label]) {
    if (typeof value === "string" && value.trim()) {
      return value.trim();
    }
  }
  return "Untitled conversation";
}

function boundedText(value, limit) {
  if (typeof value !== "string") {
    return "";
  }
  const text = value.trim();
  return text.length > limit ? `${text.slice(0, limit - 1)}…` : text;
}

function shortSessionID(value) {
  const id = boundedText(value, 64);
  return id.length > 8 ? id.slice(-8) : id;
}

function sessionAccessibleLabel(session) {
  const parts = [sessionTitle(session)];
  const summary = boundedText(session?.summary, 72);
  const model = boundedText(session?.model_alias ?? session?.model, 32);
  const id = shortSessionID(session?.id);
  if (summary && summary !== parts[0]) {
    parts.push(summary);
  }
  if (model) {
    parts.push(`model ${model}`);
  }
  if (id) {
    parts.push(`id ${id}`);
  }
  return parts.join(" · ");
}

function sessionText(session) {
  return [session?.title, session?.label, session?.summary, session?.id, session?.model_alias, session?.model]
    .map(normalizedText)
    .filter(Boolean)
    .join(" ");
}

function localMidnight(date) {
  return new Date(date.getFullYear(), date.getMonth(), date.getDate());
}

function parsedUpdatedAt(session) {
  const value = new Date(session?.updated_at);
  return Number.isNaN(value.getTime()) ? null : value;
}

function recencyBucket(session, now) {
  if (session?.pinned === true) {
    return "pinned";
  }
  const updated = parsedUpdatedAt(session);
  if (!updated) {
    return "older";
  }
  const today = localMidnight(now);
  const yesterday = new Date(today);
  yesterday.setDate(yesterday.getDate() - 1);
  const week = new Date(today);
  week.setDate(week.getDate() - 7);
  if (updated >= today) {
    return "today";
  }
  if (updated >= yesterday) {
    return "yesterday";
  }
  if (updated >= week) {
    return "week";
  }
  return "older";
}

function stableNewestFirst(left, right) {
  const leftTime = parsedUpdatedAt(left.item)?.getTime();
  const rightTime = parsedUpdatedAt(right.item)?.getTime();
  if (leftTime === undefined || Number.isNaN(leftTime)) {
    return rightTime === undefined || Number.isNaN(rightTime) ? left.index - right.index : 1;
  }
  if (rightTime === undefined || Number.isNaN(rightTime)) {
    return -1;
  }
  return rightTime - leftTime || left.index - right.index;
}

export function filterSessions(sessions, query = "") {
  const values = Array.isArray(sessions) ? sessions : [];
  const term = normalizedText(query);
  if (!term) {
    return values.slice();
  }
  return values.filter((session) => sessionText(session).includes(term));
}

export function presentSessions(sessions, { now = new Date(), query = "" } = {}) {
  const filtered = filterSessions(sessions, query);
  const groups = new Map(groupOrder.map(([key, label]) => [key, { key, label, items: [] }]));
  filtered.forEach((item, index) => {
    groups.get(recencyBucket(item, now)).items.push({ item, index });
  });
  return groupOrder
    .map(([key]) => {
      const group = groups.get(key);
      group.items.sort(stableNewestFirst);
      return { ...group, items: group.items.map(({ item }) => item) };
    })
    .filter((group) => group.items.length > 0);
}

export { normalizedText, sessionAccessibleLabel, sessionTitle };
