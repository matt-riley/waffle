const phase = Object.freeze({
  opening: "opening",
  idle: "idle",
  sending: "sending",
  streaming: "streaming",
  cancelling: "cancelling",
  disconnected: "disconnected",
});

const reconnectConfig = Object.freeze({
  maxAttempts: 8,
  // Keep the first retry snappy so a brief blip (or a test unblocking the
  // stream) recovers without waiting out a multi-second backoff ladder.
  baseDelayMs: 250,
  maxDelayMs: 2000,
});

function requestedSessionID() {
  const href = globalThis.location?.href || "http://127.0.0.1/desk/";
  const values = new URL(href).searchParams.getAll("session_id");
  if (values.length !== 1) {
    return "";
  }
  return values[0].trim();
}

const initialSessionID = requestedSessionID();
const ownerStorageKey = "waffle.desk.today.owner.v1";

function readStoredOwner() {
  try {
    const raw = globalThis.sessionStorage?.getItem(ownerStorageKey);
    if (!raw) {
      return null;
    }
    const owner = JSON.parse(raw);
    if (
      typeof owner?.client_id !== "string" ||
      owner.client_id === "" ||
      typeof owner?.reattach_token !== "string" ||
      owner.reattach_token === "" ||
      typeof owner?.session_id !== "string"
    ) {
      globalThis.sessionStorage?.removeItem(ownerStorageKey);
      return null;
    }
    return owner;
  } catch {
    return null;
  }
}

function forgetStoredOwner() {
  try {
    globalThis.sessionStorage?.removeItem(ownerStorageKey);
  } catch {
    // Storage is a recovery optimization; the server lease remains authoritative.
  }
}

// Drafts and the single follow-up queue are browser-local and keyed by session
// ID, so refresh and session switches in the same tab keep work in progress.
// Storage failures degrade to page-lifetime behaviour without breaking send.
const draftsStorageKey = "waffle.desk.today.drafts.v1";
const queueStorageKey = "waffle.desk.today.queue.v1";

function readDraftMap() {
  try {
    const raw = globalThis.sessionStorage?.getItem(draftsStorageKey);
    if (!raw) {
      return {};
    }
    const map = JSON.parse(raw);
    return map && typeof map === "object" ? map : {};
  } catch {
    return {};
  }
}

function saveDraftMap(map) {
  try {
    const entries = Object.entries(map).filter(
      ([, text]) => typeof text === "string" && text.trim() !== "",
    );
    if (entries.length === 0) {
      globalThis.sessionStorage?.removeItem(draftsStorageKey);
      return;
    }
    globalThis.sessionStorage?.setItem(
      draftsStorageKey,
      JSON.stringify(Object.fromEntries(entries)),
    );
  } catch {
    // Storage denied: drafts are best-effort.
  }
}

function getDraft(sessionID) {
  if (!sessionID) {
    return "";
  }
  return readDraftMap()[sessionID] || "";
}

function setDraft(sessionID, text) {
  if (!sessionID) {
    return;
  }
  const map = readDraftMap();
  if (typeof text === "string" && text.trim() !== "") {
    map[sessionID] = text;
  } else {
    delete map[sessionID];
  }
  saveDraftMap(map);
}

function clearDraft(sessionID) {
  if (!sessionID) {
    return;
  }
  const map = readDraftMap();
  delete map[sessionID];
  saveDraftMap(map);
}

function readQueue() {
  try {
    const raw = globalThis.sessionStorage?.getItem(queueStorageKey);
    if (!raw) {
      return null;
    }
    const queue = JSON.parse(raw);
    if (
      !queue ||
      typeof queue.sessionID !== "string" ||
      typeof queue.text !== "string" ||
      queue.text.trim() === "" ||
      typeof queue.idempotencyKey !== "string"
    ) {
      globalThis.sessionStorage?.removeItem(queueStorageKey);
      return null;
    }
    return {
      sessionID: queue.sessionID,
      text: queue.text,
      idempotencyKey: queue.idempotencyKey,
      held: queue.held === true,
    };
  } catch {
    return null;
  }
}

function persistQueue(queue) {
  state.queue = queue;
  try {
    if (queue) {
      globalThis.sessionStorage?.setItem(queueStorageKey, JSON.stringify(queue));
    } else {
      globalThis.sessionStorage?.removeItem(queueStorageKey);
    }
  } catch {
    // The in-memory queue still governs this page.
  }
  renderQueueBanner();
  updateControls();
}

function holdQueue(reason) {
  if (!state.queue) {
    return;
  }
  persistQueue({ ...state.queue, held: true });
  setStatusMessage(
    elements.composerStatus,
    `Follow-up held for review: ${reason}`,
    true,
    "composer",
  );
}

const elements = {
  shell: document.querySelector(".desk-shell"),
  title: document.querySelector("#desk-session-title"),
  connection: document.querySelector("#desk-connection"),
  connectionText: document.querySelector("#desk-connection-text"),
  connectionDetail: document.querySelector("#desk-connection-detail"),
  stale: document.querySelector("#desk-stale-status"),
  staleMessage: document.querySelector("#desk-stale-message"),
  refresh: document.querySelector("#desk-refresh"),
  phase: document.querySelector("#desk-phase"),
  transcript: document.querySelector("#desk-transcript"),
  emptyTranscript: document.querySelector("#desk-empty-transcript"),
  form: document.querySelector("#desk-composer"),
  composerActions: document.querySelector(".composer-actions"),
  slashMenu: document.querySelector("#desk-slash-menu"),
  message: document.querySelector("#desk-message"),
  send: document.querySelector("#desk-send"),
  cancel: document.querySelector("#desk-cancel"),
  composerStatus: document.querySelector("#desk-composer-status"),
  model: document.querySelector("#desk-model"),
  modelStatus: document.querySelector("#desk-model-status"),
  skill: document.querySelector("#desk-skill"),
  skillToggle: document.querySelector("#desk-skill-toggle"),
  skillStatus: document.querySelector("#desk-skill-status"),
  profile: document.querySelector("#desk-profile"),
  postureOpen: document.querySelector("#desk-posture-open"),
  workspace: document.querySelector("#desk-workspace"),
  provider: document.querySelector("#desk-provider"),
  sandbox: document.querySelector("#desk-sandbox"),
  modelErrorRow: document.querySelector("#desk-model-error-row"),
  modelError: document.querySelector("#desk-model-error"),
  newConversation: document.querySelector("#desk-new"),
  sessionRefresh: document.querySelector("#desk-session-refresh"),
  sessions: document.querySelector("#desk-sessions"),
  sessionFilter: document.querySelector("#desk-session-filter"),
  sessionOptions: document.querySelector("#desk-session-options"),
  usageRefresh: document.querySelector("#desk-usage-refresh"),
  usage: document.querySelector("#desk-usage"),
  permissionsRefresh: document.querySelector("#desk-permissions-refresh"),
  permissions: document.querySelector("#desk-permissions"),
  worksetRefresh: document.querySelector("#desk-workset-refresh"),
  workset: document.querySelector("#desk-workset"),
  helpRefresh: document.querySelector("#desk-help-refresh"),
  help: document.querySelector("#desk-help"),
  queue: document.querySelector("#desk-queue"),
};

const storedOwner = readStoredOwner();
const state = {
  currentPhase: phase.opening,
  clientID: "",
  requestToken: document.body.dataset.requestToken || "",
  eventCursor: 0,
  eventSource: null,
  streamingMessage: null,
  activeTurn: null,
  activeOperation: null,
  turnSequence: 0,
  generation: 0,
  sessionID: "",
  skills: [],
  modelAlias: "",
  connectionLabel: "Connected",
  reconnecting: false,
  reconnectAttempts: 0,
  reconnectTimer: null,
  streamGeneration: 0,
  pendingTurn: null,
  reattachToken: "",
  storedOwner,
  streamingText: "",
  toolRows: new Map(),
  toolSequence: 0,
  turnToolContainer: null,
  typingMessage: null,
  lastUserText: "",
  draftSessionID: "",
  queue: (() => {
    const stored = readQueue();
    // A restored queue had an unknown turn outcome at refresh time, so it
    // waits for explicit review instead of auto-dispatching.
    return stored ? { ...stored, held: true } : null;
  })(),
  slash: {
    open: false,
    filter: "",
    index: 0,
    commands: [],
    items: [],
  },
  sessionsList: {
    open: false,
    items: [],
    filter: "",
  },
  sessionMenuOpen: null,
};

function pushRailConnection(railState) {
  const rail = globalThis.waffleDeskRail;
  if (!rail || typeof rail.setConnection !== "function") {
    return;
  }
  rail.setConnection(railState);
}

function pushRailModel(alias, scope) {
  const rail = globalThis.waffleDeskRail;
  if (!rail || typeof rail.setModel !== "function") {
    return;
  }
  rail.setModel(alias, scope);
}

function phaseLabel(next) {
  const labels = {
    [phase.opening]: "Opening",
    [phase.idle]: "Ready",
    [phase.sending]: "Sending",
    [phase.streaming]: "Waffle is working",
    [phase.cancelling]: "Cancelling",
    [phase.disconnected]: "Disconnected",
  };
  return labels[next] || next;
}

function setPhase(next) {
  state.currentPhase = next;
  if (elements.shell) {
    elements.shell.dataset.phase = next;
  }
  if (!state.reconnecting || next === phase.disconnected) {
    elements.phase.textContent = phaseLabel(next);
  }
  const disconnected = next === phase.disconnected;
  elements.stale.hidden = !disconnected;
  elements.connection.classList.toggle("is-disconnected", disconnected);
  if (disconnected) {
    state.reconnecting = false;
    elements.connection.classList.remove("is-reconnecting");
    elements.connectionText.textContent = "Disconnected";
    elements.connectionDetail.textContent = "Stale";
    pushRailConnection(
      globalThis.waffleDeskRail?.connectionStates?.disconnected || "disconnected",
    );
  } else if (next === phase.opening) {
    pushRailConnection(
      globalThis.waffleDeskRail?.connectionStates?.connecting || "connecting",
    );
  } else {
    // Do not mask a process-health degraded rail with "connected" just because
    // an active session phase is live. Bootstrap owns degraded health.
    const degraded =
      globalThis.waffleDeskRail?.connectionStates?.degraded || "degraded";
    const current = globalThis.waffleDeskRail?.getState?.()?.connection;
    if (current !== degraded) {
      pushRailConnection(
        globalThis.waffleDeskRail?.connectionStates?.connected || "connected",
      );
    }
  }
  updateControls();
}

