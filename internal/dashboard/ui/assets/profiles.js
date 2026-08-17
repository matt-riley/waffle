// The profile editor is a UI over a trust boundary (#194). It is structured
// only: every field below is typed, and no raw TOML is ever accepted or shown.
// Narrowing is enforced by the server; this module never decides whether a
// change is allowed, it only reports what the server said.

const root = document.querySelector("#profile-editor");

if (root) {
  const elements = {
    list: document.querySelector("#profile-list"),
    errors: document.querySelector("#profile-errors"),
    newButton: document.querySelector("#profile-new"),
    form: document.querySelector("#profile-form"),
    formCancel: document.querySelector("#profile-form-cancel"),
    formTitle: document.querySelector("#profile-form-title"),
    name: document.querySelector("#profile-name"),
    system: document.querySelector("#profile-system"),
    model: document.querySelector("#profile-model"),
    sandbox: document.querySelector("#profile-sandbox"),
    allow: document.querySelector("#profile-allow"),
    deny: document.querySelector("#profile-deny"),
    denyPrefixes: document.querySelector("#profile-deny-prefixes"),
    guidance: document.querySelector("#profile-guidance"),
    maxTokens: document.querySelector("#profile-max-tokens"),
    maxIterations: document.querySelector("#profile-max-iterations"),
    allowedChildren: document.querySelector("#profile-allowed-children"),
    formStatus: document.querySelector("#profile-form-status"),
    reviewDialog: document.querySelector("#profile-review-dialog"),
    reviewTitle: document.querySelector("#profile-review-title"),
    reviewBody: document.querySelector("#profile-review-body"),
    reviewStatus: document.querySelector("#profile-review-status"),
    reviewCancel: document.querySelector("#profile-review-cancel"),
    reviewConfirm: document.querySelector("#profile-review-confirm"),
  };

  const state = {
    requestToken: document.body.dataset.requestToken || "",
    profiles: [],
    pending: null,
  };

  function clearError() {
    elements.errors.hidden = true;
    elements.errors.textContent = "";
  }

  function showError(message) {
    elements.errors.hidden = false;
    elements.errors.textContent = message;
  }

  function setFieldInvalid(field) {
    if (!field) return;
    field.setAttribute("aria-invalid", "true");
    field.focus();
  }

  function clearFieldInvalid() {
    for (const field of [
      elements.name, elements.system, elements.model, elements.sandbox,
      elements.allow, elements.deny, elements.denyPrefixes,
      elements.guidance, elements.maxTokens, elements.maxIterations,
      elements.allowedChildren,
    ]) {
      field?.removeAttribute("aria-invalid");
    }
  }

  // fieldFor maps a server-named config key back to the input that produced
  // it, so a widening refusal points at the control the operator must change.
  function fieldFor(name) {
    switch (name) {
      case "sandbox": return elements.sandbox;
      case "tools.allow": return elements.allow;
      case "tools.deny": return elements.deny;
      case "deny_prefixes": return elements.denyPrefixes;
      default: return null;
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
      error.safeMessage =
        typeof payload.message === "string"
          ? payload.message
          : "The profile change could not be completed.";
      error.field = typeof payload.field === "string" ? payload.field : "";
      error.code = typeof payload.code === "string" ? payload.code : "";
      error.payload = payload;
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

  function splitList(value) {
    return String(value || "")
      .split(/[\s,]+/)
      .map((entry) => entry.trim())
      .filter(Boolean);
  }

  function splitLines(value) {
    return String(value || "")
      .split("\n")
      .map((entry) => entry.trim())
      .filter(Boolean);
  }

  function joinList(values) {
    return Array.isArray(values) ? values.join(", ") : "";
  }

  function readForm() {
    return {
      name: elements.name.value.trim(),
      system: elements.system.value,
      model: elements.model.value.trim(),
      sandbox: elements.sandbox.value,
      allow: splitLines(elements.allow.value),
      deny: splitLines(elements.deny.value),
      // Bash prefixes contain spaces, so they are one per line rather than
      // comma-separated; the structured tool controls follow the same shape.
      deny_prefixes: splitLines(elements.denyPrefixes.value),
      guidance: elements.guidance.value,
      max_tokens: Number(elements.maxTokens.value) || 0,
      max_iterations: Number(elements.maxIterations.value) || 0,
      allowed_children: splitLines(elements.allowedChildren.value),
    };
  }

  function fillForm(profile, { name } = {}) {
    elements.name.value = name ?? profile?.name ?? "";
    elements.system.value = profile?.system || "";
    elements.model.value = profile?.model || "";
    elements.sandbox.value = profile?.sandbox || "";
    elements.allow.value = Array.isArray(profile?.allow)
      ? profile.allow.join("\n")
      : "";
    elements.deny.value = Array.isArray(profile?.deny)
      ? profile.deny.join("\n")
      : "";
    elements.denyPrefixes.value = Array.isArray(profile?.deny_prefixes)
      ? profile.deny_prefixes.join("\n")
      : "";
    elements.guidance.value = profile?.guidance || "";
    elements.maxTokens.value = profile?.max_tokens ? String(profile.max_tokens) : "";
    elements.maxIterations.value = profile?.max_iterations
      ? String(profile.max_iterations)
      : "";
    elements.allowedChildren.value = Array.isArray(profile?.allowed_children)
      ? profile.allowed_children.join("\n")
      : "";
    clearFieldInvalid();
    elements.formStatus.textContent = "";
    elements.form.hidden = false;
  }

  function appendText(parent, tag, className, text) {
    const element = document.createElement(tag);
    element.className = className;
    element.textContent = text;
    parent.append(element);
    return element;
  }

  function actionButton(label, action, run) {
    const button = document.createElement("button");
    button.type = "button";
    button.dataset.action = action;
    button.textContent = label;
    button.addEventListener("click", async () => {
      button.disabled = true;
      clearError();
      try {
        await run(button);
      } catch (error) {
        showError(error.safeMessage || "The profile action could not be completed.");
      } finally {
        button.disabled = false;
      }
    });
    return button;
  }

  function renderProfile(profile) {
    const card = document.createElement("article");
    card.className = "profile-card";
    card.dataset.profile = profile?.name || "";
    appendText(card, "h3", "", profile?.name || "Unnamed profile");
    appendText(
      card,
      "p",
      "profile-summary",
      [
        profile?.sandbox ? `Sandbox ${profile.sandbox}` : "Inherits group sandbox",
        profile?.model ? `Model ${profile.model}` : "Inherits default model",
      ].join(" · "),
    );

    const actions = document.createElement("div");
    actions.className = "profile-actions";
    actions.append(actionButton("Edit", "edit", async () => {
      fillForm(profile);
      elements.formTitle.textContent = `Edit ${profile.name}`;
      elements.name.focus();
    }));
    // Copy is prefill-then-create: there is no separate server verb, so a copy
    // is validated and previewed exactly like any other new profile.
    actions.append(actionButton("Copy", "copy", async () => {
      fillForm(profile, { name: "" });
      elements.formTitle.textContent = `New profile from ${profile.name}`;
      elements.name.focus();
    }));
    actions.append(actionButton("Delete", "delete", async (opener) => {
      await previewDelete(profile.name, opener);
    }));
    card.append(actions);
    return card;
  }

  function render(view) {
    state.profiles = Array.isArray(view?.profiles) ? view.profiles : [];
    elements.list.replaceChildren(...state.profiles.map(renderProfile));
    if (state.profiles.length === 0) {
      elements.list.textContent = "No agent profiles are configured.";
    }
  }

  async function load() {
    try {
      render(await get("/api/v1/desk/profiles"));
      clearError();
    } catch (error) {
      showError(error.safeMessage || "Profiles could not be loaded.");
    }
  }

  function policyValues(name, layer) {
    switch (name) {
      case "Sandbox":
        return layer?.sandbox_mode ? [layer.sandbox_mode] : [];
      case "Allow":
        return layer?.allow;
      case "Deny":
        return layer?.deny;
      case "Deny prefixes":
        return layer?.deny_prefixes;
      default:
        return [];
    }
  }

  // classifyPolicyChange labels a before/after row as narrowed, widened, or
  // unchanged so the review makes the direction of the change explicit (#465).
  function classifyPolicyChange(name, before, after) {
    const left = Array.isArray(before) ? before.slice().sort() : [];
    const right = Array.isArray(after) ? after.slice().sort() : [];
    if (left.length === right.length && left.every((value, index) => value === right[index])) {
      return "unchanged";
    }
    const leftSet = new Set(left);
    const rightSet = new Set(right);
    if (name === "Allow") {
      // Fewer allowed tools narrows; more allowed tools widens.
      const onlyAfter = right.filter((value) => !leftSet.has(value));
      const onlyBefore = left.filter((value) => !rightSet.has(value));
      if (onlyBefore.length === 0 && onlyAfter.length > 0) return "widened";
      if (onlyAfter.length === 0 && onlyBefore.length > 0) return "narrowed";
      return "changed";
    }
    if (name === "Deny" || name === "Deny prefixes") {
      // More denied tools narrows; fewer denied tools widens.
      const onlyAfter = right.filter((value) => !leftSet.has(value));
      const onlyBefore = left.filter((value) => !rightSet.has(value));
      if (onlyBefore.length === 0 && onlyAfter.length > 0) return "narrowed";
      if (onlyAfter.length === 0 && onlyBefore.length > 0) return "widened";
      return "changed";
    }
    // Sandbox: docker is more restricted than host.
    if (name === "Sandbox") {
      const rank = { host: 2, docker: 1 };
      const beforeRank = rank[left[0] || ""] || 0;
      const afterRank = rank[right[0] || ""] || 0;
      return afterRank < beforeRank ? "narrowed" : "widened";
    }
    return "changed";
  }

  function renderLayerList(parent, label, layer, changes) {
    const block = document.createElement("div");
    block.className = "profile-diff-side";
    appendText(block, "h4", "", label);
    const facts = document.createElement("dl");
    facts.className = "posture-facts";
    for (const name of ["Sandbox", "Allow", "Deny", "Deny prefixes"]) {
      const values = policyValues(name, layer);
      const change = changes?.[name] || "unchanged";
      const row = document.createElement("div");
      if (change !== "unchanged") {
        row.className = `policy-change is-${change}`;
      }
      appendText(row, "dt", "", change === "unchanged" ? name : `${name} (${change})`);
      appendText(
        row,
        "dd",
        "",
        Array.isArray(values) && values.length > 0 ? values.join(", ") : "—",
      );
      facts.append(row);
    }
    block.append(facts);
    parent.append(block);
  }

  function renderReview(preview) {
    elements.reviewBody.replaceChildren();
    appendText(
      elements.reviewBody,
      "p",
      "posture-note",
      preview?.exists
        ? "Review the resolved posture before and after this change."
        : "This creates a new profile. Review its resolved posture before saving.",
    );

    const prompt = document.createElement("section");
    prompt.className = "profile-diff";
    const before = document.createElement("div");
    before.className = "profile-diff-side";
    appendText(before, "h4", "", "System prompt before");
    appendText(before, "pre", "posture-prompt", preview?.before?.system?.text || "None");
    const after = document.createElement("div");
    after.className = "profile-diff-side";
    appendText(after, "h4", "", "System prompt after");
    appendText(after, "pre", "posture-prompt", preview?.after?.system?.text || "None");
    prompt.append(before, after);
    elements.reviewBody.append(prompt);

    const policy = document.createElement("section");
    policy.className = "profile-diff";
    const beforeLayer = preview?.before?.effective || {};
    const afterLayer = preview?.after?.effective || {};
    const changes = {};
    let widened = false;
    let narrowed = false;
    for (const name of ["Sandbox", "Allow", "Deny", "Deny prefixes"]) {
      const change = classifyPolicyChange(name, policyValues(name, beforeLayer), policyValues(name, afterLayer));
      changes[name] = change;
      if (change === "widened") widened = true;
      if (change === "narrowed") narrowed = true;
    }
    if (widened || narrowed) {
      appendText(
        policy,
        "p",
        widened ? "posture-note is-widening" : "posture-note is-narrowing",
        widened
          ? "This change widens policy and will be refused by the narrowing guard."
          : "This change narrows policy — safe to review.",
      );
    }
    renderLayerList(policy, "Effective policy before", beforeLayer, changes);
    renderLayerList(policy, "Effective policy after", afterLayer, changes);
    elements.reviewBody.append(policy);
  }

  async function previewSave() {
    clearError();
    clearFieldInvalid();
    const body = readForm();
    if (!body.name) {
      elements.formStatus.textContent = "A profile needs a name.";
      setFieldInvalid(elements.name);
      return;
    }
    elements.formStatus.textContent = "Resolving the change…";
    try {
      const preview = await mutate("/api/v1/desk/profiles/preview", body);
      state.pending = { kind: "save", body, token: preview.preview_token };
      elements.reviewTitle.textContent = `Review ${body.name}`;
      renderReview(preview);
      elements.reviewStatus.textContent =
        "Saving takes effect after Waffle restarts.";
      elements.reviewConfirm.textContent = "Save profile";
      elements.reviewConfirm.disabled = false;
      elements.formStatus.textContent = "";
      elements.reviewDialog.showModal();
    } catch (error) {
      elements.formStatus.textContent =
        error.safeMessage || "The change could not be resolved.";
      // A widening refusal names the field, so point at it directly.
      const field = fieldFor(error.field);
      if (field) setFieldInvalid(field);
    }
  }

  async function previewDelete(name, opener) {
    const preview = await mutate(
      `/api/v1/desk/profiles/${encodeURIComponent(name)}/delete-preview`,
      {},
    );
    elements.reviewTitle.textContent = `Delete ${name}`;
    elements.reviewBody.replaceChildren();
    if (preview?.eligible === true) {
      state.pending = { kind: "delete", name, token: preview.preview_token, opener };
      appendText(
        elements.reviewBody,
        "p",
        "posture-note",
        "Nothing references this profile. Deleting it takes effect after Waffle restarts.",
      );
      elements.reviewStatus.textContent = "";
      elements.reviewConfirm.disabled = false;
    } else {
      state.pending = null;
      appendText(
        elements.reviewBody,
        "p",
        "profile-blocked",
        "This profile cannot be deleted while it is still in use:",
      );
      const list = document.createElement("ul");
      for (const reference of preview?.references || []) {
        appendText(list, "li", "", reference);
      }
      elements.reviewBody.append(list);
      elements.reviewStatus.textContent = "Remove these references first.";
      elements.reviewConfirm.disabled = true;
    }
    elements.reviewConfirm.textContent = "Delete profile";
    elements.reviewDialog.showModal();
  }

  async function confirm() {
    if (!state.pending?.token) return;
    elements.reviewConfirm.disabled = true;
    elements.reviewStatus.textContent = "Applying…";
    const pending = state.pending;
    try {
      const response = pending.kind === "save"
        ? await mutate("/api/v1/desk/profiles", {
          ...pending.body,
          preview_token: pending.token,
        })
        : await mutate(
          `/api/v1/desk/profiles/${encodeURIComponent(pending.name)}/delete`,
          { preview_token: pending.token },
        );
      state.pending = null;
      elements.reviewDialog.close();
      await load();
      elements.formStatus.textContent = response?.restart_required === true
        ? "Saved. Restart Waffle to apply it."
        : "Saved.";
    } catch (error) {
      elements.reviewStatus.textContent =
        error.safeMessage || "The change was not applied.";
      elements.reviewConfirm.disabled = false;
    }
  }

  elements.newButton.addEventListener("click", () => {
    fillForm(null, { name: "" });
    elements.formTitle.textContent = "New profile";
    elements.name.focus();
  });
  elements.formCancel?.addEventListener("click", () => {
    elements.form.hidden = true;
    elements.formStatus.textContent = "";
  });
  elements.form.addEventListener("submit", (event) => {
    event.preventDefault();
    void previewSave();
  });
  elements.reviewCancel.addEventListener("click", () => {
    const opener = state.pending?.opener;
    state.pending = null;
    elements.reviewDialog.close();
    opener?.focus();
  });
  elements.reviewConfirm.addEventListener("click", confirm);
  void load();
}
