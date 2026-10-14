"use strict";

// ---- bootstrap: token, api, events ----------------------------------------------

const params = new URLSearchParams(location.hash.replace(/^#/, ""));
const injected = window.__VOICEIO__ || {};
if (params.get("token")) {
  sessionStorage.setItem("voiceio-token", params.get("token"));
  params.delete("token");
  history.replaceState(null, "", "#" + params.toString());
}
const TOKEN = injected.token || sessionStorage.getItem("voiceio-token") || "";

async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: { Authorization: "Bearer " + TOKEN, "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const type = res.headers.get("Content-Type") || "";
  const data = type.includes("json") ? await res.json() : null;
  if (!res.ok) {
    const err = new Error((data && data.error) || res.statusText);
    err.field = data && data.field;
    throw err;
  }
  return data;
}

const S = {
  page: params.get("page") || "overview",
  boot: null,
  config: null,
  status: null,
  models: null,
  devices: null,
  maps: {},
  history: null,
  recovery: null,
  meetings: null,
  progress: {},
  installing: false,
  installError: "",
  mic: { testing: false, level: 0, peak: null, error: "" },
  recordingShortcut: null,
  previewNonce: 1,
  llm: { models: null, test: null, busy: false, error: "" },
};

const isMac = () => (S.boot ? S.boot.platform === "darwin" : navigator.platform.startsWith("Mac"));

// ---- dom helpers -----------------------------------------------------------------

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === false || v === null || v === undefined) continue;
    if (k.startsWith("on") && typeof v === "function") el.addEventListener(k.slice(2).toLowerCase(), v);
    else if (k === "class") el.className = v;
    else if (k === "style" && typeof v === "object") Object.assign(el.style, v);
    else if (k === "html") el.innerHTML = v;
    else if (k in el && typeof v !== "string") el[k] = v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat(Infinity)) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

const svg = (paths, extra = "") =>
  h("span", {
    class: "svg",
    html: `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" ${extra}>${paths}</svg>`,
  }).firstChild;

const I = {
  overview: '<path d="M12 3l1.9 5.1L19 10l-5.1 1.9L12 17l-1.9-5.1L5 10l5.1-1.9z"/><path d="M19 15l.8 2.2L22 18l-2.2.8L19 21l-.8-2.2L16 18l2.2-.8z"/>',
  shortcuts: '<rect x="2.5" y="6" width="19" height="12" rx="2.5"/><path d="M6.5 10h1M10.5 10h1M14.5 10h1M8 14h8"/>',
  dictation: '<rect x="9" y="3" width="6" height="11.5" rx="3"/><path d="M5.5 11a6.5 6.5 0 0 0 13 0M12 17.5V21"/>',
  meetings: '<circle cx="9" cy="8.5" r="3.5"/><path d="M2.5 20c0-4 3-6 6.5-6s6.5 2 6.5 6M15.5 5.2a3.3 3.3 0 0 1 0 6.6M18 14.4c2.2.7 3.5 2.7 3.5 5.6"/>',
  appearance: '<circle cx="12" cy="12" r="9"/><path d="M12 3v18" /><path d="M12 3a9 9 0 0 1 0 18z" fill="currentColor"/>',
  models: '<path d="M12 2.5l8.5 4.8v9.4L12 21.5l-8.5-4.8V7.3z"/><path d="M3.8 7.4L12 12l8.2-4.6M12 12v9.4"/>',
  history: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3.5 2"/>',
  system: '<circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.8-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1.1-1.6 1.7 1.7 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.8 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.6-1.1 1.7 1.7 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.8.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.8V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1z"/>',
  check: '<path d="M5 12.5l4.5 4.5L19 7.5"/>',
  copy: '<rect x="8" y="8" width="12" height="12" rx="2.5"/><path d="M16 8V6a2 2 0 0 0-2-2H6a2 2 0 0 0-2 2v8a2 2 0 0 0 2 2h2"/>',
  trash: '<path d="M4 7h16M10 11v6M14 11v6M6 7l1 12.5A1.5 1.5 0 0 0 8.5 21h7a1.5 1.5 0 0 0 1.5-1.5L18 7M9 7V4.5A1.5 1.5 0 0 1 10.5 3h3A1.5 1.5 0 0 1 15 4.5V7"/>',
  warn: '<path d="M12 3.5l9.5 16.5h-19z"/><path d="M12 10v4.5M12 17.5v.01"/>',
  info: '<circle cx="12" cy="12" r="9"/><path d="M12 11v5.5M12 7.8v.01"/>',
  download: '<path d="M12 4v11M7 10.5l5 5 5-5M4.5 19.5h15"/>',
  folder: '<path d="M3 7.5A2.5 2.5 0 0 1 5.5 5H9l2 2.5h7.5A2.5 2.5 0 0 1 21 10v7.5a2.5 2.5 0 0 1-2.5 2.5h-13A2.5 2.5 0 0 1 3 17.5z"/>',
  open: '<path d="M14 4h6v6M20 4l-9 9M18 14v4.5a1.5 1.5 0 0 1-1.5 1.5h-11A1.5 1.5 0 0 1 4 18.5v-11A1.5 1.5 0 0 1 5.5 6H10"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  x: '<path d="M6 6l12 12M18 6L6 18"/>',
  mic: '<rect x="9" y="3" width="6" height="11.5" rx="3"/><path d="M5.5 11a6.5 6.5 0 0 0 13 0M12 17.5V21"/>',
  rec: '<circle cx="12" cy="12" r="8.5"/><circle cx="12" cy="12" r="4.2" fill="#ff453a" stroke="none"/>',
  wave: '<path d="M4 10v4M8 7v10M12 4v16M16 7v10M20 10v4"/>',
  key: '<rect x="3" y="7" width="18" height="10" rx="2.5"/><path d="M7 12h.01M11 12h.01M15 12h2"/>',
};

const NAV = [
  { id: "overview", title: "Overview", color: "#5e5ce6" },
  { id: "shortcuts", title: "Shortcuts", color: "#0a84ff" },
  { id: "dictation", title: "Dictation", color: "#30b0c7" },
  { id: "meetings", title: "Meetings", color: "#ff375f" },
  { id: "appearance", title: "Appearance", color: "#ff9f0a" },
  { id: "models", title: "Models", color: "#34c759" },
  { id: "history", title: "History", color: "#8e8e93" },
  { id: "system", title: "System", color: "#636366" },
];

let toastTimer = null;
function toast(message, tone = "ok") {
  const box = document.getElementById("toasts");
  const last = box.lastElementChild;
  if (last && last.dataset.msg === message) {
    clearTimeout(toastTimer);
  } else {
    const el = h("div", { class: "toast " + (tone === "error" ? "error" : "") }, tone === "error" ? svg(I.warn) : svg(I.check), message);
    el.dataset.msg = message;
    box.append(el);
    while (box.children.length > 3) box.firstChild.remove();
  }
  toastTimer = setTimeout(() => {
    const el = box.lastElementChild;
    if (!el) return;
    el.classList.add("out");
    setTimeout(() => el.remove(), 260);
  }, tone === "error" ? 4200 : 1600);
}

