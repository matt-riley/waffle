// Posture is Desk's read-only view of what the agent was told and what it may
// do (#193). It is one self-contained module loaded by both Today and
// Capabilities: a relative import between assets would resolve without the
// ?v= cache key, and assets are served immutable for a year.
//
// It never mutates. Every request here is a GET with no Desk token and no
// idempotency key, because there is nothing to make idempotent.

const dialog = document.querySelector("#desk-posture-dialog");

if (dialog) {
  const elements = {
    dialog,
    title: document.querySelector("#desk-posture-title"),
    status: document.querySelector("#desk-posture-status"),
    body: document.querySelector("#desk-posture-body"),
    close: document.querySelector("#desk-posture-close"),
  };

  let opener = null;

  function clear(node) {
    if (node) node.replaceChildren();
  }

  function appendText(parent, tag, className, text) {
    const element = document.createElement(tag);
    element.className = className;
    element.textContent = text;
    parent.append(element);
    return element;
  }

  function appendFact(parent, label, value) {
    const row = document.createElement("div");
    appendText(row, "dt", "", label);
    appendText(row, "dd", "", value);
    parent.append(row);
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
          : "The posture could not be read.";
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

  const sourceLabels = {
    default: "Inherited default prompt",
    inline: "Inline in config.toml",
    file: "Resolved from a file",
  };

  const layerLabels = {
    group: "Agent group",
    profile: "Profile narrowing",
    repo: "Repo policy (WAFFLE.md)",
    effective: "Effective",
  };

  function list(values) {
    return Array.isArray(values) && values.length > 0 ? values.join(", ") : "—";
  }

  function renderSystem(parent, system) {
    const section = document.createElement("section");
    section.className = "posture-block";
    appendText(section, "h3", "", "System prompt");

    const source = system?.source;
    const facts = document.createElement("dl");
    facts.className = "posture-facts";
    appendFact(facts, "Source", sourceLabels[source] || "Unknown");
    if (typeof system?.path === "string" && system.path) {
      // Relative to WAFFLE_HOME by construction; the server never sends an
      // absolute path here.
      appendFact(facts, "File", system.path);
    }
    section.append(facts);

    if (typeof system?.error === "string" && system.error) {
      appendText(section, "p", "posture-error", system.error);
    } else if (typeof system?.text === "string" && system.text) {
      appendText(section, "pre", "posture-prompt", system.text);
    } else {
      appendText(
        section,
        "p",
        "posture-empty",
        "This profile adds no system prompt of its own.",
      );
    }
    parent.append(section);
  }

  function renderLayer(parent, layer, isEffective) {
    const card = document.createElement("article");
    card.className = isEffective ? "posture-layer posture-layer-effective" : "posture-layer";
    card.dataset.layer = layer?.name || "";

    const header = document.createElement("header");
    appendText(header, "h4", "", layerLabels[layer?.name] || layer?.name || "Layer");
    if (!isEffective) {
      appendText(
        header,
        "p",
        "posture-layer-state",
        layer?.applied === true ? "Applied" : "No change",
      );
    }
    card.append(header);

    const facts = document.createElement("dl");
    facts.className = "posture-facts";
    if (typeof layer?.sandbox_mode === "string" && layer.sandbox_mode) {
      appendFact(facts, "Sandbox", layer.sandbox_mode);
    }
    appendFact(facts, "Allow", list(layer?.allow));
    appendFact(facts, "Deny", list(layer?.deny));
    appendFact(facts, "Deny prefixes", list(layer?.deny_prefixes));
    card.append(facts);

    if (typeof layer?.guidance === "string" && layer.guidance) {
      appendText(card, "p", "posture-guidance", layer.guidance);
    }
    parent.append(card);
  }

  function renderPolicy(parent, view) {
    const section = document.createElement("section");
    section.className = "posture-block";
    appendText(section, "h3", "", "Tool policy");
    appendText(
      section,
      "p",
      "posture-note",
      "Each tier shows only its own contribution. A profile may narrow its group and never widen it.",
    );
    const layers = Array.isArray(view?.layers) ? view.layers : [];
    for (const layer of layers) {
      renderLayer(section, layer, false);
    }
    if (view?.effective) {
      renderLayer(section, view.effective, true);
    }
    parent.append(section);
  }

  function renderLimits(parent, limits) {
    const section = document.createElement("section");
    section.className = "posture-block";
    appendText(section, "h3", "", "Limits");
    const facts = document.createElement("dl");
    facts.className = "posture-facts";
    appendFact(facts, "Model", limits?.model || "Inherited default");
    appendFact(
      facts,
      "Max tokens",
      Number.isFinite(limits?.max_tokens) && limits.max_tokens > 0
        ? String(limits.max_tokens)
        : "Provider default",
    );
    appendFact(
      facts,
      "Max iterations",
      Number.isFinite(limits?.max_iterations) && limits.max_iterations > 0
        ? String(limits.max_iterations)
        : "Agent default",
    );
    appendFact(
      facts,
      "Allowed subagents",
      Array.isArray(limits?.allowed_children) && limits.allowed_children.length > 0
        ? limits.allowed_children.join(", ")
        : "Any profile",
    );
    section.append(facts);
    parent.append(section);
  }

  function renderDenials(parent, denials) {
    const section = document.createElement("section");
    section.className = "posture-block";
    appendText(section, "h3", "", "Recent refusals");
    const records = Array.isArray(denials) ? denials : [];
    if (records.length === 0) {
      appendText(
        section,
        "p",
        "posture-empty",
        "No tool call has been refused in this conversation.",
      );
      parent.append(section);
      return;
    }
    for (const denial of records) {
      const card = document.createElement("article");
      card.className = "posture-denial";
      appendText(
        card,
        "p",
        "posture-denial-head",
        `${denial?.verdict || "denied"} · ${denial?.tool || "unknown tool"}`,
      );
      const facts = document.createElement("dl");
      facts.className = "posture-facts";
      appendFact(facts, "Rule", denial?.rule || "No named rule");
      appendFact(facts, "When", denial?.at || "—");
      if (denial?.command) {
        appendFact(facts, "Command", denial.command);
      }
      card.append(facts);
      if (denial?.detail) {
        appendText(card, "p", "posture-guidance", denial.detail);
      }
      section.append(card);
    }
    parent.append(section);
  }

  function render(view, denials) {
    clear(elements.body);
    const name = view?.profile || "default";
    elements.title.textContent = `Posture for ${name}`;
    if (view?.known === false) {
      appendText(
        elements.body,
        "p",
        "posture-error",
        `No profile named ${name} is configured. This is the posture it would inherit.`,
      );
    }
    const group = view?.group;
    if (group) {
      appendText(elements.body, "p", "posture-note", `Runs inside agent group ${group}.`);
    }
    renderSystem(elements.body, view?.system);
    renderPolicy(elements.body, view);
    renderLimits(elements.body, view?.limits);
    renderDenials(elements.body, denials);
  }

  async function open(profile, session, trigger) {
    opener = trigger || null;
    clear(elements.body);
    elements.title.textContent = "Posture";
    elements.status.textContent = "Reading posture…";
    if (!elements.dialog.open) {
      elements.dialog.showModal();
    }
    try {
      const query = profile ? `?profile=${encodeURIComponent(profile)}` : "";
      const view = await get(`/api/v1/desk/posture${query}`);
      let denials = [];
      if (session) {
        const snapshot = await get(
          `/api/v1/desk/posture/denials?session=${encodeURIComponent(session)}`,
        );
        denials = Array.isArray(snapshot?.denials) ? snapshot.denials : [];
      }
      render(view, denials);
      elements.status.textContent = "";
    } catch (error) {
      elements.status.textContent =
        error.safeMessage || "The posture could not be read.";
    }
  }

  // Delegation, so Today's static button and the profile cards Capabilities
  // builds at runtime share one code path.
  document.addEventListener("click", (event) => {
    const trigger = event.target?.closest?.("[data-posture-open]");
    if (!trigger) return;
    event.preventDefault();
    void open(
      trigger.dataset.postureProfile || "",
      trigger.dataset.postureSession || "",
      trigger,
    );
  });

  elements.close?.addEventListener("click", () => {
    elements.dialog.close();
    opener?.focus();
  });
}