function updateControls() {
  const idle = state.currentPhase === phase.idle && state.clientID !== "";
  if (!idle) {
    closeSlashMenu();
  }
  const cancellable =
    state.activeTurn !== null &&
    (state.currentPhase === phase.sending ||
      state.currentPhase === phase.streaming);
  // The composer stays usable while Waffle works so the operator can queue a
  // follow-up; it is only locked while disconnected.
  elements.message.disabled = state.currentPhase === phase.disconnected;
  const busy = !idle && state.currentPhase !== phase.disconnected;
  elements.send.disabled =
    !state.clientID || elements.message.value.trim() === "";
  if (busy) {
    elements.send.textContent = "Queue follow-up";
    elements.send.setAttribute("aria-label", "Queue follow-up");
  } else {
    elements.send.textContent = "Send message";
    elements.send.setAttribute("aria-label", "Send message");
  }
  elements.cancel.disabled = !cancellable;
  elements.model.disabled = !idle;
  elements.skill.disabled = !idle || state.skills.length === 0;
  elements.skillToggle.disabled = !idle || state.skills.length === 0;
  elements.refresh.disabled = state.currentPhase !== phase.disconnected;
  for (const control of [
    elements.newConversation,
    elements.sessionRefresh,
    elements.usageRefresh,
    elements.permissionsRefresh,
    elements.worksetRefresh,
    elements.helpRefresh,
  ]) {
    if (control) {
      control.disabled = !idle;
    }
  }
}

// syncComposerDraft restores the current session's browser-local draft after
// an open or session switch, without clobbering what the operator is typing.
function syncComposerDraft() {
  if (!state.sessionID || state.draftSessionID === state.sessionID) {
    return;
  }
  state.draftSessionID = state.sessionID;
  if (
    state.currentPhase === phase.disconnected ||
    state.activeTurn !== null
  ) {
    return;
  }
  const draft = getDraft(state.sessionID);
  if (draft !== "") {
    elements.message.value = draft;
  }
  updateControls();
}

function clearNode(node) {
  node.textContent = "";
}

// Status regions declare how they behave when cleared so callers do not rely
// on element identity inside setStatusMessage.
const statusRegionDefaults = Object.freeze({
  model: Object.freeze({
    defaultText: () => "Changes this conversation only.",
    hiddenWhenEmpty: false,
  }),
  skill: Object.freeze({
    defaultText: () =>
      selectedSkill()?.attached
        ? "Attached to this conversation."
        : "Changes this conversation only.",
    hiddenWhenEmpty: false,
  }),
  composer: Object.freeze({
    defaultText: () => "",
    hiddenWhenEmpty: true,
  }),
});

function setStatusMessage(node, message, isError, region = null) {
  if (!node) {
    return;
  }
  const defaults = region ? statusRegionDefaults[region] : null;
  if (!message) {
    node.hidden = Boolean(defaults?.hiddenWhenEmpty);
    node.textContent = defaults?.defaultText ? defaults.defaultText() : "";
    node.classList.toggle("is-error", false);
    return;
  }
  node.hidden = false;
  node.textContent = message;
  node.classList.toggle("is-error", Boolean(isError));
}

function clearControlErrors() {
  setStatusMessage(elements.modelStatus, "", false, "model");
  setStatusMessage(elements.skillStatus, "", false, "skill");
  setStatusMessage(elements.composerStatus, "", false, "composer");
}

function appendInlineMarkdown(node, text) {
  // Inline pass for inline code, strikethrough, bold, italic, and links.
  // Strong, emphasis, and link labels re-enter so `**run `go test`**` still
  // renders its inner code. Every node is built with createElement and
  // textContent so model output can never inject markup (pinned by the
  // client harness's forbiddenMarkupAssignments assertions).
  const inline =
    /(`[^`\n]+`|~~[^~\n]+~~|\*\*.+?\*\*|\*[^*\n]+\*|\[[^\]\n]+\]\([^)\s]+\))/g;
  let cursor = 0;
  for (const match of text.matchAll(inline)) {
    if (match.index > cursor) {
      node.appendChild(document.createTextNode(text.slice(cursor, match.index)));
    }
    const token = match[0];
    if (token.startsWith("`")) {
      const code = document.createElement("code");
      code.textContent = token.slice(1, -1);
      node.appendChild(code);
    } else if (token.startsWith("~~")) {
      const del = document.createElement("del");
      del.textContent = token.slice(2, -2);
      node.appendChild(del);
    } else if (token.startsWith("**")) {
      const strong = document.createElement("strong");
      appendInlineMarkdown(strong, token.slice(2, -2));
      node.appendChild(strong);
    } else if (token.startsWith("*")) {
      const em = document.createElement("em");
      appendInlineMarkdown(em, token.slice(1, -1));
      node.appendChild(em);
    } else if (token.startsWith("[")) {
      const close = token.lastIndexOf("](");
      const label = token.slice(1, close);
      const href = token.slice(close + 2, -1);
      if (isSafeLink(href)) {
        const anchor = document.createElement("a");
        anchor.setAttribute("href", href);
        anchor.setAttribute("target", "_blank");
        anchor.setAttribute("rel", "noopener noreferrer");
        appendInlineMarkdown(anchor, label);
        node.appendChild(anchor);
      } else {
        node.appendChild(document.createTextNode(token));
      }
    }
    cursor = match.index + token.length;
  }
  if (cursor < text.length) {
    node.appendChild(document.createTextNode(text.slice(cursor)));
  }
}

// isSafeLink reports whether a markdown link href may become an anchor href.
// Only http(s), mailto, and relative targets are allowed; anything else
// (notably javascript:) is rendered as plain text instead.
function isSafeLink(href) {
  const scheme = /^([a-zA-Z][a-zA-Z0-9+.-]*):/.exec(href);
  if (!scheme) {
    return true;
  }
  const name = scheme[1].toLowerCase();
  return name === "http" || name === "https" || name === "mailto";
}

async function copyCode(text, button) {
  try {
    if (globalThis.navigator?.clipboard?.writeText) {
      await globalThis.navigator.clipboard.writeText(text);
    } else {
      const textarea = document.createElement("textarea");
      textarea.value = text;
      textarea.setAttribute("readonly", "");
      document.body.appendChild(textarea);
      textarea.select();
      if (!document.execCommand?.("copy")) {
        throw new Error("copy_unavailable");
      }
      textarea.remove();
    }
    button.textContent = "Copied";
  } catch {
    button.textContent = "Copy unavailable";
  }
}

// splitTableRow splits a pipe-table row into trimmed cells, tolerating an
// optional leading/trailing pipe and escaped pipes (\|) inside cells.
function splitTableRow(line) {
  const cells = [];
  let current = "";
  for (let i = 0; i < line.length; i += 1) {
    const ch = line[i];
    if (ch === "\\" && line[i + 1] === "|") {
      current += "|";
      i += 1;
    } else if (ch === "|") {
      cells.push(current);
      current = "";
    } else {
      current += ch;
    }
  }
  cells.push(current);
  if (cells.length > 1 && cells[0].trim() === "") cells.shift();
  if (cells.length > 1 && cells[cells.length - 1].trim() === "") cells.pop();
  return cells.map((cell) => cell.trim());
}

// isTableDelimiter reports whether a line is a GFM delimiter row: every cell
// is made of only :, -, and spaces and contains at least one hyphen.
function isTableDelimiter(line) {
  const cells = splitTableRow(line);
  return cells.length > 0 && cells.every((cell) => /^:?-+:?$/.test(cell));
}

// tableRowsAt returns the row range of a pipe table starting at index, or
// null when the header/delimiter pair is not a complete table. The delimiter
// row must match the header row in cell count (GFM) so stray pipes and
// horizontal rules stay paragraphs.
function tableRowsAt(lines, index) {
  if (index + 1 >= lines.length) return null;
  const header = lines[index];
  const delimiter = lines[index + 1];
  if (!header.includes("|") || !isTableDelimiter(delimiter)) return null;
  if (splitTableRow(header).length !== splitTableRow(delimiter).length) return null;
  const rows = [header, delimiter];
  let cursor = index + 2;
  while (cursor < lines.length) {
    const line = lines[cursor];
    if (line.includes("|") && !isTableDelimiter(line)) {
      rows.push(line);
      cursor += 1;
    } else {
      break;
    }
  }
  return { start: index, end: cursor, rows };
}

// delimiterAlign maps a GFM alignment marker to a CSS text-align value.
function delimiterAlign(cell) {
  const left = cell.startsWith(":");
  const right = cell.endsWith(":");
  if (left && right) return "center";
  if (left) return "left";
  if (right) return "right";
  return null;
}

