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

const themePreferences = Object.freeze({
  system: "system",
  light: "light",
  dark: "dark",
});

const themeStorageKey = "waffle.desk.theme";
const themeRoot = document.documentElement;
const themeControl = document.querySelector("#desk-theme");
const themeMediaQuery =
  typeof window !== "undefined" && typeof window.matchMedia === "function"
    ? window.matchMedia("(prefers-color-scheme: dark)")
    : null;

function normalizeThemePreference(preference) {
  return Object.hasOwn(themePreferences, preference)
    ? preference
    : themePreferences.system;
}

function setThemeAttributes(preference, prefersDark) {
  const normalized = normalizeThemePreference(preference);
  const theme = normalized === themePreferences.system
    ? (prefersDark ? themePreferences.dark : themePreferences.light)
    : normalized;
  themeRoot?.setAttribute("data-theme", theme);
  themeRoot?.setAttribute("data-theme-preference", normalized);
  if (themeControl) {
    themeControl.value = normalized;
  }
}

function persistThemePreference(preference) {
  try {
    localStorage.setItem(themeStorageKey, preference);
  } catch {
    // Theme selection still applies when storage is unavailable.
  }
}

let themePreference = normalizeThemePreference(
  themeRoot?.getAttribute("data-theme-preference") || themePreferences.system,
);
setThemeAttributes(themePreference, themeMediaQuery?.matches === true);

themeControl?.addEventListener?.("change", (event) => {
  themePreference = normalizeThemePreference(event.target.value);
  persistThemePreference(themePreference);
  setThemeAttributes(themePreference, themeMediaQuery?.matches === true);
});

if (themeMediaQuery) {
  const handleThemeMediaChange = (event) => {
    if (themePreference === themePreferences.system) {
      setThemeAttributes(themePreference, event.matches === true);
    }
  };
  if (typeof themeMediaQuery.addEventListener === "function") {
    themeMediaQuery.addEventListener("change", handleThemeMediaChange);
  } else if (typeof themeMediaQuery.addListener === "function") {
    themeMediaQuery.addListener(handleThemeMediaChange);
  }
}

// Mobile clearance (#540): the fixed navigation and Today composer can both
// change height as content, controls, or safe-area insets change. Keep the
// CSS contract sourced from their rendered boxes rather than a breakpoint
// guess.
const deskLayoutRoot = document.documentElement;
const deskNavigation = document.querySelector(".desk-navigation");
const deskComposer = document.querySelector("#desk-composer");
const deskComposerActions = document.querySelector(".composer-actions");

function deskLayoutHeight(element) {
  return element?.getBoundingClientRect().height || 0;
}

function updateDeskLayoutMetrics() {
  deskLayoutRoot?.style?.setProperty("--desk-navigation-height", `${deskLayoutHeight(deskNavigation)}px`);
  deskLayoutRoot?.style?.setProperty("--desk-composer-height", `${deskLayoutHeight(deskComposer)}px`);
  deskLayoutRoot?.style?.setProperty("--desk-action-height", `${deskLayoutHeight(deskComposerActions)}px`);
}

if (typeof ResizeObserver === "function") {
  const deskLayoutObserver = new ResizeObserver(updateDeskLayoutMetrics);
  if (deskNavigation) {
    deskLayoutObserver.observe(deskNavigation);
  }
  if (deskComposer) {
    deskLayoutObserver.observe(deskComposer);
  }
  if (deskComposerActions) {
    deskLayoutObserver.observe(deskComposerActions);
  }
}
window.addEventListener?.("resize", updateDeskLayoutMetrics);
updateDeskLayoutMetrics();

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

