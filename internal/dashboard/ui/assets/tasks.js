const root = document.querySelector(".tasks");

if (root) {
  const elements = {
    attention: document.querySelector("#tasks-attention-count"),
    list: document.querySelector("#tasks-list"),
    errors: document.querySelector("#tasks-errors"),
    empty: document.querySelector("#tasks-empty"),
    filters: [...document.querySelectorAll("[data-task-filter]")],
    form: document.querySelector("#task-schedule-form"),
    id: document.querySelector("#task-schedule-id"),
    name: document.querySelector("#task-schedule-name"),
    cron: document.querySelector("#task-schedule-cron"),
    prompt: document.querySelector("#task-schedule-prompt"),
    deliver: document.querySelector("#task-schedule-deliver"),
    profile: document.querySelector("#task-schedule-profile"),
    enabled: document.querySelector("#task-schedule-enabled"),
    enabledRow: document.querySelector("#task-schedule-enabled-row"),
    cancel: document.querySelector("#task-schedule-cancel"),
    submit: document.querySelector("#task-schedule-submit"),
    status: document.querySelector("#task-schedule-status"),
  };

  const state = {
    filter: "all",
    tasks: [],
    requestToken: document.body.dataset.requestToken || "",
    pendingIntent: null,
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
          : "The Tasks request could not be completed.";
      throw error;
    }
    return payload;
  }

  function durationLabel(task) {
    const milliseconds = task.elapsed_ms || task.runtime_ms || 0;
    if (milliseconds < 1000) {
      return `${milliseconds} ms`;
    }
    return `${(milliseconds / 1000).toFixed(milliseconds < 10000 ? 1 : 0)} s`;
  }

  function appendText(parent, tag, className, text) {
    const element = document.createElement(tag);
    element.className = className;
    element.textContent = text;
    parent.append(element);
    return element;
  }

  function taskTitle(task) {
    if (task.name) {
      return task.name;
    }
    if (task.kind === "active") {
      return "Active scheduled run";
    }
    return "Completed scheduled run";
  }

  function renderTask(task) {
    const card = document.createElement("article");
    card.className = `task-card${task.attention ? " is-attention" : ""}`;
    card.dataset.taskId = task.id || "";
    card.dataset.taskKind = task.kind || "";

    const header = document.createElement("header");
    appendText(header, "p", "task-card-kind", `${task.kind || "task"} / ${task.source || "unknown"}`);
    appendText(header, "h3", "", taskTitle(task));
    card.append(header);

    const facts = document.createElement("dl");
    facts.className = "task-facts";
    const factValues = [
      ["State", task.phase || task.outcome || (task.enabled ? "scheduled" : "disabled")],
      ["Profile", task.profile || "default"],
      ["Runtime", durationLabel(task)],
      ["Usage", `${task.usage?.input_tokens || 0} in / ${task.usage?.output_tokens || 0} out`],
    ];
    for (const [label, value] of factValues) {
      const row = document.createElement("div");
      appendText(row, "dt", "", label);
      appendText(row, "dd", "", value);
      facts.append(row);
    }
    card.append(facts);

    const evidence = document.createElement("details");
    evidence.className = "task-evidence";
    appendText(evidence, "summary", "", task.evidence_label || "Evidence");
    appendText(
      evidence,
      "p",
      "",
      task.retry?.status ||
        (task.session ? `Persisted session: ${task.session}` : "No persisted session is attached."),
    );
    card.append(evidence);

    const actions = document.createElement("div");
    actions.className = "task-card-actions";
    if (task.open_at_desk && task.session) {
      const open = document.createElement("a");
      open.dataset.action = "open-at-desk";
      open.setAttribute("href", `/desk/?section=today&session_id=${encodeURIComponent(task.session)}`);
      open.textContent = "Open at Desk";
      actions.append(open);
    }
    if (task.kind === "schedule") {
      const edit = document.createElement("button");
      edit.type = "button";
      edit.dataset.action = "edit-schedule";
      edit.textContent = "Edit schedule";
      edit.addEventListener("click", () => beginEdit(task));
      actions.append(edit);
    }
    if (actions.children.length > 0) {
      card.append(actions);
    }
    return card;
  }

  function render(snapshot) {
    state.tasks = Array.isArray(snapshot.tasks) ? snapshot.tasks : [];
    elements.attention.textContent =
      snapshot.attention_count === 1
        ? "1 needs attention"
        : `${snapshot.attention_count || 0} need attention`;
    const partialErrors = Array.isArray(snapshot.errors) ? snapshot.errors : [];
    elements.errors.hidden = partialErrors.length === 0;
    elements.errors.textContent =
      partialErrors.length === 0
        ? ""
        : "Some task evidence is temporarily unavailable.";
    elements.list.replaceChildren(...state.tasks.map(renderTask));
    elements.empty.hidden = state.tasks.length !== 0;
    elements.empty.textContent =
      state.tasks.length === 0
        ? "No tasks match this view."
        : "";
    for (const button of elements.filters) {
      button.setAttribute("aria-pressed", String(button.dataset.taskFilter === state.filter));
    }
  }

  async function loadTasks() {
    elements.empty.hidden = false;
    elements.empty.textContent = "Loading task evidence…";
    try {
      const response = await fetch(
        `/api/v1/desk/tasks?filter=${encodeURIComponent(state.filter)}`,
        {
          method: "GET",
          credentials: "same-origin",
          cache: "no-store",
          headers: { Accept: "application/json" },
        },
      );
      render(await readJSON(response));
    } catch (error) {
      elements.errors.hidden = false;
      elements.errors.textContent =
        error.safeMessage || "Task evidence could not be loaded.";
      elements.empty.hidden = true;
    }
  }

  function resetEditor() {
    elements.id.value = "";
    elements.name.value = "";
    elements.cron.value = "";
    elements.prompt.value = "";
    elements.deliver.value = "";
    elements.profile.value = "";
    elements.enabled.checked = true;
    elements.enabledRow.hidden = true;
    elements.cancel.hidden = true;
    elements.submit.textContent = "Create schedule";
    state.pendingIntent = null;
  }

  function beginEdit(task) {
    elements.id.value = task.id || "";
    elements.name.value = task.name || "";
    elements.cron.value = task.cron || "";
    elements.prompt.value = task.prompt || "";
    elements.deliver.value = task.deliver || "";
    elements.profile.value = task.profile || "";
    elements.enabled.checked = Boolean(task.enabled);
    elements.enabledRow.hidden = false;
    elements.cancel.hidden = false;
    elements.submit.textContent = "Save schedule";
    elements.name.focus?.();
  }

  function mutationIntent(path, body) {
    const serialized = JSON.stringify(body);
    if (
      state.pendingIntent &&
      state.pendingIntent.path === path &&
      state.pendingIntent.serialized === serialized
    ) {
      return state.pendingIntent;
    }
    state.pendingIntent = {
      path,
      serialized,
      key: crypto.randomUUID(),
    };
    return state.pendingIntent;
  }

  async function saveSchedule(event) {
    event.preventDefault();
    const editing = elements.id.value !== "";
    const body = {
      name: elements.name.value,
      cron: elements.cron.value,
      prompt: elements.prompt.value,
      deliver: elements.deliver.value,
      profile: elements.profile.value,
    };
    if (editing) {
      body.enabled = elements.enabled.checked;
    }
    const path = editing
      ? `/api/v1/desk/tasks/schedules/${encodeURIComponent(elements.id.value)}`
      : "/api/v1/desk/tasks/schedules";
    const intent = mutationIntent(path, body);
    elements.submit.disabled = true;
    elements.status.textContent = editing ? "Saving schedule…" : "Creating schedule…";
    try {
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
      await readJSON(response);
      elements.status.textContent = editing ? "Schedule saved." : "Schedule created.";
      resetEditor();
      await loadTasks();
    } catch (error) {
      elements.status.textContent =
        error.safeMessage || "The schedule could not be saved.";
    } finally {
      elements.submit.disabled = false;
    }
  }

  for (const button of elements.filters) {
    button.addEventListener("click", async () => {
      state.filter = button.dataset.taskFilter || "all";
      await loadTasks();
    });
  }
  elements.form.addEventListener("submit", saveSchedule);
  elements.cancel.addEventListener("click", resetEditor);
  void loadTasks();
}