// renderTable builds a semantic table inside a labelled scroll container.
// Cells are rendered with the same inline pass as paragraphs, so model
// content can never inject markup.
function renderTable(node, table) {
  const wrap = document.createElement("div");
  wrap.className = "table-scroll";
  wrap.setAttribute("role", "group");
  wrap.setAttribute("aria-label", "Table");
  const tableEl = document.createElement("table");
  const thead = document.createElement("thead");
  const headRow = document.createElement("tr");
  const headerCells = splitTableRow(table.rows[0]);
  const delimiterCells = splitTableRow(table.rows[1]);
  headerCells.forEach((cell, column) => {
    const th = document.createElement("th");
    th.setAttribute("scope", "col");
    const align = delimiterAlign(delimiterCells[column]);
    if (align) th.style.textAlign = align;
    appendInlineMarkdown(th, cell);
    headRow.appendChild(th);
  });
  thead.appendChild(headRow);
  tableEl.appendChild(thead);
  const tbody = document.createElement("tbody");
  for (let row = 2; row < table.rows.length; row += 1) {
    const cells = splitTableRow(table.rows[row]);
    const tr = document.createElement("tr");
    cells.forEach((cell, column) => {
      const td = document.createElement("td");
      const align = delimiterAlign(delimiterCells[column]);
      if (align) td.style.textAlign = align;
      appendInlineMarkdown(td, cell);
      tr.appendChild(td);
    });
    tbody.appendChild(tr);
  }
  tableEl.appendChild(tbody);
  wrap.appendChild(tableEl);
  node.appendChild(wrap);
}

