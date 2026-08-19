export function sessionPresentationURL(moduleURL) {
  const presentationURL = new URL("./session-presentation.mjs", moduleURL);
  const version = new URL(moduleURL).searchParams.get("v");
  if (version) {
    presentationURL.searchParams.set("v", version);
  }
  return presentationURL;
}

const presentationURL = sessionPresentationURL(import.meta.url);
const {
  presentSessions,
  sessionAccessibleLabel,
  sessionTitle,
} = await import(presentationURL.href);

const sessionEndpoint = "/api/v1/desk/memory/sessions";

function requestPath(event) {
  return String(
    event?.detail?.requestConfig?.path ||
      event?.detail?.pathInfo?.requestPath ||
      event?.detail?.elt?.getAttribute?.("hx-get") ||
      "",
  );
}

function shortSessionID(value) {
  const id = String(value || "");
  return id.length > 8 ? id.slice(-8) : id;
}

function recencyLabel(value, now = new Date()) {
  const updated = new Date(value);
  if (Number.isNaN(updated.getTime())) return "";
  let age = now.getTime() - updated.getTime();
  if (age < 0) age = 0;
  const hours = Math.floor(age / 3_600_000);
  if (hours < 1) return "updated moments ago";
  if (hours < 24) return `updated ${hours} hours ago`;
  return `updated ${updated.toLocaleDateString(undefined, { day: "numeric", month: "short" })}`;
}

function sessionChoices(field) {
  return [...field.querySelectorAll(".memory-session-choice-data")].map((source) => ({
    id: source.dataset.sessionId || "",
    title: source.dataset.sessionTitle || "",
    label: source.dataset.sessionTitle || "",
    summary: source.dataset.sessionSummary || "",
    model_alias: source.dataset.sessionModelAlias || "",
    updated_at: source.dataset.sessionUpdatedAt || "",
    pinned: source.dataset.sessionPinned === "true",
  })).filter((choice) => choice.id);
}

export function sessionAccessibleLabels(sessions) {
  const values = Array.isArray(sessions) ? sessions : [];
  const labels = values.map(sessionAccessibleLabel);
  const counts = new Map();
  for (const label of labels) {
    counts.set(label, (counts.get(label) || 0) + 1);
  }

  // Keep a unique base label untouched, but reserve every such label before
  // adding ordinal suffixes so a user-authored title cannot collide with one.
  const used = new Set(labels.filter((label) => counts.get(label) === 1));
  const occurrences = new Map();
  return labels.map((label, index) => {
    const total = counts.get(label) || 0;
    if (total === 1) return label;

    const occurrence = (occurrences.get(label) || 0) + 1;
    occurrences.set(label, occurrence);
    let candidate = `${label} · conversation ${occurrence} of ${total}`;
    let attempt = 0;
    while (used.has(candidate)) {
      attempt += 1;
      candidate = `${label} · conversation ${occurrence} of ${total} · picker item ${index + 1}${attempt > 1 ? ` (${attempt})` : ""}`;
    }
    used.add(candidate);
    return candidate;
  });
}

function dispatchSessionChange(input) {
  input.dispatchEvent(new Event("change", { bubbles: true }));
}

function optionControl(document, choice, index, selectedID, select, accessibleLabel) {
  const option = document.createElement("div");
  option.className = "memory-session-option";
  option.id = `memory-session-option-${index}`;
  option.dataset.sessionId = choice.id;
  option.setAttribute("role", "option");
  option.setAttribute("aria-label", accessibleLabel);
  option.setAttribute("aria-selected", choice.id === selectedID ? "true" : "false");

  const primary = document.createElement("span");
  primary.className = "memory-session-option-title";
  primary.textContent = sessionTitle(choice);
  option.append(primary);

  if (choice.summary) {
    const summary = document.createElement("span");
    summary.className = "memory-session-option-summary";
    summary.textContent = choice.summary;
    option.append(summary);
  }

  const metadata = [recencyLabel(choice.updated_at)];
  if (choice.model_alias) metadata.push(`model ${choice.model_alias}`);
  if (choice.pinned) metadata.push("Pinned");
  if (choice.id) metadata.push(shortSessionID(choice.id));
  if (metadata.length > 0) {
    const details = document.createElement("span");
    details.className = "memory-session-option-meta";
    details.textContent = metadata.join(" · ");
    option.append(details);
  }

  option.addEventListener("click", () => select(choice));
  return option;
}