// ---- formatting ------------------------------------------------------------------

const MAC_MODS = [["ctrl", "⌃"], ["alt", "⌥"], ["shift", "⇧"], ["super", "⌘"]];
const PC_MODS = [["ctrl", "Ctrl"], ["alt", "Alt"], ["shift", "Shift"], ["super", "Super"]];
const MAC_KEYS = { space: "Space", enter: "↩", tab: "⇥", escape: "⎋", backspace: "⌫", delete: "⌦", up: "↑", down: "↓", left: "←", right: "→", home: "↖", end: "↘", pageup: "⇞", pagedown: "⇟", insert: "Insert" };
const PC_KEYS = { space: "Space", enter: "Enter", tab: "Tab", escape: "Esc", backspace: "Backspace", delete: "Delete", up: "Up", down: "Down", left: "Left", right: "Right", home: "Home", end: "End", pageup: "PgUp", pagedown: "PgDn", insert: "Insert" };

function keyCaps(binding) {
  const parts = String(binding || "").toLowerCase().split("+");
  const key = parts.pop();
  const mods = isMac() ? MAC_MODS : PC_MODS;
  const names = isMac() ? MAC_KEYS : PC_KEYS;
  const caps = mods.filter(([m]) => parts.includes(m) || (m === "super" && (parts.includes("cmd") || parts.includes("command"))) ||
    (m === "alt" && parts.includes("option")) || (m === "ctrl" && parts.includes("control"))).map(([, s]) => s);
  caps.push(names[key] || key.toUpperCase());
  return h("span", { class: "keys" }, caps.map((c) => h("kbd", {}, c)));
}

function ago(ts) {
  const s = Math.max(0, Date.now() / 1000 - ts);
  if (s < 60) return "just now";
  if (s < 3600) return Math.floor(s / 60) + " min ago";
  if (s < 86400) return Math.floor(s / 3600) + " h ago";
  return new Date(ts * 1000).toLocaleDateString(undefined, { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
}

const bytes = (n) => (n >= 1 << 30 ? (n / (1 << 30)).toFixed(2) + " GB" : n >= 1 << 20 ? (n / (1 << 20)).toFixed(0) + " MB" : Math.max(1, Math.round(n / 1024)) + " KB");
const MODE_TITLES = { dictation: "Dictation", meeting: "Meeting notes", collaborative: "Collaborative draft" };

// ---- controls ----------------------------------------------------------------------

function row(title, detail, control, opts = {}) {
  return h("div", { class: "row" + (opts.stack ? " stack" : ""), id: opts.id },
    h("div", { class: "row-label" }, h("div", { class: "row-title" }, title), detail && h("div", { class: "row-detail" }, detail),
      opts.error && h("div", { class: "row-error" }, opts.error)),
    control && h("div", { class: "row-control" }, control));
}

// fnSideEffect describes what macOS does on a 🌐 press besides Voice IO
// (AppleFnUsageType; -1 means unset, where macOS picks emoji or input source).
function fnSideEffect(action) {
  return { 1: "switches the input source", 2: "opens the emoji picker", 3: "starts macOS Dictation" }[action] ||
    "opens the emoji picker or switches the input source";
}

function group(title, rows, note, cls = "") {
  return h("section", { class: "section" }, title && h("h3", { class: "section-title" }, title),
    h("div", { class: "group " + cls }, rows), note && h("p", { class: "section-note" }, note));
}

function toggle(checked, onchange, disabled) {
  return h("label", { class: "switch" }, h("input", { type: "checkbox", role: "switch", checked, disabled,
    onchange: (e) => onchange(e.target.checked) }));
}

function segmented(options, value, onchange) {
  return h("div", { class: "seg", role: "radiogroup" }, options.map(([v, label]) =>
    h("button", { class: v === value ? "on" : "", role: "radio", "aria-checked": String(v === value), onclick: () => v !== value && onchange(v) }, label)));
}

function select(options, value, onchange, cls = "") {
  return h("select", { class: "field " + cls, onchange: (e) => onchange(e.target.value) },
    options.map(([v, label]) => h("option", { value: v, selected: v === value }, label)));
}

function textField(value, onCommit, opts = {}) {
  const input = h("input", {
    class: "field" + (opts.wide ? " wide" : "") + (opts.mono ? " mono" : ""), value: value || "", placeholder: opts.placeholder || "",
    spellcheck: false, autocomplete: "off", type: opts.type || "text",
  });
  let last = value || "";
  const commit = async () => {
    const v = input.value.trim();
    if (v === last) return;
    try {
      await onCommit(v);
      last = v;
      input.classList.remove("invalid");
    } catch (e) {
      input.classList.add("invalid");
      toast(e.message, "error");
    }
  };
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter") input.blur();
    if (e.key === "Escape") { input.value = last; input.blur(); }
  });
  input.addEventListener("blur", commit);
  return input;
}

function button(label, onclick, opts = {}) {
  return h("button", { class: "btn " + (opts.kind || "") + (opts.small ? " small" : ""), onclick, disabled: opts.disabled, title: opts.title },
    opts.icon && svg(I[opts.icon]), label);
}

function badge(text, tone = "") {
  return h("span", { class: "badge " + tone }, text);
}

function banner(tone, icon, title, text, action) {
  return h("div", { class: "banner " + tone }, svg(I[icon]), h("div", {}, h("div", { class: "banner-title" }, title),
    text && h("div", { class: "banner-text" }, text)), action);
}

// ---- data ------------------------------------------------------------------------------

async function save(section, key, value, opts = {}) {
  const res = await api("PATCH", "/api/config", { [section]: { [key]: value } });
  S.config = res.config;
  S.status = res.status;
  S.shortcutError = res.shortcut_error || "";
  if (!opts.quiet) toast("Saved");
  if (!opts.noRender) render();
  renderChrome();
  return res;
}

async function saveQuiet(section, key, value) {
  try {
    await save(section, key, value);
  } catch (e) {
    toast(e.message, "error");
    render();
  }
}

async function loadPage(page) {
  try {
    if (page === "history") {
      [S.history, S.recovery] = await Promise.all([api("GET", "/api/history?limit=300"), api("GET", "/api/recovery")]);
    } else if (page === "meetings") {
      S.meetings = await api("GET", "/api/meetings");
    } else if (page === "dictation") {
      const [v, s] = await Promise.all([api("GET", "/api/maps/vocabulary"), api("GET", "/api/maps/snippets")]);
      S.maps = { vocabulary: v.entries || {}, snippets: s.entries || {} };
    } else if (page === "overview" || page === "system") {
      if (!S.devices) S.devices = await api("GET", "/api/devices").catch(() => []);
    }
  } catch (e) {
    toast(e.message, "error");
  }
}