// Command palette (#477): a global Ctrl/Cmd+K surface backed by existing
// Desk actions and canonical chat command metadata. It never invents a
// shell or an executor — selection clicks real controls or dispatches the
// same command the composer would.
const palette = (() => {
  let isOpen = false;
  let items = [];
  let helpVisible = false;
  let commands = [];

  const elements = {
    root: document.querySelector("#command-palette"),
    search: document.querySelector("#palette-search"),
    results: document.querySelector("#palette-results"),
    newButton: document.querySelector("#palette-new"),
    tasksButton: document.querySelector("#palette-open-tasks"),
    helpButton: document.querySelector("#palette-help"),
    hint: document.querySelector("#palette-shortcut-hint"),
    openButton: document.querySelector("#palette-open"),
  };

  function section() {
    return document.querySelector(".desk-shell")?.dataset.activeSection || "today";
  }

  function run(element) {
    if (element && typeof element.click === "function") {
      element.click();
    } else {
      console.log("PALETTE_RUN none", Boolean(element));
    }
  }

  function sectionItems() {
    const items = [
      { label: "Go to Today", hint: "navigation", run: () => run(document.querySelector('a[href*="section=today"]')) },
      { label: "Go to Tasks", hint: "navigation", run: () => run(document.querySelector('a[href*="section=tasks"]')) },
      { label: "Go to Workspaces", hint: "navigation", run: () => run(document.querySelector('a[href*="section=workspaces"]')) },
      { label: "Go to Memory", hint: "navigation", run: () => run(document.querySelector('a[href*="section=memory"]')) },
      { label: "Go to Capabilities", hint: "navigation", run: () => run(document.querySelector('a[href*="section=capabilities"]')) },
    ];
    switch (section()) {
      case "today":
        items.push(
          { label: "New conversation", hint: "today", run: () => run(document.querySelector("#desk-new")) },
          { label: "Recent conversations", hint: "today", run: () => run(document.querySelector("#desk-session-refresh")) },
          { label: "Export conversation", hint: "today", run: () => run(document.querySelector("#desk-export")) },
          { label: "Schedule this draft", hint: "today", run: () => run(document.querySelector("#desk-schedule-draft")) },
          { label: "Start dictation", hint: "today", run: () => run(document.querySelector("#desk-dictate")) },
        );
        for (const command of commands) {
          items.push({
            label: command.usage || command.name,
            hint: command.description || "chat command",
            run: () => {
              document.dispatchEvent(new CustomEvent("waffle:command", { detail: { name: command.name, args: "" } }));
            },
          });
        }
        break;
      case "tasks":
        items.push({ label: "New schedule", hint: "tasks", run: () => run(document.querySelector("#task-schedule-open")) });
        break;
      case "workspaces":
        items.push({ label: "Open repository", hint: "workspaces", run: () => run(document.querySelector("#workspace-open-button")) });
        break;
      case "memory":
        items.push({
          label: "Search memory", hint: "memory", run: () => {
            document.querySelector("#memory-query")?.focus?.();
          },
        });
        break;
      case "capabilities":
        for (const link of document.querySelectorAll(".capability-tabs a")) {
          const label = link.textContent.trim();
          items.push({ label: `Open ${label}`, hint: "capabilities", run: () => run(link) });
        }
        break;
    }
    return items;
  }

  function render(query) {
    const term = String(query || "").trim().toLowerCase();
    if (helpVisible) {
      elements.results.replaceChildren();
      const heading = document.createElement("p");
      heading.className = "palette-help-heading";
      heading.textContent = "Keyboard shortcuts";
      elements.results.appendChild(heading);
      const helpItems = [
        ["Ctrl/Cmd + K", "Open the command palette"],
        ["Enter", "Send the message (Today composer)"],
        ["Shift + Enter", "New line (Today composer)"],
        ["Escape", "Stop dictation or close dialogs"],
        ["Ctrl/Cmd + Enter", "Send even with no text when attachments are attached"],
      ];
      for (const [keys, what] of helpItems) {
        const row = document.createElement("p");
        row.className = "palette-help-row";
        const k = document.createElement("kbd");
        k.textContent = keys;
        row.append(k, document.createTextNode(` — ${what}`));
        elements.results.appendChild(row);
      }
      return;
    }
    const matches = items.filter(
      (item) => !term || `${item.label} ${item.hint}`.toLowerCase().includes(term),
    );
    elements.results.replaceChildren();
    for (const item of matches) {
      const entry = document.createElement("button");
      entry.type = "button";
      entry.className = "palette-item";
      entry.setAttribute("role", "option");
      const label = document.createElement("span");
      label.className = "palette-item-label";
      label.textContent = item.label;
      entry.appendChild(label);
      if (item.hint) {
        const hint = document.createElement("span");
        hint.className = "palette-item-hint";
        hint.textContent = item.hint;
        entry.appendChild(hint);
      }
      entry.addEventListener("click", () => {
        closePalette();
        item.run();
      });
      elements.results.appendChild(entry);
    }
    if (matches.length === 0) {
      const empty = document.createElement("p");
      empty.className = "palette-empty";
      empty.textContent = "No matching action.";
      elements.results.appendChild(empty);
    }
  }

  async function loadCommands() {
    if (section() !== "today") {
      return;
    }
    try {
      const response = await fetch("/api/v1/desk/chat/commands", {
        credentials: "same-origin",
        cache: "no-store",
        headers: { Accept: "application/json" },
      });
      const payload = await response.json();
      commands = Array.isArray(payload.commands) ? payload.commands : [];
    } catch {
      commands = [];
    }
  }

  function openPalette() {
    if (!elements.root) {
      return;
    }
    isOpen = true;
    elements.root.hidden = false;
    void loadCommands().then(() => {
      items = sectionItems();
      render(elements.search.value);
    });
    elements.search.focus();
  }

  function closePalette() {
    if (!elements.root) {
      return;
    }
    isOpen = false;
    helpVisible = false;
    elements.root.hidden = true;
    elements.search.blur();
  }

  function toggle() {
    if (isOpen) {
      closePalette();
    } else {
      openPalette();
    }
  }

  elements.openButton?.addEventListener?.("click", () => {
    if (isOpen) {
      closePalette();
    } else {
      openPalette();
    }
  });

  if (elements.root && typeof document.addEventListener === "function") {
    document.addEventListener("keydown", (event) => {
      if (event.key === "Escape" && isOpen) {
        event.preventDefault();
        closePalette();
        return;
      }
      const target = event.target;
      const editable =
        target &&
        (target.matches?.("input, textarea, select") ||
          target.isContentEditable === true);
      if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "k") {
        if (editable && !isOpen) {
          // Do not hijack a common edit shortcut inside editable controls.
          return;
        }
        event.preventDefault();
        toggle();
      }
    });

    elements.search?.addEventListener("input", (event) => {
      render(event.target.value);
    });
    elements.helpButton?.addEventListener("click", () => {
      helpVisible = !helpVisible;
      render(elements.search.value);
    });
    elements.newButton?.addEventListener("click", () => {
      close();
      run(document.querySelector("#desk-new") || document.querySelector('a[href*="section=today"]'));
    });
    elements.tasksButton?.addEventListener("click", () => {
      close();
      run(document.querySelector('a[href*="section=tasks"]'));
    });
  }

  return { open: openPalette, close: closePalette, toggle };
})();

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

