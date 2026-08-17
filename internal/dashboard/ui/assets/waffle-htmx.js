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
		// Guided-only controls never reach the schedule contract; the cron
		// input is the single source of truth and delivery is reassembled
		// from the configured channel + chat id (#460).
		for (const key of ["cadence", "time", "dow", "dom", "chat_id"]) {
			delete body[key];
		}
		body.deliver = scheduleDeliverValue(form);
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
			} else if (field === "deliver") {
				const value = scheduleDeliverValue(form);
				intentsForFields[field] = value
					? { action: "replace", value }
					: { action: "preserve" };
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

	// Guided schedule editor (#460): common cadences in plain language with a
	// live human summary + next run from the server, raw cron in an explicit
	// advanced mode, and configured profile/delivery choices.
	function scheduleGuidedCron(form) {
		const cadence = input(form, "task-schedule-cadence")?.value || "weekdays";
		const time = (input(form, "task-schedule-time")?.value || "09:00").trim() || "09:00";
		const [hour = "9", minute = "0"] = time.split(":");
		const padded = (value) => String(Number(value) || 0).padStart(2, "0");
		const hm = `${padded(minute)} ${padded(hour)}`;
		switch (cadence) {
			case "daily":
				return `${hm} * * * *`;
			case "weekly":
				return `${hm} * * ${input(form, "task-schedule-dow")?.value || "1"}`;
			case "monthly":
				return `${hm} ${input(form, "task-schedule-dom")?.value || "1"} * *`;
			case "weekdays":
			default:
				return `${hm} * * 1-5`;
		}
	}

	function scheduleCronFromSpec(spec) {
		const fields = String(spec || "").trim().split(/\s+/);
		if (fields.length !== 5) return null;
		const [minute, hour, dom, month, dow] = fields;
		const time = `${String(hour || "0").padStart(2, "0")}:${String(minute || "0").padStart(2, "0")}`;
		if (dom === "*" && month === "*" && dow === "*") {
			return { cadence: "daily", time };
		}
		if (dom === "*" && month === "*" && dow === "1-5") {
			return { cadence: "weekdays", time };
		}
		if (dom === "*" && month === "*" && /^[0-7]$/.test(dow)) {
			return { cadence: "weekly", time, dow };
		}
		if (month === "*" && dow === "*" && /^\d+$/.test(dom) && Number(dom) >= 1 && Number(dom) <= 28) {
			return { cadence: "monthly", time, dom };
		}
		return null;
	}

	function scheduleDeliverValue(form) {
		const channel = input(form, "task-schedule-deliver")?.value || "";
		if (!channel) return "";
		const chatID = (input(form, "task-schedule-chat-id")?.value || "").trim();
		return chatID ? `${channel}:${chatID}` : "";
	}

	async function schedulePreview(form) {
		const name = input(form, "task-schedule-name")?.value || "";
		const prompt = input(form, "task-schedule-prompt")?.value || "";
		const cron = input(form, "task-schedule-cron")?.value || "";
		const body = {
			name, cron, prompt,
			deliver: scheduleDeliverValue(form),
			profile: input(form, "task-schedule-profile")?.value || "",
		};
		let response;
		try {
			response = await fetch("/api/v1/desk/tasks/schedules/preview", {
				method: "POST",
				credentials: "same-origin",
				cache: "no-store",
				headers: {
					Accept: "application/json",
					"Content-Type": "application/json",
					"X-Waffle-Desk-Token": document.body.dataset.requestToken || "",
					"Idempotency-Key":
						globalThis.crypto?.randomUUID?.() ||
						`${Date.now().toString(16)}-${Math.random().toString(16).slice(2)}`,
				},
				body: JSON.stringify(body),
			});
		} catch {
			return;
		}
		const preview = await response.json().catch(() => ({}));
		const summary = input(form, "task-schedule-summary");
		if (summary) {
			if (preview.human && preview.next_run) {
				const when = new Date(preview.next_run);
				const next = Number.isNaN(when.getTime())
					? ""
					: ` · next ${when.toLocaleString()}`;
				summary.textContent = `${preview.human}${next}`;
			} else if (preview.human) {
				summary.textContent = preview.human;
			} else {
				summary.textContent = "";
			}
		}
		const errors = preview.errors || {};
		const errorBox = input(form, "task-schedule-field-errors");
		if (errorBox) {
			const messages = Object.entries(errors).map(([key, message]) => `${key}: ${message}`);
			if (messages.length > 0) {
				errorBox.hidden = false;
				errorBox.replaceChildren(document.createTextNode(messages.join(" · ")));
			} else {
				errorBox.hidden = true;
				errorBox.replaceChildren();
			}
		}
	}

	function syncGuidedVisibility(form) {
		const cadence = input(form, "task-schedule-cadence")?.value || "weekdays";
		const dow = input(form, "task-schedule-dow");
		const dom = input(form, "task-schedule-dom");
		const dowRow = input(form, "task-schedule-dow-row");
		const domRow = input(form, "task-schedule-dom-row");
		if (dowRow) dowRow.hidden = cadence !== "weekly";
		if (domRow) domRow.hidden = cadence !== "monthly";
		if (dow) dow.hidden = cadence !== "weekly";
		if (dom) dom.hidden = cadence !== "monthly";
	}

	function updateScheduleGuide(form) {
		const cron = input(form, "task-schedule-cron");
		if (!cron) return;
		syncGuidedVisibility(form);
		cron.value = scheduleGuidedCron(form);
		void schedulePreview(form);
	}

	function syncGuidedFromCron(form) {
		const cron = input(form, "task-schedule-cron");
		if (!cron) return;
		const guided = scheduleCronFromSpec(cron.value);
		if (!guided) return;
		const cadence = input(form, "task-schedule-cadence");
		const time = input(form, "task-schedule-time");
		const dow = input(form, "task-schedule-dow");
		const dom = input(form, "task-schedule-dom");
		if (cadence) cadence.value = guided.cadence;
		if (time) time.value = guided.time;
		if (dow) dow.value = guided.dow || "1";
		if (dom) dom.value = guided.dom || "1";
		const dowRow = input(form, "task-schedule-dow-row");
		const domRow = input(form, "task-schedule-dom-row");
		if (dowRow) dowRow.hidden = guided.cadence !== "weekly";
		if (domRow) domRow.hidden = guided.cadence !== "monthly";
		if (dow) dow.hidden = guided.cadence !== "weekly";
		if (dom) dom.hidden = guided.cadence !== "monthly";
	}

	async function loadScheduleOptions(form) {
		let options;
		try {
			const response = await fetch("/api/v1/desk/tasks/schedules/options", {
				credentials: "same-origin",
				cache: "no-store",
				headers: { Accept: "application/json" },
			});
			options = await response.json();
		} catch {
			options = {};
		}
		const profiles = Array.isArray(options.profiles) ? options.profiles : [];
		const deliveries = Array.isArray(options.deliveries) ? options.deliveries : [];
		const profileSelect = input(form, "task-schedule-profile");
		if (profileSelect) {
			profileSelect.replaceChildren(new Option("Group default", ""));
			for (const name of profiles) {
				profileSelect.add(new Option(name, name));
			}
		}
		const profileHint = input(form, "task-schedule-profile-hint");
		if (profileHint) {
			profileHint.hidden = profiles.length > 0;
			profileHint.textContent = profiles.length === 0
				? "No agent profiles are configured. Create one in Capabilities → Profiles."
				: "";
		}
		const deliverSelect = input(form, "task-schedule-deliver");
		if (deliverSelect) {
			deliverSelect.replaceChildren(new Option("Log only (no delivery)", ""));
			for (const channel of deliveries) {
				deliverSelect.add(new Option(channel, channel));
			}
		}
		const deliverHint = input(form, "task-schedule-deliver-hint");
		if (deliverHint) {
			deliverHint.hidden = deliveries.length > 0;
			deliverHint.textContent = deliveries.length === 0
				? "No delivery channels are connected. Enroll one in Capabilities → Tools & connections."
				: "";
		}
	}

	function syncScheduleDeliverUI(form) {
		const channel = input(form, "task-schedule-deliver")?.value || "";
		const chatID = input(form, "task-schedule-chat-id");
		if (chatID) chatID.hidden = !channel;
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
		const cadence = input(form, "task-schedule-cadence");
		const time = input(form, "task-schedule-time");
		const chatID = input(form, "task-schedule-chat-id");
		const summary = input(form, "task-schedule-summary");
		const errors = input(form, "task-schedule-field-errors");
		if (cadence) cadence.value = "weekdays";
		if (time) time.value = "09:00";
		if (chatID) chatID.value = "";
		if (summary) summary.textContent = "";
		if (errors) {
			errors.hidden = true;
			errors.replaceChildren();
		}
		syncGuidedFromCron(form);
		syncScheduleDeliverUI(form);
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
		// Guided controls derive from the stored cron; complex expressions stay
		// in the explicit advanced input without being mangled (#460).
		syncGuidedFromCron(form);
		const deliver = values.deliver;
		const [channel = "", chatID = ""] = String(deliver).split(":");
		const deliverSelect = input(form, "task-schedule-deliver");
		const chatIDInput = input(form, "task-schedule-chat-id");
		if (deliverSelect && channel) {
			if (![...deliverSelect.options].some((option) => option.value === channel)) {
				deliverSelect.add(new Option(channel, channel));
			}
			deliverSelect.value = channel;
		}
		if (chatIDInput) chatIDInput.value = redacted.includes("deliver") ? "" : chatID;
		syncScheduleDeliverUI(form);
		void loadScheduleOptions(form).then(() => {
			if (deliverSelect && channel) deliverSelect.value = channel;
			const profileSelect = input(form, "task-schedule-profile");
			const profile = values.profile;
			if (profileSelect && profile && !redacted.includes("profile")) {
				if (![...profileSelect.options].some((option) => option.value === profile)) {
					profileSelect.add(new Option(profile, profile));
				}
				profileSelect.value = profile;
			}
			syncScheduleDeliverUI(form);
		});
		void schedulePreview(form);
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
		const disclosure = document.querySelector("#capability-skill-import-disclosure");
		if (!form || !localHelp || !gitHelp) return;
		// A source is only proven available once its fragment loads the
		// explicit data attribute; an untouched help node is still loading and
		// never counts as an available installer source (#464).
		const sourceState = (element) => {
			const state = element.dataset.waffleSourceAvailable;
			if (state === "true") return true;
			if (state === "false") return false;
			return null;
		};
		const localState = sourceState(localHelp);
		const gitState = sourceState(gitHelp);
		const loaded = localState !== null && gitState !== null;
		const localAvailable = localState === true;
		const gitAvailable = gitState === true;
		const availableSource = localAvailable || gitAvailable;
		form.hidden = !availableSource;
		form.querySelector?.("#capability-skill-local-path") && (form.querySelector("#capability-skill-local-path").disabled = restartLocked || !localAvailable);
		form.querySelector?.("#capability-skill-git-url") && (form.querySelector("#capability-skill-git-url").disabled = restartLocked || !gitAvailable);
		form.querySelector?.("#capability-skill-commit") && (form.querySelector("#capability-skill-commit").disabled = restartLocked || !gitAvailable);
		const submit = form.querySelector?.("button[type=submit]");
		if (submit) submit.disabled = restartLocked || !availableSource;
		// The add/review disclosure is removed while imports are disabled and
		// inert while sources are still loading, so it can never open an empty
		// panel (#464). The prerequisite line carries the safe next action.
		const showDisclosure = loaded && availableSource;
		if (disclosure) {
			disclosure.hidden = !showDisclosure;
			if (!showDisclosure) disclosure.open = false;
			disclosure.setAttribute("aria-disabled", showDisclosure ? "false" : "true");
		}
		if (prerequisite) {
			prerequisite.hidden = !loaded || availableSource;
			prerequisite.textContent = availableSource ? "" : "Skill imports are disabled. Configure an allowed local root or Git host, then restart Waffle.";
		}
	}

	// The memory attach picker gates every Attach to session action until a
	// valid persisted session is chosen, and refreshes the picker when an
	// attach fails so a stale/deleted selection recovers in place (#459).
	function refreshMemoryAttachAvailability() {
		const picker = document.querySelector("#memory-session");
		const valid = Boolean(picker && picker.value && picker.value !== "");
		for (const button of document.querySelectorAll("[data-waffle-action-id^='memory-attach-']")) {
			button.disabled = !valid;
		}
	}

	function refreshMemorySessionPicker() {
		const field = document.querySelector("#memory-session-field");
		if (!field || typeof htmx === "undefined") return;
		htmx.ajax("GET", "/api/v1/desk/memory/sessions", {
			target: "#memory-session-field",
			swap: "outerHTML",
		});
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
      for (const [identity, candidate] of intents) {
        if (candidate.key === intent.key) intents.delete(identity);
      }
    }
	});

	document.body.addEventListener("change", (event) => {
		if (event.target?.id === "memory-session") {
			refreshMemoryAttachAvailability();
		}
		const id = event.target?.id || "";
		if (id === "task-schedule-cadence" || id === "task-schedule-dow" || id === "task-schedule-dom") {
			const form = document.querySelector("#task-schedule-form");
			if (form) updateScheduleGuide(form);
		}
		if (id === "task-schedule-deliver") {
			const form = document.querySelector("#task-schedule-form");
			if (form) {
				syncScheduleDeliverUI(form);
				void schedulePreview(form);
			}
		}
	});
	document.body.addEventListener("input", (event) => {
		const id = event.target?.id || "";
		if (id === "task-schedule-time" || id === "task-schedule-cron" || id === "task-schedule-name" || id === "task-schedule-prompt" || id === "task-schedule-chat-id") {
			const form = document.querySelector("#task-schedule-form");
			if (form) {
				if (id === "task-schedule-cron") syncGuidedFromCron(form);
				if (id === "task-schedule-time") updateScheduleGuide(form);
				else void schedulePreview(form);
			}
		}
	});

	document.body.addEventListener("htmx:afterSwap", (event) => {
		applySkillPrerequisites();
		filterCatalogue();
		refreshMemoryAttachAvailability();
		const requestPath = event.detail?.pathInfo?.requestPath || "";
		if (requestPath.endsWith("/close-preview") || requestPath.endsWith("/forget-preview")) {
			// The fragment arrives closed and is already swapped, so showModal
			// can open it as a genuine native modal: no InvalidStateError,
			// backdrop, or focus leak (#457).
			const dialog = document.querySelector("#workspace-close-dialog, #memory-forget-dialog");
			dialog?.showModal?.();
			// Initial focus lands on a safe dialog control, never the
			// destructive confirmation.
			dialog?.querySelector?.("[data-waffle-dialog-cancel]")?.focus?.();
			// Escape and explicit Cancel both fire close; restoring the opener
			// there covers both paths exactly once.
			dialog?.addEventListener?.(
				"close",
				() => {
					const focusID = dialog.dataset.waffleDialogFocus || "";
					if (focusID) {
						for (const candidate of document.querySelectorAll("[data-waffle-action-id]")) {
							if (candidate.dataset.waffleActionId === focusID) {
								candidate.focus?.();
								break;
							}
						}
					}
				},
				{ once: true },
			);
		}
	});
	document.body.addEventListener("htmx:afterSettle", (event) => {
		applySkillPrerequisites();
		filterCatalogue();
		refreshMemoryAttachAvailability();
		const requestPath = event.detail?.pathInfo?.requestPath || "";
		const attachStatus = document.querySelector("#memory-attach-status");
		if (
			requestPath.includes("/memory/attach") &&
			attachStatus?.querySelector?.("[data-waffle-error='true']") &&
			document.querySelector("#memory-session")
		) {
			// Only the attach response itself resets a stale picker. A later
			// search or forget must not wipe a valid choice (#459).
			const picker = document.querySelector("#memory-session");
			if (picker) picker.value = "";
			refreshMemorySessionPicker();
			refreshMemoryAttachAvailability();
		}
	});

	document.body.addEventListener("click", (event) => {
		const copyButton = event.target?.closest?.("[data-waffle-copy]");
		if (copyButton) {
			const value = copyButton.getAttribute("data-waffle-copy");
			if (value && navigator.clipboard?.writeText) {
				void navigator.clipboard
					.writeText(value)
					.then(() => {
						const label = copyButton.textContent;
						copyButton.textContent = "Copied";
						setTimeout(() => {
							copyButton.textContent = label;
						}, 2000);
					})
					.catch(() => {
						// Clipboard denied: the label simply stays stable.
					});
			}
			return;
		}
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
			if (form) {
				resetTaskSchedule(form);
				void loadScheduleOptions(form);
				updateScheduleGuide(form);
			}
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
    event.preventDefault();
    cancel.closest?.("dialog")?.close?.();
	});

	document.body.addEventListener("input", (event) => {
		if (event.target?.id === "capability-catalogue-search") filterCatalogue();
	});

	void readBootstrap().then((bootstrap) => {
		if (typeof bootstrap.process_generation === "string") processGeneration = bootstrap.process_generation;
		if (typeof bootstrap.request_token === "string" && bootstrap.request_token) document.body.dataset.requestToken = bootstrap.request_token;
	}).catch(() => {});
})();
