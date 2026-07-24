const root = document.querySelector(".memory");

if (root) {
  const elements = {
    form: document.querySelector("#memory-search-form"),
    query: document.querySelector("#memory-query"),
    session: document.querySelector("#memory-session-id"),
    results: document.querySelector("#memory-results"),
    status: document.querySelector("#memory-status"),
    attachStatus: document.querySelector("#memory-attach-status"),
    dialog: document.querySelector("#memory-forget-dialog"),
    forgetNote: document.querySelector("#memory-forget-note"),
    forgetScope: document.querySelector("#memory-forget-scope"),
    forgetExclusions: document.querySelector("#memory-forget-exclusions"),
    forgetCancel: document.querySelector("#memory-forget-cancel"),
    forgetConfirm: document.querySelector("#memory-forget-confirm"),
  };

  const state = {
    query: "",
    hits: [],
    debounce: 0,
    pendingForget: null,
    requestToken: document.body.dataset.requestToken || "",
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
          : "The Memory request could not be completed.";
      throw error;
    }
    return payload;
  }

  async function postJSON(path, body) {
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

  function appendText(parent, tag, className, text) {
    const element = document.createElement(tag);
    element.className = className;
    element.textContent = text;
    parent.append(element);
    return element;
  }

  async function attachHit(hit, button) {
    const sessionID = elements.session.value.trim();
    if (!sessionID) {
      elements.attachStatus.textContent = "Enter the persisted session ID first.";
      elements.session.focus();
      return;
    }
    button.disabled = true;
    elements.attachStatus.textContent = "Attaching bounded reference…";
    try {
      await postJSON("/api/v1/desk/memory/attach", {
        session_id: sessionID,
        query: state.query,
        source: hit.source,
        source_id: hit.source_id,
      });
      elements.attachStatus.textContent = "Memory reference attached to the session.";
    } catch (error) {
      elements.attachStatus.textContent =
        error.safeMessage || "The memory reference could not be attached.";
    } finally {
      button.disabled = false;
    }
  }

  async function previewForget(hit, button) {
    button.disabled = true;
    elements.status.textContent = "Preparing exact note preview…";
    try {
      const preview = await postJSON(
        `/api/v1/desk/memory/${encodeURIComponent(hit.source_id)}/forget-preview`,
        { query: state.query },
      );
      state.pendingForget = preview;
      elements.forgetNote.textContent = preview.note?.excerpt || "Selected note";
      elements.forgetScope.textContent = preview.scope || "";
      const exclusions = Array.isArray(preview.excludes) ? preview.excludes : [];
      elements.forgetExclusions.replaceChildren(
        ...exclusions.map((value) => {
          const item = document.createElement("li");
          item.textContent = `Does not erase ${value}.`;
          return item;
        }),
      );
      elements.status.textContent = "";
      elements.dialog.showModal();
    } catch (error) {
      elements.status.textContent =
        error.safeMessage || "The forget preview could not be prepared.";
    } finally {
      button.disabled = false;
    }
  }

  function renderHit(hit) {
    const card = document.createElement("article");
    card.className = `memory-hit${hit.archived ? " is-archived" : ""}`;

    const metadata = document.createElement("div");
    metadata.className = "memory-hit-metadata";
    appendText(metadata, "span", "memory-source-chip", hit.source || "source");
    appendText(metadata, "span", "memory-source-id", hit.source_id || "");
    if (hit.archived) {
      appendText(metadata, "span", "memory-archive-chip", "Archived");
    }
    card.append(metadata);

    appendText(card, "p", "memory-excerpt", hit.excerpt || "");
    appendText(card, "p", "memory-provenance", hit.provenance || "Attributed source");

    const actions = document.createElement("div");
    actions.className = "memory-hit-actions";
    const attach = document.createElement("button");
    attach.type = "button";
    attach.textContent = "Attach to session";
    attach.addEventListener("click", () => attachHit(hit, attach));
    actions.append(attach);
    if (hit.source === "note" && !hit.archived) {
      const forget = document.createElement("button");
      forget.type = "button";
      forget.className = "memory-forget-button";
      forget.textContent = "Forget…";
      forget.addEventListener("click", () => previewForget(hit, forget));
      actions.append(forget);
    }
    card.append(actions);
    return card;
  }

  function renderHits(hits) {
    state.hits = Array.isArray(hits) ? hits : [];
    elements.results.replaceChildren(...state.hits.map(renderHit));
    elements.status.textContent =
      state.hits.length === 0
        ? "No attributed memory matched that search."
        : `${state.hits.length} attributed result${state.hits.length === 1 ? "" : "s"}.`;
  }

  async function loadMemory() {
    const query = elements.query.value.trim();
    state.query = query;
    if (!query) {
      state.hits = [];
      elements.results.replaceChildren();
      elements.status.textContent = "Enter a search to begin.";
      return;
    }
    elements.status.textContent = "Searching attributed memory…";
    try {
      const response = await fetch(
        `/api/v1/desk/memory?query=${encodeURIComponent(query)}`,
        {
          method: "GET",
          credentials: "same-origin",
          cache: "no-store",
          headers: { Accept: "application/json" },
        },
      );
      const payload = await readJSON(response);
      renderHits(payload.hits);
    } catch (error) {
      elements.results.replaceChildren();
      elements.status.textContent =
        error.safeMessage || "Attributed memory could not be searched.";
    }
  }

  elements.form.addEventListener("submit", (event) => {
    event.preventDefault();
    clearTimeout(state.debounce);
    void loadMemory();
  });
  elements.query.addEventListener("input", () => {
    clearTimeout(state.debounce);
    state.debounce = setTimeout(() => void loadMemory(), 250);
  });
  elements.forgetCancel.addEventListener("click", () => {
    state.pendingForget = null;
    elements.dialog.close();
  });
  elements.forgetConfirm.addEventListener("click", async () => {
    const preview = state.pendingForget;
    if (!preview?.note?.source_id || !preview.preview_token) {
      elements.dialog.close();
      elements.status.textContent = "The forget confirmation expired. Request a new preview.";
      return;
    }
    elements.forgetConfirm.disabled = true;
    try {
      await postJSON(
        `/api/v1/desk/memory/${encodeURIComponent(preview.note.source_id)}/forget`,
        { preview_token: preview.preview_token },
      );
      state.pendingForget = null;
      elements.dialog.close();
      elements.status.textContent = "Waffle-owned note archived.";
      await loadMemory();
    } catch (error) {
      elements.dialog.close();
      elements.status.textContent =
        error.safeMessage || "The note could not be forgotten.";
    } finally {
      elements.forgetConfirm.disabled = false;
    }
  });
}
