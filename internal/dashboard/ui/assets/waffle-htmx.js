// Waffle's small external htmx policy/request bridge.
// It deliberately uses external htmx events rather than inline handlers.
(() => {
  "use strict";

  if (typeof htmx === "undefined") {
    return;
  }

  htmx.config.allowEval = false;
  htmx.config.allowScriptTags = false;
  htmx.config.includeIndicatorStyles = false;

  const intents = new Map();
  const inFlight = new Set();

  function uuid() {
    if (globalThis.crypto && typeof globalThis.crypto.randomUUID === "function") {
      return globalThis.crypto.randomUUID();
    }
    return `${Date.now().toString(16)}-${Math.random().toString(16).slice(2)}`;
  }

  function objectFromParameters(parameters) {
    const output = {};
    if (!parameters) return output;
    const entries = parameters instanceof URLSearchParams
      ? parameters.entries()
      : Object.entries(parameters);
    for (const [name, raw] of entries) {
      const values = Array.isArray(raw) ? raw : [raw];
      const value = values.length === 1 ? values[0] : values;
      if (name in output) {
        output[name] = Array.isArray(output[name])
          ? [...output[name], value]
          : [output[name], value];
      } else {
        output[name] = value;
      }
    }
    return output;
  }

  function normalize(value, name) {
    if (name === "max_tokens" || name === "enabled") {
      if (name === "enabled") return value === true || value === "true" || value === "on";
      const number = Number(value);
      return Number.isFinite(number) ? number : 0;
    }
    return value;
  }

  document.body.addEventListener("htmx:configRequest", (event) => {
    const detail = event.detail || {};
    const element = detail.elt;
    const form = element?.matches?.("form") ? element : element?.closest?.("form");
    if (!form?.matches?.("[data-waffle-json]")) return;

    const body = objectFromParameters(detail.parameters || detail.unfilteredParameters);
    for (const [name, value] of Object.entries(body)) {
      body[name] = Array.isArray(value)
        ? value.map((item) => normalize(item, name))
        : normalize(value, name);
    }
    if (form.dataset.waffleJsonKind === "provider") {
      const alias = body.model_alias || "";
      const model = body.model_id || "";
      body.models = alias ? { [alias]: { model } } : {};
      body.default_model = body.make_default ? alias : "";
      body.utility_model = body.make_utility ? alias : "";
      delete body.model_alias;
      delete body.model_id;
      delete body.make_default;
      delete body.make_utility;
    }
    const serialized = JSON.stringify(body);
    const identity = `${detail.verb || "POST"} ${detail.path || ""}\u0000${serialized}`;
    const intent = intents.get(identity) || { key: uuid(), serialized };
    intents.set(identity, intent);
    detail.headers = detail.headers || {};
    detail.headers.Accept = "text/html";
    detail.headers["Content-Type"] = "application/json";
    detail.headers["X-Waffle-Desk-Token"] = document.body.dataset.requestToken || "";
    detail.headers["Idempotency-Key"] = intent.key;
    detail.waffleIntent = intent;
    detail.waffleJSON = serialized;
  });

  document.body.addEventListener("htmx:beforeRequest", (event) => {
    const intent = event.detail?.requestConfig?.waffleIntent;
    if (intent && inFlight.has(intent.key)) {
      event.preventDefault();
    }
  });

  document.body.addEventListener("htmx:beforeSend", (event) => {
    const detail = event.detail || {};
    const config = detail.requestConfig;
    if (!config?.waffleJSON || !detail.xhr) return;
    const xhr = detail.xhr;
    const originalSend = xhr.send.bind(xhr);
    xhr.send = () => originalSend(config.waffleJSON);
    if (config.waffleIntent) inFlight.add(config.waffleIntent.key);
  });

  document.body.addEventListener("htmx:afterRequest", (event) => {
    const config = event.detail?.requestConfig;
    const intent = config?.waffleIntent;
    if (!intent) return;
    inFlight.delete(intent.key);
    if (event.detail.xhr?.status >= 200 && event.detail.xhr?.status < 300) {
      const form = event.detail.elt?.matches?.("form")
        ? event.detail.elt
        : event.detail.elt?.closest?.("form");
      if (form?.matches?.("[data-waffle-json]")) {
        for (const input of form.querySelectorAll?.("input[type=password]") || []) {
          input.value = "";
        }
      }
      const path = config.path || "";
      if (path.endsWith("/open") || path.endsWith("/close") || path.endsWith("/forget")) {
        event.detail.elt?.closest?.("dialog")?.close?.();
      }
      if (path.endsWith("/close-preview") || path.endsWith("/forget-preview")) {
        document.querySelector("#workspace-close-dialog, #memory-forget-dialog")?.showModal?.();
      }
      for (const [identity, candidate] of intents) {
        if (candidate.key === intent.key) intents.delete(identity);
      }
    }
  });

  document.body.addEventListener("click", (event) => {
    const open = event.target?.closest?.("#workspace-open-button");
    if (open) {
      event.preventDefault();
      document.querySelector("#workspace-open-dialog")?.showModal?.();
      return;
    }
    const openCancel = event.target?.closest?.("#workspace-open-cancel");
    if (openCancel) {
      event.preventDefault();
      const dialog = openCancel.closest?.("dialog");
      dialog?.close?.();
      document.querySelector("#workspace-open-button")?.focus?.();
      return;
    }
    const cancel = event.target?.closest?.("[data-waffle-dialog-cancel]");
    if (!cancel) return;
    const dialog = cancel.closest?.("dialog");
    if (!dialog) return;
    event.preventDefault();
    dialog.close?.();
    const focusID = dialog.dataset.waffleDialogFocus || "";
    if (focusID && document.querySelectorAll) {
      for (const candidate of document.querySelectorAll("[data-waffle-action-id]")) {
        if (candidate.dataset.waffleActionId === focusID) {
          candidate.focus?.();
          break;
        }
      }
    }
  });
})();
