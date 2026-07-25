// The setup checklist is Desk's bootstrap surface (#192). It reports what the
// running Waffle is still missing and routes the owner to the control that
// fixes it. It decides nothing itself: every state, label, and command in the
// list is chosen by the server, so the browser cannot claim a prerequisite is
// satisfied when the runtime disagrees.
//
// The one mutation here creates the secret-store identity. No key material
// comes back — the value stays in the OS keyring, and `waffle secret
// export-identity` is how the owner backs it up.

const panel = document.querySelector("#setup-checklist");
const banner = document.querySelector("#desk-setup-banner");

if (panel || banner) {
  const elements = {
    steps: document.querySelector("#setup-steps"),
    status: document.querySelector("#setup-status"),
    bannerMessage: document.querySelector("#desk-setup-banner-message"),
  };

  const state = { requestToken: document.body.dataset.requestToken || "" };

  const STATE_LABELS = {
    configured: "Configured",
    missing: "Missing",
    misconfigured: "Misconfigured",
  };

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
          : "The setup action could not be completed.";
      throw error;
    }
    return payload;
  }

  async function get(path) {
    return readJSON(await fetch(path, {
      method: "GET",
      credentials: "same-origin",
      cache: "no-store",
      headers: { Accept: "application/json" },
    }));
  }

  async function mutate(path, body) {
    return readJSON(await fetch(path, {
      method: "POST",
      credentials: "same-origin",
      cache: "no-store",
      headers: {
        Accept: "application/json",
        "Content-Type": "application/json",
        "X-Waffle-Desk-Token": state.requestToken,
        "Idempotency-Key": crypto.randomUUID(),
      },
      body: JSON.stringify(body),
    }));
  }

  function appendText(parent, tag, className, text) {
    const element = document.createElement(tag);
    element.className = className;
    element.textContent = text;
    parent.append(element);
    return element;
  }

  // reveal brings a control on the same page into view and focuses it. The
  // checklist never duplicates a form: it points at the one that already
  // exists, so credentials keep travelling through that single boundary.
  function reveal(selector) {
    const target = document.querySelector(selector);
    if (!target) return false;
    target.scrollIntoView({ block: "center" });
    target.focus();
    return true;
  }

  async function createIdentity() {
    const response = await mutate("/api/v1/desk/setup/identity", {});
    await load();
    return response?.restart_required === true
      ? "Identity created. Restart Waffle to use it, then back it up with `waffle secret export-identity`."
      : "Identity created. Back it up with `waffle secret export-identity`.";
  }

  // ACTIONS maps a server-named action onto the control that performs it.
  // An action the browser cannot honour returns a message rather than
  // silently doing nothing.
  const ACTIONS = {
    "create-identity": createIdentity,
    "enroll-provider": async () =>
      reveal("#capability-provider-name")
        ? "Fill in the provider connection below."
        : "Provider enrollment is on the Capabilities section.",
    "set-default-model": async () =>
      reveal("#capability-default-alias")
        ? "Choose the Waffle-wide default model below."
        : "Model roles are on the Capabilities section.",
    "create-profile": async () => {
      if (!reveal("#profile-name")) {
        return "The profile editor is on the Capabilities section.";
      }
      const name = document.querySelector("#profile-name");
      const system = document.querySelector("#profile-system");
      const title = document.querySelector("#profile-form-title");
      // Prefill only what setup would have written, and only into empty
      // fields, so an edit already in progress is never overwritten.
      const starter = panel?.dataset.starterSystem || "";
      if (name && !name.value) name.value = "main";
      if (system && !system.value && starter) system.value = starter;
      if (title) title.textContent = "Starter profile";
      return "Review the starter profile below, then save it.";
    },
  };

  function renderStep(step) {
    const item = document.createElement("li");
    item.className = "setup-step";
    item.dataset.step = step?.id || "";
    item.dataset.state = step?.state || "";

    const heading = document.createElement("div");
    heading.className = "setup-step-heading";
    appendText(heading, "h3", "", step?.title || "Prerequisite");
    appendText(
      heading,
      "span",
      "setup-state",
      STATE_LABELS[step?.state] || "Unknown",
    );
    item.append(heading);

    appendText(item, "p", "setup-detail", step?.detail || "");

    const action = ACTIONS[step?.action];
    if (step?.action && action) {
      const status = document.createElement("p");
      status.className = "capability-form-status";
      status.setAttribute("role", "status");
      status.setAttribute("aria-live", "polite");

      const button = document.createElement("button");
      button.type = "button";
      button.dataset.action = step.action;
      button.textContent = step.action_label || "Fix this";
      button.addEventListener("click", async () => {
        button.disabled = true;
        status.textContent = "Working…";
        try {
          status.textContent = (await action(step)) || "";
        } catch (error) {
          status.textContent =
            error.safeMessage || "The setup action could not be completed.";
        } finally {
          button.disabled = false;
        }
      });
      item.append(button, status);
    } else if (step?.command) {
      // AC2: a prerequisite Desk cannot satisfy states the exact command.
      const hint = document.createElement("p");
      hint.className = "setup-command";
      appendText(hint, "span", "setup-command-label", "Run:");
      appendText(hint, "code", "", step.command);
      item.append(hint);
    }
    return item;
  }

  function render(view) {
    const steps = Array.isArray(view?.steps) ? view.steps : [];
    const outstanding = steps.filter((step) => step?.state !== "configured");

    if (elements.steps) {
      elements.steps.replaceChildren(...steps.map(renderStep));
    }
    if (elements.status) {
      elements.status.textContent = view?.complete === true
        ? "Waffle is fully set up."
        : `${outstanding.length} ${outstanding.length === 1 ? "prerequisite is" : "prerequisites are"} outstanding.`;
    }
    if (banner) {
      banner.hidden = view?.complete === true || outstanding.length === 0;
      if (elements.bannerMessage && outstanding.length > 0) {
        elements.bannerMessage.textContent = `Still outstanding: ${
          outstanding.map((step) => step.title).join(", ")
        }.`;
      }
    }
  }

  async function load() {
    try {
      render(await get("/api/v1/desk/setup"));
    } catch (error) {
      // A Desk without the setup surface mounted is not broken; say so
      // plainly rather than blocking the rest of the section.
      if (elements.status) {
        elements.status.textContent =
          error.safeMessage || "Setup state is unavailable.";
      }
      if (banner) banner.hidden = true;
    }
  }

  void load();
}