// ---- chrome ------------------------------------------------------------------------------

function stateLabel(st) {
  if (!st) return ["", "Connecting…"];
  const d = st.daemon;
  if (st.blocking) return ["warn", "Setup needed"];
  if (!d.enabled) return ["", "Voice input off"];
  if (d.state === "LISTENING") return ["rec", d.mode === "dictation" ? "Listening" : "Recording meeting"];
  if (d.state === "PARSING") return ["busy", "Transcribing"];
  return ["ok", "Ready"];
}

function renderChrome() {
  const [tone, label] = stateLabel(S.status);
  const bs = document.getElementById("brand-status");
  bs.replaceChildren(h("span", { class: "dot " + tone }), h("span", {}, label));
  const nav = document.getElementById("nav");
  const issues = (S.status && S.status.issues) || [];
  const needs = {
    models: issues.some((i) => i.id === "models"),
    system: issues.some((i) => i.id === "microphone" || i.id === "accessibility"),
  };
  nav.replaceChildren(...NAV.map((n) =>
    h("button", { class: "nav-item" + (S.page === n.id ? " active" : ""), onclick: () => go(n.id), "aria-current": S.page === n.id ? "page" : false },
      h("span", { class: "nav-icon", style: { background: n.color } }, svg(I[n.id])), n.title,
      needs[n.id] && h("span", { class: "nav-badge", title: "Needs attention" }))));
  document.getElementById("sidebar-foot").replaceChildren(
    S.boot ? h("div", {}, "Version " + S.boot.version) : "",
    h("div", {}, "Everything runs on this computer."));
}

async function go(page) {
  S.page = page;
  history.replaceState(null, "", "#page=" + page);
  renderChrome();
  await loadPage(page);
  render();
  document.getElementById("content").scrollTop = 0;
}

function render() {
  if (!S.config) return;
  const pages = { overview, shortcuts, dictation, meetings, appearance, models: modelsPage, history: historyPage, system };
  const content = document.getElementById("content");
  const active = document.activeElement;
  if (active && active.tagName === "INPUT" && active.type === "text" && content.contains(active)) return; // never clobber typing
  content.replaceChildren((pages[S.page] || overview)());
}

function page(title, sub, ...body) {
  return h("div", { class: "page" }, h("div", { class: "page-head" }, h("div", {}, h("h1", { class: "page-title" }, title),
    sub && h("p", { class: "page-sub" }, sub))), body);
}

// ---- overlay previews --------------------------------------------------------------------

function prefersDark() {
  return window.matchMedia("(prefers-color-scheme: dark)").matches;
}

function previewURL(state, overrides = {}) {
  const ui = { ...S.config.ui, ...overrides };
  const q = new URLSearchParams({ token: TOKEN, theme: ui.theme, accent: ui.accent || "", preview_color: ui.preview_color || "",
    dark: (ui.theme === "auto" ? prefersDark() : ui.theme === "dark") ? "1" : "0", v: String(S.previewNonce) });
  return "/api/overlay/" + state + ".png?" + q.toString();
}

function pillPreview(state, caption) {
  return h("figure", { class: "preview", style: { margin: 0 } },
    h("div", { class: "preview-art" }, h("img", { src: previewURL(state), alt: caption, draggable: false })),
    h("figcaption", { class: "preview-cap" }, caption));
}

// ---- pages: overview ------------------------------------------------------------------------

function overview() {
  const st = S.status;
  const [tone, label] = stateLabel(st);
  const mac = isMac();
  const trigger = mac && S.config.hotkeys.fn_hold ? h("span", {}, "Hold ", h("kbd", {}, "fn")) : keyCaps(S.config.hotkeys.push);
  const issues = (st && st.issues) || [];
  const needModels = issues.some((i) => i.id === "models");
  const perm = (id) => ((st && st.permissions) || []).find((p) => p.id === id);
  const fnAction = S.boot.fn_key_action;

  const steps = [];
  const check = (done) => h("span", { class: "check" + (done ? " done" : "") }, svg(I.check));
  steps.push(row([check(!needModels), "Speech models"],
    needModels ? "About 840 MB, downloaded once. Nothing is sent anywhere." : "Installed",
    needModels ? button(S.installing ? "Downloading…" : "Download", () => installModels("all"), { kind: "primary", small: true, disabled: S.installing }) : null));
  for (const id of ["microphone", "accessibility"]) {
    const p = perm(id);
    if (!p) continue;
    const ok = p.state === "granted";
    steps.push(row([check(ok), p.title], p.detail,
      ok ? null : button(p.action === "settings" ? "Open Settings" : "Allow…", () => requestPermission(id), { small: true })));
  }
  if (mac && S.config.hotkeys.fn_hold && fnAction !== 0) {
    steps.push(row([check(false), "Free up the fn key"],
      "Set “Press 🌐 key to” to Do Nothing in Keyboard settings; otherwise holding fn also " + fnSideEffect(fnAction) + ".",
      button("Keyboard Settings", () => reveal("keyboard"), { small: true })));
  }
  const allDone = steps.every((s) => s.querySelector(".check.done"));

  return page("Overview", null,
    h("div", { class: "hero" },
      h("div", { class: "hero-body" },
        h("div", { class: "hero-state" }, h("span", { class: "dot " + tone }), label),
        h("h2", { class: "hero-title" }, st && st.blocking ? "A couple of steps and you're ready" : "Talk instead of typing"),
        h("p", { class: "hero-text" },
          "The pill on the right edge of your screen is Voice IO. ", trigger, " anywhere to dictate — release and the text appears at your cursor. ",
          "Hover the pill for dictation and meeting notes.")),
      h("div", { class: "hero-art" }, h("img", { src: previewURL("hover-dictate"), alt: "Voice IO pill with dictation and meeting buttons", draggable: false }))),
    !allDone && group("Get started", steps, null, "checklist"),
    h("div", { class: "how" },
      howCard("#0a84ff", I.mic, "Dictate", mac && S.config.hotkeys.fn_hold ? "Hold fn and speak; release to paste. Or click the mic on the pill." : "Press the shortcut, speak, press again to paste."),
      howCard("#ff375f", I.rec, "Meeting notes", "Click the record button on the pill. Stop to get a speaker-labelled transcript" + (S.config.meeting.summary_model ? " and summary." : ".")),
      howCard("#30b0c7", I.wave, "Hands-free", S.config.kws.enabled ? `Say “${S.config.kws.wake_phrase}” to start and “${S.config.kws.end_phrase}” to paste and send.` : "Turn on the wake phrase in Shortcuts to start by voice.")),
    microphoneGroup());
}

function howCard(color, icon, title, text) {
  return h("div", { class: "how-card" }, h("div", { class: "how-icon", style: { background: color } }, svg(icon)), h("h4", {}, title), h("p", {}, text));
}

