const root = document.querySelector(".workspaces");

if (root) {
  const elements = {
    list: document.querySelector("#workspaces-list"),
    errors: document.querySelector("#workspaces-errors"),
    empty: document.querySelector("#workspaces-empty"),
    openButton: document.querySelector("#workspace-open-button"),
    openDialog: document.querySelector("#workspace-open-dialog"),
    openForm: document.querySelector("#workspace-open-form"),
    repository: document.querySelector("#workspace-repository"),
    profile: document.querySelector("#workspace-profile"),
    openCancel: document.querySelector("#workspace-open-cancel"),
    openStatus: document.querySelector("#workspace-open-status"),
    openPrerequisite: document.querySelector("#workspace-open-prerequisite"),
    closeDialog: document.querySelector("#workspace-close-dialog"),
    closeTitle: document.querySelector("#workspace-close-title"),
    closeDirty: document.querySelector("#workspace-close-dirty"),
    closeUnpushed: document.querySelector("#workspace-close-unpushed"),
    closeStatus: document.querySelector("#workspace-close-status"),
    closeCancel: document.querySelector("#workspace-close-cancel"),
    closeConfirm: document.querySelector("#workspace-close-confirm"),
  };

  const state = {
    requestToken: document.body.dataset.requestToken || "",
    workspaces: [],
    close: null,
    // Assume git auth is configured until Desk says otherwise: a failed
    // readiness read must not invent a prerequisite that does not exist.
    githubReady: true,
    githubGuidance: "",
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
          : "The workspace request could not be completed.";
      error.payload = payload;
      throw error;
    }
    return payload;
  }

  async function read(path) {
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

  function appendFact(parent, label, value) {
    const row = document.createElement("div");
    appendText(row, "dt", "", label);
    appendText(row, "dd", "", value);
    parent.append(row);
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
        showError(error.safeMessage || "The workspace action could not be completed.");
      } finally {
        button.disabled = false;
      }
    });
    return button;
  }

  function pluralFiles(count) {
    return count === 1 ? "1 uncommitted file" : `${count} uncommitted files`;
  }

  // renderGit projects the read-only git status returned by Desk. It never
  // triggers a lifecycle change: an idle or closed workspace simply reports
  // why its state is unavailable.
  function renderGit(container, git) {
    container.replaceChildren();
    if (!git || git.available !== true) {
      appendText(
        container,
        "p",
        "workspace-git-unavailable",
        (typeof git?.reason === "string" && git.reason) ||
          "Git status is unavailable for this workspace.",
      );
      return;
    }
    const facts = document.createElement("dl");
    facts.className = "workspace-facts workspace-git-facts";
    appendFact(
      facts,
      "Branch",
      git.detached === true ? "Detached HEAD" : git.branch || "Unknown branch",
    );
    const dirtyFiles = Number.isFinite(git.dirty_files) ? git.dirty_files : 0;
    appendFact(facts, "Working tree", git.dirty === true ? pluralFiles(dirtyFiles) : "Clean");
    appendFact(
      facts,
      "Tracking",
      git.tracking === true
        ? `${Number(git.ahead) || 0} ahead · ${Number(git.behind) || 0} behind`
        : "No upstream branch",
    );
    appendFact(
      facts,
      "Last commit",
      git.commit ? `${git.commit} ${git.subject || ""}`.trim() : "No commits yet",
    );
    container.append(facts);
  }

  async function loadGitStatus(workspace, container) {
    container.replaceChildren();
    appendText(container, "p", "workspace-git-pending", "Reading git status…");
    try {
      renderGit(
        container,
        await read(`/api/v1/desk/workspaces/${encodeURIComponent(workspace.id)}/git`),
      );
    } catch (error) {
      renderGit(container, {
        available: false,
        reason: error.safeMessage || "Git status could not be read.",
      });
    }
  }

  function renderWorkspace(workspace) {
    const card = document.createElement("article");
    card.className = "workspace-card";
    card.dataset.workspaceId = workspace.id || "";
    card.dataset.status = workspace.status || "";

    const header = document.createElement("header");
    appendText(header, "p", "workspace-status", workspace.status || "unknown");
    appendText(header, "h2", "", workspace.repository || "Unknown repository");
    card.append(header);

    const facts = document.createElement("dl");
    facts.className = "workspace-facts";
    appendFact(facts, "Session", workspace.session || "No persisted session");
    appendFact(facts, "Profile", workspace.profile || "default");
    appendFact(facts, "Image", workspace.image || "default");
    appendFact(facts, "Network", workspace.egress || "No network egress");
    card.append(facts);

    let git = null;
    if (workspace.status !== "closed") {
      git = document.createElement("section");
      git.className = "workspace-git";
      git.dataset.workspaceGit = workspace.id || "";
      card.append(git);
    }

    const actions = document.createElement("div");
    actions.className = "workspace-actions";
    if (workspace.status === "open" || workspace.status === "idle") {
      actions.append(actionButton("Open at Desk", "select", async () => {
        const selected = await mutate(
          `/api/v1/desk/workspaces/${encodeURIComponent(workspace.id)}/select`,
          {},
        );
        if (typeof selected.today_url !== "string" || selected.today_url === "") {
          throw new Error("missing_today_url");
        }
        window.location.assign(selected.today_url);
      }));
    }
    if (workspace.status === "open") {
      actions.append(actionButton("Idle", "idle", async () => {
        await mutate(
          `/api/v1/desk/workspaces/${encodeURIComponent(workspace.id)}/idle`,
          {},
        );
        await loadWorkspaces();
      }));
    }
    if (workspace.status === "idle") {
      actions.append(actionButton("Resume", "resume", async () => {
        await mutate(
          `/api/v1/desk/workspaces/${encodeURIComponent(workspace.id)}/resume`,
          {},
        );
        await loadWorkspaces();
      }));
    }
    if (git) {
      actions.append(actionButton("Refresh git", "git-refresh", async () => {
        await loadGitStatus(workspace, git);
      }));
    }
    if (workspace.status !== "closed") {
      actions.append(actionButton("Review close", "close-preview", async (opener) => {
        await previewClose(workspace, opener);
      }));
    }
    if (actions.children.length > 0) {
      card.append(actions);
    }
    return { card, git, workspace };
  }

  function render(snapshot) {
    state.workspaces = Array.isArray(snapshot.workspaces) ? snapshot.workspaces : [];
    const entries = state.workspaces.map(renderWorkspace);
    elements.list.replaceChildren(...entries.map((entry) => entry.card));
    elements.empty.hidden = state.workspaces.length !== 0;
    elements.empty.textContent =
      state.workspaces.length === 0 ? "No workspaces are open or retained." : "";
    return Promise.all(
      entries
        .filter((entry) => entry.git)
        .map((entry) => loadGitStatus(entry.workspace, entry.git)),
    );
  }

  async function loadWorkspaces() {
    elements.empty.hidden = false;
    elements.empty.textContent = "Loading workspaces…";
    try {
      const response = await fetch("/api/v1/desk/workspaces", {
        method: "GET",
        credentials: "same-origin",
        cache: "no-store",
        headers: { Accept: "application/json" },
      });
      await render(await readJSON(response));
      clearError();
    } catch (error) {
      showError(error.safeMessage || "Workspaces could not be loaded.");
      elements.empty.hidden = true;
    }
  }

  // loadGitHubReadiness names the git-auth prerequisite before the user
  // submits the open dialog, instead of letting the open attempt fail with a
  // generic message (#182).
  async function loadGitHubReadiness() {
    try {
      const records = await read("/api/v1/desk/connections");
      const github = Array.isArray(records)
        ? records.find((record) => record?.kind === "github")
        : null;
      state.githubReady = !github || github.status !== "unconfigured";
      state.githubGuidance =
        typeof github?.guidance === "string" ? github.guidance : "";
    } catch {
      state.githubReady = true;
      state.githubGuidance = "";
    }
    renderGitHubPrerequisite();
  }

  function renderGitHubPrerequisite() {
    if (!elements.openPrerequisite) {
      return;
    }
    if (state.githubReady) {
      elements.openPrerequisite.hidden = true;
      elements.openPrerequisite.textContent = "";
      return;
    }
    elements.openPrerequisite.hidden = false;
    elements.openPrerequisite.textContent = [
      "GitHub App credentials are not configured, so Waffle cannot authenticate git for a new workspace.",
      state.githubGuidance,
    ]
      .filter(Boolean)
      .join(" ");
  }

  function showError(message) {
    elements.errors.hidden = false;
    elements.errors.textContent = message;
  }

  function clearError() {
    elements.errors.hidden = true;
    elements.errors.textContent = "";
  }

  async function openWorkspace(event) {
    event.preventDefault();
    const submit = event.currentTarget.querySelector?.('button[type="submit"]');
    if (submit) submit.disabled = true;
    elements.openStatus.textContent = "Opening guarded workspace…";
    try {
      await mutate("/api/v1/desk/workspaces/open", {
        repository: elements.repository.value,
        profile: elements.profile.value,
      });
      elements.openStatus.textContent = "";
      elements.openDialog.close();
      elements.repository.value = "";
      elements.profile.value = "";
      await loadWorkspaces();
    } catch (error) {
      elements.openStatus.textContent =
        error.safeMessage || "The repository could not be opened.";
    } finally {
      if (submit) submit.disabled = false;
    }
  }

  async function previewClose(workspace, opener) {
    state.close = null;
    elements.closeTitle.textContent = `Review close for ${workspace.repository}`;
    elements.closeDirty.textContent = "Checking…";
    elements.closeUnpushed.textContent = "Checking…";
    elements.closeStatus.textContent = "Inspecting the repository without changing it…";
    elements.closeConfirm.disabled = true;
    const preview = await mutate(
      `/api/v1/desk/workspaces/${encodeURIComponent(workspace.id)}/close-preview`,
      {},
    );
    state.close = {
      id: workspace.id,
      token: preview.preview_token,
      eligible: preview.eligible === true,
      opener,
    };
    elements.closeDirty.textContent = preview.dirty || "Clean";
    elements.closeUnpushed.textContent = preview.unpushed || "None";
    elements.closeConfirm.disabled = !state.close.eligible;
    elements.closeStatus.textContent = state.close.eligible
      ? "Clean now. Confirmation performs one fresh inspection before removal."
      : "Close is unavailable while unsaved or unpushed work remains.";
    elements.closeDialog.showModal();
  }

  async function confirmClose() {
    if (!state.close?.eligible || !state.close.token) {
      return;
    }
    elements.closeConfirm.disabled = true;
    elements.closeStatus.textContent = "Inspecting again and closing…";
    try {
      await mutate(
        `/api/v1/desk/workspaces/${encodeURIComponent(state.close.id)}/close`,
        { preview_token: state.close.token },
      );
      state.close = null;
      elements.closeDialog.close();
      await loadWorkspaces();
    } catch (error) {
      elements.closeStatus.textContent =
        error.safeMessage || "The workspace was not closed.";
      if (error.payload?.dirty) {
        elements.closeDirty.textContent = error.payload.dirty;
      }
      if (error.payload?.unpushed) {
        elements.closeUnpushed.textContent = error.payload.unpushed;
      }
    }
  }

  elements.openButton.addEventListener("click", () => {
    elements.openStatus.textContent = "";
    elements.openDialog.showModal();
    elements.repository.focus();
  });
  elements.openCancel.addEventListener("click", () => {
    elements.openDialog.close();
    elements.openButton.focus();
  });
  elements.openForm.addEventListener("submit", openWorkspace);
  elements.closeCancel.addEventListener("click", () => {
    const opener = state.close?.opener;
    state.close = null;
    elements.closeDialog.close();
    opener?.focus();
  });
  elements.closeConfirm.addEventListener("click", confirmClose);
  void loadWorkspaces();
  void loadGitHubReadiness();
}
