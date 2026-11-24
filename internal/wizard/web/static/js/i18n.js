// The wizard's UI translations: the same scheme as foxess-local's web UI
// (internal/web/static/js/i18n.js). tr("English text", {name: value})
// looks the text up in static/i18n/<lang>.json and fills {name}
// placeholders; a missing entry falls back to the English. index.html
// marks its static text with data-i18n="English".
//
// The language: the one picked here (kept on this device), else the
// browser's first supported language, else English. The catalogues are
// machine-made; the wizard's TestCatalogues keeps them complete.

export const LANGS = [
  ["en", "English"],
  ["de", "Deutsch"],
  ["fr", "Français"],
  ["it", "Italiano"],
  ["es", "Español"],
  ["zh-Hans", "简体中文"],
];

const KEY = "fw.lang";
let lang = "en";
let cat = {};

function stored() {
  try { return localStorage.getItem(KEY); } catch { return null; }
}

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

export const currentLang = () => lang;

export function setLang(code) {
  try { localStorage.setItem(KEY, code); } catch { /* private mode: this page only */ }
  location.reload();
}

lang = detect();
document.documentElement.lang = lang;
if (lang !== "en") {
  try {
    const r = await fetch(new URL(`static/i18n/${lang}.json`, document.baseURI));
    if (r.ok) cat = await r.json();
  } catch { /* offline: English */ }
}
for (const el of document.querySelectorAll("[data-i18n]")) el.textContent = tr(el.dataset.i18n);
for (const el of document.querySelectorAll("[data-i18n-placeholder]")) el.setAttribute("placeholder", tr(el.dataset.i18nPlaceholder));