function renderMarkdown(node, text) {
  clearNode(node);
  const lines = String(text || "").replace(/\r\n?/g, "\n").split("\n");
  let index = 0;
  while (index < lines.length) {
    const line = lines[index];
    if (line.startsWith("```")) {
      const language = line.slice(3).trim();
      const codeLines = [];
      index += 1;
      while (index < lines.length && !lines[index].startsWith("```")) {
        codeLines.push(lines[index]);
        index += 1;
      }
      if (index < lines.length) {
        index += 1;
      }
      const block = document.createElement("div");
      block.className = "code-block";
      const copy = document.createElement("button");
      copy.className = "code-copy";
      copy.type = "button";
      copy.textContent = "Copy";
      const codeText = codeLines.join("\n");
      copy.addEventListener("click", () => copyCode(codeText, copy));
      const pre = document.createElement("pre");
      const code = document.createElement("code");
      if (language) {
        code.setAttribute("data-language", language);
      }
      code.textContent = codeText;
      pre.appendChild(code);
      block.append(copy, pre);
      node.appendChild(block);
      continue;
    }
    const heading = /^(#{1,6})\s+(.+)$/.exec(line);
    if (heading) {
      const element = document.createElement(`h${heading[1].length}`);
      appendInlineMarkdown(element, heading[2]);
      node.appendChild(element);
      index += 1;
      continue;
    }
    const listMatch = /^(\s*)([-*+]|\d+\.)\s+(.+)$/.exec(line);
    if (listMatch) {
      const ordered = /\d+\./.test(listMatch[2]);
      const list = document.createElement(ordered ? "ol" : "ul");
      while (index < lines.length) {
        const item = /^(\s*)([-*+]|\d+\.)\s+(.+)$/.exec(lines[index]);
        if (!item || /\d+\./.test(item[2]) !== ordered) {
          break;
        }
        const listItem = document.createElement("li");
        appendInlineMarkdown(listItem, item[3]);
        list.appendChild(listItem);
        index += 1;
      }
      node.appendChild(list);
      continue;
    }
    const table = tableRowsAt(lines, index);
    if (table) {
      renderTable(node, table);
      index = table.end;
      continue;
    }
    if (line.trim() === "") {
      index += 1;
      continue;
    }
    const paragraphLines = [line];
    index += 1;
    while (
      index < lines.length &&
      lines[index].trim() !== "" &&
      !lines[index].startsWith("```") &&
      !/^(#{1,6})\s+/.test(lines[index]) &&
      !/^(\s*)([-*+]|\d+\.)\s+/.test(lines[index])
    ) {
      paragraphLines.push(lines[index]);
      index += 1;
    }
    const paragraph = document.createElement("p");
    appendInlineMarkdown(paragraph, paragraphLines.join("\n"));
    node.appendChild(paragraph);
  }
}

function appendMessage(role, text, beforeNode = null, allowEmpty = false) {
  if (!text && !allowEmpty) {
    return null;
  }
  if (elements.emptyTranscript) {
    elements.emptyTranscript.remove();
    elements.emptyTranscript = null;
  }
  const article = document.createElement("article");
  article.className = `message ${role === "user" ? "user-message" : "waffle-message"}`;
  article.dataset.rawText = text;
  const label = document.createElement("p");
  label.className = "message-author";
  label.textContent = role === "user" ? "You" : "Waffle";
  const body = document.createElement("div");
  body.className = "message-body";
  if (role === "user") {
    body.appendChild(document.createTextNode(text));
  } else {
    renderMarkdown(body, text);
  }
  article.append(label, body);
  if (!allowEmpty) {
    attachCopyButton(article);
  }
  if (beforeNode) {
    elements.transcript.insertBefore(article, beforeNode);
  } else {
    elements.transcript.appendChild(article);
  }
  article.scrollIntoView({ block: "nearest" });
  return article;
}

function appendDelta(text) {
  if (!text) {
    return;
  }
  clearTypingIndicator();
  if (!state.streamingMessage) {
    state.streamingMessage = appendMessage("assistant", "", null, true);
    state.streamingText = "";
  }
  state.streamingText += text;
  const body = state.streamingMessage.querySelector(".message-body");
  renderMarkdown(body, state.streamingText);
  const caret = document.createElement("span");
  caret.className = "stream-caret";
  caret.setAttribute("aria-hidden", "true");
  body.appendChild(caret);
  state.streamingMessage.scrollIntoView({ block: "nearest" });
}

function appendToolActivity(kind, data) {
  const tool = data.tool_name || "Tool";
  const callID = data.tool_call_id || `unpaired-${tool}-${++state.toolSequence}`;
  let chip = state.toolRows.get(callID);
  if (!chip) {
    if (!state.turnToolContainer) {
      state.turnToolContainer = document.createElement("div");
      state.turnToolContainer.className = "tool-chips";
      state.turnToolContainer.setAttribute("role", "list");
      elements.transcript.appendChild(state.turnToolContainer);
      state.turnToolContainer.scrollIntoView({ block: "nearest" });
    }
    chip = document.createElement("div");
    chip.className = "tool-chip";
    chip.setAttribute("role", "listitem");
    const spinner = document.createElement("span");
    spinner.className = "tool-chip-spinner";
    spinner.setAttribute("aria-hidden", "true");
    const label = document.createElement("span");
    label.className = "tool-chip-label";
    const status = document.createElement("span");
    status.className = "tool-chip-status";
    chip.append(spinner, label, status);
    chip._label = label;
    chip._status = status;
    state.turnToolContainer.appendChild(chip);
    state.toolRows.set(callID, chip);
  }
  const label = chip._label;
  const status = chip._status;
  if (kind === "tool_started") {
    chip.classList.remove("is-error", "is-success");
    chip.classList.add("is-running");
    label.textContent = tool;
    status.textContent = "running…";
    chip.setAttribute("aria-label", `${tool} running`);
    return;
  }
  const duration = Math.max(0, Number(data.duration_ms) || 0);
  const outcome = data.is_error ? "failed" : "succeeded";
  chip.classList.remove("is-running");
  chip.classList.toggle("is-error", Boolean(data.is_error));
  chip.classList.toggle("is-success", !data.is_error);
  label.textContent = tool;
  status.textContent = data.is_error
    ? `failed · ${duration} ms`
    : `✓ ${duration} ms · ${Math.max(0, Number(data.byte_count) || 0)} B`;
  chip.setAttribute("aria-label", `${tool} ${outcome}`);
}

function messageText(message) {
  if (!message || !Array.isArray(message.blocks)) {
    return "";
  }
  return message.blocks
    .filter((block) => block && block.type === "text")
    .map((block) => block.text || "")
    .join("");
}

function renderHistory(history) {
  clearNode(elements.transcript);
  elements.emptyTranscript = null;
  state.streamingMessage = null;
  state.streamingText = "";
  state.typingMessage = null;
  state.turnToolContainer = null;
  state.toolRows = new Map();
  for (const message of history) {
    appendMessage(message.role === "user" ? "user" : "assistant", messageText(message));
  }
  if (!elements.transcript.hasChildNodes()) {
    const empty = document.createElement("p");
    empty.className = "empty-transcript";
    empty.textContent = "The desk is ready. What are we working on?";
    elements.transcript.appendChild(empty);
    elements.emptyTranscript = empty;
  }
}

function persistOwner() {
  if (!state.clientID || !state.reattachToken) {
    return;
  }
  const owner = {
    client_id: state.clientID,
    reattach_token: state.reattachToken,
    session_id: state.sessionID || "",
  };
  state.storedOwner = owner;
  try {
    globalThis.sessionStorage?.setItem(ownerStorageKey, JSON.stringify(owner));
  } catch {
    // A denied storage write only removes reload recovery, not server authority.
  }
}

function renderModels(models, currentAlias) {
  clearNode(elements.model);
  const available = Array.isArray(models) ? models : [];
  for (const model of available) {
    const option = document.createElement("option");
    option.value = model.alias || "";
    option.textContent = model.provider
      ? `${model.alias} · ${model.provider}`
      : model.alias || "Unnamed model";
    option.selected = Boolean(model.current) || model.alias === currentAlias;
    elements.model.appendChild(option);
  }
  if (!elements.model.hasChildNodes() && currentAlias) {
    const option = document.createElement("option");
    option.value = currentAlias;
    option.textContent = currentAlias;
    option.selected = true;
    elements.model.appendChild(option);
  }
}

function selectedSkill() {
  return state.skills.find((skill) => skill.name === elements.skill.value) || null;
}

function updateSkillControl() {
  const skill = selectedSkill();
  elements.skillToggle.textContent = skill?.attached ? "Detach skill" : "Attach skill";
  if (elements.skillStatus && !elements.skillStatus.classList.contains("is-error")) {
    elements.skillStatus.textContent = skill?.attached
      ? "Attached to this conversation."
      : "Changes this conversation only.";
  }
}

function renderSkills(skills) {
  const selected = elements.skill.value;
  clearNode(elements.skill);
  state.skills = Array.isArray(skills)
    ? skills.filter((skill) => skill && skill.name && !skill.missing)
    : [];
  for (const skill of state.skills) {
    const option = document.createElement("option");
    option.value = skill.name;
    option.textContent = skill.description
      ? `${skill.name} · ${skill.description}`
      : skill.name;
    option.selected = skill.name === selected;
    elements.skill.appendChild(option);
  }
  updateSkillControl();
}

function renderCanonicalState(chatState, includeHistory) {
  if (!chatState) {
    return;
  }
  state.sessionID = chatState.session_id || state.sessionID;
  syncComposerDraft();
  markSessionSelection();
  state.modelAlias = chatState.model_alias || state.modelAlias;
  state.connectionLabel = chatState.connection_mode || state.connectionLabel || "Connected";
  elements.title.textContent = chatState.title || "Untitled conversation";
  if (!state.reconnecting && state.currentPhase !== phase.disconnected) {
    elements.connectionText.textContent = state.connectionLabel;
    elements.connectionDetail.textContent = state.connectionLabel;
  }
  elements.profile.textContent = chatState.profile || "Default";
  // posture.js reads these off the trigger, so the read-only posture view
  // always describes the profile this session is actually running (#193).
  if (elements.postureOpen) {
    elements.postureOpen.dataset.postureProfile = chatState.profile || "";
    elements.postureOpen.dataset.postureSession = state.sessionID || "";
  }
  elements.workspace.textContent = chatState.workspace || "No workspace";
  elements.provider.textContent = chatState.provider_label || "Not reported";
  if (elements.sandbox) {
    elements.sandbox.textContent = chatState.sandbox_mode || "Not reported";
  }
  if (elements.modelErrorRow && elements.modelError) {
    const modelError = chatState.model_error || "";
    elements.modelErrorRow.hidden = modelError === "";
    elements.modelError.textContent = modelError;
    elements.modelError.classList.toggle("is-error", modelError !== "");
  }
  renderModels(chatState.models, chatState.model_alias);
  renderSkills(chatState.skills);
  if (chatState.model_alias) {
    pushRailModel(
      chatState.model_alias,
      globalThis.waffleDeskRail?.modelScopes?.session || "session",
    );
  }
  if (includeHistory && Array.isArray(chatState.history)) {
    renderHistory(chatState.history);
  }
  persistOwner();
}

function restoreModelSelection() {
  if (!state.modelAlias || !elements.model) {
    return;
  }
  for (const option of elements.model.childNodes) {
    option.selected = option.value === state.modelAlias;
  }
  if (elements.model.value !== state.modelAlias) {
    elements.model.value = state.modelAlias;
  }
}

async function readJSON(response) {
  let payload = {};
  try {
    payload = await response.json();
  } catch {
    payload = {};
  }
  if (!response.ok) {
    const error = new Error("request_failed");
    error.safeCode = typeof payload.code === "string" ? payload.code : "";
    error.safeMessage =
      typeof payload.message === "string"
        ? payload.message
        : "The Desk request could not be completed.";
    throw error;
  }
  return payload;
}

async function getBootstrap() {
  const response = await fetch("/api/v1/desk/bootstrap", {
    method: "GET",
    credentials: "same-origin",
    cache: "no-store",
    headers: { Accept: "application/json" },
  });
  return readJSON(response);
}

function validateBootstrap(bootstrap) {
  if (
    !bootstrap ||
    typeof bootstrap.request_token !== "string" ||
    bootstrap.request_token === "" ||
    !Number.isSafeInteger(bootstrap.event_cursor) ||
    bootstrap.event_cursor < 0
  ) {
    const error = new Error("invalid_bootstrap");
    error.safeMessage = "Waffle Desk could not verify the live session. Refresh to try again.";
    throw error;
  }
  return {
    requestToken: bootstrap.request_token,
    eventCursor: bootstrap.event_cursor,
  };
}

async function postMutation(path, body, options = {}) {
  const headers = {
    Accept: "application/json",
    "Content-Type": "application/json",
    "X-Waffle-Desk-Token": state.requestToken,
    "Idempotency-Key": options.idempotencyKey || crypto.randomUUID(),
  };
  let response;
  try {
    response = await fetch(path, {
      method: "POST",
      credentials: "same-origin",
      cache: "no-store",
      keepalive: Boolean(options.keepalive),
      headers,
      body: JSON.stringify(body),
    });
  } catch {
    const error = new Error("network_error");
    error.network = true;
    error.safeMessage = "The Desk request could not reach Waffle.";
    throw error;
  }
  return readJSON(response);
}

function clearReconnectTimer() {
  if (state.reconnectTimer !== null) {
    clearTimeout(state.reconnectTimer);
    state.reconnectTimer = null;
  }
}

function markStreamConnected() {
  if (!state.reconnecting && state.reconnectAttempts === 0) {
    return;
  }
  state.reconnecting = false;
  state.reconnectAttempts = 0;
  clearReconnectTimer();
  elements.connection.classList.remove("is-reconnecting");
  if (state.currentPhase !== phase.disconnected) {
    elements.connectionText.textContent = state.connectionLabel || "Connected";
    elements.connectionDetail.textContent = state.connectionLabel || "Connected";
    elements.phase.textContent = phaseLabel(state.currentPhase);
  }
}

function showReconnecting() {
  if (state.currentPhase === phase.disconnected) {
    return;
  }
  state.reconnecting = true;
  elements.connection.classList.add("is-reconnecting");
  elements.connection.classList.remove("is-disconnected");
  elements.connectionText.textContent = "Reconnecting";
  elements.connectionDetail.textContent = "Reconnecting";
  elements.phase.textContent = "Reconnecting";
}

function noteEventCursor(envelope) {
  if (
    envelope &&
    Number.isSafeInteger(envelope.cursor) &&
    envelope.cursor >= 0 &&
    envelope.cursor > state.eventCursor
  ) {
    state.eventCursor = envelope.cursor;
  }
}

function disconnect(message) {
  clearReconnectTimer();
  state.streamGeneration += 1;
  if (state.eventSource) {
    state.eventSource.close();
    state.eventSource = null;
  }
  holdQueue("the connection dropped");
  state.activeTurn = null;
  state.activeOperation = null;
  state.streamingMessage = null;
  state.reconnecting = false;
  state.reconnectAttempts = 0;
  elements.staleMessage.textContent =
    message || "The transcript is still here, but sending is paused.";
  setPhase(phase.disconnected);
}

function handleDeskEvent(event) {
  let envelope;
  try {
    envelope = JSON.parse(event.data);
  } catch {
    // Skip unparseable frames; do not tear down a recoverable stream.
    return;
  }
  noteEventCursor(envelope);
  markStreamConnected();
  if (
    envelope.resource !== "chat" ||
    envelope.resource_id !== state.clientID
  ) {
    return;
  }
  const data = envelope.data || {};
  switch (envelope.type) {
    case "state":
      renderCanonicalState(data.state, false);
      break;
    case "text_delta":
      if (state.currentPhase === phase.sending) {
        setPhase(phase.streaming);
      }
      appendDelta(data.text || "");
      break;
    case "tool_started":
    case "tool_finished":
      appendToolActivity(envelope.type, data);
      break;
    case "notice":
      if (data.text) {
        elements.phase.textContent = data.text;
      }
      break;
    case "turn_done": {
      renderCanonicalState(data.state, false);
      finalizeStreamingMessage();
      const turn = state.activeTurn;
      if (turn && turn.generation === state.generation) {
        turn.eventSettled = true;
        settleTurn(turn);
      }
      break;
    }
  }
}

function scheduleReconnect() {
  if (state.currentPhase === phase.disconnected) {
    return;
  }
  if (state.reconnectTimer !== null) {
    return;
  }
  if (state.reconnectAttempts >= reconnectConfig.maxAttempts) {
    disconnect(
      "The live connection closed and could not be restored. Refresh before sending again.",
    );
    return;
  }
  const attempt = state.reconnectAttempts;
  state.reconnectAttempts += 1;
  showReconnecting();
  // First retry is immediate so recovery is not gated on the base delay after
  // the stream becomes reachable again (e.g. after a brief network blip).
  const delay =
    attempt === 0
      ? 0
      : Math.min(
          reconnectConfig.baseDelayMs * 2 ** (attempt - 1),
          reconnectConfig.maxDelayMs,
        );
  state.reconnectTimer = setTimeout(() => {
    state.reconnectTimer = null;
    if (state.currentPhase === phase.disconnected) {
      return;
    }
    try {
      openEventStream();
    } catch {
      scheduleReconnect();
    }
  }, delay);
}

function openEventStream() {
  if (state.eventSource) {
    state.eventSource.close();
    state.eventSource = null;
  }
  if (!Number.isSafeInteger(state.eventCursor) || state.eventCursor < 0) {
    throw new Error("invalid_event_cursor");
  }
  const eventSource = new EventSource(
    `/api/v1/desk/events?after=${encodeURIComponent(String(state.eventCursor))}`,
  );
  const streamGeneration = ++state.streamGeneration;
  const handleCurrentEvent = (event) => {
    if (streamGeneration !== state.streamGeneration) {
      return;
    }
    handleDeskEvent(event);
  };
  state.eventSource = eventSource;
  eventSource.onopen = () => {
    if (streamGeneration !== state.streamGeneration) {
      return;
    }
    markStreamConnected();
  };
  for (const kind of [
    "state",
    "text_delta",
    "tool_started",
    "tool_finished",
    "notice",
    "turn_done",
  ]) {
    eventSource.addEventListener(kind, handleCurrentEvent);
  }
  eventSource.addEventListener("resync_required", () => {
    if (streamGeneration !== state.streamGeneration) {
      return;
    }
    eventSource.close();
    if (state.eventSource === eventSource) {
      state.eventSource = null;
    }
    disconnect("Live updates expired. Refresh to load canonical state.");
  });
  eventSource.addEventListener("error", () => {
    if (streamGeneration !== state.streamGeneration) {
      return;
    }
    eventSource.close();
    if (state.eventSource === eventSource) {
      state.eventSource = null;
    }
    scheduleReconnect();
  });
}

async function openDesk() {
  clearReconnectTimer();
  state.generation += 1;
  state.streamGeneration += 1;
  const generation = state.generation;
  state.activeTurn = null;
  state.activeOperation = null;
  state.streamingMessage = null;
  state.pendingTurn = null;
  state.reconnecting = false;
  state.reconnectAttempts = 0;
  if (state.eventSource) {
    state.eventSource.close();
    state.eventSource = null;
  }
  setPhase(phase.opening);
  clearControlErrors();
  try {
    const bootstrap = validateBootstrap(await getBootstrap());
    if (generation !== state.generation) {
      return;
    }
    state.requestToken = bootstrap.requestToken;
    state.eventCursor = bootstrap.eventCursor;
    const requested = state.sessionID || initialSessionID;
    let owner =
      state.clientID && state.reattachToken
        ? {
            client_id: state.clientID,
            reattach_token: state.reattachToken,
            session_id: state.sessionID || "",
          }
        : state.storedOwner || readStoredOwner();
    if (
      owner &&
      requested &&
      owner.session_id &&
      owner.session_id !== requested
    ) {
      try {
        await postMutation("/api/v1/desk/chat/close", {
          client_id: owner.client_id,
          reattach_token: owner.reattach_token,
        });
      } catch (error) {
        if (error.safeCode !== "chat_client_not_found") {
          throw error;
        }
      }
      if (generation !== state.generation) {
        return;
      }
      state.clientID = "";
      state.reattachToken = "";
      state.storedOwner = null;
      forgetStoredOwner();
      owner = null;
    }
    const openBody = {
      continue: requested === "",
      session_id: requested,
      profile: "",
      capabilities: [],
    };
    if (owner) {
      openBody.reattach_client_id = owner.client_id;
      openBody.reattach_token = owner.reattach_token;
    }
    let opened;
    try {
      opened = await postMutation("/api/v1/desk/chat/open", openBody);
    } catch (error) {
      if (!owner || error.safeCode !== "chat_client_not_found") {
        throw error;
      }
      state.clientID = "";
      state.reattachToken = "";
      state.storedOwner = null;
      forgetStoredOwner();
      opened = await postMutation("/api/v1/desk/chat/open", {
        continue: requested === "",
        session_id: requested,
        profile: "",
        capabilities: [],
      });
    }
    if (generation !== state.generation) {
      return;
    }
    state.clientID = opened.client_id || "";
    state.reattachToken = opened.reattach_token || "";
    if (!state.clientID || !state.reattachToken) {
      throw new Error("missing_client_lease");
    }
    renderCanonicalState(opened.state, true);
    openEventStream();
    setPhase(phase.idle);
    // Do not steal focus if the user already moved it (e.g. skip link → main)
    // while the async open was in flight. Autofocus only when focus is still
    // on the document default or the composer itself.
    const active = document.activeElement;
    if (
      !active ||
      active === document.body ||
      active === document.documentElement ||
      active === elements.message
    ) {
      elements.message.focus();
    }
  } catch (error) {
    if (generation !== state.generation) {
      return;
    }
    disconnect(error.safeMessage || "Waffle Desk could not open. Refresh to try again.");
  }
}

function settleTurn(turn) {
  if (
    state.activeTurn !== turn ||
    turn.generation !== state.generation ||
    state.currentPhase === phase.disconnected
  ) {
    return;
  }
  if (!turn.postSettled || !turn.eventSettled || !turn.cancelSettled) {
    if (
      turn.postSettled &&
      state.currentPhase !== phase.cancelling
    ) {
      setPhase(phase.streaming);
    }
    return;
  }
  state.streamingMessage = null;
  state.streamingText = "";
  state.activeTurn = null;
  state.activeOperation = null;
  state.pendingTurn = null;
  setPhase(phase.idle);
  maybeDispatchFollowUp(turn);
}

function turnIdempotencyKey(text, provided) {
  if (provided) {
    state.pendingTurn = { text, idempotencyKey: provided };
    return provided;
  }
  if (state.pendingTurn && state.pendingTurn.text === text) {
    return state.pendingTurn.idempotencyKey;
  }
  const idempotencyKey = crypto.randomUUID();
  state.pendingTurn = { text, idempotencyKey };
  return idempotencyKey;
}

async function submitTurn(event, explicitText, idempotencyKey) {
  event.preventDefault();
  const text = String(explicitText ?? elements.message.value).trim();
  if (!text || state.clientID === "") {
    return;
  }
  if (state.currentPhase !== phase.idle) {
    queueFollowUp(text);
    return;
  }
  await sendTurn(text, turnIdempotencyKey(text, idempotencyKey), {
    clearComposer: explicitText === undefined,
  });
}

// queueFollowUp stores the single visible follow-up for the current session
// while a turn is running. It is dispatched only after the running turn
// completes successfully in the same session.
function queueFollowUp(text) {
  if (!state.sessionID) {
    return;
  }
  if (state.queue && state.queue.sessionID !== state.sessionID) {
    setStatusMessage(
      elements.composerStatus,
      "A follow-up for another session is held. Switch back to review it.",
      true,
      "composer",
    );
    return;
  }
  const existing = state.queue;
  if (existing) {
    if (existing.text === text) {
      setStatusMessage(
        elements.composerStatus,
        "That message is already queued.",
        false,
        "composer",
      );
      return;
    }
    const replace = globalThis.confirm?.(
      "Replace the queued follow-up with this message?",
    );
    if (!replace) {
      elements.message.focus();
      return;
    }
  }
  persistQueue({
    sessionID: state.sessionID,
    text,
    idempotencyKey: crypto.randomUUID(),
    held: false,
  });
  setStatusMessage(
    elements.composerStatus,
    "Follow-up queued. It will send when Waffle finishes.",
    false,
    "composer",
  );
}

function renderQueueBanner() {
  if (!elements.queue) {
    return;
  }
  clearNode(elements.queue);
  const queue = state.queue;
  if (!queue || queue.sessionID !== state.sessionID) {
    elements.queue.hidden = true;
    return;
  }
  elements.queue.hidden = false;
  const label = document.createElement("p");
  label.className = "queue-label";
  label.textContent = queue.held
    ? "Follow-up held for review"
    : "Follow-up queued";
  const text = document.createElement("p");
  text.className = "queue-text";
  text.textContent = queue.text;
  const actions = document.createElement("div");
  actions.className = "queue-actions";
  const edit = document.createElement("button");
  edit.type = "button";
  edit.className = "queue-edit";
  edit.textContent = "Edit";
  edit.setAttribute("aria-label", "Edit queued follow-up");
  edit.addEventListener("click", () => {
    const current = state.queue;
    if (!current) {
      return;
    }
    elements.message.value = current.text;
    persistQueue(null);
    elements.message.focus();
    setStatusMessage(
      elements.composerStatus,
      "Queued text is back in the composer.",
      false,
      "composer",
    );
  });
  const remove = document.createElement("button");
  remove.type = "button";
  remove.className = "queue-remove";
  remove.textContent = "Remove";
  remove.setAttribute("aria-label", "Remove queued follow-up");
  remove.addEventListener("click", () => {
    persistQueue(null);
    setStatusMessage(elements.composerStatus, "Follow-up removed.", false, "composer");
  });
  actions.append(edit, remove);
  elements.queue.append(label, text, actions);
}

// maybeDispatchFollowUp fires the queued follow-up after the running turn
// settles normally. Cancelled, rejected, and disconnected turns hold the queue
// instead (see holdQueue), and a held or foreign-session queue never fires.
function maybeDispatchFollowUp(turn) {
  const queue = state.queue;
  if (
    turn.cancelled ||
    !queue ||
    queue.held ||
    queue.sessionID !== state.sessionID
  ) {
    return;
  }
  const followUp = queue;
  persistQueue(null);
  setStatusMessage(
    elements.composerStatus,
    "Follow-up sent.",
    false,
    "composer",
  );
  void sendTurn(followUp.text, followUp.idempotencyKey, {
    clearComposer: false,
  });
}

async function sendTurn(text, idempotencyKey, options) {
  const generation = state.generation;
  const turn = {
    id: ++state.turnSequence,
    generation,
    postSettled: false,
    eventSettled: false,
    cancelSettled: true,
    cancelled: false,
    text,
    idempotencyKey,
  };
  state.activeTurn = turn;
  state.lastUserText = text;
  setPhase(phase.sending);
  setStatusMessage(elements.composerStatus, "", false, "composer");
  appendMessage("user", text, state.streamingMessage);
  showTypingIndicator();
  if (options?.clearComposer) {
    elements.message.value = "";
  }
  updateControls();
  try {
    await postMutation("/api/v1/desk/chat/turn", {
      client_id: state.clientID,
      text,
    }, { idempotencyKey });
    if (state.activeTurn !== turn || generation !== state.generation) {
      return;
    }
    state.pendingTurn = null;
    clearDraft(state.sessionID);
    turn.postSettled = true;
    settleTurn(turn);
  } catch (error) {
    if (state.activeTurn !== turn || generation !== state.generation) {
      return;
    }
    state.activeTurn = null;
    state.activeOperation = null;
    clearTypingIndicator();
    if (error.network) {
      holdQueue("the send outcome is unknown");
      disconnect(
        error.safeMessage ||
          "The turn outcome is unknown. Refresh before sending another message.",
      );
      return;
    }
    // Clear HTTP rejection: message stays, same Idempotency-Key on identical retry.
    holdQueue("the send was rejected");
    setPhase(phase.idle);
    setStatusMessage(
      elements.composerStatus,
      error.safeMessage ||
        "The turn was rejected. Edit the message or send again to retry.",
      true,
      "composer",
    );
    const retry = document.createElement("button");
    retry.type = "button";
    retry.className = "retry-button";
    retry.textContent = "Retry";
    retry.addEventListener("click", () => {
      setStatusMessage(elements.composerStatus, "", false, "composer");
      elements.message.value = turn.text;
      elements.message.focus();
      updateControls();
      void submitTurn({ preventDefault() {} });
    });
    elements.composerActions?.appendChild(retry);
    updateControls();
  }
}

async function cancelTurn() {
  if (
    state.activeTurn === null ||
    state.currentPhase !== phase.sending &&
    state.currentPhase !== phase.streaming
  ) {
    return;
  }
  const turn = state.activeTurn;
  turn.cancelled = true;
  turn.cancelSettled = false;
  holdQueue("the running turn was cancelled");
  setPhase(phase.cancelling);
  setStatusMessage(elements.composerStatus, "", false, "composer");
  try {
    await postMutation("/api/v1/desk/chat/cancel", {
      client_id: state.clientID,
    });
    if (
      state.activeTurn !== turn ||
      turn.generation !== state.generation
    ) {
      return;
    }
    turn.cancelSettled = true;
    settleTurn(turn);
  } catch (error) {
    if (
      state.activeTurn !== turn ||
      turn.generation !== state.generation
    ) {
      return;
    }
    if (state.currentPhase === phase.disconnected) {
      return;
    }
    turn.cancelSettled = true;
    if (turn.postSettled) {
      setPhase(phase.streaming);
    } else {
      setPhase(phase.sending);
    }
    setStatusMessage(
      elements.composerStatus,
      error.safeMessage || "Cancel could not be confirmed. Try again.",
      true,
    );
  }
}

async function runCommandOperation(label, operation) {
  if (state.currentPhase !== phase.idle) {
    return null;
  }
  const generation = state.generation;
  state.activeOperation = "command";
  setPhase(phase.sending);
  elements.phase.textContent = label;
  setStatusMessage(elements.composerStatus, "", false, "composer");
  try {
    const result = await operation();
    if (generation !== state.generation) {
      return null;
    }
    state.activeOperation = null;
    if (state.currentPhase !== phase.disconnected) {
      setPhase(phase.idle);
    }
    return result;
  } catch (error) {
    if (generation !== state.generation || state.currentPhase === phase.disconnected) {
      return null;
    }
    state.activeOperation = null;
    setPhase(phase.idle);
    setStatusMessage(
      elements.composerStatus,
      error.safeMessage || "The command could not be completed.",
      true,
      "composer",
    );
    return null;
  }
}

function commandMutation(name, args = "") {
  return postMutation("/api/v1/desk/chat/command", {
    client_id: state.clientID,
    command: { name, args },
  });
}

async function newConversation() {
  await runCommandOperation("Starting conversation", async () => {
    const preview = await commandMutation("new");
    if (preview.confirm) {
      const confirmed = globalThis.confirm?.(
        preview.text || "Start a new conversation?",
      );
      if (!confirmed) {
        return preview;
      }
    }
    const result = preview.confirm
      ? await commandMutation("new", "confirm")
      : preview;
    if (result.state) {
      renderCanonicalState(result.state, true);
    }
    return result;
  });
}

function formatSessionUpdated(value) {
  const updated = new Date(value);
  if (Number.isNaN(updated.getTime())) {
    return "Update time unavailable";
  }
  return updated.toLocaleDateString("en-GB", {
    day: "2-digit",
    month: "short",
    year: "numeric",
    timeZone: "UTC",
  });
}

async function resumeSession(sessionID) {
  await runCommandOperation("Resuming conversation", async () => {
    const result = await commandMutation("resume", sessionID);
    if (result.state) {
      renderCanonicalState(result.state, true);
    }
    return result;
  });
}

// toggleSessions opens or closes the recent-conversation disclosure. The first
// open loads the list; subsequent opens reuse the cached items.
async function toggleSessions() {
  if (state.sessionsList.open) {
    closeSessionsList();
    return;
  }
  if (state.sessionsList.items.length === 0) {
    const result = await runCommandOperation("Loading conversations", () =>
      commandMutation("sessions"),
    );
    if (!result) {
      return;
    }
    state.sessionsList.items = Array.isArray(result.sessions)
      ? result.sessions
      : [];
  }
  openSessionsList();
}

function openSessionsList() {
  state.sessionsList.open = true;
  if (elements.sessionRefresh) {
    elements.sessionRefresh.setAttribute("aria-expanded", "true");
  }
  renderSessionList();
  if (elements.sessionFilter) {
    elements.sessionFilter.value = state.sessionsList.filter;
    elements.sessionFilter.focus();
  }
}

function closeSessionsList() {
  if (!state.sessionsList.open) {
    return;
  }
  state.sessionsList.open = false;
  if (elements.sessionRefresh) {
    elements.sessionRefresh.setAttribute("aria-expanded", "false");
    elements.sessionRefresh.focus();
  }
  if (elements.sessions) {
    elements.sessions.hidden = true;
  }
}

function renderSessions(sessions) {
  state.sessionsList.items = Array.isArray(sessions) ? sessions : [];
  if (state.sessionsList.open) {
    renderSessionList();
  }
}

// renderSessionList paints the bounded, filterable listbox. Every row is a
// resume choice plus a keyboard-accessible action menu (rename, pin, delete).
// The current conversation is programmatically and visually selected.
function renderSessionList() {
  if (!elements.sessionOptions) {
    return;
  }
  clearNode(elements.sessionOptions);
  if (elements.sessions) {
    elements.sessions.hidden = false;
  }
  const available = state.sessionsList.items;
  if (available.length === 0) {
    const empty = document.createElement("p");
    empty.textContent = "No recent conversations.";
    elements.sessionOptions.appendChild(empty);
    return;
  }
  const filter = state.sessionsList.filter.trim().toLowerCase();
  const matches = filter
    ? available.filter((session) => {
        const haystack = [
          session.title,
          session.summary,
          session.id,
          session.model_alias,
        ]
          .filter(Boolean)
          .join(" ")
          .toLowerCase();
        return haystack.includes(filter);
      })
    : available;
  if (matches.length === 0) {
    const empty = document.createElement("p");
    empty.textContent = "No conversations match.";
    elements.sessionOptions.appendChild(empty);
    return;
  }
  for (const session of matches) {
    const row = document.createElement("div");
    row.className = "session-row";
    const selected = session.id === state.sessionID;
    const button = document.createElement("button");
    button.type = "button";
    button.className = "session-choice";
    button.setAttribute("role", "option");
    button.setAttribute("data-session-id", session.id || "");
    button.setAttribute(
      "aria-selected",
      selected ? "true" : "false",
    );
    if (selected) {
      button.classList.add("is-selected");
      button.setAttribute("aria-current", "true");
    }
    const title = document.createElement("span");
    title.className = "session-title";
    const titleText =
      session.title || session.summary || "Untitled conversation";
    title.textContent = titleText;
    const meta = document.createElement("span");
    meta.className = "session-meta";
    const parts = [];
    if (session.pinned) {
      parts.push("Pinned");
    }
    parts.push(formatSessionUpdated(session.updated_at));
    if (session.model_alias) {
      parts.push(session.model_alias);
    }
    meta.textContent = parts.join(" · ");
    if (session.summary && session.summary !== titleText) {
      const detail = document.createElement("span");
      detail.className = "session-detail";
      detail.textContent = session.summary;
      button.append(title, detail, meta);
    } else {
      button.append(title, meta);
    }
    button.addEventListener("click", () => resumeSession(session.id || ""));
    row.appendChild(button);
    attachSessionActions(row, session);
    elements.sessionOptions.appendChild(row);
  }
}

// attachSessionActions adds the per-row action menu whose accessible name
// includes the conversation title (#470).
function attachSessionActions(row, session) {
  const trigger = document.createElement("button");
  trigger.type = "button";
  trigger.className = "session-menu-trigger";
  trigger.textContent = "⋯";
  trigger.setAttribute(
    "aria-label",
    `Actions for ${session.title || "Untitled conversation"}`,
  );
  trigger.setAttribute("aria-haspopup", "menu");
  trigger.setAttribute("aria-expanded", "false");
  const popover = document.createElement("div");
  popover.className = "session-menu-popover";
  popover.setAttribute("role", "menu");
  popover.hidden = true;
  popover.append(
    menuItem("Rename", () => beginSessionRename(row, session)),
    menuItem(session.pinned ? "Unpin" : "Pin", () =>
      toggleSessionPin(session),
    ),
    menuItem("Delete", () => deleteSession(session)),
  );
  trigger.addEventListener("click", () =>
    toggleSessionMenu(trigger, popover),
  );
  row.append(trigger, popover);
}

function menuItem(label, action) {
  const item = document.createElement("button");
  item.type = "button";
  item.className = "session-menu-item";
  item.setAttribute("role", "menuitem");
  item.textContent = label;
  item.addEventListener("click", () => {
    closeSessionMenus();
    void action();
  });
  return item;
}

function closeSessionMenus() {
  if (!state.sessionMenuOpen) {
    return;
  }
  state.sessionMenuOpen.popover.hidden = true;
  state.sessionMenuOpen.trigger.setAttribute("aria-expanded", "false");
  state.sessionMenuOpen = null;
}

function toggleSessionMenu(trigger, popover) {
  if (state.sessionMenuOpen && state.sessionMenuOpen.popover === popover) {
    closeSessionMenus();
    return;
  }
  closeSessionMenus();
  popover.hidden = false;
  trigger.setAttribute("aria-expanded", "true");
  state.sessionMenuOpen = { trigger, popover };
  popover.querySelector("button")?.focus();
}

async function refreshSessionList() {
  const result = await runCommandOperation("Updating conversations", () =>
    commandMutation("sessions"),
  );
  if (result) {
    state.sessionsList.items = Array.isArray(result.sessions)
      ? result.sessions
      : [];
    if (state.sessionsList.open) {
      renderSessionList();
    }
  }
}

async function toggleSessionPin(session) {
  const pin = !session.pinned;
  const result = await runCommandOperation(
    pin ? "Pinning conversation" : "Unpinning conversation",
    () => commandMutation(pin ? "pin" : "unpin", session.id),
  );
  if (!result) {
    return;
  }
  await refreshSessionList();
}

async function deleteSession(session) {
  const title = session.title || "Untitled conversation";
  const confirmed = globalThis.confirm?.(
    `Delete conversation "${title}"? This cannot be undone.`,
  );
  if (!confirmed) {
    return;
  }
  const result = await runCommandOperation("Deleting conversation", () =>
    commandMutation("delete", session.id),
  );
  if (!result) {
    return;
  }
  if (result.state) {
    renderCanonicalState(result.state, true);
  }
  // The deleted conversation's browser-local work is gone with it.
  clearDraft(session.id);
  if (state.queue && state.queue.sessionID === session.id) {
    persistQueue(null);
  }
  await refreshSessionList();
}

// beginSessionRename swaps the row for an inline bounded title form.
function beginSessionRename(row, session) {
  closeSessionMenus();
  const form = document.createElement("form");
  form.className = "session-rename";
  const input = document.createElement("input");
  input.type = "text";
  input.value = session.title || "";
  input.maxLength = 200;
  input.setAttribute("aria-label", "Conversation title");
  const save = document.createElement("button");
  save.type = "submit";
  save.textContent = "Save";
  const cancel = document.createElement("button");
  cancel.type = "button";
  cancel.textContent = "Cancel";
  form.append(input, save, cancel);
  for (const child of [...row.childNodes]) {
    child.remove();
  }
  row.appendChild(form);
  input.focus();
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const title = input.value.trim();
    if (!title) {
      input.focus();
      return;
    }
    const result = await runCommandOperation("Renaming conversation", () =>
      commandMutation("rename", `${session.id} ${title}`),
    );
    if (!result) {
      return;
    }
    if (session.id === state.sessionID) {
      elements.title.textContent = title;
    }
    await refreshSessionList();
  });
  cancel.addEventListener("click", () => {
    void refreshSessionList();
  });
}