function microphoneGroup() {
  const devices = S.devices || [];
  const current = S.config.audio.device;
  const opts = [["", "System default" + (devices.find((d) => d.default) ? " (" + devices.find((d) => d.default).name + ")" : "")]]
    .concat(devices.map((d) => [d.name, d.name]));
  const meter = h("div", { class: "meter", id: "mic-meter" }, h("i", { style: { width: Math.round(S.mic.level * 100) + "%" } }));
  let note = "Speak normally — the bar should reach the right half.";
  if (S.mic.error) note = S.mic.error;
  else if (S.mic.peak !== null) note = S.mic.peak > 0.55 ? "Sounds good." : S.mic.peak > 0.25 ? "A little quiet — move closer or raise input volume." : "Very quiet — check the input device and macOS input volume.";
  return group("Microphone", [
    row("Input device", "Voice IO listens only to this microphone.", select(opts, current, (v) => saveQuiet("audio", "device", v))),
    row("Test", h("span", { id: "mic-note" }, note), [meter, button(S.mic.testing ? "Listening…" : "Test microphone", testMic, { small: true, disabled: S.mic.testing })]),
  ]);
}

async function testMic() {
  S.mic = { testing: true, level: 0, peak: null, error: "" };
  render();
  try {
    await api("POST", "/api/mic/test", { device: S.config.audio.device, seconds: 6 });
  } catch (e) {
    S.mic = { testing: false, level: 0, peak: null, error: e.message };
    render();
  }
}

// ---- pages: shortcuts -------------------------------------------------------------------------

function shortcutField(key) {
  const binding = S.config.hotkeys[key];
  const recording = S.recordingShortcut === key;
  const el = h("button", { class: "shortcut" + (recording ? " recording" : ""), onclick: () => startRecording(key), title: "Click to record a new shortcut" },
    recording ? h("span", { class: "hint" }, "Press keys…") : keyCaps(binding),
    h("span", { class: "hint" }, recording ? "esc" : "Change"));
  return el;
}

const CODE_KEYS = { Space: "space", Enter: "enter", NumpadEnter: "enter", Tab: "tab", Backspace: "backspace", Delete: "delete",
  ArrowUp: "up", ArrowDown: "down", ArrowLeft: "left", ArrowRight: "right", Home: "home", End: "end", PageUp: "pageup", PageDown: "pagedown", Insert: "insert" };

async function startRecording(key) {
  if (S.recordingShortcut) return;
  S.recordingShortcut = key;
  await api("POST", "/api/shortcuts/pause", { paused: true }).catch(() => {});
  render();
  const finish = async (binding) => {
    window.removeEventListener("keydown", onKey, true);
    S.recordingShortcut = null;
    try {
      if (binding) await save("hotkeys", key, binding, { noRender: true });
    } catch (e) {
      toast(e.message, "error");
    }
    const res = await api("POST", "/api/shortcuts/pause", { paused: false }).catch(() => null);
    if (res) S.shortcutError = res.shortcut_error || "";
    render();
  };
  const onKey = (e) => {
    e.preventDefault();
    e.stopPropagation();
    if (["Shift", "Control", "Alt", "Meta", "CapsLock", "Fn"].includes(e.key)) return;
    const mods = [];
    if (e.ctrlKey) mods.push("ctrl");
    if (e.altKey) mods.push("alt");
    if (e.shiftKey) mods.push("shift");
    if (e.metaKey) mods.push("super");
    if (e.code === "Escape" && !mods.length) return finish(null);
    let k = CODE_KEYS[e.code];
    if (!k && /^Key[A-Z]$/.test(e.code)) k = e.code.slice(3).toLowerCase();
    if (!k && /^Digit\d$/.test(e.code)) k = e.code.slice(5);
    if (!k && /^F\d{1,2}$/.test(e.code)) k = e.code.toLowerCase();
    if (!k && e.code === "Escape") k = "escape";
    if (!k) return;
    if (!mods.length) {
      toast("Include a modifier such as " + (isMac() ? "⌃ ⌥ or ⌘" : "Ctrl or Alt"), "error");
      return;
    }
    finish(mods.concat(k).join("+"));
  };
  window.addEventListener("keydown", onKey, true);
}

function shortcuts() {
  const mac = isMac();
  const c = S.config;
  const fnAction = S.boot.fn_key_action;
  const rows = [];
  if (mac) {
    rows.push(row(h("span", {}, "Hold ", h("kbd", {}, "fn"), " to dictate"),
      "Hold the fn (🌐) key and speak; release to paste. A quick tap does nothing.", toggle(c.hotkeys.fn_hold, (v) => saveQuiet("hotkeys", "fn_hold", v))));
  }
  rows.push(row("Start / finish", "Press to start, press again to paste." + (mac ? "" : " GNOME needs “Apply GNOME shortcuts” below."), shortcutField("push")));
  if (mac) {
    rows.push(row("Hold shortcut to talk", "Holding the shortcut records until you let go; a tap keeps recording until the next press.",
      toggle(c.hotkeys.hold_to_talk, (v) => saveQuiet("hotkeys", "hold_to_talk", v))));
  }
  const err = S.shortcutError;
  return page("Shortcuts", "Start dictating from anywhere without touching the mouse.",
    mac && c.hotkeys.fn_hold && fnAction !== 0 && fnAction !== undefined ? banner("", "warn", "The 🌐 key also has a system action",
      "Set System Settings › Keyboard › “Press 🌐 key to” to Do Nothing; otherwise holding fn also " + fnSideEffect(fnAction) + ".",
      button("Keyboard Settings", () => reveal("keyboard"), { small: true })) : null,
    err ? banner("danger", "warn", "A shortcut couldn't be registered", err) : null,
    group("Dictate", rows),
    group("More shortcuts", [
      row("Voice input on / off", "Pause listening without quitting.", shortcutField("toggle")),
      row("Cancel", "Discard the current recording.", shortcutField("cancel")),
    ], !mac ? h("span", {}, button("Apply GNOME shortcuts", applyGnome, { small: true }), "  Registers these keys with GNOME (replacing earlier Voice IO entries).") : null),
    group("Hands-free", [
      row("Listen for the wake phrase", "Keeps the microphone open while idle so you can start by voice. macOS shows the orange microphone indicator.",
        toggle(c.kws.enabled, (v) => saveQuiet("kws", "enabled", v))),
      row("Wake phrase", "Starts a session.", textField(c.kws.wake_phrase, (v) => save("kws", "wake_phrase", v), { placeholder: "hey voice" })),
      row("End phrase", "Pastes and presses Return (dictation only).", textField(c.kws.end_phrase, (v) => save("kws", "end_phrase", v), { placeholder: "voice send" })),
    ], "Phrases use English words from the wake-word model's dictionary; unsupported words are rejected."),
    group("Default for the shortcut and wake phrase", [
      row("Records", "The pill's buttons always offer both dictation and meeting notes.",
        segmented([["dictation", "Dictation"], ["meeting", "Meeting notes"], ["collaborative", "Collaborative"]], c.meeting.mode,
          (v) => saveQuiet("meeting", "mode", v))),
    ], c.meeting.mode === "collaborative" ? "Collaborative records several people on one microphone and pastes an editable, speaker-labelled draft. It never presses Return." : null));
}

