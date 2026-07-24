const root = document.querySelector("#desk-capabilities");

if (root) {
  const elements = {
    status: document.querySelector("#capability-status"),
    restart: document.querySelector("#capability-restart-status"),
    models: document.querySelector("#capability-models"),
    skills: document.querySelector("#capability-skills"),
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
    stageForm: document.querySelector("#capability-skill-stage-form"),
    stageLocal: document.querySelector("#capability-skill-local-path"),
    stageGit: document.querySelector("#capability-skill-git-url"),
    stageCommit: document.querySelector("#capability-skill-commit"),
    review: document.querySelector("#capability-skill-review"),
    preview: document.querySelector("#capability-skill-preview"),
    install: document.querySelector("#capability-skill-install"),
  };

  const state = {
    requestToken: document.body.dataset.requestToken || "",
    staged: null,
    restarting: false,
  };

  function setStatus(message) {
    elements.status.textContent = message;
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

  async function loadCapabilities() {
    const snapshot = await getCapabilities();
    renderModels(snapshot.providers);
    renderSkills(snapshot.skills);
    setStatus("Capabilities are current.");
  }

  function delay(milliseconds) {
    return new Promise((resolve) => setTimeout(resolve, milliseconds));
  }

  async function pollRestart() {
    state.restarting = true;
    elements.restart.hidden = false;
    for (;;) {
      try {
        const response = await fetch("/api/v1/desk/bootstrap", {
          method: "GET",
          credentials: "same-origin",
          cache: "no-store",
          headers: { Accept: "application/json" },
        });
        const bootstrap = await readJSON(response);
        if (typeof bootstrap.request_token !== "string" || !bootstrap.request_token) {
          throw new Error("invalid_bootstrap");
        }
        state.requestToken = bootstrap.request_token;
        state.restarting = false;
        elements.restart.hidden = true;
        await loadCapabilities();
        return;
      } catch {
        await delay(1000);
      }
    }
  }

  async function runMutation(action, successMessage) {
    try {
      const result = await action();
      setStatus(successMessage);
      if (result.restart_required) {
        await pollRestart();
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
      await pollRestart();
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

  void loadCapabilities().catch((error) => {
    setStatus(error.safeMessage || "Capabilities could not be loaded.");
  });
}