// markSessionSelection re-marks the current conversation after canonical state
// changes without rebuilding the list.
function markSessionSelection() {
  if (!elements.sessionOptions || !state.sessionsList.open) {
    return;
  }
  for (const child of elements.sessionOptions.querySelectorAll(".session-choice")) {
    const selected = child.getAttribute("data-session-id") === state.sessionID;
    child.classList.toggle("is-selected", selected);
    if (selected) {
      child.setAttribute("aria-current", "true");
    } else {
      child.removeAttribute("aria-current");
    }
    child.setAttribute("aria-selected", selected ? "true" : "false");
  }
}

function onSessionFilterInput() {
  state.sessionsList.filter = elements.sessionFilter.value;
  renderSessionList();
}

// handleSessionsKeydown gives the listbox arrows/selection and Escape closes
// the disclosure with focus restored to its trigger.
function handleSessionsKeydown(event) {
  if (!state.sessionsList.open) {
    return;
  }
  if (event.key === "Escape") {
    event.preventDefault();
    if (state.sessionMenuOpen) {
      const trigger = state.sessionMenuOpen.trigger;
      closeSessionMenus();
      trigger.focus();
      return;
    }
    closeSessionsList();
    return;
  }
  if (event.key !== "ArrowDown" && event.key !== "ArrowUp") {
    return;
  }
  const options = [...elements.sessionOptions.querySelectorAll(".session-choice")];
  if (options.length === 0) {
    return;
  }
  event.preventDefault();
  const active = document.activeElement;
  let index = options.indexOf(active);
  if (index === -1) {
    index = event.key === "ArrowDown" ? -1 : 0;
  }
  index =
    (index + (event.key === "ArrowDown" ? 1 : -1) + options.length) %
    options.length;
  options[index].focus();
}