// Modal dialogs must contain Tab even when they expose a single focusable
// control; the native trap alone is not reliable across shapes, and this page
// is the one script every Desk section loads (#457).
const dialogFocusableSelector =
  'button:not([disabled]), [href], input:not([disabled]):not([type="hidden"]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';
// Guard for unit harnesses that only stub a subset of document APIs.
if (
  typeof document.addEventListener === "function" &&
  typeof document.querySelector === "function"
) {
  document.addEventListener("keydown", (event) => {
    if (event.key !== "Tab") {
      return;
    }
    const dialog = document.querySelector("dialog:modal");
    if (!dialog) {
      return;
    }
    const focusables = Array.from(
      dialog.querySelectorAll(dialogFocusableSelector),
    ).filter(
      (element) => element.offsetParent !== null || element === document.activeElement,
    );
    if (focusables.length === 0) {
      return;
    }
    const first = focusables[0];
    const last = focusables[focusables.length - 1];
    const active = document.activeElement;
    if (event.shiftKey) {
      if (active === first || !dialog.contains(active)) {
        event.preventDefault();
        last.focus();
      }
    } else if (active === last || !dialog.contains(active)) {
      event.preventDefault();
      first.focus();
    }
  });
}

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

function initCapabilityTabs() {
  const nav = document.querySelector(".capability-tabs");
  if (!nav || typeof nav.addEventListener !== "function") {
    return;
  }
  const panels = document.querySelectorAll(".capability-grid > .capability-panel");
  if (!panels.length) {
    return;
  }

  function activate(id) {
    for (const panel of panels) {
      panel.classList?.toggle?.("is-active", panel.id === id);
    }
    for (const link of nav.querySelectorAll("a[href^='#']")) {
      const current = link.getAttribute("href") === `#${id}`;
      if (current) {
        link.setAttribute("aria-current", "true");
      } else {
        link.removeAttribute("aria-current");
      }
    }
  }

  nav.addEventListener("click", (event) => {
    const link = event.target?.closest?.("a[href^='#']");
    if (!link) {
      return;
    }
    const id = link.getAttribute("href")?.slice(1);
    if (!id || !document.getElementById(id)) {
      return;
    }
    event.preventDefault();
    activate(id);
    if (globalThis.history?.replaceState) {
      globalThis.history.replaceState(null, "", `#${id}`);
    }
  });

  globalThis.addEventListener?.("hashchange", () => {
    const id = (globalThis.location?.hash || "").slice(1);
    if (id && document.getElementById(id)) {
      activate(id);
    }
  });

  const initial = (globalThis.location?.hash || "").slice(1);
  if (initial && document.getElementById(initial)) {
    activate(initial);
  }
}

void hydrateRail();
initCapabilityTabs();

const section = document.querySelector(".desk-shell")?.dataset.activeSection || "today";
const moduleName = {
  today: "today.js",
}[section];

if (moduleName) {
  const moduleURL = new URL(`./${moduleName}`, import.meta.url);
  const version = new URL(import.meta.url).searchParams.get("v");
  if (version) {
    moduleURL.searchParams.set("v", version);
  }
  void import(moduleURL.href);
}
