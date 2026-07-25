const root = document.querySelector("#desk-capabilities");

if (root) {
  const elements = {
    status: document.querySelector("#capability-status"),
    restart: document.querySelector("#capability-restart-status"),
    models: document.querySelector("#capability-models"),
    skills: document.querySelector("#capability-skills"),
    connections: document.querySelector("#capability-connections"),
    providerForm: document.querySelector("#capability-provider-form"),
    providerStatus: document.querySelector("#capability-provider-status"),
    providerName: document.querySelector("#capability-provider-name"),
    providerType: document.querySelector("#capability-provider-type"),
    providerBaseURL: document.querySelector("#capability-provider-base-url"),
    providerBaseURLGuidance: document.querySelector("#capability-provider-base-url-guidance"),
    providerAlias: document.querySelector("#capability-provider-model-alias"),
    providerModelID: document.querySelector("#capability-provider-model-id"),
    providerMaxTokens: document.querySelector("#capability-provider-max-tokens"),
    providerCredential: document.querySelector("#capability-provider-credential"),
    providerDefault: document.querySelector("#capability-provider-default"),
    providerUtility: document.querySelector("#capability-provider-utility"),
    providerTest: document.querySelector("#capability-provider-test"),
    defaultForm: document.querySelector("#capability-default-form"),
    defaultStatus: document.querySelector("#capability-default-status"),
    defaultAlias: document.querySelector("#capability-default-alias"),
    defaultEmpty: document.querySelector("#capability-default-empty"),
    utilityForm: document.querySelector("#capability-utility-form"),
    utilityStatus: document.querySelector("#capability-utility-status"),
    utilityAlias: document.querySelector("#capability-utility-alias"),
    utilityEmpty: document.querySelector("#capability-utility-empty"),
    modelForm: document.querySelector("#capability-model-form"),
    modelStatus: document.querySelector("#capability-model-status"),
    modelConnection: document.querySelector("#capability-model-connection"),
    modelConnectionEmpty: document.querySelector("#capability-model-connection-empty"),
    modelAlias: document.querySelector("#capability-model-alias"),
    modelID: document.querySelector("#capability-model-id"),
    catalogueForm: document.querySelector("#capability-catalogue-form"),
    catalogueStatus: document.querySelector("#capability-catalogue-status"),
    catalogueConnection: document.querySelector("#capability-catalogue-connection"),
    catalogueEmpty: document.querySelector("#capability-catalogue-empty"),
    catalogueSearch: document.querySelector("#capability-catalogue-search"),
    catalogueSummary: document.querySelector("#capability-catalogue-summary"),
    catalogueResults: document.querySelector("#capability-catalogue-results"),
    stageForm: document.querySelector("#capability-skill-stage-form"),
    stageStatus: document.querySelector("#capability-skill-stage-status"),
    stageLocal: document.querySelector("#capability-skill-local-path"),
    stageGit: document.querySelector("#capability-skill-git-url"),
    stageCommit: document.querySelector("#capability-skill-commit"),
    review: document.querySelector("#capability-skill-review"),
    preview: document.querySelector("#capability-skill-preview"),
    install: document.querySelector("#capability-skill-install"),
    installStatus: document.querySelector("#capability-skill-install-status"),
  };

  const RESTART_POLL_INTERVAL_MS = 1000;
  const RESTART_POLL_TIMEOUT_MS = 60_000;
  const PENDING_LABEL = "Working…";

  const state = {
    requestToken: document.body.dataset.requestToken || "",
    processGeneration: "",
    catalogueConnection: "",
    catalogueModels: [],
    staged: null,
    restarting: false,
    loadGeneration: 0,
    formIntents: Object.create(null),
    providerState: null,
    providerPresetName: "",
    providerPresetBaseURL: "",
  };

  function setPageStatus(message) {
    if (elements.status) {
      elements.status.textContent = message;
    }
  }

  function formStatusNode(form) {
    if (!form) return null;
    if (form === elements.providerForm) return elements.providerStatus;
    if (form === elements.defaultForm) return elements.defaultStatus;
    if (form === elements.utilityForm) return elements.utilityStatus;
    if (form === elements.modelForm) return elements.modelStatus;
    if (form === elements.catalogueForm) return elements.catalogueStatus;
    if (form === elements.stageForm) return elements.stageStatus;
    const describedBy = form.getAttribute?.("aria-describedby") || "";
    if (describedBy && typeof document.querySelector === "function") {
      return document.querySelector(`#${describedBy.split(/\s+/)[0]}`);
    }
    return null;
  }

  function setFormStatus(form, message, tone = "") {
    const node = formStatusNode(form);
    if (!node) return;
    node.textContent = message || "";
    if (tone) {
      node.dataset.tone = tone;
    } else if (node.dataset) {
      delete node.dataset.tone;
    }
  }

  function setControlStatus(statusNode, message, tone = "") {
    if (!statusNode) return;
    statusNode.textContent = message || "";
    if (tone) {
      statusNode.dataset.tone = tone;
    } else if (statusNode.dataset) {
      delete statusNode.dataset.tone;
    }
  }

  function clearFieldInvalid(form) {
    if (!form) return;
    const markValid = (field) => {
      if (!field || typeof field.removeAttribute !== "function") {
        if (field && field.attributes) {
          delete field.attributes["aria-invalid"];
        }
        return;
      }
      field.removeAttribute("aria-invalid");
    };
    if (typeof form.querySelectorAll === "function") {
      for (const field of form.querySelectorAll("input, select, textarea")) {
        markValid(field);
      }
    }
    if (Array.isArray(form.controls)) {
      for (const control of form.controls) {
        if (control && control.type !== "submit" && control.type !== "button") {
          markValid(control);
        }
      }
    }
  }

  function setFieldInvalid(field) {
    if (!field) return;
    if (typeof field.setAttribute === "function") {
      field.setAttribute("aria-invalid", "true");
    } else if (field.attributes) {
      field.attributes["aria-invalid"] = "true";
    }
    if (typeof field.focus === "function") {
      field.focus();
    }
  }

  function findSubmitControl(form) {
    if (!form) return null;
    if (typeof form.querySelector === "function") {
      const button = form.querySelector('button[type="submit"]');
      if (button) return button;
    }
    if (Array.isArray(form.controls)) {
      for (const control of form.controls) {
        if (control && control.type === "submit") {
          return control;
        }
      }
    }
    return null;
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
    setSkillActivateButtonsDisabled(disabled);
  }

  function setSkillActivateButtonsDisabled(disabled) {
    if (!elements.skills) {
      return;
    }
    if (typeof elements.skills.querySelectorAll === "function") {
      const buttons = elements.skills.querySelectorAll(
        'button[data-skill-activate="true"]',
      );
      // Real DOM returns a NodeList (possibly empty). A fake harness that
      // implements querySelectorAll but stores cards only in childNodes falls
      // through to the tree walk below when nothing matched.
      if (buttons && buttons.length > 0) {
        for (const button of buttons) {
          button.disabled = disabled;
        }
        return;
      }
      // Empty NodeList on a real element means there are no activate buttons.
      if (buttons && typeof buttons.length === "number" && !Array.isArray(elements.skills.childNodes)) {
        return;
      }
    }
    // Fake/test harness: walk rendered skill cards without a real DOM.
    const visit = (node) => {
      if (!node) {
        return;
      }
      if (node.dataset && node.dataset.skillActivate === "true") {
        node.disabled = disabled;
      }
      if (Array.isArray(node.childNodes)) {
        for (const child of node.childNodes) {
          visit(child);
        }
      }
    };
    visit(elements.skills);
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
      if (typeof payload.field === "string" && payload.field) {
        error.field = payload.field;
      }
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

  function mutationIntent(formKey, path, body) {
    const serialized = JSON.stringify(body);
    const existing = state.formIntents[formKey];
    if (
      existing &&
      existing.path === path &&
      existing.serialized === serialized
    ) {
      return existing;
    }
    const intent = {
      path,
      serialized,
      key: crypto.randomUUID(),
    };
    state.formIntents[formKey] = intent;
    return intent;
  }

  function clearFormIntent(formKey) {
    delete state.formIntents[formKey];
  }

  async function postMutation(path, body, formKey) {
    const intent = formKey
      ? mutationIntent(formKey, path, body)
      : {
          path,
          serialized: JSON.stringify(body),
          key: crypto.randomUUID(),
        };
    const response = await fetch(path, {
      method: "POST",
      credentials: "same-origin",
      cache: "no-store",
      headers: {
        Accept: "application/json",
        "Content-Type": "application/json",
        "X-Waffle-Desk-Token": state.requestToken,
        "Idempotency-Key": intent.key,
      },
      body: intent.serialized,
    });
    return readJSON(response);
  }

  function clearNode(node) {
    node.replaceChildren();
  }

  function setPickerOptions(select, values, selectedValue, emptyMessage, form, emptyNode) {
    if (!select) return;
    clearNode(select);
    const choices = Array.isArray(values) ? values : [];
    for (const value of choices) {
      const option = document.createElement("option");
      option.value = value;
      option.textContent = value;
      select.appendChild(option);
    }
    const hasChoices = choices.length > 0;
    select.hidden = !hasChoices;
    select.disabled = !hasChoices || state.restarting;
    select.required = hasChoices;
    const submit = findSubmitControl(form);
    if (submit) submit.disabled = !hasChoices || state.restarting;
    if (emptyNode) {
      emptyNode.hidden = hasChoices;
      emptyNode.textContent = hasChoices ? "" : emptyMessage;
    }
    if (!hasChoices) {
      select.value = "";
      return;
    }
    select.value = choices.includes(selectedValue) ? selectedValue : choices[0];
  }

  function renderProviderPresets(presets) {
    if (!elements.providerType) return;
    clearNode(elements.providerType);
    const choices = Array.isArray(presets) ? presets : [];
    for (const preset of choices) {
      if (!preset || typeof preset.name !== "string" || !preset.name) continue;
      const option = document.createElement("option");
      option.value = preset.name;
      option.textContent = preset.name;
      option.dataset.requiresBaseURL = preset.requires_base_url ? "true" : "false";
      option.dataset.baseURL = typeof preset.base_url === "string" ? preset.base_url : "";
      elements.providerType.appendChild(option);
    }
    if (choices.length && !elements.providerType.value) {
      elements.providerType.value = choices[0].name;
    }
    updateProviderPresetGuidance();
  }

  function selectedProviderPreset() {
    const name = elements.providerType?.value || "";
    for (const option of elements.providerType?.childNodes || []) {
      if (option.value === name) return option;
    }
    return null;
  }

  function updateProviderPresetGuidance() {
    const preset = selectedProviderPreset();
    const presetName = elements.providerType?.value || "";
    const presetBaseURL = preset?.dataset?.baseURL || "";
    if (elements.providerBaseURL &&
      (elements.providerBaseURL.value.trim() === "" || elements.providerBaseURL.value === state.providerPresetBaseURL)) {
      elements.providerBaseURL.value = presetBaseURL;
    }
    state.providerPresetName = presetName;
    state.providerPresetBaseURL = presetBaseURL;
    const requiresBaseURL = preset?.dataset?.requiresBaseURL === "true";
    if (elements.providerBaseURL) elements.providerBaseURL.required = requiresBaseURL;
    if (elements.providerBaseURLGuidance) {
      elements.providerBaseURLGuidance.textContent = requiresBaseURL
        ? "Base URL is required for this provider type."
        : "Base URL is optional; leave blank to use the provider default.";
    }
  }

  function findFieldByName(form, name) {
    if (!form || !name) return null;
    if (typeof form.querySelector === "function") {
      const byName = form.querySelector(`[name="${name}"]`);
      if (byName) return byName;
    }
    if (Array.isArray(form.controls)) {
      for (const control of form.controls) {
        if (control && control.name === name) return control;
      }
    }
    return null;
  }

  function validateRequiredFields(form, fields) {
    clearFieldInvalid(form);
    for (const field of fields) {
      if (!field) continue;
      const value = typeof field.value === "string" ? field.value.trim() : "";
      if (!value) {
        setFieldInvalid(field);
        setFormStatus(form, "Fill in the required field.", "error");
        return false;
      }
    }
    return true;
  }

  function applyFieldError(form, error) {
    if (!error || typeof error.field !== "string" || !error.field) {
      return;
    }
    const field = findFieldByName(form, error.field);
    if (field) {
      setFieldInvalid(field);
    }
  }

  async function withSubmitPending(form, run, { statusNode, pendingLabel = PENDING_LABEL } = {}) {
    if (!form) {
      return run(null);
    }
    if (form.dataset.submitting === "true" || form.dataset.restartLocked === "true") {
      return null;
    }
    if (state.restarting && restartPendingForms().includes(form)) {
      return null;
    }

    const submit = findSubmitControl(form);
    const originalLabel = submit ? submit.textContent : "";
    form.dataset.submitting = "true";
    if (submit) {
      submit.disabled = true;
      submit.textContent = pendingLabel;
    }
    if (statusNode) {
      setControlStatus(statusNode, pendingLabel, "pending");
    } else {
      setFormStatus(form, pendingLabel, "pending");
    }

    try {
      return await run(submit);
    } finally {
      form.dataset.submitting = "false";
      if (submit) {
        submit.textContent = originalLabel;
        const locked =
          state.restarting ||
          form.dataset.restartLocked === "true";
        if (!locked) {
          submit.disabled = false;
        }
      }
      // Leave success/error text in place; only clear pending tone if still pending.
      if (statusNode && statusNode.dataset?.tone === "pending") {
        setControlStatus(statusNode, statusNode.textContent, "");
      } else if (!statusNode) {
        const node = formStatusNode(form);
        if (node && node.dataset?.tone === "pending") {
          setFormStatus(form, node.textContent, "");
        }
      }
    }
  }

  function renderModels(providerState) {
    clearNode(elements.models);
    const models = providerState?.models || {};
    const aliases = Object.keys(models).sort();
    const connections = Object.keys(providerState?.providers || {}).sort();
    setPickerOptions(
      elements.defaultAlias,
      aliases,
      providerState?.default_model || "",
      "Add a model before choosing a Waffle-wide default.",
      elements.defaultForm,
      elements.defaultEmpty,
    );
    setPickerOptions(
      elements.utilityAlias,
      aliases,
      providerState?.utility_model || "",
      "Add a model before choosing a utility model.",
      elements.utilityForm,
      elements.utilityEmpty,
    );
    setPickerOptions(
      elements.catalogueConnection,
      connections,
      state.catalogueConnection,
      "Enroll a provider first to browse its catalogue.",
      elements.catalogueForm,
      elements.catalogueEmpty,
    );
    setPickerOptions(
      elements.modelConnection,
      connections,
      "",
      "Enroll a provider first to add a model.",
      elements.modelForm,
      elements.modelConnectionEmpty,
    );
    if (elements.providerDefault) {
      elements.providerDefault.checked = !providerState?.default_model;
    }
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
      const makeDefault = document.createElement("button");
      makeDefault.type = "button";
      makeDefault.textContent = "Make default";
      makeDefault.disabled = state.restarting || providerState.default_model === alias;
      makeDefault.addEventListener("click", async () => {
        if (makeDefault.disabled) return;
        makeDefault.disabled = true;
        try {
          const result = await postMutation(
            "/api/v1/desk/models/default",
            { alias },
            `model-default:${alias}`,
          );
          clearFormIntent(`model-default:${alias}`);
          if (result.restart_required) await handleRestartRequired(result);
          else await loadCapabilities();
        } catch (error) {
          setFormStatus(elements.defaultForm, error.safeMessage || "Capability request could not be completed.", "error");
        } finally {
          if (!state.restarting) makeDefault.disabled = false;
        }
      });
      card.appendChild(makeDefault);
      const makeUtility = document.createElement("button");
      makeUtility.type = "button";
      makeUtility.textContent = "Make utility";
      makeUtility.disabled = state.restarting || providerState.utility_model === alias;
      makeUtility.addEventListener("click", async () => {
        if (makeUtility.disabled) return;
        makeUtility.disabled = true;
        try {
          const result = await postMutation(
            "/api/v1/desk/models/utility",
            { alias },
            `model-utility:${alias}`,
          );
          clearFormIntent(`model-utility:${alias}`);
          if (result.restart_required) await handleRestartRequired(result);
          else await loadCapabilities();
        } catch (error) {
          setFormStatus(elements.utilityForm, error.safeMessage || "Capability request could not be completed.", "error");
        } finally {
          if (!state.restarting) makeUtility.disabled = false;
        }
      });
      card.appendChild(makeUtility);
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
        activate.dataset.skillActivate = "true";
        activate.textContent = "Activate";
        // Always re-read state so re-renders during restart-wait stay locked.
        activate.disabled = state.restarting;
        activate.addEventListener("click", async () => {
          if (state.restarting || activate.disabled) {
            return;
          }
          activate.disabled = true;
          const original = activate.textContent;
          activate.textContent = PENDING_LABEL;
          try {
            const result = await postMutation(
              `/api/v1/desk/skills/${encodeURIComponent(item.name)}/activate`,
              {},
              `skill-activate:${item.name}`,
            );
            clearFormIntent(`skill-activate:${item.name}`);
            setPageStatus("Skill activated.");
            if (result.restart_required) {
              await handleRestartRequired(result);
            } else {
              await loadCapabilities();
            }
          } catch (error) {
            setPageStatus(error.safeMessage || "Capability request could not be completed.");
          } finally {
            activate.textContent = original;
            if (!state.restarting) {
              activate.disabled = false;
            }
          }
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
      if (item?.kind === "provider" && typeof item?.name === "string" && item.name) {
        const test = document.createElement("button");
        test.type = "button";
        test.textContent = "Test connection";
        const testStatus = document.createElement("p");
        testStatus.className = "connection-test-status";
        testStatus.setAttribute("role", "status");
        testStatus.setAttribute("aria-live", "polite");
        test.addEventListener("click", async () => {
          if (test.disabled || state.restarting) return;
          test.disabled = true;
          const original = test.textContent;
          test.textContent = PENDING_LABEL;
          try {
            const result = await postMutation(
              `/api/v1/desk/providers/${encodeURIComponent(item.name)}/test`,
              {},
              `provider-test:${item.name}`,
            );
            clearFormIntent(`provider-test:${item.name}`);
            const messages = {
              success: "Connection test succeeded.",
              authentication_failed: "Connection test authentication failed.",
              unreachable: "Connection test could not reach the endpoint.",
            };
            setControlStatus(testStatus, messages[result.outcome] || "Connection test could not be completed.", result.outcome === "success" ? "" : "error");
          } catch (error) {
            setControlStatus(testStatus, error.safeMessage || "Connection test could not be completed.", "error");
          } finally {
            test.textContent = original;
            test.disabled = false;
          }
        });
        card.appendChild(test);
        card.appendChild(testStatus);
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
      if (model.enrolled_alias) {
        const enrolled = document.createElement("p");
        enrolled.textContent = `Enrolled as ${model.enrolled_alias}.`;
        card.appendChild(enrolled);
      } else if (model.id) {
        const alias = document.createElement("input");
        alias.tagName = "INPUT";
        alias.type = "text";
        alias.value = model.alias_suggestion || "";
        alias.setAttribute("aria-label", `Alias for ${model.id}`);
        card.appendChild(alias);
        const add = document.createElement("button");
        add.type = "button";
        add.textContent = "Add as alias";
        add.disabled = state.restarting;
        add.addEventListener("click", async () => {
          if (add.disabled) return;
          const selectedAlias = alias.value.trim();
          if (!selectedAlias) {
            setFieldInvalid(alias);
            setFormStatus(elements.catalogueForm, "Enter an alias before adding the catalogue model.", "error");
            return;
          }
          add.disabled = true;
          const original = add.textContent;
          add.textContent = PENDING_LABEL;
          try {
            const result = await postMutation(
              "/api/v1/desk/models",
              {
                connection_name: state.catalogueConnection,
                alias: selectedAlias,
                upstream_model: model.id,
                default: false,
                utility: false,
              },
              `catalogue-add:${state.catalogueConnection}:${model.id}`,
            );
            clearFormIntent(`catalogue-add:${state.catalogueConnection}:${model.id}`);
            model.enrolled_alias = selectedAlias;
            renderCatalogue();
            setFormStatus(elements.catalogueForm, "Model added.", "");
            if (result.restart_required) await handleRestartRequired(result);
            else await loadCapabilities();
          } catch (error) {
            setFormStatus(elements.catalogueForm, error.safeMessage || "Capability request could not be completed.", "error");
          } finally {
            add.textContent = original;
            if (!model.enrolled_alias && !state.restarting) add.disabled = false;
          }
        });
        card.appendChild(add);
      }
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
    state.providerState = snapshot.providers;
    renderModels(snapshot.providers);
    renderProviderPresets(snapshot.provider_presets);
    renderSkills(snapshot.skills);
    renderConnections(connections);
    setPageStatus("Capabilities are current.");
    return true;
  }

  function delay(milliseconds) {
    return new Promise((resolve) => setTimeout(resolve, milliseconds));
  }

  function endRestartWait(statusMessage) {
    state.restarting = false;
    setRestartFormsDisabled(false);
    if (statusMessage) {
      setPageStatus(statusMessage);
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

  async function runFormMutation({
    form,
    formKey,
    path,
    body,
    successMessage,
    requiredFields = [],
    onSuccess,
  }) {
    if (!validateRequiredFields(form, requiredFields)) {
      return null;
    }

    return withSubmitPending(form, async () => {
      try {
        const result = await postMutation(path, body, formKey);
        clearFormIntent(formKey);
        setFormStatus(form, successMessage, "");
        if (onSuccess) {
          await onSuccess(result);
        } else if (result.restart_required) {
          await handleRestartRequired(result);
        } else {
          await loadCapabilities();
        }
        return result;
      } catch (error) {
        applyFieldError(form, error);
        setFormStatus(
          form,
          error.safeMessage || "Capability request could not be completed.",
          "error",
        );
        return null;
      }
    });
  }

  elements.providerForm?.addEventListener("submit", async (event) => {
    event.preventDefault();
    const form = elements.providerForm;
    const required = [
      elements.providerName,
      elements.providerType,
      elements.providerAlias,
      elements.providerModelID,
    ];
    if (!validateRequiredFields(form, required)) {
      return;
    }

    const alias = elements.providerAlias.value.trim();
    const modelID = elements.providerModelID.value.trim();
    const maxTokens = Number.parseInt(elements.providerMaxTokens.value, 10) || 0;
    const body = {
      connection_name: elements.providerName.value.trim(),
      type: elements.providerType.value.trim(),
      base_url: elements.providerBaseURL.value.trim(),
      max_tokens: maxTokens,
      api_key: elements.providerCredential.value,
      models: { [alias]: { model: modelID } },
      default_model: elements.providerDefault.checked ? alias : "",
      utility_model: elements.providerUtility.checked ? alias : "",
    };

    await withSubmitPending(form, async () => {
      try {
        let result;
        try {
          result = await postMutation("/api/v1/desk/providers", body, "provider");
          clearFormIntent("provider");
          setFormStatus(form, "Provider enrolled.", "");
        } finally {
          // Always clear the credential field after an enroll attempt.
          clearFormIntent("provider");
          elements.providerCredential.value = "";
        }
        if (result.restart_required) {
          await handleRestartRequired(result);
        } else {
          await loadCapabilities();
        }
      } catch (error) {
        applyFieldError(form, error);
        setFormStatus(
          form,
          error.safeMessage || "Capability request could not be completed.",
          "error",
        );
      }
    });
  });

  elements.providerType?.addEventListener("change", updateProviderPresetGuidance);

  elements.providerTest?.addEventListener("click", async () => {
    const name = elements.providerName.value.trim();
    if (!name) {
      setFieldInvalid(elements.providerName);
      setFormStatus(elements.providerForm, "Enter the connection name before testing it.", "error");
      return;
    }
    if (elements.providerTest.disabled) return;
    const original = elements.providerTest.textContent;
    elements.providerTest.disabled = true;
    elements.providerTest.textContent = PENDING_LABEL;
    const formKey = "provider-prospective-test";
    const body = {
      connection_name: name,
      type: elements.providerType.value.trim(),
      base_url: elements.providerBaseURL.value.trim(),
      max_tokens: Number.parseInt(elements.providerMaxTokens.value, 10) || 0,
      model: elements.providerModelID.value.trim(),
      api_key: elements.providerCredential.value,
    };
    try {
      const result = await postMutation(
        "/api/v1/desk/providers/test",
        body,
        formKey,
      );
      const messages = {
        success: "Connection test succeeded.",
        authentication_failed: "Connection test authentication failed; check the credential.",
        unreachable: "Connection test could not reach the endpoint.",
      };
      setFormStatus(elements.providerForm, messages[result.outcome] || "Connection test could not be completed.", result.outcome === "success" ? "" : "error");
    } catch (error) {
      setFormStatus(elements.providerForm, error.safeMessage || "Connection test could not be completed.", "error");
    } finally {
      clearFormIntent(formKey);
      elements.providerCredential.value = "";
      elements.providerTest.textContent = original;
      elements.providerTest.disabled = false;
    }
  });

  elements.defaultForm?.addEventListener("submit", async (event) => {
    event.preventDefault();
    await runFormMutation({
      form: elements.defaultForm,
      formKey: "default",
      path: "/api/v1/desk/models/default",
      body: { alias: elements.defaultAlias.value.trim() },
      successMessage: "Waffle-wide default changed.",
      requiredFields: [elements.defaultAlias],
    });
  });

  elements.utilityForm?.addEventListener("submit", async (event) => {
    event.preventDefault();
    await runFormMutation({
      form: elements.utilityForm,
      formKey: "utility",
      path: "/api/v1/desk/models/utility",
      body: { alias: elements.utilityAlias.value.trim() },
      successMessage: "Utility model changed.",
      requiredFields: [elements.utilityAlias],
    });
  });

  elements.modelForm?.addEventListener("submit", async (event) => {
    event.preventDefault();
    await runFormMutation({
      form: elements.modelForm,
      formKey: "model",
      path: "/api/v1/desk/models",
      body: {
        connection_name: elements.modelConnection.value.trim(),
        alias: elements.modelAlias.value.trim(),
        upstream_model: elements.modelID.value.trim(),
        default: false,
        utility: false,
      },
      successMessage: "Model added.",
      requiredFields: [
        elements.modelConnection,
        elements.modelAlias,
        elements.modelID,
      ],
    });
  });

  elements.catalogueForm?.addEventListener("submit", async (event) => {
    event.preventDefault();
    const form = elements.catalogueForm;
    if (!validateRequiredFields(form, [elements.catalogueConnection])) {
      return;
    }
    const body = {
      connection: elements.catalogueConnection.value.trim(),
    };
    await withSubmitPending(form, async () => {
      try {
        const result = await postMutation(
          "/api/v1/desk/models/catalogue/refresh",
          body,
          "catalogue",
        );
        clearFormIntent("catalogue");
        state.catalogueConnection =
          result.connection || elements.catalogueConnection.value.trim();
        state.catalogueModels = Array.isArray(result.models) ? result.models : [];
        renderCatalogue();
        setFormStatus(form, "Catalogue refreshed.", "");
      } catch (error) {
        applyFieldError(form, error);
        setFormStatus(
          form,
          error.safeMessage || "Catalogue could not be refreshed.",
          "error",
        );
      }
    });
  });

  elements.catalogueSearch?.addEventListener("input", () => {
    renderCatalogue();
  });

  elements.stageForm?.addEventListener("submit", async (event) => {
    event.preventDefault();
    const form = elements.stageForm;
    clearFieldInvalid(form);
    const localPath = elements.stageLocal.value.trim();
    const gitURL = elements.stageGit.value.trim();
    if (!localPath && !gitURL) {
      setFieldInvalid(elements.stageLocal);
      setFormStatus(form, "Provide a local path or Git URL.", "error");
      return;
    }
    const body = {
      local_path: localPath,
      git_url: gitURL,
      commit: elements.stageCommit.value.trim(),
    };
    await withSubmitPending(form, async () => {
      try {
        state.staged = await postMutation("/api/v1/desk/skills/stage", body, "skill-stage");
        clearFormIntent("skill-stage");
        elements.preview.textContent = JSON.stringify(state.staged, null, 2);
        elements.review.hidden = false;
        setFormStatus(form, "Review the complete manifest before installing.", "");
      } catch (error) {
        applyFieldError(form, error);
        setFormStatus(
          form,
          error.safeMessage || "Skill review could not be staged.",
          "error",
        );
      }
    });
  });

  elements.install?.addEventListener("click", async () => {
    if (!state.staged) return;
    if (elements.install.disabled) return;
    const original = elements.install.textContent;
    elements.install.disabled = true;
    elements.install.textContent = PENDING_LABEL;
    setControlStatus(elements.installStatus, PENDING_LABEL, "pending");
    try {
      const result = await postMutation(
        "/api/v1/desk/skills/install",
        {
          stage_id: state.staged.stage_id,
          digest: state.staged.content_digest,
        },
        "skill-install",
      );
      clearFormIntent("skill-install");
      setControlStatus(elements.installStatus, "Skill installed inactive.", "");
      state.staged = null;
      elements.preview.textContent = "";
      elements.review.hidden = true;
      if (result.restart_required) {
        await handleRestartRequired(result);
      } else {
        await loadCapabilities();
      }
    } catch (error) {
      setControlStatus(
        elements.installStatus,
        error.safeMessage || "Capability request could not be completed.",
        "error",
      );
    } finally {
      elements.install.textContent = original;
      if (!state.restarting) {
        elements.install.disabled = false;
      }
    }
  });

  async function initialize() {
    const bootstrap = validateBootstrap(await getBootstrap());
    state.requestToken = bootstrap.request_token;
    state.processGeneration = bootstrap.process_generation;
    await loadCapabilities();
  }

  void initialize().catch((error) => {
    setPageStatus(error.safeMessage || "Capabilities could not be loaded.");
  });
}