function renderUsage(rows) {
  clearNode(elements.usage);
  const usage = Array.isArray(rows) ? rows : [];
  if (usage.length === 0) {
    elements.usage.textContent = "No usage recorded.";
    return;
  }
  for (const row of usage) {
    const item = document.createElement("p");
    item.textContent = `${row.period || "usage"} · ${row.requests || 0} requests · ${
      row.input_tokens || 0
    } in · ${row.output_tokens || 0} out · ${row.reserved_tokens || 0} reserved`;
    elements.usage.appendChild(item);
  }
}

function renderPermissions(permissions) {
  clearNode(elements.permissions);
  if (!permissions) {
    elements.permissions.textContent = "No permission policy reported.";
    return;
  }
  const rows = [
    ["Sandbox", permissions.sandbox_mode || "Not reported"],
    ["Allow", (permissions.allow || []).join(", ") || "None"],
    ["Deny", (permissions.deny || []).join(", ") || "None"],
    ["Deny prefixes", (permissions.deny_prefixes || []).join(", ") || "None"],
  ];
  for (const [label, value] of rows) {
    const item = document.createElement("p");
    item.textContent = `${label}: ${value}`;
    elements.permissions.appendChild(item);
  }
}

function renderWorkset(items) {
  clearNode(elements.workset);
  const workset = Array.isArray(items) ? items : [];
  if (workset.length === 0) {
    elements.workset.textContent = "The working set is empty.";
    return;
  }
  const list = document.createElement("ul");
  for (const item of workset) {
    const row = document.createElement("li");
    row.textContent = `${item.id || "item"} · ${item.text || ""}`;
    list.appendChild(row);
  }
  elements.workset.appendChild(list);
}

