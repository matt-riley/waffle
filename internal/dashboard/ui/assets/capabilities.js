const root = document.querySelector("#desk-capabilities");

if (root) {
  const elements = {
    status: document.querySelector("#capability-status"),
    restart: document.querySelector("#capability-restart-status"),
    models: document.querySelector("#capability-models"),
    skills: document.querySelector("#capability-skills"),
    connections: document.querySelector("#capability-connections"),
    providerForm: document.querySelector("#capability-provider-form"),
    providerName: document.querySelector("#capability-provider-name"),
    providerType: document.querySelector("#capability-provider-type"),
    providerBaseURL: document.querySelector("#capability-provider-base-url"),
    providerAlias: document.querySelector("#capability-provider-model-alias"),
    providerModelID: document.querySelector("#capability-provider-model-id"),
    providerCredential: document.querySelector("#capability-provider-credential"),
    defaultForm: document.querySelector("#capability-default-form"),
    defaultAlias: document.querySelector("#capability-default-alias"),
    utilityForm: document.querySelector("#capability-utility-form"),
    utilityAlias: document.querySelector("#capability-utility-alias"),
    modelForm: document.querySelector("#capability-model-form"),
    modelConnection: document.querySelector("#capability-model-connection"),
    modelAlias: document.querySelector("#capability-model-alias"),
    modelID: document.querySelector("#capability-model-id"),
    catalogueForm: document.querySelector("#capability-catalogue-form"),
    catalogueConnection: document.querySelector("#capability-catalogue-connection"),
    catalogueSearch: document.querySelector("#capability-catalogue-search"),
    catalogueSummary: document.querySelector("#capability-catalogue-summary"),
    catalogueResults: document.querySelector("#capability-catalogue-results"),
    stageForm: document.querySelector("#capability-skill-stage-form"),
    stageLocal: document.querySelector("#capability-skill-local-path"),
    stageGit: document.querySelector("#capability-skill-git-url"),
    stageCommit: document.querySelector("#capability-skill-commit"),
    review: document.querySelector("#capability-skill-review"),
    preview: document.querySelector("#capability-skill-preview"),
    install: document.querySelector("#capability-skill-install"),
  };

  const RESTART_POLL_INTERVAL_MS = 1000;
  const RESTART_POLL_TIMEOUT_MS = 60_000;

  const state = {
    requestToken: document.body.dataset.requestToken || "",
    processGeneration: "",
    catalogueConnection: "",
    catalogueModels: [],
    staged: null,
    restarting: false,
    loadGeneration: 0,
  };

  function setStatus(message) {
    elements.status.textContent = message;
  }

  function restartPendingForms() {
    return [
      elements.providerForm,
      elements.defaultForm,
      elements.utilityForm,
      elements.modelForm,
    ].filter(Boolean);
  }

  function setRestartFormsDisabled(disabled) {
    for (const form of restartPendingForms()) {
      form.dataset.restartLocked = disabled ? "true" : "false";
      if (typeof form.querySelectorAll === "function") {
        for (const control of form.querySelectorAll("input, button, select, textarea")) {
          control.disabled = disabled;
        }
      }
      // Fake/test harnesses may expose controls without a full DOM tree.
      if (Array.isArray(form.controls)) {
        for (const control of form.controls) {
          control.disabled = disabled;
        }
      }
    }
  }

  function setRestartBanner({ title, detail, hidden }) {
    if (!elements.restart) return;
    elements.restart.hidden = Boolean(hidden);
    if (hidden) {
      return;
    }
    clearNode(elements.restart);
    const strong = document.createElement("strong");
    strong.textContent = title;
    const span = document.createElement("span");
    span.textContent = detail;
    elements.restart.appendChild(strong);
    elements.restart.appendChild(span);
  }

  function readRestartOutcome(result) {
    const restart = result && typeof result.restart === "object" && result.restart
      ? result.restart
      : null;
    if (restart && typeof restart.code === "string" && restart.code) {
      return {
        code: restart.code,
        message: typeof restart.message === "string" ? restart.message : "",
        scheduled: Boolean(restart.scheduled),
      };
    }
    // Legacy responses only carried restart_required; treat as scheduled.
    return {
      code: "restart_scheduled",
      message: "Waffle restart was scheduled.",
      scheduled: true,
    };
  }

  async function readJSON(response) {
    let payload = {};
    try {
      payload = await response.json();
    } catch {
      payload = {};
    }
    if (!response.ok) {
      const error = new Error("capability_failed");
      error.safeMessage =
        typeof payload.message === "string"
          ? payload.message
          : "Capability request could not be completed.";
      throw error;
    }
    return payload;
  }

  async function getCapabilities() {
    const response = await fetch("/api/v1/desk/capabilities", {
      method: "GET",
      credentials: "same-origin",
      cache: "no-store",
      headers: { Accept: "application/json" },
    });
    return readJSON(response);
  }

  async function getConnections() {
    const response = await fetch("/api/v1/desk/connections", {
      method: "GET",
      credentials: "same-origin",
      cache: "no-store",
      headers: { Accept: "application/json" },
    });
    return readJSON(response);
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
      typeof bootstrap.request_token !== "string" ||
      !bootstrap.request_token ||
      typeof bootstrap.process_generation !== "string" ||
      !bootstrap.process_generation
    ) {
      throw new Error("invalid_bootstrap");
    }
    return bootstrap;
  }

  async function postMutation(path, body) {
    const response = await fetch(path, {
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
    });
    return readJSON(response);
  }

  function clearNode(node) {
    node.replaceChildren();
  }

  function renderModels(providerState) {
    clearNode(elements.models);
    const models = providerState?.models || {};
    const aliases = Object.keys(models).sort();
    for (const alias of aliases) {
      const model = models[alias] || {};
      const card = document.createElement("article");
      card.className = "capability-card";
      const title = document.createElement("strong");
      title.textContent = alias;
      const detail = document.createElement("p");
      const roles = [];
      if (providerState.default_model === alias) roles.push("Waffle-wide default");
      if (providerState.utility_model === alias) roles.push("Utility");
      detail.textContent = `${model.provider || "Unknown provider"} · ${model.model || "Unknown model"}${roles.length ? ` · ${roles.join(", ")}` : ""}`;
      card.appendChild(title);
      card.appendChild(detail);
      elements.models.appendChild(card);
    }
    if (aliases.length === 0) {
      elements.models.textContent = "No models are enrolled.";
    }
  }

  function renderSkills(skills) {
    clearNode(elements.skills);
    for (const item of Array.isArray(skills) ? skills : []) {
      const card = document.createElement("article");
      card.className = "capability-card";
      const title = document.createElement("strong");
      title.textContent = item.name || "Unnamed skill";
      const detail = document.createElement("p");
      detail.textContent = item.active
        ? "Active"
        : "Installed inactive — review before activation";
      card.appendChild(title);
      card.appendChild(detail);
      if (!item.active && item.name) {
        const activate = document.createElement("button");
        activate.type = "button";
        activate.textContent = "Activate";
        activate.disabled = state.restarting;
        activate.addEventListener("click", async () => {
          await runMutation(
            () => postMutation(`/api/v1/desk/skills/${encodeURIComponent(item.name)}/activate`, {}),
            "Skill activated.",
          );
        });
        card.appendChild(activate);
      }
      elements.skills.appendChild(card);
    }
    if (!elements.skills.childNodes.length) {
      elements.skills.textContent = "No skills are installed.";
    }
  }

  function renderConnections(connections) {
    clearNode(elements.connections);
    elements.connections.textContent = "";
    const records = Array.isArray(connections) ? connections : [];
    const kindLabels = {
      provider: "Provider",
      adapter: "Adapter",
      mcp: "MCP",
      profile: "Profile",
    };
    const statusLabels = {
      configured: "Configured",
      healthy: "Healthy",
      stale: "Stale",
    };
    const sandboxLabels = {
      host: "Host tools",
      docker: "Docker sandbox",
    };
    const egressLabels = {
      disabled: "Egress disabled",
      restricted: "Restricted egress",
      enabled: "Open egress",
    };
    for (const item of records) {
      const card = document.createElement("article");
      card.className = "capability-card connection-card";
      const title = document.createElement("strong");
      title.textContent = typeof item?.name === "string" ? item.name : "Unnamed connection";
      const details = document.createElement("p");
      details.className = "connection-detail";
      const summary = [
        kindLabels[item?.kind] || "Connection",
        statusLabels[item?.status] || "Status unavailable",
      ];
      if (typeof item?.profile === "string" && item.profile) {
        summary.push(`Profile ${item.profile}`);
      }
      if (sandboxLabels[item?.sandbox_mode]) {
        summary.push(sandboxLabels[item.sandbox_mode]);
      }
      if (egressLabels[item?.egress]) {
        summary.push(egressLabels[item.egress]);
      }
      details.textContent = summary.join(" · ");
      card.appendChild(title);
      card.appendChild(details);
      if (typeof item?.guidance === "string" && item.guidance) {
        const guidance = document.createElement("p");
        guidance.className = "connection-guidance";
        guidance.textContent = item.guidance;
        card.appendChild(guidance);
      }
      elements.connections.appendChild(card);
    }
    if (!records.length) {
      elements.connections.textContent = "No tools or connections are configured.";
    }
  }

  function renderCatalogue() {
    clearNode(elements.catalogueResults);
    const query = elements.catalogueSearch.value.trim().toLowerCase();
    const matches = state.catalogueModels.filter((model) => {
      if (!query) return true;
      return [model.id, model.display_name, model.owner].some(
        (value) => typeof value === "string" && value.toLowerCase().includes(query),
      );
    });
    for (const model of matches) {
      const card = document.createElement("article");
      card.className = "capability-card";
      const title = document.createElement("strong");
      title.textContent = model.display_name || model.id || "Unnamed model";
      const detail = document.createElement("p");
      const owner = model.owner ? ` · ${model.owner}` : "";
      detail.textContent = `${model.id || "Unknown model ID"}${owner}`;
      card.appendChild(title);
      card.appendChild(detail);
      elements.catalogueResults.appendChild(card);
    }
    if (matches.length === 0) {
      elements.catalogueResults.textContent = query
        ? "No refreshed models match this search."
        : "This catalogue returned no models.";
    }
    elements.catalogueSummary.textContent = query
      ? `${matches.length} of ${state.catalogueModels.length} models match.`
      : `${state.catalogueModels.length} models from ${state.catalogueConnection}.`;
  }

  async function loadCapabilities() {
    const generation = ++state.loadGeneration;
    let snapshot;
    let connections;
    try {
      [snapshot, connections] = await Promise.all([
        getCapabilities(),
        getConnections(),
      ]);
    } catch (error) {
      if (generation !== state.loadGeneration) return false;
      throw error;
    }
    if (generation !== state.loadGeneration) return false;
    renderModels(snapshot.providers);
    renderSkills(snapshot.skills);
    renderConnections(connections);
    setStatus("Capabilities are current.");
    return true;
  }

  function delay(milliseconds) {
    return new Promise((resolve) => setTimeout(resolve, milliseconds));
  }

  function endRestartWait(statusMessage) {
    state.restarting = false;
    setRestartFormsDisabled(false);
    if (statusMessage) {
      setStatus(statusMessage);
    }
  }

  function finishManualOrFailedRestart(outcome) {
    const message =
      outcome.message ||
      (outcome.code === "restart_schedule_failed"
        ? "restart could not be scheduled; restart waffle serve to apply the change"
        : "Change committed; restart waffle serve to apply.");
    setRestartBanner({
      title: outcome.code === "restart_schedule_failed" ? "Restart could not be scheduled." : "Restart required.",
      detail: message,
      hidden: false,
    });
    endRestartWait(message);
  }

  async function pollRestart(outcome) {
    const startedAt = Date.now();
    const baseMessage = outcome.message || "Waffle restart was scheduled.";
    setRestartBanner({
      title: "Restart scheduled.",
      detail: `${baseMessage} Waiting for a new Waffle process (0s).`,
      hidden: false,
    });

    while (Date.now() - startedAt < RESTART_POLL_TIMEOUT_MS) {
      const elapsedSeconds = Math.floor((Date.now() - startedAt) / 1000);
      setRestartBanner({
        title: "Restart scheduled.",
        detail: `${baseMessage} Waiting for a new Waffle process (${elapsedSeconds}s).`,
        hidden: false,
      });
      try {
        const bootstrap = validateBootstrap(await getBootstrap());
        if (bootstrap.process_generation !== state.processGeneration) {
          state.requestToken = bootstrap.request_token;
          state.processGeneration = bootstrap.process_generation;
          setRestartBanner({ title: "", detail: "", hidden: true });
          endRestartWait("Capabilities are current.");
          await loadCapabilities();
          return;
        }
      } catch {
        // Keep polling until the bound expires; generation may still change.
      }
      await delay(RESTART_POLL_INTERVAL_MS);
    }

    const timeoutMessage =
      "Restart did not complete in time. Restart waffle serve to apply the change.";
    setRestartBanner({
      title: "Restart timed out.",
      detail: timeoutMessage,
      hidden: false,
    });
    endRestartWait(timeoutMessage);
  }

  async function handleRestartRequired(result) {
    const outcome = readRestartOutcome(result);
    state.restarting = true;
    setRestartFormsDisabled(true);

    if (
      outcome.code === "manual_restart_required" ||
      outcome.code === "restart_schedule_failed"
    ) {
      finishManualOrFailedRestart(outcome);
      return;
    }

    await pollRestart(outcome);
  }

  async function runMutation(action, successMessage) {
    try {
      const result = await action();
      setStatus(successMessage);
      if (result.restart_required) {
        await handleRestartRequired(result);
      } else {
        await loadCapabilities();
      }
      return result;
    } catch (error) {
      setStatus(error.safeMessage || "Capability request could not be completed.");
      return null;
    }
  }

  elements.providerForm?.addEventListener("submit", async (event) => {
    event.preventDefault();
    const alias = elements.providerAlias.value.trim();
    const modelID = elements.providerModelID.value.trim();
    let result;
    try {
      result = await postMutation("/api/v1/desk/providers", {
        connection_name: elements.providerName.value.trim(),
        type: elements.providerType.value.trim(),
        base_url: elements.providerBaseURL.value.trim(),
        api_key: elements.providerCredential.value,
        models: { [alias]: { model: modelID } },
        default_model: alias,
      });
    } catch (error) {
      setStatus(error.safeMessage || "Capability request could not be completed.");
      return;
    } finally {
      elements.providerCredential.value = "";
    }
    setStatus("Provider enrolled.");
    if (result.restart_required) {
      await handleRestartRequired(result);
    } else {
      await loadCapabilities();
    }
  });

  elements.defaultForm?.addEventListener("submit", async (event) => {
    event.preventDefault();
    await runMutation(
      () => postMutation("/api/v1/desk/models/default", { alias: elements.defaultAlias.value.trim() }),
      "Waffle-wide default changed.",
    );
  });

  elements.utilityForm?.addEventListener("submit", async (event) => {
    event.preventDefault();
    await runMutation(
      () => postMutation("/api/v1/desk/models/utility", { alias: elements.utilityAlias.value.trim() }),
      "Utility model changed.",
    );
  });

  elements.modelForm?.addEventListener("submit", async (event) => {
    event.preventDefault();
    await runMutation(
      () => postMutation("/api/v1/desk/models", {
        connection_name: elements.modelConnection.value.trim(),
        alias: elements.modelAlias.value.trim(),
        upstream_model: elements.modelID.value.trim(),
      }),
      "Model added.",
    );
  });

  elements.catalogueForm?.addEventListener("submit", async (event) => {
    event.preventDefault();
    try {
      const result = await postMutation("/api/v1/desk/models/catalogue/refresh", {
        connection: elements.catalogueConnection.value.trim(),
      });
      state.catalogueConnection = result.connection || elements.catalogueConnection.value.trim();
      state.catalogueModels = Array.isArray(result.models) ? result.models : [];
      renderCatalogue();
      setStatus("Catalogue refreshed.");
    } catch (error) {
      setStatus(error.safeMessage || "Catalogue could not be refreshed.");
    }
  });

  elements.catalogueSearch?.addEventListener("input", () => {
    renderCatalogue();
  });

  elements.stageForm?.addEventListener("submit", async (event) => {
    event.preventDefault();
    try {
      state.staged = await postMutation("/api/v1/desk/skills/stage", {
        local_path: elements.stageLocal.value.trim(),
        git_url: elements.stageGit.value.trim(),
        commit: elements.stageCommit.value.trim(),
      });
      elements.preview.textContent = JSON.stringify(state.staged, null, 2);
      elements.review.hidden = false;
      setStatus("Review the complete manifest before installing.");
    } catch (error) {
      setStatus(error.safeMessage || "Skill review could not be staged.");
    }
  });

  elements.install?.addEventListener("click", async () => {
    if (!state.staged) return;
    const installed = await runMutation(
      () => postMutation("/api/v1/desk/skills/install", {
        stage_id: state.staged.stage_id,
        digest: state.staged.content_digest,
      }),
      "Skill installed inactive.",
    );
    if (installed) {
      state.staged = null;
      elements.preview.textContent = "";
      elements.review.hidden = true;
    }
  });

  async function initialize() {
    const bootstrap = validateBootstrap(await getBootstrap());
    state.requestToken = bootstrap.request_token;
    state.processGeneration = bootstrap.process_generation;
    await loadCapabilities();
  }

  void initialize().catch((error) => {
    setStatus(error.safeMessage || "Capabilities could not be loaded.");
  });
}