async function applyGnome() {
  try {
    await api("POST", "/api/shortcuts/apply");
    toast("GNOME shortcuts applied");
  } catch (e) {
    toast(e.message, "error");
  }
}

// ---- pages: dictation --------------------------------------------------------------------------

function mapEditor(kind, keyLabel, valueLabel, placeholder) {
  const tbody = h("tbody");
  const emptyNote = h("span", { class: "muted" }, "None yet.");
  const sync = () => { emptyNote.hidden = tbody.children.length > 0; };
  const persist = async () => {
    const next = {};
    for (const tr of tbody.children) {
      const [a, b] = tr.querySelectorAll("input");
      if (a.value.trim()) next[a.value.trim()] = b.value;
    }
    try {
      const res = await api("PUT", "/api/maps/" + kind, next);
      S.maps[kind] = res.entries;
      toast("Saved");
    } catch (e) {
      toast(e.message, "error");
    }
  };
  const addRow = (k = "", v = "", focus = false) => {
    const a = h("input", { value: k, placeholder: placeholder[0], spellcheck: false, onchange: persist });
    const b = h("input", { value: v, placeholder: placeholder[1], spellcheck: false, onchange: persist });
    const tr = h("tr", {}, h("td", {}, a), h("td", {}, b),
      h("td", { class: "x" }, h("button", { class: "icon-btn", title: "Remove", onclick: () => { tr.remove(); sync(); persist(); } }, svg(I.x))));
    tbody.append(tr);
    sync();
    if (focus) a.focus();
  };
  Object.entries(S.maps[kind] || {}).forEach(([k, v]) => addRow(k, v));
  sync();
  return h("div", { class: "group" },
    h("table", { class: "table-edit" }, h("colgroup", {}, h("col", { class: "k" }), h("col"), h("col", { class: "x" })),
      h("thead", {}, h("tr", {}, h("th", {}, keyLabel), h("th", {}, valueLabel), h("th", {}))), tbody),
    h("div", { class: "table-foot" }, button("Add", () => addRow("", "", true), { kind: "plain", small: true, icon: "plus" }), emptyNote));
}

function dictation() {
  const c = S.config;
  const t = S.llm.test;
  const ax = ((S.status && S.status.permissions) || []).find((p) => p.id === "accessibility");
  const finish = [
    row("Paste automatically", "Puts the text wherever your cursor is when you finish. Off: copies it so you can paste it yourself.",
      toggle(c.exec.auto_paste, (v) => saveQuiet("exec", "auto_paste", v)),
      { error: c.exec.auto_paste && ax && ax.state !== "granted" ? "Needs Accessibility permission — until then the text is copied." : null }),
  ];
  if (c.exec.auto_paste) {
    finish.push(row("After pasting", "Only the spoken end phrase always presses Return; this applies to every finish.",
      segmented([["", "Do nothing"], ["enter", "Press Return"], ["ctrl+enter", isMac() ? "⌃ Return" : "Ctrl+Enter"]], c.exec.auto_submit,
        (v) => saveQuiet("exec", "auto_submit", v))));
  }
  if (c.exec.auto_paste && ax && ax.state !== "granted") {
    finish[0].querySelector(".row-control").prepend(button("Allow…", () => requestPermission("accessibility"), { small: true }));
  }
  return page("Dictation", "Where finished text goes and how it is cleaned up.",
    group("When you finish", finish),
    group("Cleanup", [
      row("Clean up text", "Fix custom words, drop filler like “um” and “uh”, and repair glued words — all on this computer.",
        toggle(c.polish.deterministic, (v) => saveQuiet("polish", "deterministic", v))),
      row("Remove filler words", null, toggle(c.polish.filler, (v) => saveQuiet("polish", "filler", v), !c.polish.deterministic)),
      row("Spoken emoji", "Say “thumbs up emoji” or “heart emoji” to insert 👍 ❤️.", toggle(c.polish.emoji, (v) => saveQuiet("polish", "emoji", v), !c.polish.deterministic)),
    ]),
    h("section", { class: "section" }, h("h3", { class: "section-title" }, "Vocabulary"),
      mapEditor("vocabulary", "When I hear", "Write", ["kubernetees", "Kubernetes"]),
      h("p", { class: "section-note" }, "Whole-word, case-insensitive replacements applied before anything else.")),
    h("section", { class: "section" }, h("h3", { class: "section-title" }, "Snippets"),
      mapEditor("snippets", "Say", "Insert exactly", ["my address", "221B Baker Street, London"]),
      h("p", { class: "section-note" }, "Say a snippet keyword on its own (up to two extra words) to paste the exact text, unpolished.")),
    group("AI polish", [
      row("Polish with a language model", "Fixes grammar and punctuation in short dictations once an endpoint is set below.",
        toggle(c.polish.enabled, (v) => saveQuiet("polish", "enabled", v))),
      row("Endpoint", "An OpenAI-compatible server, e.g. Ollama at http://127.0.0.1:11434. A remote endpoint receives your text.",
        textField(c.llm.endpoint, (v) => save("llm", "endpoint", v), { wide: true, mono: true, placeholder: "http://127.0.0.1:11434" })),
      row("Model", S.llm.error ? h("span", { style: { color: "var(--danger)" } }, S.llm.error) : "Leave empty to use the server's largest loaded model.",
        [S.llm.models && S.llm.models.length ? select([["", "Largest loaded"]].concat(S.llm.models.map((m) => [m, m])), c.llm.model, (v) => saveQuiet("llm", "model", v))
          : textField(c.llm.model, (v) => save("llm", "model", v), { mono: true, placeholder: "model name" }),
         button("Find models", findModels, { small: true, disabled: !c.llm.endpoint })]),
      row("API key variable", "Name of an environment variable holding a key — never the key itself.",
        textField(c.llm.api_key_env, (v) => save("llm", "api_key_env", v), { mono: true, placeholder: "OPENAI_API_KEY" })),
      row("Try it", t ? h("div", { class: "selectable" }, h("div", { class: "muted" }, "“" + t.input + "”"), h("div", { style: { marginTop: "4px" } }, "→ " + t.output),
        h("div", { class: "muted", style: { marginTop: "2px" } }, t.ms + " ms")) : "Sends a sample sentence and shows the polished result.",
        button(S.llm.busy ? "Testing…" : "Test", () => testLLM(c.llm), { small: true, disabled: !c.llm.endpoint || S.llm.busy })),
    ]));
}