function renderHelp(commands) {
  clearNode(elements.help);
  const available = Array.isArray(commands) ? commands : [];
  if (available.length === 0) {
    elements.help.textContent = "No commands reported.";
    return;
  }
  const list = document.createElement("ul");
  for (const command of available) {
    const item = document.createElement("li");
    item.textContent = `${command.usage || `/${command.name || ""}`} · ${
      command.description || ""
    }`;
    list.appendChild(item);
  }
  elements.help.appendChild(list);
}

async function refreshResultPanel(name, label, render) {
  const result = await runCommandOperation(label, () => commandMutation(name));
  if (result) {
    render(result[name]);
  }
}

async function selectModel() {
  if (state.currentPhase !== phase.idle) {
    return;
  }
  const alias = elements.model.value;
  if (!alias) {
    return;
  }
  state.activeOperation = "model";
  const generation = state.generation;
  setPhase(phase.sending);
  elements.phase.textContent = "Changing model";
  setStatusMessage(elements.modelStatus, "", false, "model");
  try {
    const result = await postMutation("/api/v1/desk/chat/command", {
      client_id: state.clientID,
      command: { name: "model", args: alias },
    });
    if (generation !== state.generation) {
      return;
    }
    renderCanonicalState(result.state, false);
    state.activeOperation = null;
    if (state.currentPhase !== phase.disconnected) {
      setPhase(phase.idle);
    }
  } catch (error) {
    if (generation !== state.generation) {
      return;
    }
    if (state.currentPhase === phase.disconnected) {
      return;
    }
    state.activeOperation = null;
    restoreModelSelection();
    setPhase(phase.idle);
    setStatusMessage(
      elements.modelStatus,
      error.safeMessage || "The model change could not be confirmed.",
      true,
    );
  }
}

async function toggleSkill() {
  if (state.currentPhase !== phase.idle) {
    return;
  }
  const current = selectedSkill();
  if (!current) {
    return;
  }
  const action = current.attached ? "detach" : "attach";
  const generation = state.generation;
  state.activeOperation = "skill";
  setPhase(phase.sending);
  elements.phase.textContent = action === "attach" ? "Attaching skill" : "Detaching skill";
  setStatusMessage(elements.skillStatus, "", false, "skill");
  try {
    const result = await postMutation("/api/v1/desk/chat/command", {
      client_id: state.clientID,
      command: { name: "skills", args: `${action} ${current.name}` },
    });
    if (generation !== state.generation) {
      return;
    }
    renderCanonicalState(result.state, false);
    state.activeOperation = null;
    if (state.currentPhase !== phase.disconnected) {
      setPhase(phase.idle);
    }
  } catch (error) {
    if (generation !== state.generation) {
      return;
    }
    if (state.currentPhase === phase.disconnected) {
      return;
    }
    state.activeOperation = null;
    setPhase(phase.idle);
    setStatusMessage(
      elements.skillStatus,
      error.safeMessage || "The skill change could not be confirmed.",
      true,
    );
  }
}

function attachCopyButton(article) {
  if (!article || article.querySelector(".message-copy")) {
    return;
  }
  const body = article.querySelector(".message-body");
  if (!body) {
    return;
  }
  const copy = document.createElement("button");
  copy.type = "button";
  copy.className = "message-copy";
  copy.textContent = "Copy";
  copy.setAttribute("aria-label", "Copy message");
  copy.addEventListener("click", async () => {
    // Prefer the original markdown so tables and code keep their delimiters;
    // fall back to the rendered text for legacy nodes without raw text.
    const plain = (article.dataset.rawText ?? body.textContent) || "";
    try {
      await navigator.clipboard.writeText(plain);
    } catch {
      const fallback = document.createElement("textarea");
      fallback.value = plain;
      document.body.appendChild(fallback);
      fallback.select();
      document.execCommand("copy");
      fallback.remove();
    }
    copy.textContent = "Copied";
    setTimeout(() => {
      copy.textContent = "Copy";
    }, 1500);
  });
  article.appendChild(copy);
}

function finalizeStreamingMessage() {
  clearTypingIndicator();
  if (!state.streamingMessage) {
    return;
  }
  state.streamingMessage.querySelector(".stream-caret")?.remove();
  state.streamingMessage.dataset.rawText = state.streamingText;
  attachCopyButton(state.streamingMessage);
  state.streamingMessage = null;
  state.streamingText = "";
}

