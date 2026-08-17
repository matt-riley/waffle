// Browser-local speechSynthesis helper for Desk (#483). No audio leaves the
// browser. Utterances are chunked and cancelled on stop so long responses
// never freeze or leak across conversations.

const chunkChars = 180;
let speakingArticle = null;
let currentButton = null;
let voices = [];

function supported() {
  return Boolean(
    globalThis.speechSynthesis &&
    typeof globalThis.SpeechSynthesisUtterance !== "undefined",
  );
}

function refreshVoices() {
  try {
    voices = globalThis.speechSynthesis?.getVoices?.() || [];
  } catch {
    voices = [];
  }
}

if (supported()) {
  refreshVoices();
  globalThis.speechSynthesis?.addEventListener?.("voiceschanged", refreshVoices);
}

// sanitizeForSpeech strips Markdown punctuation and renders code, links, and
// tables as sensible spoken text from the visible message content only.
function sanitizeForSpeech(text) {
  const lines = String(text || "").split(/\r?\n/);
  const spoken = [];
  let inCode = false;
  let codeLines = [];
  const flushCode = () => {
    if (inCode) {
      spoken.push(`Code: ${codeLines.join(", ")}`);
      codeLines = [];
      inCode = false;
    }
  };
  for (const rawLine of lines) {
    let line = rawLine;
    if (/^```/.test(line.trim())) {
      if (inCode) {
        flushCode();
      } else {
        inCode = true;
        codeLines = [];
      }
      continue;
    }
    if (inCode) {
      codeLines.push(line.trim());
      continue;
    }
    const heading = /^#{1,6}\s+(.+)$/.exec(line);
    if (heading) {
      spoken.push(heading[1].replace(/[*_`]/g, ""));
      continue;
    }
    const list = /^\s*(?:[-*+]|\d+\.)\s+(.+)$/.exec(line);
    if (list) {
      spoken.push(list[1].replace(/[*_`]/g, ""));
      continue;
    }
    // Table delimiter rows (| --- | --- |) carry no content and are skipped.
    if (/^\|.*\|$/.test(line.trim()) && /^[\s|:+-]+$/.test(line.trim())) {
      continue;
    }
    const table = /^\|.*\|$/.test(line.trim());
    if (table) {
      const cells = line
        .replace(/^\||\|$/g, "")
        .split("|")
        .map((cell) => cell.trim().replace(/[*_`]/g, ""))
        .filter(Boolean);
      if (cells.length > 0) {
        spoken.push(cells.join(", "));
      }
      continue;
    }
    // Links read as their visible text; emphasis and code ticks drop.
    line = line.replace(/\[(.+?)\]\([^)]*\)/g, "$1");
    line = line.replace(/[*_`]/g, "");
    if (line.trim() !== "") {
      spoken.push(line.trim());
    }
  }
  flushCode();
  return spoken.filter(Boolean).join(". ") || "No readable content.";
}

// chunkText splits the sanitized text at sentence-ish boundaries so every
// utterance stays within the synthesis engine's comfortable length.
function chunkText(text) {
  const units = text.split(/(?<=\.)\s+/).map((part) => part.trim()).filter(Boolean);
  const chunks = [];
  let current = "";
  for (const unit of units) {
    if (current && current.length + unit.length > chunkChars) {
      chunks.push(current);
      current = unit;
    } else {
      current = current ? `${current} ${unit}` : unit;
    }
  }
  if (current) {
    chunks.push(current);
  }
  return chunks.length > 0 ? chunks : [text];
}

function resetButton(button) {
  if (!button) {
    return;
  }
  const label = button.dataset.readAloudLabel || "Read aloud";
  button.textContent = label;
  button.setAttribute("aria-label", "Read this response aloud");
  button.classList.remove("is-speaking");
  button.setAttribute("aria-pressed", "false");
}

function stop() {
  const button = currentButton;
  speakingArticle = null;
  currentButton = null;
  resetButton(button);
  if (!supported()) {
    return;
  }
  try {
    globalThis.speechSynthesis.cancel();
  } catch {
    // Synthesis teardown is best-effort.
  }
}

function start(article, button) {
  if (!supported()) {
    return;
  }
  stop();
  const body = article.querySelector(".message-body");
  if (!body) {
    return;
  }
  const plain = (article.dataset.rawText ?? body.textContent) || "";
  const chunks = chunkText(sanitizeForSpeech(plain));
  speakingArticle = article;
  currentButton = button;
  button.textContent = "Stop";
  button.setAttribute("aria-label", "Stop reading");
  button.classList.add("is-speaking");
  button.setAttribute("aria-pressed", "true");
  try {
    const voice = voices.find((item) => item?.default) || null;
    const utterances = chunks.map((chunk) => {
      const utterance = new SpeechSynthesisUtterance(chunk);
      utterance.voice = voice;
      return utterance;
    });
    const last = utterances[utterances.length - 1];
    if (last) {
      last.onend = () => {
        if (speakingArticle === article) {
          stop();
        }
      };
      last.onerror = (event) => {
        // cancel() during start, or a replaced utterance, reports
        // interrupted/canceled — that is not a failed read.
        const reason = event?.error;
        if (reason === "interrupted" || reason === "canceled") {
          return;
        }
        if (speakingArticle === article) {
          stop();
        }
      };
    }
    for (const utterance of utterances) {
      globalThis.speechSynthesis.speak(utterance);
    }
  } catch {
    stop();
  }
}

globalThis.waffleReadAloud = Object.freeze({
  supported,
  start,
  stop,
});