async function findModels() {
  try {
    S.llm.models = await api("POST", "/api/llm/models", { endpoint: S.config.llm.endpoint, api_key_env: S.config.llm.api_key_env });
    S.llm.error = S.llm.models.length ? "" : "The server lists no models.";
  } catch (e) {
    S.llm.error = e.message;
  }
  render();
}

async function testLLM(cfg) {
  S.llm.busy = true;
  S.llm.error = "";
  render();
  try {
    S.llm.test = await api("POST", "/api/llm/test", cfg);
  } catch (e) {
    S.llm.error = e.message;
    S.llm.test = null;
  }
  S.llm.busy = false;
  render();
}

// ---- pages: meetings -----------------------------------------------------------------------------

function meetings() {
  const c = S.config;
  const list = S.meetings || [];
  return page("Meetings", "Record a conversation from the pill's record button; stop to get a transcript with speakers labelled.",
    banner("info", "info", "Microphone only", "Voices that only play in your headphones aren't recorded. Speaker labels are acoustic estimates — similar voices can be merged."),
    group("Summary", [
      row("Endpoint", "OpenAI-compatible base URL. Leave empty to keep meetings fully offline (transcript only).",
        textField(c.meeting.summary_endpoint, (v) => save("meeting", "summary_endpoint", v), { wide: true, mono: true, placeholder: "http://127.0.0.1:11434/v1" })),
      row("Model", null, textField(c.meeting.summary_model, (v) => save("meeting", "summary_model", v), { mono: true, placeholder: "model name" })),
      row("API key variable", "Environment variable name, never the key.", textField(c.meeting.summary_api_key_env, (v) => save("meeting", "summary_api_key_env", v), { mono: true, placeholder: "OPENAI_API_KEY" })),
    ], "A summary failure never loses the transcript."),
    h("section", { class: "section" },
      h("div", { style: { display: "flex", alignItems: "center", marginBottom: "8px" } }, h("h3", { class: "section-title", style: { margin: "0 0 0 2px" } }, "Notes"),
        h("span", { class: "spacer" }), button("Open folder", () => reveal("meetings"), { kind: "plain", small: true, icon: "folder" })),
      h("div", { class: "group" }, list.length ? list.map(meetingItem) : h("div", { class: "empty" }, "No meeting notes yet. Hover the pill and click the red record button."))));
}

function meetingItem(m) {
  const text = h("div", { class: "list-text selectable" }, m.excerpt || "");
  return h("div", { class: "list-item" },
    h("div", { class: "list-main" }, h("div", { class: "row-title" }, m.title.replace(/^Meeting · /, "")),
      h("div", { class: "list-meta" }, ago(m.modified), m.summary ? badge("Summary", "accent") : badge("Transcript")), text),
    h("div", { class: "list-actions" },
      h("button", { class: "icon-btn", title: "Open", onclick: () => api("POST", "/api/meetings/" + encodeURIComponent(m.name) + "/open").catch((e) => toast(e.message, "error")) }, svg(I.open)),
      h("button", { class: "icon-btn", title: "Show in folder", onclick: () => api("POST", "/api/meetings/" + encodeURIComponent(m.name) + "/open?reveal=1").catch((e) => toast(e.message, "error")) }, svg(I.folder)),
      h("button", { class: "icon-btn", title: "Delete", onclick: async () => {
        if (!confirm("Delete these meeting notes?")) return;
        await api("DELETE", "/api/meetings/" + encodeURIComponent(m.name)).catch((e) => toast(e.message, "error"));
        S.meetings = await api("GET", "/api/meetings");
        render();
      } }, svg(I.trash))));
}

// ---- pages: appearance -------------------------------------------------------------------------------

const ACCENTS = [["gradient", "Gradient"], ["", "Monochrome"], ["#0a84ff", "Blue"], ["#5e5ce6", "Indigo"], ["#bf5af2", "Purple"],
  ["#ff375f", "Pink"], ["#ff9f0a", "Orange"], ["#ffd60a", "Yellow"], ["#30d158", "Green"]];

function appearance() {
  const c = S.config;
  const custom = c.ui.accent && !ACCENTS.some(([v]) => v === c.ui.accent);
  const swatches = h("div", { class: "swatches" }, ACCENTS.map(([v, name]) =>
    h("button", { class: "swatch" + (v === "" ? " mono" : v === "gradient" ? " gradient" : "") + (c.ui.accent === v ? " on" : ""),
      title: name, "aria-label": name, style: v.startsWith("#") ? { background: v } : {}, onclick: () => saveAppearance("accent", v) })),
    h("label", { class: "swatch custom" + (custom ? " on" : ""), title: "Custom color", style: custom ? { background: c.ui.accent } : {} },
      h("input", { type: "color", value: custom ? c.ui.accent : "#4f8cff", "aria-label": "Custom color",
        onchange: (e) => saveAppearance("accent", e.target.value) })));
  const pillDark = c.ui.theme === "dark" || (c.ui.theme === "auto" && prefersDark());
  const previewColor = c.ui.preview_color || (pillDark ? "#ffffff" : "#1d1d1f");
  return page("Appearance", "The pill lives on the right edge of your screen. Drag it up or down to move it.",
    h("div", { class: "previews" },
      pillPreview("hover-dictate", "Hover: dictate or take meeting notes"),
      pillPreview("recording", "Dictating with live preview"),
      pillPreview("meeting", "Recording a meeting"),
      pillPreview("processing", "Transcribing")),
    group("Pill", [
      row("Theme", null, segmented([["dark", "Dark"], ["light", "Light"], ["auto", "Match system"]], c.ui.theme, (v) => saveAppearance("theme", v))),
      row("Waveform color", "Gradient colors the waveform blue to rose; Monochrome is the classic black-and-white look.", swatches),
      row("Show the resting pill", "When off, the pill appears only while recording; use shortcuts, fn or the menu bar to start.",
        toggle(c.ui.idle_handle, (v) => saveAppearance("idle_handle", v))),
      row("Live preview", "Shows the last few words beside the pill while you speak.", toggle(c.ui.live_preview, (v) => saveAppearance("live_preview", v))),
      row("Preview text color", c.ui.preview_color ? null : "Matches the pill theme.", [
        c.ui.preview_color ? button("Default", () => saveAppearance("preview_color", ""), { kind: "plain", small: true }) : null,
        h("label", { class: "color-dot", title: "Choose color", style: { background: previewColor } },
          h("input", { type: "color", value: previewColor, "aria-label": "Preview text color", onchange: (e) => saveAppearance("preview_color", e.target.value) }))]),
      row("Position", "Drag the pill along the edge to move it.", button("Reset to center", resetPosition, { small: true })),
    ]),
    group("Feedback", [
      row("Sounds", "Soft chimes when recording starts, text is pasted, or something needs attention.", toggle(c.ui.sounds, (v) => saveQuiet("ui", "sounds", v))),
      row("Show the overlay", "Turn off to rely on the menu bar icon only.", toggle(c.overlay.enabled, (v) => saveQuiet("overlay", "enabled", v))),
    ]));
}

