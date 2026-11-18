// UI translations (owner, 2026-11-18). The web UI only: the API, logs and
// notifications stay English.
//
// tr("English text", {name: value}) looks the English text up in the
// chosen language's catalogue (/static/i18n/<lang>.json, a flat
// English → translation map) and fills {name} placeholders; a missing
// entry falls back to the English. index.html marks its static text with
// data-i18n="English" (and data-i18n-aria-label, -title, -placeholder).
//
// The language: the one picked in the UI (kept on this device), else the
// browser's first supported language, else English. Changing it reloads
// the page. The catalogue loads before app.js runs (top-level await), so
// app.js may call tr() while it initialises.
//
// The translations are machine-made; internal/web's TestCatalogues keeps
// every catalogue complete and its placeholders matching.

export const LANGS = [
  ["en", "English"],
  ["de", "Deutsch"],
  ["fr", "Français"],
  ["it", "Italiano"],
  ["es", "Español"],
  ["zh-Hans", "简体中文"],
];

const KEY = "fl.lang";

let lang = "en";
let cat = {};

function stored() {
  try { return localStorage.getItem(KEY); } catch { return null; }
}

// detect: the stored choice, else the browser's first supported language.
function detect() {
  const known = (c) => LANGS.some(([k]) => k === c);
  const want = stored();
  if (want && known(want)) return want;
  for (const l of navigator.languages ?? [navigator.language ?? "en"]) {
    const low = String(l).toLowerCase();
    if (low.startsWith("zh")) return "zh-Hans";
    const base = low.split("-")[0];
    if (known(base)) return base;
  }
  return "en";
}

// tr translates one UI string; vars fill its {placeholders}.
export function tr(s, vars) {
  let out = cat[s] ?? s;
  if (vars) out = out.replace(/\{(\w+)\}/g, (m, k) => (k in vars ? String(vars[k]) : m));
  return out;
}

// lang is the UI language code.
export const currentLang = () => lang;

// locale is the language for Intl formatting (dates, numbers): undefined
// for English, so English users keep their browser's regional format.
export const locale = () => (lang === "en" ? undefined : lang);

// setLang keeps a choice on this device and reloads into it.
export function setLang(code) {
  try { localStorage.setItem(KEY, code); } catch { /* private mode: this page only */ }
  location.reload();
}

// translateDOM applies the catalogue to index.html's marked elements.
function translateDOM() {
  for (const el of document.querySelectorAll("[data-i18n]")) el.textContent = tr(el.dataset.i18n);
  for (const attr of ["aria-label", "title", "placeholder"]) {
    const data = "i18n" + attr.replace(/(^|-)(\w)/g, (_, __, c) => c.toUpperCase());
    for (const el of document.querySelectorAll(`[data-i18n-${attr}]`)) el.setAttribute(attr, tr(el.dataset[data]));
  }
}

lang = detect();
document.documentElement.lang = lang;
if (lang !== "en") {
  try {
    const r = await fetch(`/static/i18n/${lang}.json`);
    if (r.ok) cat = await r.json();
  } catch { /* offline or blocked: English */ }
}
translateDOM();
