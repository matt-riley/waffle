const connectionStates = Object.freeze({
  connecting: "connecting",
  connected: "connected",
  degraded: "degraded",
  disconnected: "disconnected",
});

const connectionLabels = Object.freeze({
  [connectionStates.connecting]: "Connecting…",
  [connectionStates.connected]: "Connected",
  [connectionStates.degraded]: "Degraded",
  [connectionStates.disconnected]: "Disconnected",
});

const modelScopes = Object.freeze({
  session: "session",
  waffleWide: "waffle-wide",
});

const railElements = {
  status: document.querySelector("#rail-status"),
  dot: document.querySelector("#rail-status-dot"),
  connection: document.querySelector("#rail-connection"),
  model: document.querySelector("#rail-model"),
};

const railState = {
  connection: connectionStates.connecting,
  modelAlias: "",
  modelScope: "",
};

function modelScopeLabel(scope) {
  if (scope === modelScopes.session) {
    return "session";
  }
  if (scope === modelScopes.waffleWide) {
    return "waffle-wide";
  }
  return "";
}

function modelDisplay(alias, scope) {
  const trimmed = typeof alias === "string" ? alias.trim() : "";
  if (!trimmed) {
    return "—";
  }
  const scopeLabel = modelScopeLabel(scope);
  return scopeLabel ? `${trimmed} · ${scopeLabel}` : trimmed;
}

function railAriaLabel() {
  const connection = connectionLabels[railState.connection] || railState.connection;
  const model = modelDisplay(railState.modelAlias, railState.modelScope);
  // Avoid the substring "Session model" so Playwright getByLabel("Session model")
  // still uniquely resolves the Today select (#desk-model), not this rail region.
  if (railState.modelScope === modelScopes.session) {
    return `Connection and model: ${connection}, ${model} (this conversation)`;
  }
  if (railState.modelScope === modelScopes.waffleWide) {
    return `Connection and model: ${connection}, ${model} (Waffle-wide default)`;
  }
  return `Connection and model: ${connection}, ${model}`;
}

function setRailConnection(state) {
  const next =
    state === connectionStates.connected ||
    state === connectionStates.degraded ||
    state === connectionStates.disconnected ||
    state === connectionStates.connecting
      ? state
      : connectionStates.connecting;
  railState.connection = next;
  const label = connectionLabels[next];
  if (railElements.connection) {
    railElements.connection.textContent = label;
  }
  if (railElements.status) {
    railElements.status.dataset.connectionState = next;
    railElements.status.setAttribute("aria-label", railAriaLabel());
  }
  if (railElements.dot) {
    railElements.dot.className = `status-dot is-${next}`;
  }
}

function setRailModel(alias, scope) {
  const trimmed = typeof alias === "string" ? alias.trim() : "";
  const nextScope =
    scope === modelScopes.session || scope === modelScopes.waffleWide
      ? scope
      : "";
  // Session model wins over the Waffle-wide default once a chat is open.
  if (
    nextScope === modelScopes.waffleWide &&
    railState.modelScope === modelScopes.session
  ) {
    return;
  }
  railState.modelAlias = trimmed;
  railState.modelScope = trimmed ? nextScope : "";
  if (railElements.model) {
    railElements.model.textContent = modelDisplay(trimmed, railState.modelScope);
    railElements.model.dataset.modelScope = railState.modelScope;
  }
  if (railElements.status) {
    railElements.status.setAttribute("aria-label", railAriaLabel());
  }
}

function applyBootstrapHealth(health) {
  if (health && health.healthy === true) {
    setRailConnection(connectionStates.connected);
    return;
  }
  if (health && health.healthy === false) {
    setRailConnection(connectionStates.degraded);
    return;
  }
  setRailConnection(connectionStates.connecting);
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
    error.safeMessage =
      typeof payload.message === "string"
        ? payload.message
        : "The Desk request could not be completed.";
    throw error;
  }
  return payload;
}

async function hydrateRail() {
  try {
    const bootstrap = await readJSON(
      await fetch("/api/v1/desk/bootstrap", {
        method: "GET",
        credentials: "same-origin",
        cache: "no-store",
        headers: { Accept: "application/json" },
      }),
    );
    applyBootstrapHealth(bootstrap.health);
  } catch {
    setRailConnection(connectionStates.disconnected);
    return;
  }
  try {
    const capabilities = await readJSON(
      await fetch("/api/v1/desk/capabilities", {
        method: "GET",
        credentials: "same-origin",
        cache: "no-store",
        headers: { Accept: "application/json" },
      }),
    );
    const defaultModel =
      capabilities &&
      capabilities.providers &&
      typeof capabilities.providers.default_model === "string"
        ? capabilities.providers.default_model
        : "";
    if (defaultModel) {
      setRailModel(defaultModel, modelScopes.waffleWide);
    }
  } catch {
    // Model is best-effort; leave the neutral placeholder when unavailable.
  }
}

// Shared seam for section scripts (Today updates live connection + session model).
globalThis.waffleDeskRail = Object.freeze({
  connectionStates,
  modelScopes,
  setConnection: setRailConnection,
  setModel: setRailModel,
  applyBootstrapHealth,
  // Test seam: inspect current rail presentation state.
  getState: () => ({ ...railState }),
});

// Hash navigation alone does not reliably move focus; make the skip link do so.
// Guard for unit harnesses that only stub a subset of document APIs.
if (typeof document.querySelectorAll === "function") {
  document.querySelectorAll("a.skip-link[href^='#']").forEach((link) => {
    link.addEventListener("click", () => {
      const id = link.getAttribute("href")?.slice(1);
      if (!id) {
        return;
      }
      const target = document.getElementById(id);
      if (target && typeof target.focus === "function") {
        target.focus();
      }
    });
  });
}

void hydrateRail();

const section = document.querySelector(".desk-shell")?.dataset.activeSection || "today";
const moduleName = {
  today: "today.js",
  tasks: "tasks.js",
}[section];

if (moduleName) {
  const moduleURL = new URL(`./${moduleName}`, import.meta.url);
  const version = new URL(import.meta.url).searchParams.get("v");
  if (version) {
    moduleURL.searchParams.set("v", version);
  }
  void import(moduleURL.href);
}