async function saveAppearance(key, value) {
  S.previewNonce++;
  await saveQuiet("ui", key, value);
}

async function resetPosition() {
  await api("POST", "/api/overlay/reset-position").catch((e) => toast(e.message, "error"));
  toast("Pill centered");
}

// ---- pages: models ----------------------------------------------------------------------------------

function modelsPage() {
  const all = (S.models && S.models.models) || [];
  const groupRows = (g) => all.filter((m) => m.group === g).map((m) => {
    const p = S.progress[m.id];
    let status = m.installed ? badge("Installed", "ok") : badge("Not installed", "warn");
    if (p && p.phase === "download") status = badge(Math.round((100 * p.done) / Math.max(1, p.total)) + "%", "accent");
    if (p && p.phase === "extract") status = badge("Unpacking", "accent");
    return h("div", { class: "model" },
      h("div", {}, h("div", { class: "model-title" }, m.title), h("div", { class: "model-detail" }, m.detail, " · ", bytes(m.size), " · ",
        m.license_url ? h("a", { href: m.license_url, target: "_blank" }, m.license) : m.license)),
      status,
      p && p.phase === "download" ? h("div", { class: "progress", style: { gridColumn: "1 / -1" } }, h("i", { style: { width: (100 * p.done / Math.max(1, p.total)).toFixed(1) + "%" } })) : null);
  });
  const coreMissing = all.some((m) => m.group === "core" && !m.installed);
  const meetingMissing = all.some((m) => m.group === "meeting" && !m.installed);
  const overall = Object.values(S.progress).find((p) => p.total_bytes);
  return page("Models", "Speech recognition runs entirely on this computer. Models download once from their pinned sources and are checked with SHA-256.",
    S.installError ? banner("danger", "warn", "Download failed", S.installError) : null,
    S.installing && overall ? h("div", { class: "group", style: { padding: "12px 14px", marginBottom: "6px" } },
      h("div", { style: { display: "flex" } }, h("strong", {}, "Downloading"), h("span", { class: "spacer" }),
        h("span", { class: "muted" }, bytes(overall.total_done) + " of " + bytes(overall.total_bytes)), "\u00a0\u00a0",
        button("Cancel", () => api("POST", "/api/models/cancel"), { kind: "plain", small: true })),
      h("div", { class: "progress" }, h("i", { style: { width: (100 * overall.total_done / Math.max(1, overall.total_bytes)).toFixed(1) + "%" } }))) : null,
    h("section", { class: "section" }, h("div", { style: { display: "flex", alignItems: "center", marginBottom: "8px" } },
      h("h3", { class: "section-title", style: { margin: "0 0 0 2px" } }, "Dictation"), h("span", { class: "spacer" }),
      coreMissing ? button("Download", () => installModels("core"), { kind: "primary", small: true, icon: "download", disabled: S.installing }) : null),
      h("div", { class: "group" }, groupRows("core"))),
    h("section", { class: "section" }, h("div", { style: { display: "flex", alignItems: "center", marginBottom: "8px" } },
      h("h3", { class: "section-title", style: { margin: "0 0 0 2px" } }, "Meetings and collaborative drafts"), h("span", { class: "spacer" }),
      meetingMissing ? button("Download", () => installModels("meeting"), { kind: "primary", small: true, icon: "download", disabled: S.installing }) : null),
      h("div", { class: "group" }, groupRows("meeting"))),
    group("Location", [row("Models folder", h("span", { class: "path selectable" }, S.models ? S.models.dir : ""),
      button("Show", () => reveal("models"), { kind: "plain", small: true }))]));
}

async function installModels(group) {
  S.installError = "";
  S.installing = true;
  S.progress = {};
  render();
  try {
    await api("POST", "/api/models/install", { group });
  } catch (e) {
    S.installing = false;
    S.installError = e.message;
    render();
  }
}

// ---- pages: history ------------------------------------------------------------------------------------

function historyPage() {
  const c = S.config;
  const q = (S.historyQuery || "").toLowerCase();
  const items = (S.history || []).filter((e) => !q || e.text.toLowerCase().includes(q));
  const recovery = S.recovery || [];
  const kind = { draft: ["In progress", "accent"], commit: ["Not pasted yet", "warn"], failed: ["Paste failed", "danger"] };
  const copy = (text) => api("POST", "/api/copy", { text }).then(() => toast("Copied")).catch((e) => toast(e.message, "error"));
  return page("History", "Everything stays on this computer.",
    recovery.length ? h("section", { class: "section" }, h("h3", { class: "section-title" }, "Saved for recovery"),
      h("div", { class: "group" }, recovery.map((e) => h("div", { class: "list-item" },
        h("div", { class: "list-main" }, h("div", { class: "list-meta" }, ago(e.ts), badge(...(kind[e.kind] || [e.kind]))),
          h("div", { class: "list-text selectable" }, e.text)),
        h("div", { class: "list-actions", style: { opacity: 1 } },
          button("Copy", () => copy(e.text), { small: true, icon: "copy" }),
          h("button", { class: "icon-btn", title: "Discard", onclick: async () => {
            await api("DELETE", "/api/recovery/" + e.id);
            S.recovery = await api("GET", "/api/recovery");
            render();
          } }, svg(I.trash)))))),
      h("p", { class: "section-note" }, "Text that wasn't pasted is kept for " + Math.round(c.dictation.recovery_ttl / 3600) + " hours.")) : null,
    h("section", { class: "section" },
      h("div", { style: { display: "flex", alignItems: "center", gap: "8px", marginBottom: "8px" } },
        h("h3", { class: "section-title", style: { margin: "0 0 0 2px" } }, "Dictations"), h("span", { class: "spacer" }),
        toggle(c.dictation.keep_history, (v) => saveQuiet("dictation", "keep_history", v)), h("span", { class: "muted" }, "Keep history"),
        (S.history || []).length ? button("Clear…", async () => {
          if (!confirm("Delete all dictation history?")) return;
          await api("DELETE", "/api/history");
          S.history = [];
          render();
        }, { kind: "plain", small: true }) : null),
      (S.history || []).length > 5 ? h("div", { class: "search" }, h("input", { class: "field", placeholder: "Search history", value: S.historyQuery || "", type: "search",
        oninput: (e) => { S.historyQuery = e.target.value; const pos = e.target.selectionStart; render(); const el = document.querySelector(".search input"); if (el) { el.focus(); el.setSelectionRange(pos, pos); } } })) : null,
      h("div", { class: "group" }, items.length ? items.map((e) => h("div", { class: "list-item" },
        h("div", { class: "list-main" }, h("div", { class: "list-meta" }, ago(e.ts), e.mode && e.mode !== "dictation" ? badge(MODE_TITLES[e.mode] || e.mode) : null),
          h("div", { class: "list-text selectable", onclick: (ev) => ev.currentTarget.classList.toggle("full") }, e.text)),
        h("div", { class: "list-actions" },
          h("button", { class: "icon-btn", title: "Copy", onclick: () => copy(e.text) }, svg(I.copy)),
          h("button", { class: "icon-btn", title: "Delete", onclick: async () => {
            await api("DELETE", "/api/history/" + e.key);
            S.history = S.history.filter((x) => x.key !== e.key);
            render();
          } }, svg(I.trash))))) : h("div", { class: "empty" }, q ? "No matches." : c.dictation.keep_history ? "Your dictations will appear here." : "History is off."))));
}