export function initMemorySessionPicker(root = document) {
  const field = root?.querySelector?.("#memory-session-field") || (root?.id === "memory-session-field" ? root : null);
  if (!field || field.dataset.waffleMemoryPickerBound === "true") return false;
  const input = field.querySelector("#memory-session");
  const trigger = field.querySelector("#memory-session-trigger");
  const popover = field.querySelector("#memory-session-popover");
  const query = field.querySelector("#memory-session-query");
  const options = field.querySelector("#memory-session-options");
  const noMatches = field.querySelector("#memory-session-no-matches");
  const clear = field.querySelector("#memory-session-clear");
  const choices = sessionChoices(field);
  if (!input || !trigger || !popover || !query || !options || choices.length === 0) return false;

  field.dataset.waffleMemoryPickerBound = "true";
  const accessibleLabels = sessionAccessibleLabels(choices);
  const accessibleLabelByChoice = new Map(choices.map((choice, index) => [choice, accessibleLabels[index]]));
  let visibleChoices = [];
  let activeIndex = -1;

  function updateTrigger(choice) {
    if (choice) {
      trigger.textContent = sessionTitle(choice);
      trigger.setAttribute("aria-label", accessibleLabelByChoice.get(choice));
      clear.hidden = false;
    } else {
      trigger.textContent = "Select a conversation…";
      trigger.removeAttribute("aria-label");
      clear.hidden = true;
    }
  }

  function selectedChoice() {
    return choices.find((choice) => choice.id === input.value) || null;
  }

  function render({ scrollSelected = false } = {}) {
    const groups = presentSessions(choices, { query: query.value });
    options.replaceChildren();
    visibleChoices = [];
    let optionIndex = 0;
    let groupIndex = 0;
    for (const group of groups) {
      const wrapper = document.createElement("div");
      wrapper.className = "memory-session-group";
      wrapper.setAttribute("role", "group");
      const heading = document.createElement("h3");
      heading.className = "memory-session-group-label";
      heading.id = `memory-session-group-${groupIndex}`;
      wrapper.setAttribute("aria-labelledby", heading.id);
      heading.textContent = group.label;
      wrapper.append(heading);
      for (const choice of group.items) {
        visibleChoices.push(choice);
        wrapper.append(optionControl(document, choice, optionIndex, input.value, select, accessibleLabelByChoice.get(choice)));
        optionIndex += 1;
      }
      options.append(wrapper);
      groupIndex += 1;
    }
    noMatches.hidden = visibleChoices.length > 0;
    if (visibleChoices.length === 0) {
      activeIndex = -1;
      query.setAttribute("aria-activedescendant", "");
      return;
    }
    const selectedIndex = visibleChoices.findIndex((choice) => choice.id === input.value);
    if (activeIndex < 0 || activeIndex >= visibleChoices.length) {
      activeIndex = selectedIndex >= 0 ? selectedIndex : 0;
    }
    updateActive();
    if (scrollSelected && selectedIndex >= 0) {
      options.querySelector(`#memory-session-option-${selectedIndex}`)?.scrollIntoView?.({ block: "nearest" });
    }
  }

  function updateActive({ scrollIntoView = false } = {}) {
    const active = options.querySelector(`#memory-session-option-${activeIndex}`);
    for (const option of options.querySelectorAll('[role="option"]')) {
      if (option === active) option.setAttribute("data-active", "true");
      else option.removeAttribute("data-active");
    }
    query.setAttribute("aria-activedescendant", active?.id || "");
    if (scrollIntoView) active?.scrollIntoView?.({ block: "nearest" });
  }

  function moveActive(offset) {
    if (visibleChoices.length === 0) return;
    activeIndex = (activeIndex + offset + visibleChoices.length) % visibleChoices.length;
    updateActive({ scrollIntoView: true });
  }

  function close() {
    popover.hidden = true;
    trigger.setAttribute("aria-expanded", "false");
    query.setAttribute("aria-expanded", "false");
    query.value = "";
    activeIndex = -1;
    query.setAttribute("aria-activedescendant", "");
    trigger.focus();
  }

  function keepPickerClearOfNavigation() {
    if (!window.matchMedia?.("(max-width: 768px)").matches) return;
    field.scrollIntoView({ block: "start", inline: "nearest", behavior: "instant" });
    const navigation = document.querySelector(".desk-navigation")?.getBoundingClientRect();
    const panel = popover.getBoundingClientRect();
    const scrollOwner = field.closest("main");
    if (!navigation || !scrollOwner || panel.bottom <= navigation.top) return;
    scrollOwner.scrollTop += panel.bottom - navigation.top + 8;
  }

  function select(choice) {
    input.value = choice.id;
    document.body.dataset.waffleMemorySessionSelection = choice.id;
    updateTrigger(choice);
    close();
    dispatchSessionChange(input);
  }

  function open() {
    popover.hidden = false;
    trigger.setAttribute("aria-expanded", "true");
    query.setAttribute("aria-expanded", "true");
    query.value = "";
    activeIndex = -1;
    render({ scrollSelected: true });
    query.focus();
    keepPickerClearOfNavigation();
  }

  trigger.addEventListener("click", () => {
    if (popover.hidden) open();
    else close();
  });
  trigger.addEventListener("keydown", (event) => {
    if (event.key !== "Enter" && event.key !== " ") return;
    event.preventDefault();
    open();
  });
  query.addEventListener("input", () => {
    activeIndex = -1;
    render();
  });
  query.addEventListener("keydown", (event) => {
    switch (event.key) {
      case "ArrowDown":
        event.preventDefault();
        moveActive(1);
        break;
      case "ArrowUp":
        event.preventDefault();
        moveActive(-1);
        break;
      case "Home":
        event.preventDefault();
        activeIndex = visibleChoices.length > 0 ? 0 : -1;
        updateActive({ scrollIntoView: true });
        break;
      case "End":
        event.preventDefault();
        activeIndex = visibleChoices.length > 0 ? visibleChoices.length - 1 : -1;
        updateActive({ scrollIntoView: true });
        break;
      case "Enter":
        event.preventDefault();
        if (visibleChoices[activeIndex]) select(visibleChoices[activeIndex]);
        break;
      case "Escape":
        event.preventDefault();
        event.stopPropagation();
        close();
        break;
      default:
        break;
    }
  });
  clear.addEventListener("click", () => {
    input.value = "";
    delete document.body.dataset.waffleMemorySessionSelection;
    updateTrigger(null);
    dispatchSessionChange(input);
    close();
  });
  field.addEventListener("keydown", (event) => {
    if (event.key !== "Escape" || popover.hidden) return;
    event.preventDefault();
    event.stopPropagation();
    close();
  });

  const restored = document.body.dataset.waffleMemorySessionSelection || "";
  const choice = choices.find((candidate) => candidate.id === restored);
  if (choice && !input.value) {
    input.value = choice.id;
    updateTrigger(choice);
    dispatchSessionChange(input);
  } else {
    updateTrigger(selectedChoice());
  }
  delete document.body.dataset.waffleMemorySessionSelection;
  render();
  return true;
}

if (typeof document !== "undefined" && document.body) {
  document.body.addEventListener("htmx:beforeRequest", (event) => {
    if (!requestPath(event).includes(sessionEndpoint)) return;
    const input = document.querySelector("#memory-session");
    if (input?.value) document.body.dataset.waffleMemorySessionSelection = input.value;
    else delete document.body.dataset.waffleMemorySessionSelection;
  });
  document.body.addEventListener("htmx:afterSettle", () => {
    initMemorySessionPicker(document);
  });
  initMemorySessionPicker(document);
}
