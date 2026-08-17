// Dictation (#482): optional speech-to-text for the Today composer draft.
// Audio is never sent to Waffle. The browser speech service may process
// audio off-device; that is disclosed next to the control before any
// permission prompt. No recognition object is created until the first
// explicit activation.

const waffleDeskDictate = (() => {
  let recognition = null;
  let listening = false;
  let button = null;

  function supported() {
    return Boolean(
      globalThis.SpeechRecognition || globalThis.webkitSpeechRecognition,
    );
  }

  function isListening() {
    return listening;
  }

  function setButtonState(active) {
    if (!button) {
      return;
    }
    button.textContent = active ? "Stop dictation" : "Dictate";
    button.setAttribute("aria-label", active ? "Stop dictation" : "Start dictation");
    button.setAttribute("aria-pressed", String(active));
    button.classList.toggle("is-listening", active);
  }

  function stop({ returnFocus = false, textarea = null } = {}) {
    if (!listening) {
      return;
    }
    listening = false;
    try {
      recognition?.stop?.();
    } catch {
      // Stopping a dead recognizer is best-effort.
    }
    setButtonState(false);
    if (returnFocus && textarea) {
      textarea.focus();
    }
  }

  // insertAtSelection merges the transcript into the draft at the caret,
  // never destroying what the operator already typed.
  function insertAtSelection(textarea, text) {
    const start = typeof textarea.selectionStart === "number" ? textarea.selectionStart : textarea.value.length;
    const end = typeof textarea.selectionEnd === "number" ? textarea.selectionEnd : start;
    const prefix = textarea.value.slice(0, start);
    const suffix = textarea.value.slice(end);
    const separator = prefix && !/\s$/.test(prefix) ? " " : "";
    textarea.value = prefix + separator + text + suffix;
    const caret = prefix.length + separator.length + text.length;
    textarea.selectionStart = caret;
    textarea.selectionEnd = caret;
    if (typeof Event !== "undefined") {
      textarea.dispatchEvent?.(new Event("input", { bubbles: true }));
    }
  }

  function start(textarea, trigger, announce = () => {}) {
    button = trigger || button;
    if (listening) {
      stop();
      return;
    }
    const Constructor = globalThis.SpeechRecognition || globalThis.webkitSpeechRecognition;
    if (!Constructor) {
      announce("Dictation is not supported in this browser.", true);
      return;
    }
    try {
      recognition = new Constructor();
    } catch {
      announce("Dictation is not supported in this browser.", true);
      return;
    }
    recognition.continuous = false;
    recognition.interimResults = false;
    recognition.lang = "en-US";
    recognition.onresult = (event) => {
      const transcript = event.results?.[0]?.[0]?.transcript || "";
      listening = false;
      setButtonState(false);
      if (transcript) {
        insertAtSelection(textarea, transcript.trim());
        announce("Dictation finished.");
      }
    };
    recognition.onerror = (event) => {
      listening = false;
      setButtonState(false);
      const messages = {
        "not-allowed": "Microphone access was denied.",
        "service-not-allowed": "Dictation is not allowed on this page.",
        "no-speech": "No speech was heard.",
        network: "Speech recognition is unavailable right now.",
        aborted: "Dictation stopped.",
      };
      announce(messages[event?.error] || "Dictation could not start.", true);
    };
    recognition.onend = () => {
      listening = false;
      setButtonState(false);
    };
    listening = true;
    setButtonState(true);
    announce("Listening…");
    try {
      recognition.start();
    } catch {
      listening = false;
      setButtonState(false);
      announce("Dictation could not start.", true);
    }
  }

  return { supported, listening: isListening, start, stop };
})();

globalThis.waffleDeskDictate = waffleDeskDictate;