// ---- pages: system ----------------------------------------------------------------------------------------

function system() {
  const st = S.status;
  const perms = (st && st.permissions) || [];
  const login = S.boot.login;
  const c = S.config;
  const tone = { granted: ["Allowed", "ok"], prompt: ["Not asked yet", "warn"], denied: ["Not allowed", "danger"], missing: ["Missing", "danger"], optional: ["Optional", ""] };
  return page("System", null,
    S.boot.config_error ? banner("danger", "warn", "config.toml has an error", S.boot.config_error + " — defaults are in use until it's fixed.",
      button("Show file", () => reveal("config"), { small: true })) : null,
    group(isMac() ? "Permissions" : "Desktop integration", perms.map((p) => row(p.title, p.detail,
      [badge(...(tone[p.state] || [p.state])), p.state !== "granted" && p.action === "request" ? button("Allow…", () => requestPermission(p.id), { small: true }) : null,
        p.state !== "granted" && p.action === "settings" ? button("Open Settings", () => requestPermission(p.id), { small: true }) : null]))),
    group("Startup", [
      row("Open at login", login.supported ? (login.approval ? "Approve Voice IO in System Settings › General › Login Items." : "Starts quietly in the menu bar.") :
        isMac() ? "Available in the installed Voice IO app." : "Install the voice-io systemd user unit to enable this.",
        toggle(login.enabled, async (v) => {
          try {
            S.boot.login = await api("PUT", "/api/login", { enabled: v });
          } catch (e) {
            toast(e.message, "error");
          }
          render();
        }, !login.supported)),
    ]),
    group("Sessions", [
      row("Longest session", "Recording finishes automatically after this long.", select([[600, "10 minutes"], [1800, "30 minutes"], [3600, "1 hour"], [7200, "2 hours"], [10800, "3 hours"]].map(([v, l]) => [String(v), l]),
        String(Math.round(c.dictation.session_max_s)), (v) => saveQuiet("dictation", "session_max_s", Number(v)))),
      row("Keep unpasted text for", null, select([[3600, "1 hour"], [86400, "24 hours"], [604800, "7 days"]].map(([v, l]) => [String(v), l]),
        String(Math.round(c.dictation.recovery_ttl)), (v) => saveQuiet("dictation", "recovery_ttl", Number(v)))),
    ]),
    group("Data on this computer", ["config", "data", "logs"].map((k) => row({ config: "Settings file", data: "Data", logs: "Log" }[k],
      h("span", { class: "path selectable" }, S.boot.paths[k]), button("Show", () => reveal(k), { kind: "plain", small: true })))),
    group("About", [
      row("Voice IO", "Version " + S.boot.version + " · " + (isMac() ? "macOS" : "Linux"), null),
      row("Speech engine", "sherpa-onnx with NVIDIA Parakeet TDT (transcription), X-ASR (live preview), Silero VAD and a Zipformer wake-word model.", null),
    ]));
}

async function requestPermission(id) {
  try {
    await api("POST", "/api/permissions/" + id + "/request");
  } catch (e) {
    toast(e.message, "error");
  }
}

async function reveal(target) {
  try {
    await api("POST", "/api/reveal", { target });
  } catch (e) {
    toast(e.message, "error");
  }
}

// ---- live events ---------------------------------------------------------------------------------------------

function connectEvents() {
  const es = new EventSource("/api/events?token=" + encodeURIComponent(TOKEN));
  es.onmessage = (msg) => {
    const ev = JSON.parse(msg.data);
    if (ev.type === "status") {
      const before = JSON.stringify(S.status && [S.status.issues, S.status.permissions, S.status.blocking]);
      S.status = ev.data;
      renderChrome();
      if (before !== JSON.stringify([S.status.issues, S.status.permissions, S.status.blocking]) && ["overview", "system", "dictation"].includes(S.page)) render();
    } else if (ev.type === "config") {
      S.config = ev.data;
      if (!S.recordingShortcut) render();
    } else if (ev.type === "models") {
      const p = ev.data;
      if (p.phase === "done" || p.phase === "error") {
        S.installing = false;
        S.progress = {};
        if (p.phase === "error") S.installError = p.error;
        else toast("Models installed");
        api("GET", "/api/models").then((m) => { S.models = m; render(); });
      } else {
        S.installing = true;
        S.progress[p.ModelID] = { phase: p.Phase, done: p.Done, total: p.Total, total_done: p.TotalDone, total_bytes: p.TotalBytes };
        if (S.page === "models" || S.page === "overview") render();
      }
    } else if (ev.type === "mictest") {
      const d = ev.data;
      if (d.error) S.mic = { testing: false, level: 0, peak: null, error: d.error };
      else if (d.done) S.mic = { testing: false, level: 0, peak: d.peak, error: "" };
      else S.mic.level = d.level;
      const bar = document.querySelector("#mic-meter > i");
      if (bar && !d.done && !d.error) bar.style.width = Math.round(d.level * 100) + "%";
      else if (S.page === "overview") render();
    }
  };
  es.onerror = () => {
    document.getElementById("brand-status").replaceChildren(h("span", { class: "dot warn" }), h("span", {}, "Reconnecting…"));
  };
}

window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => {
  S.previewNonce++;
  render();
});

async function main() {
  if (!TOKEN) {
    document.getElementById("content").replaceChildren(h("div", { class: "loading" }, "Open Settings from the Voice IO menu."));
    return;
  }
  try {
    S.boot = await api("GET", "/api/bootstrap");
  } catch (e) {
    document.getElementById("content").replaceChildren(h("div", { class: "loading" }, "Settings are unavailable: " + e.message));
    return;
  }
  S.config = S.boot.config;
  S.status = S.boot.status;
  S.models = S.boot.models;
  S.installing = !!(S.models && S.models.installing);
  S.shortcutError = S.boot.shortcut_error || "";
  document.documentElement.dataset.platform = S.boot.platform;
  renderChrome();
  await loadPage(S.page);
  render();
  connectEvents();
}

main();