function showTypingIndicator() {
  if (state.typingMessage || !elements.transcript) {
    return;
  }
  const article = document.createElement("article");
  article.className = "message waffle-message typing-message";
  const label = document.createElement("p");
  label.className = "message-author";
  label.textContent = "Waffle";
  const body = document.createElement("div");
  body.className = "message-body";
  const dots = document.createElement("span");
  dots.className = "typing-dots";
  dots.setAttribute("aria-hidden", "true");
  for (let i = 0; i < 3; i += 1) {
    const dot = document.createElement("span");
    dot.className = "typing-dot";
    dots.appendChild(dot);
  }
  body.appendChild(dots);
  article.append(label, body);
  elements.transcript.appendChild(article);
  article.scrollIntoView({ block: "nearest" });
  state.typingMessage = article;
}

function clearTypingIndicator() {
  if (!state.typingMessage) {
    return;
  }
  state.typingMessage.remove();
  state.typingMessage = null;
}

function extractSlashToken(value, selectionStart) {
  const caret = Math.min(selectionStart ?? value.length, value.length);
  const before = value.slice(0, caret);
  const match = /(?:^|\s)(\/[^\s]*)$/.exec(before);
  return match ? match[1] : "";
}

function slashCommandItems(filter) {
  const clean = filter.slice(1).toLowerCase();
  return (state.slash.commands || []).filter((command) => {
    const name = String(command.name || "").toLowerCase();
    const aliases = (command.aliases || []).map((alias) =>
      String(alias).toLowerCase(),
    );
    return name.startsWith(clean) || aliases.some((alias) => alias.startsWith(clean));
  });
}

function slashSkillItems(filter) {
  const clean = filter.slice(1).toLowerCase();
  return (state.skills || []).filter((skill) => {
    const name = String(skill.name || "").toLowerCase();
    const description = String(skill.description || "").toLowerCase();
    return clean === "" || name.includes(clean) || description.includes(clean);
  });
}

function rebuildSlashItems(filter) {
  const commands = slashCommandItems(filter).map((command) => ({
    kind: "command",
    command,
    label: command.usage || `/${command.name || ""}`,
    description: command.description || "",
  }));
  const skills = slashSkillItems(filter).map((skill) => ({
    kind: "skill",
    skill,
    label: skill.name,
    description: skill.description || "",
  }));
  state.slash.items = [...commands, ...skills];
  if (state.slash.index >= state.slash.items.length) {
    state.slash.index = Math.max(0, state.slash.items.length - 1);
  }
}

function renderSlashMenu() {
  const menu = elements.slashMenu;
  menu.replaceChildren();
  let lastKind = null;
  state.slash.items.forEach((item, index) => {
    if (item.kind !== lastKind) {
      const heading = document.createElement("p");
      heading.className = "slash-menu-heading";
      heading.textContent = item.kind === "command" ? "Commands" : "Skills";
      menu.appendChild(heading);
      lastKind = item.kind;
    }
    const entry = document.createElement("button");
    entry.type = "button";
    entry.className = "slash-menu-item";
    entry.setAttribute("role", "option");
    entry.setAttribute("aria-selected", String(index === state.slash.index));
    if (index === state.slash.index) {
      entry.classList.add("is-selected");
    }
    const name = document.createElement("span");
    name.className = "slash-menu-name";
    name.textContent = item.label;
    const description = document.createElement("span");
    description.className = "slash-menu-description";
    description.textContent = item.description;
    entry.append(name, description);
    entry.addEventListener("click", () => {
      state.slash.index = index;
      selectSlashItem();
    });
    menu.appendChild(entry);
  });
  menu.hidden = false;
}

function syncSlashMenu() {
  if (!elements.slashMenu) {
    return;
  }
  const token = extractSlashToken(
    elements.message.value,
    elements.message.selectionStart,
  );
  if (!token.startsWith("/")) {
    closeSlashMenu();
    return;
  }
  if (state.slash.filter !== token) {
    state.slash.index = 0;
    state.slash.filter = token;
  }
  rebuildSlashItems(token);
  if (state.slash.items.length === 0) {
    closeSlashMenu();
    return;
  }
  state.slash.open = true;
  renderSlashMenu();
}

function moveSlashSelection(delta) {
  if (state.slash.items.length === 0) {
    return;
  }
  state.slash.index =
    (state.slash.index + delta + state.slash.items.length) %
    state.slash.items.length;
  renderSlashMenu();
}

function closeSlashMenu() {
  state.slash.open = false;
  if (elements.slashMenu) {
    elements.slashMenu.hidden = true;
  }
}

function selectSlashItem() {
  const item = state.slash.items[state.slash.index];
  if (!item) {
    closeSlashMenu();
    return;
  }
  if (item.kind === "command") {
    const insertion = `/${item.command.name || ""} `;
    const value = elements.message.value;
    const caret = Math.min(
      elements.message.selectionStart ?? value.length,
      value.length,
    );
    const before = value.slice(0, caret);
    const match = /(?:^|\s)(\/[^\s]*)$/.exec(before);
    const tokenStart = match ? caret - match[1].length : caret;
    const next = value.slice(0, tokenStart) + insertion + value.slice(caret);
    elements.message.value = next;
    const nextCaret = tokenStart + insertion.length;
    elements.message.selectionStart = nextCaret;
    elements.message.selectionEnd = nextCaret;
  } else {
    const skill = item.skill;
    const action = skill.attached ? "detach" : "attach";
    const generation = state.generation;
    setPhase(phase.sending);
    elements.phase.textContent =
      action === "attach" ? "Attaching skill" : "Detaching skill";
    void postMutation("/api/v1/desk/chat/command", {
      client_id: state.clientID,
      command: { name: "skills", args: `${action} ${skill.name}` },
    })
      .then((result) => {
        if (generation !== state.generation) {
          return;
        }
        renderCanonicalState(result.state, false);
        state.activeOperation = null;
        if (state.currentPhase !== phase.disconnected) {
          setPhase(phase.idle);
        }
        setStatusMessage(
          elements.composerStatus,
          action === "attach"
            ? `Attached skill ${skill.name}.`
            : `Detached skill ${skill.name}.`,
          false,
          "composer",
        );
      })
      .catch(() => {
        if (generation !== state.generation) {
          return;
        }
        state.activeOperation = null;
        if (state.currentPhase !== phase.disconnected) {
          setPhase(phase.idle);
        }
        setStatusMessage(
          elements.composerStatus,
          `Could not ${action} skill ${skill.name}.`,
          true,
          "composer",
        );
      });
  }
  closeSlashMenu();
  elements.message.focus();
  updateControls();
}

async function fetchCommands() {
  try {
    const response = await fetch("/api/v1/desk/chat/commands", {
      headers: { Accept: "application/json" },
    });
    if (!response.ok) {
      state.slash.commands = [];
      return;
    }
    const body = await response.json();
    state.slash.commands = Array.isArray(body.commands) ? body.commands : [];
  } catch {
    state.slash.commands = [];
  }
}

function onComposerInput() {
  setDraft(state.sessionID, elements.message.value);
  updateControls();
  syncSlashMenu();
}

function handleComposerKeydown(event) {
  if (event.key === "Escape" && state.slash.open) {
    event.preventDefault();
    closeSlashMenu();
    return;
  }
  if (
    state.slash.open &&
    (event.key === "ArrowDown" || event.key === "ArrowUp")
  ) {
    event.preventDefault();
    moveSlashSelection(event.key === "ArrowDown" ? 1 : -1);
    return;
  }
  if (state.slash.open && (event.key === "Enter" || event.key === "Tab")) {
    event.preventDefault();
    selectSlashItem();
    return;
  }
  if (event.key === "Enter" && !event.shiftKey && !event.ctrlKey && !event.metaKey) {
    if (elements.message.value.trim() !== "") {
      event.preventDefault();
      void submitTurn({ preventDefault() {} });
    }
    return;
  }
  if (event.key === "Enter" && (event.ctrlKey || event.metaKey)) {
    event.preventDefault();
    void submitTurn({ preventDefault() {} });
  }
}

function closeOwnerOnPageHide() {
  if (!state.clientID || !state.reattachToken || !state.requestToken) {
    return;
  }
  const lease = {
    client_id: state.clientID,
    reattach_token: state.reattachToken,
  };
  void postMutation("/api/v1/desk/chat/close", lease, { keepalive: true }).catch(
    () => {
      // Navigation cleanup is best effort. The rotated lease and idle reaper
      // remain the recovery paths after a dropped keepalive request.
    },
  );
}

if (elements.form) {
  elements.form.addEventListener("submit", submitTurn);
  elements.message.addEventListener("input", onComposerInput);
  elements.message.addEventListener("keydown", handleComposerKeydown);
  elements.cancel.addEventListener("click", cancelTurn);
  elements.model.addEventListener("change", selectModel);
  elements.skill.addEventListener("change", updateSkillControl);
  elements.skillToggle.addEventListener("click", toggleSkill);
  elements.refresh.addEventListener("click", openDesk);
  elements.newConversation?.addEventListener("click", newConversation);
  elements.sessionRefresh?.addEventListener("click", toggleSessions);
  elements.sessionFilter?.addEventListener("input", onSessionFilterInput);
  elements.sessions?.addEventListener("keydown", handleSessionsKeydown);
  elements.usageRefresh?.addEventListener("click", () =>
    refreshResultPanel("usage", "Loading usage", renderUsage),
  );
  elements.permissionsRefresh?.addEventListener("click", () =>
    refreshResultPanel(
      "permissions",
      "Loading permissions",
      renderPermissions,
    ),
  );
  elements.worksetRefresh?.addEventListener("click", () =>
    refreshResultPanel("workset", "Loading working set", renderWorkset),
  );
  elements.helpRefresh?.addEventListener("click", async () => {
    const result = await runCommandOperation("Loading commands", () =>
      commandMutation("help"),
    );
    if (result) {
      renderHelp(result.commands);
    }
  });
  globalThis.addEventListener?.("pagehide", closeOwnerOnPageHide);
  void openDesk();
  void fetchCommands();
}
