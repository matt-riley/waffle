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
  activity: document.querySelector("#desk-tool-activity"),
  emptyActivity: document.querySelector("#desk-empty-activity"),
  form: document.querySelector("#desk-composer"),
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
  workspace: document.querySelector("#desk-workspace"),
  provider: document.querySelector("#desk-provider"),
};

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
  const cancellable =
    state.activeTurn !== null &&
    (state.currentPhase === phase.sending ||
      state.currentPhase === phase.streaming);
  elements.message.disabled = !idle;
  elements.send.disabled = !idle || elements.message.value.trim() === "";
  elements.cancel.disabled = !cancellable;
  elements.model.disabled = !idle;
  elements.skill.disabled = !idle || state.skills.length === 0;
  elements.skillToggle.disabled = !idle || state.skills.length === 0;
  elements.refresh.disabled = state.currentPhase !== phase.disconnected;
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
  const label = document.createElement("p");
  label.className = "message-author";
  label.textContent = role === "user" ? "You" : "Waffle";
  const body = document.createElement("p");
  body.className = "message-body";
  body.appendChild(document.createTextNode(text));
  article.append(label, body);
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
  if (!state.streamingMessage) {
    state.streamingMessage = appendMessage("assistant", "", null, true);
  }
  state.streamingMessage
    .querySelector(".message-body")
    .appendChild(document.createTextNode(text));
}

function appendToolActivity(kind, data) {
  if (elements.emptyActivity) {
    elements.emptyActivity.remove();
    elements.emptyActivity = null;
  }
  const row = document.createElement("p");
  row.className = "activity-row";
  const tool = data.tool_name || "Tool";
  const status = kind === "tool_started" ? "started" : "finished";
  row.textContent = `${tool} ${status}`;
  elements.activity.appendChild(row);
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
  state.modelAlias = chatState.model_alias || state.modelAlias;
  state.connectionLabel = chatState.connection_mode || state.connectionLabel || "Connected";
  elements.title.textContent = chatState.title || "Untitled conversation";
  if (!state.reconnecting && state.currentPhase !== phase.disconnected) {
    elements.connectionText.textContent = state.connectionLabel;
    elements.connectionDetail.textContent = state.connectionLabel;
  }
  elements.profile.textContent = chatState.profile || "Default";
  elements.workspace.textContent = chatState.workspace || "No workspace";
  elements.provider.textContent = chatState.provider_label || "Not reported";
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
  const recovering = state.currentPhase === phase.disconnected;
  const staleClientID = state.clientID;
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
    if (recovering && staleClientID) {
      try {
        await postMutation("/api/v1/desk/chat/close", {
          client_id: staleClientID,
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
    }
    const sessionID = state.sessionID || initialSessionID;
    const opened = await postMutation("/api/v1/desk/chat/open", {
      continue: sessionID === "",
      session_id: sessionID,
      profile: "",
      capabilities: [],
    });
    if (generation !== state.generation) {
      return;
    }
    state.clientID = opened.client_id || "";
    if (!state.clientID) {
      throw new Error("missing_client_id");
    }
    renderCanonicalState(opened.state, true);
    openEventStream();
    setPhase(phase.idle);
    elements.message.focus();
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
  state.activeTurn = null;
  state.activeOperation = null;
  state.pendingTurn = null;
  setPhase(phase.idle);
}

function turnIdempotencyKey(text) {
  if (state.pendingTurn && state.pendingTurn.text === text) {
    return state.pendingTurn.idempotencyKey;
  }
  const idempotencyKey = crypto.randomUUID();
  state.pendingTurn = { text, idempotencyKey };
  return idempotencyKey;
}

async function submitTurn(event) {
  event.preventDefault();
  const text = elements.message.value.trim();
  if (state.currentPhase !== phase.idle || !text) {
    return;
  }
  state.streamingMessage = null;
  state.activeOperation = "turn";
  const generation = state.generation;
  const idempotencyKey = turnIdempotencyKey(text);
  const turn = {
    id: ++state.turnSequence,
    generation,
    postSettled: false,
    eventSettled: false,
    cancelSettled: true,
    text,
    idempotencyKey,
  };
  state.activeTurn = turn;
  setPhase(phase.sending);
  setStatusMessage(elements.composerStatus, "", false, "composer");
  try {
    await postMutation("/api/v1/desk/chat/turn", {
      client_id: state.clientID,
      text,
    }, { idempotencyKey });
    if (state.activeTurn !== turn || generation !== state.generation) {
      return;
    }
    appendMessage("user", text, state.streamingMessage);
    elements.message.value = "";
    state.pendingTurn = null;
    turn.postSettled = true;
    settleTurn(turn);
  } catch (error) {
    if (state.activeTurn !== turn || generation !== state.generation) {
      return;
    }
    state.activeTurn = null;
    state.activeOperation = null;
    if (error.network) {
      disconnect(
        error.safeMessage ||
          "The turn outcome is unknown. Refresh before sending another message.",
      );
      return;
    }
    // Clear HTTP rejection: message stays, same Idempotency-Key on identical retry.
    setPhase(phase.idle);
    setStatusMessage(
      elements.composerStatus,
      error.safeMessage ||
        "The turn was rejected. Edit the message or send again to retry.",
      true,
    );
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
  turn.cancelSettled = false;
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

if (elements.form) {
  elements.form.addEventListener("submit", submitTurn);
  elements.message.addEventListener("input", updateControls);
  elements.cancel.addEventListener("click", cancelTurn);
  elements.model.addEventListener("change", selectModel);
  elements.skill.addEventListener("change", updateSkillControl);
  elements.skillToggle.addEventListener("click", toggleSkill);
  elements.refresh.addEventListener("click", openDesk);
  void openDesk();
}
