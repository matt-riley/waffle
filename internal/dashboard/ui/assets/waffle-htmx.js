// Waffle's small external htmx policy/request bridge.
// It deliberately uses external htmx events rather than inline handlers.
(() => {
  "use strict";

  if (typeof htmx === "undefined") {
    return;
  }

	htmx.config.allowEval = false;
	htmx.config.allowScriptTags = false;
	htmx.config.includeIndicatorStyles = false;
	// Server-rendered safe error fragments must reach the declared target while
	// retaining their original HTTP status for JSON clients and observability.
	htmx.config.responseHandling = [
		{ code: "204", swap: false },
		{ code: "[23]..", swap: true },
		{ code: "[45]..", swap: true, error: true },
	];

	const intents = new Map();
	const inFlight = new Set();
	let processGeneration = "";
	let restartPolling = false;
	let restartPollStartedAt = 0;
	let restartPollTimer = 0;
	let restartLocked = false;
	const restartPollInterval = 1000;
	const restartPollTimeout = 120000;

  function uuid() {
    if (globalThis.crypto && typeof globalThis.crypto.randomUUID === "function") {
      return globalThis.crypto.randomUUID();
    }
    return `${Date.now().toString(16)}-${Math.random().toString(16).slice(2)}`;
  }

  function objectFromParameters(parameters) {
    const output = {};
    if (!parameters) return output;
    const entries = parameters instanceof URLSearchParams
      ? parameters.entries()
      : Object.entries(parameters);
    for (const [name, raw] of entries) {
      const values = Array.isArray(raw) ? raw : [raw];
      const value = values.length === 1 ? values[0] : values;
      if (name in output) {
        output[name] = Array.isArray(output[name])
          ? [...output[name], value]
          : [output[name], value];
      } else {
        output[name] = value;
      }
    }
    return output;
  }

	function normalize(value, name) {
    if (name === "max_tokens" || name === "enabled") {
      if (name === "enabled") return value === true || value === "true" || value === "on";
      const number = Number(value);
      return Number.isFinite(number) ? number : 0;
    }
		return value;
	}

	function input(form, id) {
		return form?.querySelector?.(`#${id}`) || null;
	}

	function taskScheduleBody(form, body, detail) {
		const idControl = input(form, "task-schedule-id");
		const id = idControl?.value?.trim?.() || "";
		delete body.id;
		if (!id) {
			delete body.enabled;
			delete body.field_intents;
			return;
		}

		body.enabled = Boolean(input(form, "task-schedule-enabled")?.checked);
		const redacted = (form.dataset.waffleRedactedFields || "")
			.split(",")
			.map((field) => field.trim())
			.filter(Boolean);
		const intentsForFields = {};
		for (const field of redacted) {
			const clear = input(form, `task-schedule-${field}-clear`);
			const fieldInput = input(form, `task-schedule-${field}`);
			if (clear?.checked) {
				intentsForFields[field] = { action: "clear" };
			} else if (fieldInput?.value) {
				intentsForFields[field] = { action: "replace", value: fieldInput.value };
			} else {
				intentsForFields[field] = { action: "preserve" };
			}
		}
		if (Object.keys(intentsForFields).length > 0) {
			body.field_intents = intentsForFields;
		} else {
			delete body.field_intents;
		}
		const path = `/api/v1/desk/tasks/schedules/${encodeURIComponent(id)}`;
		detail.path = path;
		if (detail.requestConfig) detail.requestConfig.path = path;
	}

	function resetTaskSchedule(form) {
		const id = input(form, "task-schedule-id");
		const enabled = input(form, "task-schedule-enabled");
		if (id) {
			id.value = "";
			id.disabled = true;
		}
		if (enabled) {
			enabled.checked = true;
			enabled.disabled = true;
		}
		for (const field of ["name", "cron", "prompt", "deliver", "profile"]) {
			const control = input(form, `task-schedule-${field}`);
			if (control) {
				control.value = "";
				control.required = field === "name" || field === "cron" || field === "prompt";
			}
		}
		for (const field of ["deliver", "profile"]) {
			const clear = input(form, `task-schedule-${field}-clear`);
			const row = input(form, `task-schedule-${field}-clear-row`);
			if (clear) clear.checked = false;
			if (row) row.hidden = true;
		}
		const enabledRow = input(form, "task-schedule-enabled-row");
		const cancel = input(form, "task-schedule-cancel");
		const submit = input(form, "task-schedule-submit");
		if (enabledRow) enabledRow.hidden = true;
		if (cancel) {
			cancel.hidden = false;
			cancel.textContent = "Cancel";
		}
		if (submit) submit.textContent = "Create schedule";
		delete form.dataset.waffleRedactedFields;
	}

	function openTaskScheduleDialog() {
		document.querySelector("#task-schedule-dialog")?.showModal?.();
	}

	function closeTaskScheduleDialog() {
		document.querySelector("#task-schedule-dialog")?.close?.();
	}

	function beginTaskEdit(button) {
		const card = button.closest?.("[data-task-id]");
		const form = document.querySelector("#task-schedule-form");
		if (!card || !form) return;
		const id = input(form, "task-schedule-id");
		const enabled = input(form, "task-schedule-enabled");
		if (!id || !enabled) return;
		id.value = card.dataset.taskId || "";
		id.disabled = false;
		enabled.checked = card.dataset.taskEnabled === "true";
		enabled.disabled = false;
		const redacted = (card.dataset.taskRedactedFields || "")
			.split(",")
			.map((field) => field.trim())
			.filter(Boolean);
		form.dataset.waffleRedactedFields = redacted.join(",");
		const values = {
			name: card.dataset.taskName || "",
			cron: card.dataset.taskCron || "",
			prompt: card.dataset.taskPrompt || "",
			deliver: card.dataset.taskDeliver || "",
			profile: card.dataset.taskProfile || "",
		};
		for (const [field, value] of Object.entries(values)) {
			const control = input(form, `task-schedule-${field}`);
			if (!control) continue;
			const isRedacted = redacted.includes(field);
			control.value = isRedacted ? "" : value;
			control.required = !isRedacted && (field === "name" || field === "cron" || field === "prompt");
		}
		for (const field of ["deliver", "profile"]) {
			const row = input(form, `task-schedule-${field}-clear-row`);
			if (row) row.hidden = !redacted.includes(field);
		}
		const enabledRow = input(form, "task-schedule-enabled-row");
		const cancel = input(form, "task-schedule-cancel");
		const submit = input(form, "task-schedule-submit");
		if (enabledRow) enabledRow.hidden = false;
		if (cancel) {
			cancel.hidden = false;
			cancel.textContent = "Cancel edit";
		}
		if (submit) submit.textContent = "Save schedule";
		openTaskScheduleDialog();
		form.querySelector?.("#task-schedule-name")?.focus?.();
	}

	function applySkillPrerequisites() {
		const localHelp = document.querySelector("#capability-skill-local-help");
		const gitHelp = document.querySelector("#capability-skill-git-help");
		const form = document.querySelector("#capability-skill-stage-form");
		const prerequisite = document.querySelector("#capability-skill-stage-prerequisite");
		if (!form || !localHelp || !gitHelp) return;
		const available = (element) => element.dataset.waffleSourceAvailable === "true" || !element.textContent.includes("none configured");
		const localAvailable = available(localHelp);
		const gitAvailable = available(gitHelp);
		const availableSource = localAvailable || gitAvailable;
		form.hidden = !availableSource;
		form.querySelector?.("#capability-skill-local-path") && (form.querySelector("#capability-skill-local-path").disabled = restartLocked || !localAvailable);
		form.querySelector?.("#capability-skill-git-url") && (form.querySelector("#capability-skill-git-url").disabled = restartLocked || !gitAvailable);
		form.querySelector?.("#capability-skill-commit") && (form.querySelector("#capability-skill-commit").disabled = restartLocked || !gitAvailable);
		const submit = form.querySelector?.("button[type=submit]");
		if (submit) submit.disabled = restartLocked || !availableSource;
		if (prerequisite) {
			prerequisite.hidden = availableSource;
			prerequisite.textContent = availableSource ? "" : "Skill imports are disabled. Configure an allowed local root or Git host, then restart Waffle.";
		}
	}

	function filterCatalogue() {
		const search = document.querySelector("#capability-catalogue-search");
		const results = document.querySelector("#capability-catalogue-results");
		if (!search || !results) return;
		const query = (search.value || "").trim().toLowerCase();
		const cards = [...results.querySelectorAll(".catalogue-card")];
		let visible = 0;
		for (const card of cards) {
			const matches = !query || card.textContent.toLowerCase().includes(query);
			card.hidden = !matches;
			if (matches) visible++;
		}
		const summary = document.querySelector("#capability-catalogue-summary");
		if (summary && cards.length > 0) summary.textContent = query ? `${visible} of ${cards.length} models match.` : `${cards.length} models available.`;
	}

	function setRestartControlsDisabled(disabled) {
		restartLocked = disabled;
		for (const form of document.querySelectorAll("#capability-provider-form, #capability-default-form, #capability-utility-form, #capability-model-form, #capability-catalogue-form")) {
			form.dataset.restartLocked = disabled ? "true" : "false";
			for (const control of form.querySelectorAll("input, button, select, textarea")) control.disabled = disabled;
		}
		for (const button of document.querySelectorAll("#capability-skills button")) button.disabled = disabled;
		applySkillPrerequisites();
	}

	async function readBootstrap() {
		const response = await fetch("/api/v1/desk/bootstrap", { credentials: "same-origin", cache: "no-store", headers: { Accept: "application/json" } });
		if (!response.ok) throw new Error("bootstrap unavailable");
		return response.json();
	}

	function finishRestart() {
		restartPolling = false;
		restartLocked = false;
		if (restartPollTimer) clearTimeout(restartPollTimer);
		restartPollTimer = 0;
		const banner = document.querySelector("#capability-restart-status");
		if (banner) banner.hidden = true;
		setRestartControlsDisabled(false);
		htmx.trigger(document.body, "waffle:refresh");
	}

	async function pollRestart() {
		if (!restartPolling) return;
		if (Date.now() - restartPollStartedAt >= restartPollTimeout) {
			restartPolling = false;
			const banner = document.querySelector("#capability-restart-status");
			if (banner) banner.querySelector("span")?.replaceChildren(document.createTextNode("Restart did not complete in time. Restart waffle serve to apply the change."));
			return;
		}
		try {
			const bootstrap = await readBootstrap();
			if (typeof bootstrap.process_generation === "string" && bootstrap.process_generation && bootstrap.process_generation !== processGeneration) {
				processGeneration = bootstrap.process_generation;
				document.body.dataset.requestToken = bootstrap.request_token || document.body.dataset.requestToken || "";
				finishRestart();
				return;
			}
		} catch {
			// A restarting process may briefly refuse bootstrap; keep the bounded poll.
		}
		restartPollTimer = setTimeout(pollRestart, restartPollInterval);
	}

	function beginRestartPolling() {
		if (restartPolling) return;
		restartPolling = true;
		restartPollStartedAt = Date.now();
		setRestartControlsDisabled(true);
		void pollRestart();
	}

	document.body.addEventListener("htmx:configRequest", (event) => {
    const detail = event.detail || {};
    const element = detail.elt;
    const form = element?.matches?.("form") ? element : element?.closest?.("form");
    if (!form?.matches?.("[data-waffle-json]")) return;

    const body = objectFromParameters(detail.parameters || detail.unfilteredParameters);
		for (const [name, value] of Object.entries(body)) {
      body[name] = Array.isArray(value)
        ? value.map((item) => normalize(item, name))
        : normalize(value, name);
		}
		if (form.dataset.waffleJsonKind === "task-schedule") {
			taskScheduleBody(form, body, detail);
		}
		const prospectiveProvider = form.dataset.waffleJsonKind === "provider" && (detail.path || "").endsWith("/providers/test");
		if (prospectiveProvider) {
			body.model = body.model_id || "";
			delete body.model_id;
			delete body.model_alias;
			delete body.make_default;
			delete body.make_utility;
		} else if (form.dataset.waffleJsonKind === "provider") {
      const alias = body.model_alias || "";
      const model = body.model_id || "";
      body.models = alias ? { [alias]: { model } } : {};
      body.default_model = body.make_default ? alias : "";
      body.utility_model = body.make_utility ? alias : "";
      delete body.model_alias;
      delete body.model_id;
      delete body.make_default;
      delete body.make_utility;
    }
    const serialized = JSON.stringify(body);
    const identity = `${detail.verb || "POST"} ${detail.path || ""}\u0000${serialized}`;
    const intent = intents.get(identity) || { key: uuid(), serialized };
    intents.set(identity, intent);
    detail.headers = detail.headers || {};
    detail.headers.Accept = "text/html";
    detail.headers["Content-Type"] = "application/json";
    detail.headers["X-Waffle-Desk-Token"] = document.body.dataset.requestToken || "";
    detail.headers["Idempotency-Key"] = intent.key;
    detail.waffleIntent = intent;
    detail.waffleJSON = serialized;
  });

  document.body.addEventListener("htmx:beforeRequest", (event) => {
    const intent = event.detail?.requestConfig?.waffleIntent;
    if (intent && inFlight.has(intent.key)) {
      event.preventDefault();
    }
  });

  document.body.addEventListener("htmx:beforeSend", (event) => {
    const detail = event.detail || {};
    const config = detail.requestConfig;
    if (!config?.waffleJSON || !detail.xhr) return;
    const xhr = detail.xhr;
    const originalSend = xhr.send.bind(xhr);
    xhr.send = () => originalSend(config.waffleJSON);
    if (config.waffleIntent) inFlight.add(config.waffleIntent.key);
  });

	document.body.addEventListener("htmx:afterRequest", (event) => {
    const config = event.detail?.requestConfig;
    const intent = config?.waffleIntent;
    if (!intent) return;
    inFlight.delete(intent.key);
	if (event.detail.xhr?.status >= 200 && event.detail.xhr?.status < 300) {
      const path = config.path || "";
      const form = event.detail.elt?.matches?.("form")
        ? event.detail.elt
        : event.detail.elt?.closest?.("form");
		if (form?.matches?.("[data-waffle-json]")) {
        for (const input of form.querySelectorAll?.("input[type=password]") || []) {
          input.value = "";
		}
		if (path.endsWith("/models")) {
			const action = form.querySelector?.("[data-waffle-action-id]");
			if (action?.dataset?.waffleActionId?.startsWith("catalogue-add-")) {
				action.disabled = true;
				action.textContent = "Enrolled";
				action.setAttribute("aria-disabled", "true");
			}
		}
		if (form?.dataset.waffleJsonKind === "task-schedule") {
			resetTaskSchedule(form);
			closeTaskScheduleDialog();
		}
		if (event.detail.xhr?.responseText?.includes('id="capability-restart-status"')) beginRestartPolling();
      }
      if (path.endsWith("/open") || path.endsWith("/close") || path.endsWith("/forget")) {
        event.detail.elt?.closest?.("dialog")?.close?.();
      }
      if (path.endsWith("/close-preview") || path.endsWith("/forget-preview")) {
        document.querySelector("#workspace-close-dialog, #memory-forget-dialog")?.showModal?.();
      }
      for (const [identity, candidate] of intents) {
        if (candidate.key === intent.key) intents.delete(identity);
      }
    }
	});

	document.body.addEventListener("htmx:afterSwap", () => {
		applySkillPrerequisites();
		filterCatalogue();
	});
	document.body.addEventListener("htmx:afterSettle", () => {
		applySkillPrerequisites();
		filterCatalogue();
	});

	document.body.addEventListener("click", (event) => {
		const taskEdit = event.target?.closest?.("[data-waffle-task-edit]");
		if (taskEdit) {
			event.preventDefault();
			beginTaskEdit(taskEdit);
			return;
		}
		const scheduleOpen = event.target?.closest?.("#task-schedule-open");
		if (scheduleOpen) {
			event.preventDefault();
			const form = document.querySelector("#task-schedule-form");
			if (form) resetTaskSchedule(form);
			openTaskScheduleDialog();
			document.querySelector("#task-schedule-name")?.focus?.();
			return;
		}
		const scheduleCancel = event.target?.closest?.("#task-schedule-cancel");
		if (scheduleCancel) {
			event.preventDefault();
			const form = document.querySelector("#task-schedule-form");
			if (form) resetTaskSchedule(form);
			closeTaskScheduleDialog();
			document.querySelector("#task-schedule-open")?.focus?.();
			return;
		}
    const open = event.target?.closest?.("#workspace-open-button");
    if (open) {
      event.preventDefault();
      document.querySelector("#workspace-open-dialog")?.showModal?.();
      return;
    }
    const openCancel = event.target?.closest?.("#workspace-open-cancel");
    if (openCancel) {
      event.preventDefault();
      const dialog = openCancel.closest?.("dialog");
      dialog?.close?.();
      document.querySelector("#workspace-open-button")?.focus?.();
      return;
    }
    const cancel = event.target?.closest?.("[data-waffle-dialog-cancel]");
    if (!cancel) return;
    const dialog = cancel.closest?.("dialog");
    if (!dialog) return;
    event.preventDefault();
    dialog.close?.();
    const focusID = dialog.dataset.waffleDialogFocus || "";
    if (focusID && document.querySelectorAll) {
      for (const candidate of document.querySelectorAll("[data-waffle-action-id]")) {
        if (candidate.dataset.waffleActionId === focusID) {
          candidate.focus?.();
          break;
        }
      }
    }
	});

	document.body.addEventListener("input", (event) => {
		if (event.target?.id === "capability-catalogue-search") filterCatalogue();
	});

	void readBootstrap().then((bootstrap) => {
		if (typeof bootstrap.process_generation === "string") processGeneration = bootstrap.process_generation;
		if (typeof bootstrap.request_token === "string" && bootstrap.request_token) document.body.dataset.requestToken = bootstrap.request_token;
	}).catch(() => {});
})();
