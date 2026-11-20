// app.js — foxesslocal operator UI (plan 2, task 11).
//
// htmx 4.0.1 + the htmx-4 hx-sse extension (vendored; pinned in
// internal/web/vendor.txt). Consumes /api/v1 only (C-005); the HTML
// itself carries no data — every datum is an authenticated API call.
//
// No inline handlers or styles (the CSP has no 'unsafe-inline'): all
// interactions are delegated listeners + data-action / data-cmd
// attributes. Commands are typed JSON under the strict CSP (no
// 'unsafe-eval'): htmx 4 evaluates js: attribute content via
// new Function, which the CSP blocks — so no js: anywhere. Instead:
//   - static bodies: plain JSON in hx-vals (htmx JSON.parses it, no
//     eval) — e.g. the work-mode buttons;
//   - dynamic bodies: named form fields (htmx collects them) + the
//     hx-encoding="json" marker; the htmx:before:request hook turns
//     the collected values into the typed JSON body (htmx 4 has no
//     built-in JSON encoding — its default body is URLSearchParams).

import { tr, locale, LANGS, currentLang, setLang } from "./i18n.js";
import { smallMultiples } from "./charts.js";

// API is the API's path, relative to the page: the UI also works behind
// a reverse proxy under a path prefix (/foxess/ → /foxess/api/v1). A
// path, not a URL: the htmx hooks compare it with request paths.
const API = new URL("api/v1", document.baseURI).pathname;
const TOKEN_KEY = "fl.token";

const IMMEDIATE_MODES = ["self-use", "feed-in"];
const SCHED_MODES = ["self-use", "feed-in", "backup", "peak-shaving", "power-station", "force-charge", "force-discharge"];
const FRAME_TYPES = ["telemetry-150", "keepalive-135", "snap01", "snap02", "snap04", "snap60", "snap61", "snap65", "rssi", "modbus-read", "modbus-write", "modbus-read-response", "write-ack", "registration", "firmware-metadata", "firmware-chunk", "unknown"];
const ROLES = ["read-only", "standard", "expert", "installer"];

// storedToken: the remembered token (this device), else this tab's.
function storedToken() {
  for (const st of [localStorage, sessionStorage]) {
    try {
      const v = st.getItem(TOKEN_KEY);
      if (v) return v;
    } catch { /* blocked */ }
  }
  return "";
}

// tokenRemembered: whether the current token is kept on this device.
function tokenRemembered() {
  try { return !!localStorage.getItem(TOKEN_KEY); } catch { return false; }
}

const S = {
  token: storedToken(),
  state: null,           // full state (GET /state or a config event payload)
  forwardMode: null,     // {mode}
  feed: "—",             // live-stream state (connected | connecting… | reconnecting… | …)
  nowWindow: loadPref("fl.nowWindow", ["now", "5m", "30m"], "now"), // Now panel: instant or a trailing average
  avg: null,             // GET /power/average payload (averaged Now)
  histRange: loadPref("fl.histRange", ["1h", "6h", "24h", "72h", "7d", "30d"], "24h"), // dashboard history range
  public: false,          // unauthenticated public session (spec §5.5)
  epsSeen: false,        // the EPS port has supplied power (backup branch shown)
  epsRing: [],           // the last EPS_MEDIAN_N live backup-port readings (quirk eps_dropouts)
  quirks: {},            // id → on (GET /quirks)
  cfg: null,             // GET /config
  pageVersion: null,     // the version this page's code came from (first GET /config)
  reloadPending: false,  // another version is answering: the page reloads into it
  offlineSince: null,    // display ts of the last connection:offline
  cmdBusy: 0,            // in-flight command count (PUT/POST/DELETE only)
  busyWatchdog: null,    // timer that force-clears a stuck busy state
  roleLocked: new Map(), // cmd -> required role (sticky until sign-out)
  audit: { items: [], before: null, exhausted: false },
  auditFilters: { source: "", action: "" },
  messages: { items: [], before: null, exhausted: false },
  msgFilters: { dir: "", frame: "" },
};

// ---------- helpers ----------

// loadPref/savePref: per-browser conveniences only — storage can be
// unavailable (private mode, blocked site data); the page works without it.
function loadPref(key, allowed, dflt) {
  try {
    const v = localStorage.getItem(key);
    return allowed.includes(v) ? v : dflt;
  } catch {
    return dflt;
  }
}
function savePref(key, v) {
  try { localStorage.setItem(key, v); } catch { /* not persisted */ }
}

const $ = (sel, root = document) => root.querySelector(sel);
const byId = (id) => document.getElementById(id);

function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) => (
    { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]
  ));
}

const fmtW = (v) => (v == null ? "—" : (v > 0 ? "+" : "") + v + " W");
const fmtV = (v) => (v == null ? "—" : v.toFixed(2) + " V");
const fmtWh = (v) => (v == null ? "—" : (v / 1000).toFixed(2) + " kWh");
const fmtBytes = (n) => (n == null ? "—" : n + " B");
const fmtTime = (s) => (s ? new Date(s).toLocaleString(locale()) : "—");
const fmtRole = (r) => (r == null ? tr("cloud") : r);

async function apiFetch(path, init = {}) {
  const headers = { ...(init.headers || {}) };
  if (S.token) headers.Authorization = "Bearer " + S.token;
  return fetch(API + path, { ...init, headers });
}

async function problemOf(resp) {
  try { return await resp.json(); }
  catch { return { code: "unknown", title: resp.statusText, status: resp.status }; }
}

// ---------- toasts (fixed reserved region — no reflow) ----------

function toast(msg, kind = "info", detail) {
  const box = byId("toasts");
  const t = document.createElement("div");
  t.className = "toast " + kind;
  t.textContent = msg;
  if (detail) {
    const d = document.createElement("div");
    d.className = "toast-detail";
    d.textContent = detail;
    t.appendChild(d);
  }
  box.appendChild(t);
  while (box.children.length > 5) box.firstChild.remove();
  setTimeout(() => {
    t.classList.add("out");
    setTimeout(() => t.remove(), 320);
  }, 6000);
}

// ---------- gate / login ----------

function authed() { return S.token !== "" || S.public; }

// dashOnly: a tokenless session sees the dashboard alone, without the
// page tabs, unless the server allows every page ([access]
// public_all_pages). The server enforces it too (401 on the rest).
function dashOnly() { return S.public && !S.cfg?.public_all_pages; }

// paintNav shows or hides the page tabs for the session.
function paintNav() {
  const only = dashOnly();
  byId("tabs").hidden = only;
  byId("tabbar").hidden = only;
  byId("app").classList.toggle("dash-only", only);
}

// The token lives in this tab's session, or — "Keep me signed in on this
// device" — in localStorage until signing out. Storage can be blocked
// (private windows): then it lasts as long as the page.
function setToken(t, remember = tokenRemembered()) {
  S.token = t;
  for (const st of [localStorage, sessionStorage]) {
    try { st.removeItem(TOKEN_KEY); } catch { /* blocked */ }
  }
  if (!t) return;
  try { (remember ? localStorage : sessionStorage).setItem(TOKEN_KEY, t); } catch { /* not persisted */ }
}

function gate(msg) {
  teardownSSE();
  stopHistTimer();
  stopFreshTimer();
  stopAvgTimer();
  S.state = null;
  S.public = false;
  S.forwardMode = null;
  S.cfg = null;
  S.offlineSince = null;
  S.cmdBusy = 0;
  clearTimeout(S.busyWatchdog);
  renderPending = false;
  setBusy(false);
  S.feed = "—";
  paintConn();
  byId("forward-badge").textContent = tr("cloud: {mode}", { mode: "—" });
  byId("forward-badge").className = "badge";
  byId("public-badge").hidden = true;
  byId("banner-inverter").hidden = true;
  byId("banner-feed").hidden = true;
  byId("app").hidden = true;
  byId("app").setAttribute("inert", "");
  byId("gate-msg").textContent = msg || "";
  byId("gate").hidden = false;
  openLogin();
}

function openLogin() {
  byId("login-error").textContent = "";
  byId("login-token").value = "";
  byId("dlg-login").showModal();
  byId("login-token").focus();
}

async function signOut() {
  setToken("");
  S.roleLocked.clear();
  if (await probePublic()) return;
  gate(tr("signed out — sign in to continue"));
}

// probePublic: check unauthenticated read-only access (spec §5.5 — the
// config flag `[access] public_read_only`, default on). Returns true when the dashboard is
// viewable without a token.
async function probePublic() {
  S.public = false;
  try {
    const resp = await fetch(API + "/state");
    if (resp.ok) {
      S.public = true;
      S.state = await resp.json();
      S.forwardMode = { mode: S.state.forward_mode };
      // the page set depends on public_all_pages: know it before the
      // first paint (unknown keeps the dashboard alone)
      try {
        const c = await fetch(API + "/config");
        S.cfg = c.ok ? await c.json() : null;
      } catch { S.cfg = null; }
      enterApp();
      return true;
    }
  } catch {
    // unreachable — fall through to the gate
  }
  return false;
}

function enterApp() {
  byId("dlg-login").close();
  byId("gate").hidden = true;
  const app = byId("app");
  app.hidden = false;
  app.removeAttribute("inert");
  setOnline(S.state?.online);
  renderForwardBadge();
  byId("public-badge").hidden = !S.public;
  const sb = byId("signout");
  sb.textContent = S.public ? tr("sign in") : tr("sign out");
  sb.dataset.action = S.public ? "sign-in" : "sign-out";
  paintNav();
  if (!location.hash || dashOnly()) history.replaceState(null, "", "#/dashboard");
  render();
  connectSSE();
  refreshMeta();
  refreshUpdateBadge();
  loadQuirks();
  setInterval(() => refreshUpdateBadge(), 30 * 60 * 1000);
  maybeStartSetup();
}

// maybeStartSetup opens the setup wizard once per browser session for an
// installer whose setup is not finished (F12). The UI cannot see a
// token's role, so it asks: GET /setup answers installers only.
async function maybeStartSetup() {
  if (S.public || routeName() === "setup") return;
  try {
    if (sessionStorage.getItem("fl.setupOffered")) return;
  } catch { /* storage blocked: offer every time */ }
  const r = await apiFetch("/setup");
  if (!r.ok) return;
  const s = await r.json();
  if (s.completed) return;
  try { sessionStorage.setItem("fl.setupOffered", "1"); } catch { /* best effort */ }
  location.hash = "#/setup";
}

async function tryLogin(t) {
  const resp = await fetch(API + "/state", { headers: { Authorization: "Bearer " + t } });
  if (resp.status === 401 || !resp.ok) return false;
  setToken(t, byId("login-remember")?.checked ?? true);
  S.public = false; // signed in from a tokenless session: full UI
  S.state = await resp.json();
  S.forwardMode = { mode: S.state.forward_mode };
  enterApp();
  return true;
}

async function loginSubmit() {
  const t = byId("login-token").value.trim();
  const errEl = byId("login-error");
  if (!t) { errEl.textContent = tr("enter a token"); return; }
  errEl.textContent = "";
  let ok = false;
  try { ok = await tryLogin(t); }
  catch { ok = false; }
  if (!ok) {
    setToken("");
    errEl.textContent = tr("that token was rejected — create one under More → Settings → Access tokens (installer role)");
    byId("login-token").select();
  }
}

// ---------- htmx request plumbing ----------

// Every htmx request carries the token (header — the UI never uses
// ?token=; the header works on every endpoint, SSE connect included).
// body spec per command: field name -> "int" (coerced) | "num" |
// "string" | "bool" (a checkbox: present = true).
// "schedule" is nested (groups[]) and handled by scheduleBody.
const BODY_SPECS = {
  limits: { import_w: "int", export_w: "int" },
  "current-limits": { import_current_a: "num", export_current_a: "num" },
  "port-limits": { load_limit_w: "int", generation_limit_w: "int" },
  "battery-currents": { max_charge_current_a: "num", max_discharge_current_a: "num" },
  "power-limit": { power_limit_pct: "int" },
  "eps-mode": { eps_mode: "string" },
  "meter-comp": { w: "int" },
  forward: { mode: "string" },
  "maint-open": { duration: "string", capture: "bool" },
  tokens: { name: "string", role: "string" },
};

function cmdOf(elt) {
  return elt?.dataset?.cmd ?? elt?.closest("form")?.dataset?.cmd ?? null;
}

// jsonBody builds the typed command body for a json-marked request
// (no eval — static hx-vals JSON is parsed by htmx itself; form
// fields arrive as strings and are coerced per the body spec).
function jsonBody(ctx) {
  const el = ctx.sourceElement;
  if (el?.hasAttribute("hx-vals")) return ctx.vals ?? null; // static JSON
  const f = el?.matches("form") ? el : el?.closest("form");
  if (!f) return null;
  const cmd = cmdOf(el);
  if (cmd === "schedule") return scheduleBody(f);
  const spec = BODY_SPECS[cmd];
  if (!spec) return null;
  const vals = ctx.request.body;
  const out = {};
  for (const [k, t] of Object.entries(spec)) {
    const v = vals?.get?.(k);
    if (t === "bool") { out[k] = v != null; continue; }
    if (v == null) return null;
    out[k] = t === "int" ? Math.trunc(Number(v)) : t === "num" ? Number(v) : v;
    if ((t === "int" || t === "num") && Number.isNaN(out[k])) return null;
  }
  return out;
}

document.body.addEventListener("htmx:config:request", (e) => {
  const ctx = e.detail.ctx;
  if (S.token) ctx.request.headers.Authorization = "Bearer " + S.token;
});

// Typed JSON commands: htmx 4 has no built-in JSON encoding (its
// default body is URLSearchParams, which it (re)builds after
// config:request), so the body must be finalized at
// htmx:before:request — htmx passes the request object straight to
// fetch() right after that event.
document.body.addEventListener("htmx:before:request", (e) => {
  const ctx = e.detail.ctx;
  const el = ctx.sourceElement;
  if (!cmdOf(el) || el?.getAttribute("hx-encoding") !== "json") return;
  const body = jsonBody(ctx);
  if (body !== null) {
    ctx.request.body = JSON.stringify(body);
    ctx.request.headers["Content-Type"] = "application/json";
  }
});

// In-flight tracking: commands only (PUT/POST/DELETE under /api/v1).
// SSE connects are long-lived GETs — never counted (htmx:finally fires
// on their teardown, long after the command window).
function isCmd(ctx) {
  const m = ctx.request.method;
  return (m === "PUT" || m === "POST" || m === "DELETE") &&
    ctx.request.action.startsWith(API + "/");
}

document.body.addEventListener("htmx:before:request", (e) => {
  const ctx = e.detail.ctx;
  if (!isCmd(ctx) || ctx._cmdCounted) return; // count each request once
  ctx._cmdCounted = true;
  S.cmdBusy++;
  setBusy(true);
  // Safety net: htmx:finally:request is dispatched on the source
  // element; if that element is detached before the request settles
  // the event never reaches this listener and the count would stick.
  // (render() is gated on cmdBusy precisely to keep the element
  // attached — this watchdog covers everything else.)
  clearTimeout(S.busyWatchdog);
  S.busyWatchdog = setTimeout(() => {
    S.cmdBusy = 0;
    setBusy(false);
    renderNow();
  }, 15000);
});

document.body.addEventListener("htmx:finally:request", (e) => {
  const ctx = e.detail.ctx;
  if (!isCmd(ctx) || !ctx._cmdCounted) return;
  ctx._cmdCounted = false;
  S.cmdBusy = Math.max(0, S.cmdBusy - 1);
  if (S.cmdBusy === 0) {
    clearTimeout(S.busyWatchdog);
    renderNow(); // apply the render deferred while the command ran
  }
  setBusy(S.cmdBusy > 0);
});

function setBusy(on) {
  document.body.classList.toggle("busy", on);
  const view = byId("view");
  if (on) view.setAttribute("aria-busy", "true");
  else view.removeAttribute("aria-busy");
  for (const b of view.querySelectorAll("[data-cmd]")) b.disabled = on;
  applyRoleLocks(view);
}

function applyRoleLocks(root) {
  for (const b of root.querySelectorAll("[data-cmd]")) {
    const need = S.roleLocked.get(b.dataset.cmd);
    if (need) {
      b.disabled = true;
      b.title = tr("requires role: {role}", { role: need });
    }
  }
}

// ---------- errors (RFC 7807) ----------

const CODE_TEXT = {
  unauthorized: () => tr("the token was rejected"),
  write_budget: (p) => tr("too many changes to this setting in the last hour — protecting the inverter's memory; try again later") + (p.detail?.includes("next allowed at") ? ` (${tr("after {time}", { time: p.detail.replace(/.*next allowed at /, "").replace(/\)$/, "") })})` : ""),
  insufficient_space: (p) => tr("not enough free disk space for the update") + (p.detail ? ` (${p.detail.replace(/^update: [^:]*: /, "")})` : ""),
  insufficient_role: (p) => tr("requires role {need} (your token: {have})", { need: p.required_role, have: p.token_role }),
  not_found: () => tr("not found"),
  invalid_value: (p) => (p.errors?.length ? p.errors.map((er) => er.field + ": " + er.message).join(" · ") : tr("invalid value")),
  schedule_overlap: () => tr("partially overlapping enabled windows (containment and adjacency are legal)"),
  command_in_flight: () => tr("a command is already in flight — wait for it to complete"),
  conflict: () => tr("conflict — the state no longer matches; the list was refreshed"),
  inverter_offline: () => tr("the inverter is offline — the command was not sent"),
  command_failed: (p) => tr("no read-back from the inverter (applied: {applied})", { applied: p.applied == null ? tr("unknown") : p.applied }),
  internal: () => tr("internal server error"),
  update_not_possible: (p) => tr("this installation cannot update itself:") + " " + (p.detail ?? ""),
  update_not_available: () => tr("already on the latest release"),
  update_verification_failed: (p) => tr("the download failed verification and was not installed:") + " " + (p.detail ?? ""),
  update_failed: (p) => tr("update failed:") + " " + (p.detail ?? ""),
  write_unsupported: (p) => p.reason === "not_supported"
    ? tr("this inverter does not support that setting")
    : tr("this setting is not mapped for this inverter model yet — sharing a diagnostic bundle helps us add it"),
  write_unverified: () => tr("untested on this inverter model / firmware — an installer can enable untested writes on the control tab"),
  device_not_identified: () => tr("the inverter has not been identified yet — try again in a minute"),
  checks_failing: () => tr("a plausibility check is failing for this setting — the register map looks wrong, so writes are blocked"),
};

document.body.addEventListener("htmx:response:error", (e) => {
  const ctx = e.detail.ctx;
  let p = { code: "unknown", title: tr("request failed"), status: ctx.response.status };
  try { p = JSON.parse(ctx.text); }
  catch { /* non-JSON error body */ }
  if (ctx.response.status === 401) {
    gate(tr("unauthorized — the token was rejected; sign in again"));
    return;
  }
  const cmd = ctx.sourceElement?.dataset?.cmd;
  if (p.code === "insufficient_role" && cmd) {
    S.roleLocked.set(cmd, p.required_role);
    applyRoleLocks(byId("view"));
  }
  const warn = p.code === "command_in_flight" || p.code === "inverter_offline";
  toast(`${p.code}: ${CODE_TEXT[p.code] ? CODE_TEXT[p.code](p) : p.title}`, warn ? "warn" : "error");
});

// ---------- success (read-back / model update) ----------

document.body.addEventListener("htmx:after:request", (e) => {
  const ctx = e.detail.ctx;
  if (ctx.response.status >= 400) return;
  const path = ctx.request.action.replace(location.origin, "");
  let body = null;
  try { body = ctx.text ? JSON.parse(ctx.text) : null; }
  catch { /* 204 */ }

  if (path === API + "/work-mode" && body) {
    S.state.work_mode = body.work_mode;
    toast(tr("work mode confirmed: {mode}", { mode: body.work_mode }));
  } else if (path === API + "/limits" && body) {
    S.state.limits = { ...S.state.limits, ...body }; // keep the read-only current limits
    toast(tr("limits confirmed: import {imp} W · export {exp} W", { imp: body.import_w, exp: body.export_w }));
  } else if (SETTING_PATHS.includes(path.slice(API.length)) && body) {
    if (body.eps_mode) S.state.eps_mode = body.eps_mode;
    S.state.limits = { ...S.state.limits, ...body };
    toast(tr("confirmed:") + " " + Object.entries(body).map(([k, v]) => `${SETTING_LABEL[k] ?? k} ${v}`).join(" · "));
  } else if (path === API + "/meter-compensation" && body) {
    S.state.meter_compensation_w = body.w;
    toast(tr("meter compensation confirmed: {w} W", { w: body.w }));
  } else if (path === API + "/schedule" && body) {
    S.state.schedule = body;
    toast(tr("schedule confirmed (read-back)"));
  } else if (path === API + "/forward-mode" && body) {
    S.forwardMode = body;
    if (S.state) S.state.forward_mode = body.mode;
    renderForwardBadge();
    toast(tr("cloud forwarding: {mode} — the inverter reconnects under it within seconds", { mode: body.mode }));
  } else if (path === API + "/maintenance" && body) {
    toast(body.active
      ? tr("maintenance window open (until {time})", { time: fmtTime(body.expires_at) })
      : tr("maintenance window closed"));
  } else if (path === API + "/update/apply") {
    toast(tr("updating to {to} — the app restarts; this page reloads by itself when it is back", { to: body?.to ?? tr("the new release") }));
  } else if (path === API + "/update/check") {
    toast(body?.available ? tr("update available: {v}", { v: body.latest }) : tr("no newer release"));
    if (body) refreshUpdateBadge(body);
  } else if (path === API + "/write-override") {
    toast(body?.granted ? tr("untested writes enabled for this exact inverter") : tr("untested writes disabled"));
  } else if (path === API + "/config/tokens" && body) {
    if (body.token) openTokenReveal(body.token);
  }
  render();
});

// ---------- confirmations (data-confirm → custom dialog) ----------
// The htmx:confirm js: expression would need new Function (blocked by
// the CSP), so confirms ride the capture phase instead: stop the
// htmx-triggered request, show the dialog, then re-dispatch the
// action (with a skip flag) to let htmx issue it.

function confirmDlg(title, body, okLabel = tr("confirm")) {
  return new Promise((resolve) => {
    byId("confirm-title").textContent = title;
    byId("confirm-body").textContent = body;
    byId("confirm-ok").textContent = okLabel;
    byId("dlg-confirm").returnValue = "";
    byId("dlg-confirm").showModal();
    byId("dlg-confirm")._resolve = resolve;
  });
}

byId("dlg-confirm").addEventListener("close", (e) => {
  const r = e.target._resolve;
  e.target._resolve = null;
  r?.(e.target.returnValue === "ok");
});

const CONFIRMS = {
  forward: (elt) => {
    const mode = document.querySelector('#forward-form input[name="mode"]:checked')?.value;
    if (mode !== "off") return Promise.resolve(true);
    return confirmDlg(
      tr("switch cloud forwarding to off?"),
      tr("The egress gate blocks the inverter's cloud channel entirely (the cloud goes dark; local telemetry and control continue)."),
      tr("switch to off"));
  },
  maintOpen: () => confirmDlg(
    tr("open the maintenance window?"),
    tr("Unfettered bidirectional comms until the deadline: the cloud may do anything, including firmware pushes. The configured mode is restored at the deadline."),
    tr("open")),
  maintClose: () => confirmDlg(
    tr("close the maintenance window?"),
    tr("The configured cloud-forwarding mode is restored immediately."),
    tr("close")),
  writeOverride: () => confirmDlg(
    tr("enable UNTESTED writes on this inverter?"),
    tr("The settings this app writes have not been verified on this inverter model and firmware. A write could go to a register that means something different here and misconfigure the inverter: limits, battery or grid behaviour. Only continue if you can watch the inverter and undo a change from its own app or display. The permission applies to this exact model and firmware only; any firmware change withdraws it. Every change is audited."),
    tr("I accept the risk — enable")),
  updateApply: (elt) => confirmDlg(
    tr("update to {to}?", { to: elt.dataset.to ?? tr("the latest release") }),
    tr("The new version is downloaded, its signature checked, and the app restarts into it (about a minute without live data). If it fails to start, the previous version is restored automatically."),
    tr("update")),
  writeOverrideRevoke: () => confirmDlg(
    tr("disable untested writes?"),
    tr("Writes return to verified-only for this inverter."),
    tr("disable")),
  epsMode: () => {
    const m = document.querySelector('#eps-form input[name="eps_mode"]:checked')?.value;
    return confirmDlg(
      tr("switch the backup port to {mode} mode?", { mode: m === "ups" ? "UPS" : "EPS" }),
      tr("The circuits on the backup (EPS) port may lose power briefly while the inverter changes over.") + " " +
      (m === "ups"
        ? tr("UPS mode switches them to the battery within milliseconds when the grid fails.")
        : tr("EPS mode switches them over more slowly (about a second or more) when the grid fails — sensitive equipment may restart.")),
      tr("switch"));
  },
  tokenRevoke: (elt) => confirmDlg(
    tr("revoke this token?"),
    tr("Token \"{name}\" ({role}) is rejected from its next request.", { name: elt.dataset.name ?? "?", role: elt.dataset.role ?? "?" }),
    tr("revoke")),
};

async function confirmGate(kind, elt, redo) {
  let ok = true;
  try { ok = CONFIRMS[kind] ? await CONFIRMS[kind](elt) : true; }
  catch { ok = false; }
  if (ok) redo();
}

// capture phase: runs before htmx's bubble-phase request wiring, so
// the request is held until the dialog resolves.
document.addEventListener("click", (e) => {
  const elt = e.target.closest("[data-confirm]");
  // a confirming form is the submit handler's: a click inside it (a
  // radio, its submit button) must reach the form as usual
  if (!elt || elt instanceof HTMLFormElement || elt.dataset.confirming) return;
  e.preventDefault();
  e.stopPropagation();
  confirmGate(elt.dataset.confirm, elt, () => {
    elt.dataset.confirming = "1";
    elt.click(); // re-dispatch: htmx issues the request this time
    delete elt.dataset.confirming;
  });
}, true);

document.addEventListener("submit", (e) => {
  const f = e.target;
  const elt = e.submitter ?? f.querySelector("[data-confirm]");
  if (!f.matches("[data-confirm]") && !elt?.dataset?.confirm) return;
  if (elt?.dataset?.confirming) return;
  e.preventDefault();
  e.stopPropagation();
  const kind = (f.matches("[data-confirm]") ? f : elt).dataset.confirm;
  confirmGate(kind, elt ?? f, () => {
    const skip = elt ?? f;
    skip.dataset.confirming = "1";
    f.requestSubmit(e.submitter ?? undefined);
    delete skip.dataset.confirming;
  });
}, true);

// ---------- one-time token reveal ----------

let lastToken = "";

function openTokenReveal(t) {
  lastToken = t;
  byId("token-plaintext").textContent = t;
  byId("dlg-token").showModal();
}

// ---------- SSE (live feed) ----------

let sseEl = null;

function connectSSE() {
  teardownSSE();
  const el = document.createElement("div");
  el.setAttribute("hx-sse:connect", API + "/events");
  el.setAttribute("hx-swap", "none");
  el.setAttribute("hidden", "");
  el.setAttribute("aria-hidden", "true");
  document.body.appendChild(el);
  sseEl = el;
  // htmx 4 has no MutationObserver: wire the JS-created element
  // explicitly. The extension's default trigger "load" fires
  // immediately on process.
  htmx.process(el);
}

// Teardown: strip the connect attr (so the forced process does not
// re-establish the stream), then force-cleanup (the extension aborts
// the fetch and fires htmx:sse:close), then remove the element.
function teardownSSE() {
  if (!sseEl) return;
  sseEl.removeAttribute("hx-sse:connect");
  htmx.process(sseEl, true);
  sseEl.remove();
  sseEl = null;
}

// paintConn: the one connection indicator — the live stream and the
// inverter's data freshness folded into a single status (status colors
// are reserved for state and always ship with a word).
function paintConn() {
  const el = byId("conn");
  const txt = byId("conn-text");
  const online = S.state?.online;
  let cls = "";
  let label = "—";
  let title = "";
  if (S.feed && S.feed !== "connected" && S.feed !== "—") {
    cls = "warn";
    label = S.feed.startsWith("connecting") ? tr("connecting…") : tr("reconnecting…");
    title = tr("live feed: {state}", { state: S.feedLabel ?? S.feed });
  } else if (online === false) {
    cls = "err";
    label = tr("inverter offline");
    title = S.offlineSince ? tr("no inverter data since {time}", { time: fmtTime(S.offlineSince) }) : tr("no recent inverter data");
  } else if (online === true && S.feed === "connected") {
    cls = "ok";
    label = tr("live");
    title = tr("receiving inverter data");
  }
  el.className = "conn " + cls;
  txt.textContent = label;
  el.title = title;
}

// setFeed: text is the state the code compares ("connected", …); label
// is what the page shows (translated).
function setFeed(text, label = text) {
  S.feed = text;
  S.feedLabel = label;
  paintConn();
  const banner = byId("banner-feed");
  const degraded = text !== "connected" && text !== "—" && authed();
  banner.hidden = !degraded;
  if (degraded) {
    banner.textContent = tr("live feed: {state} — values shown are last known until the stream recovers.", { state: label });
  }
}

document.body.addEventListener("htmx:sse:before:connection", (e) => {
  const c = e.detail.connection;
  if (c.attempt === 0) setFeed("connecting…", tr("connecting…"));
  else setFeed("reconnecting…", tr("reconnecting (attempt {n})…", { n: c.attempt }));
});

document.body.addEventListener("htmx:sse:after:connection", () => {
  // a reconnect may be to a restarted, updated service: its version
  // decides whether the page reloads (checkVersion)
  const reconnect = S.feed !== "connected" && S.pageVersion;
  setFeed("connected");
  if (reconnect) refreshMeta();
});

document.body.addEventListener("htmx:sse:error", (e) => {
  if (!e.detail?.connection) return; // orphaned in-flight fetch from a torn-down stream
  if (e.detail.status === 401) {
    gate(tr("unauthorized — the event stream rejected the token; sign in again"));
    return;
  }
  setFeed("disconnected", tr("disconnected (retrying)"));
});

document.body.addEventListener("htmx:sse:close", (e) => {
  if (!e.detail?.connection) return; // orphaned stream already torn down
  if (authed()) setFeed("disconnected", tr("disconnected ({reason})", { reason: e.detail.reason || tr("closed") }));
});

// Named SSE events arrive as DOM events of the same name (the hx-sse
// extension dispatches on the connect element; detail = {data, id}).

function onSSE(name, fn) {
  document.body.addEventListener(name, (e) => {
    if (!authed()) return;
    let data;
    try { data = JSON.parse(e.detail.data); }
    catch { return; }
    fn(data);
  });
}

// snapshot: coalesce bursts with requestAnimationFrame (one paint per
// frame, not per frame event).
let snapPending = null;
let snapQueued = false;

function queueSnapshot(d) {
  // every reading joins the ring, also those the frame coalescing skips
  const e = d?.power?.eps_w;
  if (e != null) S.epsRing = [...S.epsRing, e].slice(-EPS_MEDIAN_N);
  snapPending = d;
  if (snapQueued) return;
  snapQueued = true;
  requestAnimationFrame(() => {
    snapQueued = false;
    const s = snapPending;
    snapPending = null;
    if (!s || !S.state) return;
    S.state.power = s.power;
    S.state.battery = { ...(S.state.battery || {}), soc_pct: s.soc_pct, soc_estimate_pct: s.soc_estimate_pct, state: s.battery_state };
    S.state.as_of = s.ts;
    if (routeName() === "dashboard") paintNow(S.state);
    if (routeName() === "control") { paintNow(S.state); paintLimits(S.state); }
  });
}

onSSE("snapshot", queueSnapshot);

// config: the payload carries the FULL new state — render from it,
// never re-fetch (brief rule). `changed` names the sections (the spec's
// vocabulary): a write through this app or the cloud, or a setting the
// poll found changed on the inverter itself.
const ROUTE_SECTIONS = {
  "dashboard": ["work-mode", "limits", "meter-compensation", "identity"],
  "control": ["work-mode", "limits", "meter-compensation", "identity"],
  "schedule": ["schedule", "identity"],
  "device": ["identity"],
};

// editing: an unsaved edit on the page (never re-rendered away)
function editing() {
  const dirty = byId("sched-dirty");
  if (dirty?.classList.contains("warn")) return true;
  const a = document.activeElement;
  return !!a && byId("view").contains(a) && a.matches("input, select, textarea");
}

onSSE("config", (d) => {
  S.state = d.state;
  const ch = d.changed ?? [];
  if (ch.includes("forward-mode")) {
    S.forwardMode = { mode: d.state.forward_mode };
    renderForwardBadge();
  }
  setOnline(d.state.online);
  paintDeviceBanner();
  const r = routeName();
  if (r === "dashboard") paintNow(S.state);
  if ((ROUTE_SECTIONS[r] ?? []).some((k) => ch.includes(k)) && !editing()) render();
});

onSSE("connection", (d) => setOnline(d.state === "online"));

// SYSTEM_TITLE: the system events' titles (the detail is the server's
// own text, which stays English).
const SYSTEM_TITLE = {
  "forward-mode-changed": tr("Cloud forwarding changed"),
  "maintenance-opened": tr("Maintenance window opened"),
  "maintenance-extended": tr("Maintenance window extended"),
  "maintenance-expired": tr("Maintenance window expired"),
  "maintenance-closed": tr("Maintenance window closed"),
  "alert": tr("Alert"),
  "gate-rule-loss": tr("Gate rules missing"),
  "upgrade-rolled-back": tr("Upgrade rolled back"),
  "grid-lost": tr("Grid down"),
  "grid-restored": tr("Grid back"),
  "unclean-restart": tr("Restarted after an unexpected stop"),
  "battery-low": tr("Battery low"),
  "battery-forecast-low": tr("Battery running low"),
  "firmware-task": tr("The cloud requested a firmware update"),
};

onSSE("system", (d) => {
  const bad = d.type.includes("failed") || d.type.includes("error");
  const warn = d.type.startsWith("battery-") || d.type === "grid-lost" || d.type === "unclean-restart" || d.type === "firmware-task";
  toast(SYSTEM_TITLE[d.type] ?? d.type, bad ? "error" : warn ? "warn" : "info", d.detail || undefined);
  if (routeName() === "maintenance" &&
      ["maintenance-opened", "maintenance-extended", "maintenance-expired", "maintenance-closed"].includes(d.type)) {
    render();
  }
  if (d.type === "forward-mode-changed") {
    apiFetch("/forward-mode").then((r) => (r.ok ? r.json() : null)).then((j) => {
      if (!j) return;
      S.forwardMode = j;
      if (S.state) S.state.forward_mode = j.mode;
      renderForwardBadge();
    }).catch(() => {});
  }
});

// gap/resync: the stream skipped — re-sync from the full snapshot.
onSSE("gap", (d) => {
  toast(tr("missed {n} event(s) — re-syncing", { n: d.missed }), "warn");
  refreshState();
});

onSSE("resync", () => refreshState());

async function refreshState() {
  const resp = await apiFetch("/state");
  if (resp.status === 401) { gate(tr("the token was rejected — sign in again")); return; }
  if (!resp.ok) return;
  S.state = await resp.json();
  paintDeviceBanner();
  render();
}

// ---------- online / forward state (header) ----------

function setOnline(online) {
  if (S.state) S.state.online = online;
  if (online === true) S.offlineSince = null;
  else if (online === false && !S.offlineSince) S.offlineSince = new Date().toISOString();
  paintConn();
  const b = byId("banner-inverter");
  if (online == null || online) {
    b.hidden = true;
  } else {
    b.hidden = false;
    b.textContent = tr("Inverter offline since {time}. Showing the last known values; changes can't be sent until it reconnects.", { time: fmtTime(S.offlineSince) });
  }
}

function renderForwardBadge() {
  const mode = S.forwardMode?.mode;
  const el = byId("forward-badge");
  if (!mode) {
    el.textContent = tr("cloud: {mode}", { mode: "—" });
    el.className = "badge";
    return;
  }
  el.textContent = tr("cloud: {mode}", { mode: FORWARD_NAME()[mode] ?? mode.replace("_", " ") });
  el.className = "badge";
}

// refreshUpdateBadge shows a header badge, linked to Settings, while a
// newer eligible version is known (the server's last check: no network).
async function refreshUpdateBadge(u) {
  if (!u) {
    try {
      const r = await apiFetch("/update");
      if (!r.ok) return;
      u = await r.json();
    } catch { return; }
  }
  const el = byId("update-badge");
  el.hidden = !u.available;
  if (!u.available) return;
  const long = u.latest_prerelease
    ? tr("update: {v} (prerelease)", { v: u.latest })
    : tr("update: {v}", { v: u.latest });
  // the short form is for narrow screens; the title keeps the version
  el.innerHTML = `<span class="long">${esc(long)}</span><span class="short">${esc(tr("update ↑"))}</span>`;
  el.title = long + " — " + tr("A newer version is available: see Settings → Updates.");
}

async function refreshMeta() {
  try {
    const resp = await apiFetch("/config");
    if (!resp.ok) return;
    S.cfg = await resp.json();
    byId("ver").textContent = S.cfg.version;
    paintNav();
    checkVersion(S.cfg.version);
  } catch { /* non-fatal */ }
}

// ---------- router ----------

const ROUTES = {
  "dashboard": renderDashboard,
  "schedule": renderSchedule,
  "control": renderControl,
  "activity": renderAudit,
  "more": renderMore,
  "device": renderDevice,
  "cloud": renderCloud,
  "messages": renderMessages,
  "settings": renderSettings,
  "setup": renderSetup,
  "report": renderReport,
  "quirks": renderQuirks,
};

// old links keep working
const ROUTE_ALIASES = { "audit": "activity", "maintenance": "cloud" };

const ROUTE_TITLES = {
  "dashboard": tr("Home"), "schedule": tr("Schedule"), "control": tr("Control"),
  "activity": tr("Activity"), "more": tr("More"), "device": tr("Device"),
  "cloud": tr("Cloud & maintenance"), "messages": tr("Frame messages"), "settings": tr("Settings"),
  "setup": tr("Setup"), "report": tr("Report a problem"), "quirks": tr("Inverter quirks"),
};

// FORWARD_NAME: the cloud-forwarding modes' display names.
const FORWARD_NAME = () => ({ off: tr("off"), telemetry_only: tr("telemetry only"), full: tr("full") });

// the primary nav item each route lives under
const NAV_OF = { "device": "more", "cloud": "more", "messages": "more", "settings": "more", "setup": "more", "report": "more", "quirks": "more" };

function routeName() {
  if (dashOnly()) return "dashboard";
  let h = location.hash.replace(/^#\/?/, "");
  h = ROUTE_ALIASES[h] ?? h;
  return ROUTES[h] ? h : "dashboard";
}

function markTab() {
  const r = routeName();
  const nav = NAV_OF[r] ?? r;
  for (const a of document.querySelectorAll("#tabs a, #tabbar a")) {
    const active = a.dataset.route === nav;
    a.classList.toggle("active", active);
    if (active) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
}

let renderSeq = 0;
let renderPending = false;

// AFTER: post-render hooks per route (the charts need the DOM in #view).
const AFTER = { "device": () => scanPoll(), "dashboard": afterDashboard, "cloud": afterCloud, "schedule": afterSchedule, "control": () => paintNow(S.state) };

// afterSchedule positions the timeline blocks (CSSOM, CSP-safe).
function afterSchedule() {
  for (const b of byId("view").querySelectorAll(".tl-block")) {
    b.style.setProperty("--a", b.dataset.a);
    b.style.setProperty("--b", b.dataset.b);
  }
}

// renderNow applies a render that was deferred by a gate below.
function renderNow() {
  if (!renderPending) return;
  renderPending = false;
  render();
}

async function render() {
  if (!authed()) return;
  // A full re-render replaces #view and would detach any in-flight
  // command's source element, so htmx:finally:request (dispatched on
  // that element) would never reach the body listener and the busy
  // state would stick. Defer the render until the command settles —
  // the settlement handlers (after:request / finally) re-render.
  if (S.cmdBusy > 0) {
    renderPending = true;
    return;
  }
  const seq = ++renderSeq;
  clearInterval(S.maintTimer);
  markTab();
  if (routeName() !== "dashboard") {
    stopHistTimer();
    stopFreshTimer();
    stopAvgTimer();
  }
  const html = await ROUTES[routeName()]();
  if (seq !== renderSeq || !html) return; // superseded or gated mid-fetch
  const view = byId("view");
  // every view gets a level-one heading (visually hidden: the tab bar
  // shows where you are) and a matching document title (WCAG 2.4.2)
  const title = ROUTE_TITLES[routeName()] ?? "foxesslocal";
  document.title = title + " · foxesslocal";
  // the shell's layout may depend on the page (the wide dashboard)
  byId("app").dataset.route = routeName();
  view.innerHTML = `<h1 class="sr-only">${esc(title)}</h1>` + html;
  paintDeviceBanner();
  watchWide();
  htmx.process(view);
  applyRoleLocks(view);
  if (AFTER[routeName()]) AFTER[routeName()]();
  setBusy(S.cmdBusy > 0);
}

// ---------- the wide dashboard ----------
// Two columns (the live view beside the history) need a screen at least
// WIDE_MIN_PX wide and tall enough for the whole live view: on a shorter
// one its base would sit below the fold, so the dashboard stays one
// column. fitWide measures the live view in the wide layout and keeps
// it only if it fits; the class goes on and off within one task, so the
// browser never paints the trial. It runs on each render, on a resize,
// and when the live view or a banner above it changes size.
const WIDE_MIN_PX = 1400;
const wideRO = new ResizeObserver(() => fitWide());

function fitWide() {
  const app = byId("app");
  const now = document.querySelector("#view .now");
  if (app.dataset.route !== "dashboard" || !now || window.innerWidth < WIDE_MIN_PX) {
    app.classList.remove("wide");
    return;
  }
  app.classList.add("wide");
  const bottom = now.getBoundingClientRect().bottom + window.scrollY + parseFloat(getComputedStyle(now).marginBottom);
  app.classList.toggle("wide", bottom <= window.innerHeight);
}

function watchWide() {
  wideRO.disconnect();
  for (const el of [byId("banner-inverter"), byId("banner-feed"), byId("banner-device"), document.querySelector("#view .now")]) {
    if (el) wideRO.observe(el);
  }
  fitWide();
}

window.addEventListener("resize", fitWide);

// ---------- dashboard (2026-11-16 redesign: dataviz skill) ----------
//
// Hierarchy: "Now" (power flow + battery — the one-glance answer), then
// "Energy" (one range control scoping the KPI tiles, the small-multiple
// history and its table view). Entity colors are fixed per entity
// (validated palette: solar orange, home blue, battery aqua, grid
// neutral — css --c-*); text always wears ink tokens, never the series
// color. Signs are translated into words for the reader.

const ACTIVE_W = 30;          // below this a flow reads as idle

// fmtPower: absolute value, kW from 1 kW up.
function fmtPower(w) {
  if (w == null) return "—";
  const a = Math.abs(w);
  return a >= 1000 ? (a / 1000).toFixed(a >= 10000 ? 1 : 2) + " kW" : Math.round(a) + " W";
}

// fmtAxisW: compact signed axis/tooltip value.
function fmtAxisW(v, tick) {
  const a = Math.abs(v);
  if (a >= 1000) {
    const k = v / 1000;
    return (tick ? String(Math.round(k * 10) / 10) : k.toFixed(2)) + " kW";
  }
  return Math.round(v) + " W";
}

function fmtAmps(a) {
  return a == null ? tr("not read yet") : a.toFixed(1) + " A";
}

function fmtKWh(wh) {
  if (wh == null) return "—";
  const k = wh / 1000;
  return (k >= 100 ? k.toFixed(0) : k >= 10 ? k.toFixed(1) : k.toFixed(2)) + " kWh";
}

function fmtDuration(h) {
  if (h < 1) return Math.max(1, Math.round(h * 60)) + " min";
  if (h < 48) return (h < 10 ? h.toFixed(1) : Math.round(h)) + " h";
  return tr("more than 2 days");
}

// flowOf: per-entity direction + phrase for the current power reading.
// The power-flow diagram (viewBox 400×300, the box is 4:3 so node
// percentages and SVG units agree): DC sources into the inverter, its AC
// output into the switchboard, which feeds the home and meets the grid;
// the EPS (backup) port hangs off the inverter itself. The top row is
// outside the house (solar, grid), the bottom row inside (battery,
// backup, home). [key, from x,y,
// to x,y, arrow position along the line]; "fwd" flows from → to.
const FLOW_LINES = [
  ["solar", 60, 50, 180, 150, 0.42],
  ["batt", 180, 150, 60, 250, 0.6], // fwd = charging (inverter → battery)
  ["ac", 180, 150, 288, 150, 0.75], // fwd = inverter AC output
  ["home", 288, 150, 340, 250, 0.5],
  ["grid", 340, 50, 288, 150, 0.5], // fwd = import (grid → switchboard)
  ["eps", 180, 150, 200, 258, 0.5], // fwd = backup port output
];

// paintFlowLine sets one connector's state and its arrow's direction.
function paintFlowLine(k, d) {
  const ln = byId("fl-" + k);
  if (ln) ln.setAttribute("class", "fl s-" + k + " " + d);
  const ar = byId("ar-" + k);
  const g = FLOW_LINES.find(([key]) => key === k);
  if (!ar || !g) return;
  const [, x1, y1, x2, y2, t] = g;
  const deg = Math.atan2(y2 - y1, x2 - x1) * 180 / Math.PI + (d === "rev" ? 180 : 0);
  ar.setAttribute("transform", `translate(${x1 + (x2 - x1) * t} ${y1 + (y2 - y1) * t}) rotate(${deg})`);
  ar.setAttribute("class", "ar s-" + k + " " + d);
}

// The KH's AC output (ac_port_w) and household load (load_w) both
// INCLUDE the EPS (backup) port — verified 2026-11-17: a 1.6 kW load
// switched onto the backup port raised eps_w, ac_port_w and load_w alike,
// grid and the conversion loss unchanged (profile §5). So the diagram
// splits them: the switchboard gets AC output − backup, the main-board
// circuits are load − backup.

// inverterLoss: DC in minus AC out (solar − battery charging − AC
// output, which includes the backup port); null when a term is missing.
function inverterLoss(p) {
  if (p?.pv_w == null || p?.battery_w == null || p?.ac_port_w == null) return null;
  return p.pv_w - p.battery_w - p.ac_port_w;
}

// splitEPS: the grid-port output (to the switchboard) and the main-board
// load, net of the backup port (both registers include it). The load is
// AC output + grid, so the grid meter's offset and the backup-port
// reading's noise can put "load − backup" below 0 when the main board
// draws little: it never shows below 0 (measurement error, not a load).
function splitEPS(p) {
  const eps = p?.eps_w ?? 0;
  return {
    toBoard: p?.ac_port_w == null ? null : p.ac_port_w - eps,
    mainLoad: p?.load_w == null ? null : Math.max(0, p.load_w - eps),
  };
}

function flowOf(p) {
  const dir = (w, pos, neg, idle) =>
    w == null ? { d: "idle", txt: "—" }
      : w >= ACTIVE_W ? { d: "fwd", txt: pos }
      : w <= -ACTIVE_W ? { d: "rev", txt: neg }
      : { d: "idle", txt: idle };
  return {
    solar: dir(p?.pv_w, tr("generating"), tr("generating"), tr("not generating")),
    home: dir(p?.load_w, tr("using"), tr("using"), tr("idle")),
    grid: dir(p?.grid_w, tr("importing"), tr("exporting"), tr("balanced")),
    batt: dir(p?.battery_w, tr("charging"), tr("discharging"), tr("idle")),
    ac: dir(splitEPS(p).toBoard, tr("out"), tr("in"), tr("idle")),
    eps: dir(p?.eps_w, tr("EPS port"), tr("EPS port"), tr("EPS idle")),
  };
}

// nowPower: the power shown in the Now panel — the live reading, or the
// trailing-window average when one is selected (null until it loads).
function nowPower(st) {
  // the averaged window is the dashboard's control; elsewhere it is live
  const live = S.nowWindow === "now" || routeName() !== "dashboard";
  if (!live) return S.avg?.power ?? null;
  const p = st?.power;
  // quirk eps_dropouts: the backup port as the median of its last readings
  // (single low readings the AC output does not share are ignored)
  if (p && S.quirks.eps_dropouts && S.epsRing.length > 0) return { ...p, eps_w: median(S.epsRing) };
  return p;
}

// EPS_MEDIAN_N: the backup-port readings the eps_dropouts quirk takes the
// median of (5 × ~5 s: up to two dropouts in a row are ignored).
const EPS_MEDIAN_N = 5;

function median(xs) {
  const s = [...xs].sort((a, b) => a - b);
  const m = s.length >> 1;
  return s.length % 2 ? s[m] : Math.round((s[m - 1] + s[m]) / 2);
}

// loadQuirks reads which inverter quirks are on (the dashboard applies
// them); without it every quirk counts as off.
async function loadQuirks() {
  try {
    const r = await apiFetch("/quirks");
    if (!r.ok) return null;
    const b = await r.json();
    S.quirks = Object.fromEntries(b.quirks.map((q) => [q.id, q.enabled]));
    return b.quirks;
  } catch { return null; }
}

function paintNow(st) {
  const p = nowPower(st);
  const f = flowOf(p);
  // the backup branch appears once the EPS port supplies power (and
  // stays: a cycling load must not make the diagram jump)
  if (!S.epsSeen && (p?.eps_w ?? 0) >= ACTIVE_W) S.epsSeen = true;
  const split = splitEPS(p);
  const vals = { solar: p?.pv_w, home: split.mainLoad, grid: p?.grid_w, batt: p?.battery_w, eps: p?.eps_w };
  for (const k of ["solar", "home", "grid", "batt", "eps"]) {
    const v = byId("fv-" + k);
    if (v) v.textContent = fmtPower(vals[k]);
    const ph = byId("fp-" + k);
    if (ph) ph.textContent = f[k].txt;
  }
  // with the backup branch shown, Home is the main-board circuits; its
  // caption carries the whole household
  const hp0 = byId("fp-home");
  if (hp0 && S.epsSeen && p?.load_w != null) {
    hp0.textContent = tr("of {total}", { total: fmtPower(p.load_w) });
    hp0.title = tr("main-board circuits, of {total} for the whole household (backup circuits included)", { total: fmtPower(p.load_w) });
  }
  for (const [k] of FLOW_LINES) paintFlowLine(k, f[k].d);
  // the inverter: its AC output (in when charging from the grid) and its
  // conversion loss
  const loss = inverterLoss(p);
  const hub = byId("fv-hub");
  if (hub) hub.textContent = fmtPower(p?.ac_port_w);
  const hp = byId("fp-hub");
  if (hp) {
    // the arrow shows the direction; "AC in" (charging from the grid)
    // is the case worth spelling out
    const lossTxt = loss == null ? "" : tr("loss {w}", { w: fmtPower(Math.max(0, loss)) });
    hp.textContent = f.ac.d === "idle" ? tr("idle")
      : f.ac.d === "rev" ? tr("AC in") + (lossTxt ? " · " + lossTxt : "")
      : lossTxt || tr("AC out");
  }
  const flow = byId("flow");
  if (flow) {
    flow.classList.toggle("has-eps", S.epsSeen);
    flow.setAttribute("aria-label",
      tr("Solar {w} {state}", { w: fmtPower(p?.pv_w), state: f.solar.txt }) + "; " +
      tr("battery {w} {state}", { w: fmtPower(p?.battery_w), state: f.batt.txt }) + "; " +
      tr("inverter AC {state} {w}", { state: f.ac.txt, w: fmtPower(p?.ac_port_w) }) +
      (loss == null ? "" : ", " + tr("loss {w}", { w: fmtPower(Math.max(0, loss)) })) + "; " +
      (S.epsSeen
        ? tr("main-board circuits {w}; backup port {eps}; household {total}", { w: fmtPower(split.mainLoad), eps: fmtPower(p?.eps_w), total: fmtPower(p?.load_w) })
        : tr("home {w}", { w: fmtPower(p?.load_w) })) + "; " +
      tr("grid {w} {state}", { w: fmtPower(p?.grid_w), state: f.grid.txt }));
  }
  paintBattery(st);
  paintFreshness();
}

function paintBattery(st) {
  const b = st?.battery;
  const w = nowPower(st)?.battery_w;
  // the estimate between the inverter's whole-percent readings (two
  // decimals, D-028) when there is one, else the reported SoC
  const est = b?.soc_estimate_pct;
  const soc = est ?? b?.soc_pct;
  const socTxt = soc == null ? null : est != null ? est.toFixed(2) : String(Math.round(soc));
  const socEl = byId("soc");
  if (socEl) {
    if (socTxt == null) socEl.textContent = "—";
    else {
      // the decimals smaller than the whole percent
      const [whole, frac] = socTxt.split(".");
      const small = document.createElement("span");
      small.className = "hero-frac";
      small.textContent = frac ? "." + frac : "";
      socEl.replaceChildren(whole, small, " %");
    }
  }
  const meter = byId("soc-meter");
  if (meter) {
    meter.setAttribute("aria-valuenow", soc == null ? 0 : soc);
    meter.setAttribute("aria-valuetext", soc == null ? tr("unknown") : tr("{n} percent", { n: socTxt }));
  }
  const bar = byId("soc-bar");
  if (bar) {
    bar.style.width = (soc == null ? 0 : Math.max(0, Math.min(100, soc))) + "%";
    bar.classList.toggle("low", soc != null && soc < 20 && soc >= 10);
    bar.classList.toggle("crit", soc != null && soc < 10);
  }
  const f = flowOf(nowPower(st)).batt;
  const phrase = byId("batt-phrase");
  if (phrase) {
    phrase.textContent = w == null ? "—"
      : f.d === "idle" ? tr("Idle")
      : f.d === "fwd" ? tr("Charging {w}", { w: fmtPower(w) }) : tr("Discharging {w}", { w: fmtPower(w) });
  }
  // usable energy = SoC × the configured capacity (the API's capacity_wh;
  // recomputed here because live snapshots carry only the SoC and its
  // estimate)
  const cap = b?.capacity_wh;
  const stored = soc != null && cap ? Math.round(soc * cap / 100) : b?.stored_energy_wh;
  const eta = byId("batt-eta");
  if (eta) {
    let txt = "";
    if (stored != null && w != null && Math.abs(w) >= 100) {
      if (w < 0) txt = tr("≈ {d} of charge left at this rate", { d: fmtDuration(stored / -w) });
      else if (soc != null && soc < 100 && cap) txt = tr("≈ {d} to full at this rate", { d: fmtDuration(Math.max(0, cap - stored) / w) });
    }
    eta.textContent = txt;
    eta.hidden = txt === "";
  }
  const sw = byId("stored-wh");
  if (sw) sw.textContent = stored == null ? "—"
    : cap ? tr("{stored} of {cap} kWh", { stored: (stored / 1000).toFixed(1), cap: (cap / 1000).toFixed(2) }) : (stored / 1000).toFixed(1) + " kWh";
  const pv = byId("pack-v");
  if (pv) pv.textContent = fmtV(b?.pack_voltage_v);
}

// freshness: "updated 3 s ago" from the newest reflected frame.
let freshTimer = 0;
function paintFreshness() {
  const el = byId("now-age");
  if (!el) return;
  if (S.nowWindow !== "now") {
    const a = S.avg;
    const label = S.nowWindow === "5m" ? "5 min" : "30 min";
    el.classList.toggle("stale", !!a && a.samples < a.expected_samples * 0.8);
    el.textContent = !a ? tr("loading the {window} average…", { window: label })
      : a.samples === 0 ? tr("no data in the last {window}", { window: label })
      : tr("average of the last {window} · {n} samples", { window: label, n: a.samples }) +
        (a.samples < a.expected_samples * 0.8 ? " " + tr("(partial)") : "");
    return;
  }
  const ts = S.state?.as_of ? Date.parse(S.state.as_of) : NaN;
  if (Number.isNaN(ts)) { el.textContent = tr("waiting for data"); return; }
  const age = Math.max(0, Math.round((Date.now() - ts) / 1000));
  el.textContent = age < 60 ? tr("updated {n} s ago", { n: age })
    : tr("no new data for {d}", { d: fmtDuration(age / 3600) });
  el.classList.toggle("stale", age >= 30);
}
function startFreshTimer() {
  if (!freshTimer) freshTimer = setInterval(paintFreshness, 1000);
}
function stopFreshTimer() {
  clearInterval(freshTimer);
  freshTimer = 0;
}

// averaged Now: refetch the trailing-window average every 15 s.
let avgTimer = 0;
async function fetchAverage() {
  const win = S.nowWindow;
  if (win === "now") return;
  try {
    const resp = await apiFetch("/power/average?window=" + win);
    if (resp.status === 401) { gate(tr("the token was rejected — sign in again")); return; }
    if (!resp.ok || win !== S.nowWindow) return;
    S.avg = await resp.json();
  } catch {
    return; // keep the last average; the next tick retries
  }
  if (routeName() === "dashboard") paintNow(S.state);
}
function startAvgTimer() {
  stopAvgTimer();
  if (S.nowWindow === "now") return;
  fetchAverage();
  avgTimer = setInterval(fetchAverage, 15000);
}
function stopAvgTimer() {
  clearInterval(avgTimer);
  avgTimer = 0;
}

// ---------- export (CSV for spreadsheets, self-describing JSON for agents) ----------

function download(name, type, text) {
  const url = URL.createObjectURL(new Blob([text], { type }));
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10000);
}

function exportName(ext) {
  const d = new Date();
  const p = (n) => String(n).padStart(2, "0");
  return `foxesslocal-history-${S.histRange}-${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}.${ext}`;
}

// exportRows: steps with data only (an all-empty step is a gap — the
// timestamps show it; a spreadsheet full of blank rows helps no one).
function exportRows(h) {
  const out = [];
  h.ts.forEach((t, i) => {
    const r = { solar_w: h.sol[i], home_w: h.haus[i], grid_w: h.grid[i], battery_w: h.batt[i], soc_pct: h.soc[i] };
    if (Object.values(r).every((v) => v == null)) return;
    r.eps_w = h.eps?.[i] ?? null;
    r.soc_estimate_pct = h.soc_est?.[i] ?? null;
    out.push({ time: new Date(t * 1000).toISOString(), ts: t, ...r });
  });
  return out;
}

function exportCSV(h) {
  const cols = ["time", "ts", "solar_w", "home_w", "grid_w_import_positive", "battery_w_charge_positive", "soc_pct", "eps_w"];
  const lines = [cols.join(",")];
  for (const r of exportRows(h)) {
    const v = (x) => (x == null ? "" : String(x));
    lines.push([r.time, r.ts, v(r.solar_w), v(r.home_w), v(r.grid_w), v(r.battery_w), v(r.soc_pct), v(r.eps_w)].join(","));
  }
  download(exportName("csv"), "text/csv", lines.join("\n") + "\n");
}

function exportJSON(h) {
  const doc = {
    source: "foxesslocal",
    description: "Inverter power and battery history: one row per step that has data (steps with no data are omitted — gaps show in the timestamps); each value is the mean over the step; null = that series had no data in the step.",
    generated_at: new Date().toISOString(),
    range: h.range,
    step_seconds: STEP_S[h.step] ?? null,
    api: { endpoint: "GET /api/v1/history?range=" + h.range, note: "the same data, directly from the API (read-only token or public read-only access)" },
    fields: {
      time: { unit: "ISO 8601 UTC", description: "start of the step" },
      ts: { unit: "unix seconds", description: "start of the step" },
      solar_w: { unit: "W", description: "PV generation (≥ 0)" },
      home_w: { unit: "W", description: "household load (≥ 0), the backup (EPS) port included" },
      eps_w: { unit: "W", description: "the backup (EPS) port's output: the part of home_w the backup circuits drew (null = not recorded)" },
      grid_w: { unit: "W", description: "grid at the panel: positive = importing, negative = exporting" },
      battery_w: { unit: "W", description: "battery: positive = charging, negative = discharging" },
      soc_pct: { unit: "%", description: "battery state of charge at the end of the step" },
    },
    battery_capacity_wh: S.state?.battery?.capacity_wh ?? null,
    totals_wh: h.totals,
    rows: exportRows(h),
  };
  download(exportName("json"), "application/json", JSON.stringify(doc, null, 2) + "\n");
}

// ---------- energy: KPI tiles + small multiples + table ----------

// [range, button, label, wide]: phones get 1h/6h/24h/72h (60-100 points
// fit a narrow chart); 7d and 30d are for wide screens only.
const RANGES = [
  ["1h", "1 h", tr("last hour")], ["6h", "6 h", tr("last 6 hours")],
  ["24h", "24 h", tr("last 24 hours")], ["72h", "72 h", tr("last 72 hours")],
  ["7d", "7 d", tr("last 7 days"), true], ["30d", "30 d", tr("last 30 days"), true],
];
const STEP_S = { "1m": 60, "5m": 300, "15m": 900, "1h": 3600 };
const NARROW = window.matchMedia("(max-width: 720px)");

// a wide-only range remembered from a wide screen falls back on a phone
function fitRange() {
  if (NARROW.matches && RANGES.find(([r]) => r === S.histRange)?.[3]) S.histRange = "72h";
}

let histTimer = 0;
let histDispose = null;
let histData = null;

function kpiTile(cls, label, value, sub) {
  return `<div class="kpi" role="group" aria-label="${esc(label)}"><div class="kpi-label"><span class="sm-key ${cls}" aria-hidden="true"></span>${esc(label)}</div>` +
    `<div class="kpi-value">${esc(value)}</div><div class="kpi-sub muted small">${esc(sub)}</div></div>`;
}

// dirTile: a two-direction total — the larger direction is the headline,
// the other the subline (never "0.00 kWh in" as the headline).
function dirTile(cls, label, a, aWord, b, bWord) {
  const [hv, hw, sv, sw] = (a ?? 0) >= (b ?? 0) ? [a, aWord, b, bWord] : [b, bWord, a, aWord];
  return kpiTile(cls, label, fmtKWh(hv), hw + " · " + fmtKWh(sv) + " " + sw);
}

function renderKpis(h) {
  const t = h?.totals;
  const el = byId("kpis");
  if (!el) return;
  if (!t) { el.innerHTML = ""; return; }
  el.innerHTML =
    kpiTile("s-solar", tr("Solar"), fmtKWh(t.sol_wh), tr("generated")) +
    kpiTile("s-home", tr("Home"), fmtKWh(t.haus_wh), tr("used")) +
    dirTile("s-grid", tr("Grid"), t.grid_import_wh, tr("imported"), t.grid_export_wh, tr("exported")) +
    dirTile("s-batt", tr("Battery"), t.batt_charged_wh, tr("charged"), t.batt_discharged_wh, tr("discharged"));
}

function histRows(h) {
  const w = (v, tick) => fmtAxisW(v, tick);
  return [
    { key: "sol", title: tr("Solar"), cls: "s-solar", values: h.sol, fmt: w },
    homeRow(h, w),
    { key: "grid", title: tr("Grid"), cls: "s-grid", values: h.grid, fmt: w, legend: tr("above 0 = importing · below 0 = exporting") },
    { key: "batt", title: tr("Battery"), cls: "s-batt", values: h.batt, fmt: w, legend: tr("above 0 = charging · below 0 = discharging") },
    socRow(h),
    { key: "bms", title: tr("BMS charge limit"), cls: "s-batt", values: h.bms_chg_a ?? [], area: false, fmt: fmtAxisA,
      legend: tr("from battery temperature and charge") },
    { key: "temp", title: tr("Temperature"), cls: "s-tbatt", values: h.t_cell_min ?? [], zero: false, area: false, fmt: fmtAxisC,
      series: [
        // the BMS's coldest cell: what the charge limit follows
        { title: tr("coldest cell"), cls: "s-tbatt", values: h.t_cell_min ?? [] },
        { title: tr("battery"), cls: "s-tbatt", values: h.t_batt ?? [], dash: true },
        { title: tr("inverter"), cls: "s-tinv", values: h.t_inv ?? [] },
        { title: tr("ambient"), cls: "s-tamb", values: h.t_amb ?? [], dash: true },
      ] },
  ];
}

// homeRow: the household load. It includes the backup (EPS) port; when
// the range has backup-port output, its share is drawn on the same chart
// (the dashboard's Home shows the main-board circuits, load − backup).
function homeRow(h, w) {
  const row = { key: "haus", title: tr("Home"), cls: "s-home", values: h.haus, fmt: w };
  if (!(h.eps ?? []).some((v) => v != null && v >= ACTIVE_W)) return row;
  return { ...row, series: [
    { title: tr("whole household"), cls: "s-home", values: h.haus },
    // drawn no higher than the household load that includes it (the
    // difference is within measurement error; the table shows the raw value)
    { title: tr("backup port"), cls: "s-eps", values: h.eps.map((v, i) => (v == null || h.haus[i] == null ? v : Math.min(v, h.haus[i]))), dash: true },
  ] };
}

// socRow: the state of charge. Where the range has the SoC estimate
// (D-028: the BMS's own 0.1 % SoC, between the inverter's whole-percent
// readings), it is drawn with the inverter's reading dashed beside it.
function socRow(h) {
  const row = { key: "soc", title: tr("State of charge"), cls: "s-batt", values: h.soc, fixed: [0, 100], area: false, fmt: fmtAxisSoc };
  if (!(h.soc_est ?? []).some((v) => v != null)) return row;
  return { ...row, values: h.soc_est, series: [
    { title: tr("estimate"), cls: "s-batt", values: h.soc_est },
    { title: tr("inverter reading"), cls: "s-batt", values: h.soc, dash: true },
  ] };
}

// fmtAxisSoc: whole percent on the axis, two decimals for an estimate
const fmtAxisSoc = (v, tick) => (tick || Number.isInteger(v) ? Math.round(v) : v.toFixed(2)) + " %";

// fmtAxisC / fmtAxisA: temperature and current, one decimal in the tooltip
const fmtAxisC = (v, tick) => (tick ? Math.round(v) : v.toFixed(1)) + " °C";
const fmtAxisA = (v, tick) => (tick ? Math.round(v) : v.toFixed(1)) + " A";

function renderHistTable(h) {
  const el = byId("hist-table");
  if (!el || el.hidden) return;
  const rows = [];
  for (let i = h.ts.length - 1; i >= 0; i--) {
    if ([h.sol, h.haus, h.grid, h.batt, h.soc].every((s) => s[i] == null)) continue;
    const c = (v) => `<td class="num">${v == null ? "—" : esc(fmtAxisW(v, false))}</td>`;
    const u = (s, f) => `<td class="num">${s?.[i] == null ? "—" : esc(f(s[i], false))}</td>`;
    rows.push(`<tr><td>${esc(new Date(h.ts[i] * 1000).toLocaleString(locale()))}</td>${c(h.sol[i])}${c(h.haus[i])}${c(h.grid[i])}${c(h.batt[i])}` +
      `<td class="num">${h.soc[i] == null ? "—" : h.soc[i] + " %"}</td>${u(h.soc_est, fmtAxisSoc)}${c(h.eps?.[i])}` +
      `${u(h.bms_chg_a, fmtAxisA)}${u(h.t_cell_min, fmtAxisC)}${u(h.t_batt, fmtAxisC)}${u(h.t_inv, fmtAxisC)}${u(h.t_amb, fmtAxisC)}</tr>`);
  }
  const th = (s) => `<th scope="col">${esc(s)}</th>`;
  el.innerHTML = rows.length === 0 ? `<p class="muted small">${esc(tr("No data in this range yet."))}</p>` :
    `<table><caption class="sr-only">${esc(tr("History values, {range}, newest first", { range: RANGES.find(([r]) => r === S.histRange)?.[2] ?? "" }))}</caption>` +
    `<thead><tr>${th(tr("time"))}${th(tr("solar"))}${th(tr("home"))}${th(tr("grid (+ import)"))}${th(tr("battery (+ charging)"))}${th(tr("SoC"))}${th(tr("SoC estimate"))}${th(tr("backup port"))}` +
    `${th(tr("BMS charge limit"))}${th(tr("coldest cell"))}${th(tr("battery temp"))}${th(tr("inverter temp"))}${th(tr("ambient temp"))}</tr></thead>` +
    `<tbody>${rows.join("")}</tbody></table>`;
}

async function renderHistory(range) {
  const root = byId("sm");
  if (!root) { stopHistTimer(); return; } // route changed before the fetch
  root.classList.add("loading"); // refetch keeps the frame (reduced opacity)
  let h;
  try {
    const resp = await apiFetch("/history?range=" + range);
    if (resp.status === 401) { gate(tr("the token was rejected — sign in again")); return; }
    if (!resp.ok) throw new Error("status " + resp.status);
    h = await resp.json();
  } catch {
    root.classList.remove("loading");
    if (!histData) root.textContent = tr("History unavailable.");
    return;
  }
  if (!byId("sm") || range !== S.histRange) return; // route or range changed mid-fetch
  histDispose?.();
  histData = h;
  histDispose = smallMultiples(root, {
    ts: h.ts, step: STEP_S[h.step] ?? 60, rows: histRows(h),
    rangeLabel: RANGES.find(([r]) => r === range)?.[2] ?? "",
  });
  root.classList.remove("loading");
  renderKpis(h);
  renderHistTable(h);
}

function startHistTimer() {
  if (histTimer) return;
  histTimer = setInterval(() => { if (authed() && byId("sm")) renderHistory(S.histRange); }, 60000);
}

function stopHistTimer() {
  if (histTimer) clearInterval(histTimer);
  histTimer = 0;
  histDispose?.();
  histDispose = null;
  histData = null;
}

// afterDashboard: post-render hook (AFTER) — paint the live values,
// draw the history and arm the refresh + freshness timers.
function afterDashboard() {
  histDispose?.();
  histDispose = null;
  histData = null;
  paintNow(S.state);
  renderHistory(S.histRange);
  startHistTimer();
  startFreshTimer();
  startAvgTimer();
}

// ---------- surfaces ----------

async function loadState() {
  if (S.state) return true;
  const resp = await apiFetch("/state");
  if (resp.status === 401) { gate(tr("the token was rejected — sign in again")); return false; }
  if (!resp.ok) return false;
  S.state = await resp.json();
  S.forwardMode = { mode: S.state.forward_mode };
  return true;
}

function flowNode(k, label, area) {
  return `<div class="node ${area}">
      <div class="node-label"><span class="sm-key s-${k}" aria-hidden="true"></span>${label}</div>
      <div id="fv-${k}" class="node-value">—</div>
      <div id="fp-${k}" class="node-phrase muted">—</div>
    </div>`;
}

// FLOW_LIMITS: where each inverter limit acts on the diagram (a numbered
// marker at x,y in the viewBox) — the Control page's legend explains each.
const FLOW_LIMITS = [
  // each sits on the stretch of its line no node covers at phone width
  { n: 1, x: 300, y: 126, key: "grid" },   // the grid connection (CT)
  { n: 2, x: 244, y: 132, key: "port" },   // the inverter's grid port
  { n: 3, x: 128, y: 194, key: "batt" },   // the battery
  { n: 4, x: 180, y: 94, key: "inv" },     // the inverter's output rating
  { n: 5, x: 230, y: 199, key: "eps" },    // the backup (EPS) port
];

const fmtKW = (w) => (w == null ? "—" : (w / 1000).toFixed(w % 1000 === 0 ? 0 : 1) + " kW");
const fmtA = (a) => (a == null ? "—" : (Number.isInteger(a) ? a : a.toFixed(1)) + " A");
const EPS_TEXT = { ups: tr("UPS mode"), eps: tr("EPS mode"), disabled: tr("disabled") };

// usage: a "now" line — the live value against its limit
function usage(now, limit, verb) {
  if (now == null) return "";
  const pct = limit ? Math.round(100 * now / limit) : null;
  return tr("now {verb} {w}", { verb, w: fmtPower(now) }) + (pct == null ? "" : ` (${pct} %)`);
}

// limitRows: the legend's rows from the state (limits) and the power now.
function limitRows(st, p) {
  const l = st?.limits ?? {};
  const split = splitEPS(p);
  const port = split.toBoard; // + = output to the switchboard
  const g = p?.grid_w;
  return {
    grid: {
      title: tr("Grid connection"),
      limits: tr("import ≤ {iw} / {ia}, export ≤ {ew} / {ea}", { iw: fmtKW(l.import_w), ia: fmtA(l.import_current_a), ew: fmtKW(l.export_w), ea: fmtA(l.export_current_a) }),
      where: tr("Applied to the whole house at the CT clamp (as FoxESS describes it — not verified here)."),
      now: g == null ? "" : g >= 0 ? usage(g, l.import_w, tr("importing")) : usage(-g, l.export_w, tr("exporting")),
    },
    port: {
      title: tr("Inverter grid port"),
      limits: tr("draw ≤ {load} (load limit), output ≤ {gen} (generation limit)", { load: fmtKW(l.load_limit_w), gen: fmtKW(l.generation_limit_w) }),
      where: tr("Between the inverter and the switchboard. The load limit covers battery charging plus backup pass-through (verified 17 Nov); the generation limit is presumed to cap output (untested)."),
      now: port == null ? "" : port < 0 ? usage(-port, l.load_limit_w, tr("drawing")) : usage(port, l.generation_limit_w, tr("supplying")),
    },
    batt: {
      title: tr("Battery"),
      limits: tr("charge ≤ {c}, discharge ≤ {d}", { c: fmtA(l.max_charge_current_a), d: fmtA(l.max_discharge_current_a) }),
      where: tr("Battery-side (DC) current. At the pack's voltage this is far above the inverter's rating, so it does not bind."),
      now: p?.battery_w == null ? "" : tr("now {verb} {w}", { verb: p.battery_w >= 0 ? tr("charging") : tr("discharging"), w: fmtPower(p.battery_w) }),
    },
    inv: {
      title: tr("Inverter output"),
      limits: l.power_limit_pct == null ? "—" : tr("power limit {pct} % of rating", { pct: l.power_limit_pct }),
      where: tr("The inverter's total AC output (switchboard plus backup)."),
      now: p?.ac_port_w == null ? "" : tr("now {verb} {w}", { verb: p.ac_port_w >= 0 ? tr("output") : tr("input"), w: fmtPower(p.ac_port_w) }),
    },
    eps: {
      title: tr("Backup (EPS) port"),
      limits: EPS_TEXT[st?.eps_mode] ?? "—",
      where: tr("No separate limit. While the grid is up, its load counts toward the inverter grid port (2) when the battery and solar can't cover it."),
      now: p?.eps_w == null ? "" : tr("now {w}", { w: fmtPower(p.eps_w) }),
    },
  };
}

function limitsLegend(st) {
  const rows = limitRows(st, nowPower(st));
  return `<ol class="lim-legend">${FLOW_LIMITS.map(({ n, key }) => {
    const r = rows[key];
    return `<li><span class="lim-num" aria-hidden="true">${n}</span><div>
        <div class="lim-title">${esc(r.title)}: <span class="num">${esc(r.limits)}</span></div>
        <div class="muted small">${esc(r.where)}</div>
        <div id="lim-now-${key}" class="small lim-now">${esc(r.now)}</div></div></li>`;
  }).join("")}</ol>`;
}

// paintLimits refreshes the legend's "now" lines (live updates).
function paintLimits(st) {
  const rows = limitRows(st, nowPower(st));
  for (const { key } of FLOW_LIMITS) {
    const el = byId("lim-now-" + key);
    if (el) el.textContent = rows[key].now;
  }
}

// flowMarkup is the power-flow diagram (the dashboard's, and the
// Control page's with the limit markers — FLOW_LIMITS).
function flowMarkup(withLimits) {
  const marks = withLimits
    ? FLOW_LIMITS.map((l) => `<g class="lim-mark"><circle cx="${l.x}" cy="${l.y}" r="10"/><text x="${l.x}" y="${l.y}">${l.n}</text></g>`).join("")
    : "";
  return `
      <div id="flow" class="flow${S.epsSeen ? " has-eps" : ""}" role="img">
        <svg class="flow-lines" viewBox="0 0 400 300" aria-hidden="true">
          ${FLOW_LINES.map(([k, x1, y1, x2, y2]) =>
            `<line id="fl-${k}" class="fl s-${k} idle" x1="${x1}" y1="${y1}" x2="${x2}" y2="${y2}"/>`).join("")}
          ${FLOW_LINES.map(([k]) => `<path id="ar-${k}" class="ar s-${k} idle" d="M-6,-5 L6,0 L-6,5 Z"/>`).join("")}
          <circle class="board-dot" cx="288" cy="150" r="5"/>
          ${marks}
        </svg>
        <span class="board-label muted">${esc(tr("switchboard"))}</span>
        ${flowNode("solar", esc(tr("Solar")), "a-solar")}
        ${flowNode("batt", esc(tr("Battery")), "a-batt")}
        <div class="node inv a-hub">
          <div class="node-label">${esc(tr("Inverter"))}</div>
          <div id="fv-hub" class="node-value">—</div>
          <div id="fp-hub" class="node-phrase muted">—</div>
        </div>
        ${flowNode("home", esc(tr("Home")), "a-home")}
        ${flowNode("grid", esc(tr("Grid")), "a-grid")}
        ${flowNode("eps", esc(tr("Backup")), "a-eps")}
      </div>`;
}

async function renderDashboard() {
  if (!(await loadState())) return null;
  fitRange();
  const label = RANGES.find(([r]) => r === S.histRange)?.[2] ?? "";
  return `
  <section class="panel now" aria-labelledby="now-h">
    <div class="sec-head">
      <h2 id="now-h" class="sr-only">${esc(tr("Power now"))}</h2>
      <span id="now-age" class="muted small grow" aria-live="off"></span>
      <div class="seg" role="group" aria-label="${esc(tr("Show power as"))}">
        ${[["now", tr("instant")], ["5m", tr("5 min avg")], ["30m", tr("30 min avg")]].map(([w, l]) =>
          `<button type="button" data-window="${w}" aria-pressed="${S.nowWindow === w}">${esc(l)}</button>`).join("")}
      </div>
    </div>
    <div class="now-body">
      ${flowMarkup(false)}
      <div class="batt">
        <h3 class="node-label"><span class="sm-key s-batt" aria-hidden="true"></span>${esc(tr("Battery"))}</h3>
        <div id="soc" class="hero">—</div>
        <div id="soc-meter" class="meter" role="meter" aria-label="${esc(tr("state of charge"))}" aria-valuemin="0" aria-valuemax="100" aria-valuenow="0">
          <div id="soc-bar" class="meter-fill"></div>
        </div>
        <p id="batt-phrase" class="batt-phrase">—</p>
        <p id="batt-eta" class="muted small" hidden></p>
        <dl class="kv small">
          <div><dt>${esc(tr("stored"))}</dt><dd id="stored-wh">—</dd></div>
          <div><dt>${esc(tr("pack voltage"))}</dt><dd id="pack-v">—</dd></div>
        </dl>
      </div>
    </div>
  </section>

  <section class="energy" aria-labelledby="energy-h">
    <div class="sec-head">
      <h2 id="energy-h">${esc(tr("Energy"))} <span class="muted">· <span id="range-label">${esc(label)}</span></span></h2>
      <div class="seg" role="group" aria-label="${esc(tr("History range"))}">
        ${RANGES.map(([r, short, , wide]) => `<button type="button" data-range="${r}" aria-pressed="${S.histRange === r}"${wide ? ' class="wide-only"' : ""}>${short}</button>`).join("")}
      </div>
    </div>
    <div id="kpis" class="kpis"></div>
    <div class="panel charts">
      <div id="sm" class="sm"></div>
      <div class="charts-foot">
        <button type="button" class="ghost small-btn" data-action="export-csv">${esc(tr("export CSV"))}</button>
        <button type="button" class="ghost small-btn" data-action="export-json">${esc(tr("export JSON"))}</button>
        <button type="button" class="ghost small-btn" data-action="toggle-hist-table" aria-expanded="false" aria-controls="hist-table">${esc(tr("show data table"))}</button>
      </div>
      <div id="hist-table" class="tablewrap" hidden></div>
    </div>
  </section>`;
}

async function fetchAuditPage(before, replace) {
  const f = S.auditFilters;
  const params = new URLSearchParams({ limit: "100" });
  if (f.source) params.set("source", f.source);
  if (f.action) params.set("action", f.action);
  if (before) params.set("before", before);
  const resp = await apiFetch("/audit?" + params);
  if (resp.status === 401) { gate(tr("the token was rejected — sign in again")); return false; }
  if (!resp.ok) return false;
  const data = await resp.json();
  const items = data.items ?? [];
  S.audit.cloudHidden = !!data.cloud_hidden;
  S.audit.items = replace ? items : S.audit.items.concat(items);
  S.audit.before = items.length ? items[items.length - 1].ts : null;
  S.audit.exhausted = items.length < 100;
  return true;
}

async function fetchMessagesPage(before, replace) {
  const f = S.msgFilters;
  const params = new URLSearchParams({ limit: "100" });
  if (f.dir) params.set("dir", f.dir);
  if (f.frame) params.set("frame", f.frame);
  if (before) params.set("before", before);
  const resp = await apiFetch("/messages?" + params);
  // the frame log needs the expert role: a lower token (403) or a
  // tokenless public session (401) gets an explanation, not the gate
  S.messages.denied = resp.status === 403 || (resp.status === 401 && S.public);
  if (S.messages.denied) return true;
  if (resp.status === 401) { gate(tr("the token was rejected — sign in again")); return false; }
  if (!resp.ok) return false;
  const data = await resp.json();
  const items = data.items ?? [];
  S.messages.items = replace ? items : S.messages.items.concat(items);
  S.messages.before = items.length ? items[items.length - 1].ts : null;
  S.messages.exhausted = items.length < 100;
  return true;
}

// ---------- shared page pieces (phone first) ----------

// writeNote: the per-feature availability line under a control (C-008):
// impossible on this model, or untested (an installer grant can enable it).
function writeSupport(feature) {
  return S.state?.identity?.writes?.[feature] ?? "verified";
}

function writeNote(feature) {
  const s = writeSupport(feature);
  if (s === "unmapped") return `<p class="note warn">${esc(tr("Not available on this inverter model yet — the setting is not mapped. Sharing a diagnostic bundle (More → Settings) helps add it."))}</p>`;
  if (s === "not_supported") return `<p class="note warn">${esc(tr("This inverter does not support this setting."))}</p>`;
  if (s === "untested") return `<p class="note warn">${esc(tr("Untested on this inverter model. An installer can enable untested writes under More → Device."))}</p>`;
  return "";
}

function writable(feature) {
  const s = writeSupport(feature);
  return s === "verified" || s === "untested";
}

function pageHead(title, sub) {
  return `<header class="page-head"><h2>${esc(title)}</h2>${sub ? `<p class="muted">${sub}</p>` : ""}</header>`;
}

// fmtAgo: a short relative time ("4 min ago"), the absolute time in the title.
function fmtAgo(ts) {
  if (!ts) return "—";
  const s = (Date.now() - new Date(ts).getTime()) / 1000;
  if (s < 45) return tr("just now");
  if (s < 3600) return tr("{n} min ago", { n: Math.round(s / 60) });
  if (s < 86400) return tr("{n} h ago", { n: Math.round(s / 3600) });
  return tr("{n} d ago", { n: Math.round(s / 86400) });
}

function dayLabel(ts) {
  const d = new Date(ts);
  const today = new Date();
  const y = new Date(); y.setDate(today.getDate() - 1);
  if (d.toDateString() === today.toDateString()) return tr("Today");
  if (d.toDateString() === y.toDateString()) return tr("Yesterday");
  return d.toLocaleDateString(locale(), { weekday: "short", day: "numeric", month: "short" });
}

// ---------- control ----------

const WORK_MODE_TEXT = {
  "self-use": [tr("Self-use"), tr("Solar and battery power the house first; surplus charges the battery, then exports.")],
  "feed-in": [tr("Feed-in first"), tr("Solar is exported first; the battery charges from what is left.")],
};

async function renderControl() {
  if (!(await loadState())) return null;
  const st = S.state;
  const wmOK = writable("work_mode");
  const wmBtn = (m) => {
    const active = m === st.work_mode;
    const [title, desc] = WORK_MODE_TEXT[m];
    // static JSON hx-vals (htmx parses it — no eval, CSP-safe)
    return `<button data-cmd="work-mode" type="button" class="choice" aria-pressed="${active}" ${wmOK ? "" : "disabled"}
        hx-put="${API}/work-mode" hx-encoding="json" hx-vals='{"work_mode":"${m}"}' hx-swap="none">
        <span class="choice-title">${esc(title)}${active
          ? (st.active_mode?.source === "schedule" ? ` <span class="chip">${esc(tr("set"))}</span>` : ` <span class="chip ok">${esc(tr("active"))}</span>`)
          : ""}</span>
        <span class="choice-desc">${esc(desc)}</span></button>`;
  };
  const am = st.active_mode ?? {};
  const allDay = (st.schedule?.enabled ? st.schedule.groups ?? [] : [])
    .find((g) => g.enabled && g.start === "00:00" && hhmmMin(g.end) >= 23 * 60 + 59);
  return `
  ${pageHead(tr("Control"), esc(tr("Changes are sent to the inverter and confirmed by reading them back.")))}
  <section class="stack" aria-label="${esc(tr("controls"))}">
    <article class="panel">
      <h3>${esc(tr("Running now"))}</h3>
      <p class="running"><strong>${esc(SCHED_TEXT[am.mode] ?? am.mode ?? "—")}</strong>
        <span class="muted">${am.source === "schedule"
          ? `${esc(tr("schedule group {n} ({from}–{to})", { n: am.group, from: am.start, to: am.end }))} · <a href="#/schedule">${esc(tr("Schedule"))}</a>`
          : esc(tr("the base work mode (no schedule group applies)"))}</span></p>
      ${flowMarkup(true)}
      ${limitsLegend(st)}
    </article>

    <article class="panel">
      <h3>${esc(tr("Base work mode"))}</h3>
      <p class="muted small">${esc(tr("Used when no schedule group covers the time."))}${allDay
        ? " " + esc(tr("Schedule group {n} runs all day ({mode}), so the base mode currently never applies — the FoxESS app's \"work mode\" is that group.", { n: allDay.index, mode: SCHED_TEXT[allDay.work_mode] ?? allDay.work_mode }))
        : ""}</p>
      <div class="choices">${IMMEDIATE_MODES.map(wmBtn).join("")}</div>
      ${writeNote("work_mode")}
      <p class="muted small">${esc(tr("Force charge / discharge and grid charging are time windows: see Schedule."))}</p>
    </article>

    ${limitsPanel(st)}
  </section>`;
}

// ---------- the Control page's limits (markers 1–5) ----------

// the setting endpoints the limits panel writes (paths after API)
const SETTING_PATHS = ["/current-limits", "/port-limits", "/battery-currents", "/power-limit", "/eps-mode"];

const SETTING_LABEL = {
  import_current_a: tr("import current (A)"), export_current_a: tr("export current (A)"),
  load_limit_w: tr("load limit (W)"), generation_limit_w: tr("generation limit (W)"),
  max_charge_current_a: tr("max charge current (A)"), max_discharge_current_a: tr("max discharge current (A)"),
  power_limit_pct: tr("power limit (%)"), eps_mode: tr("EPS mode"),
};

// numIn is one numeric field of a limits form.
function numIn(name, label, value, unit, min, max, step, ok) {
  return `<label>${esc(label)}<span class="unit-in"><input name="${name}" type="number" inputmode="decimal" min="${min}" max="${max}" step="${step}" value="${value ?? ""}" required ${ok ? "" : "disabled"}><span>${unit}</span></span></label>`;
}

// limitForm is one marker's form: fields, an apply button, the write note.
function limitForm(id, cmd, path, feature, fields, button) {
  const ok = writable(feature);
  return `<form id="${id}" data-cmd="${cmd}" hx-put="${API}${path}" hx-encoding="json" hx-swap="none">
      <div class="fields two">${fields(ok)}</div>
      <button data-cmd="${cmd}" type="submit" class="cmd" ${ok ? "" : "disabled"}>${esc(button)}</button>
    </form>${writeNote(feature)}`;
}

// limitsPanel: every inverter limit, numbered like the diagram's markers.
// Installer role except meter compensation (expert).
function limitsPanel(st) {
  const l = st.limits ?? {};
  const head = (n, title) => `<h4 class="lim-head"><span class="lim-num" aria-hidden="true">${n}</span>${esc(title)}</h4>`;
  const compOK = writable("meter_compensation");
  const epsOK = writable("eps_mode");
  const eps = st.eps_mode;
  return `<article class="panel limits">
      <h3>${esc(tr("Limits"))}</h3>
      <p class="muted small">${esc(tr("Numbered as on the diagram above. Installer role, except meter compensation (expert). Each change is read back from the inverter; the inverter may round values."))}</p>

      ${head(1, tr("Grid connection"))}
      ${limitForm("limits-form", "limits", "/limits", "power_limits", (ok) =>
        numIn("import_w", tr("Import limit"), l.import_w, "W", 0, 30000, 1, ok) +
        numIn("export_w", tr("Export limit"), l.export_w, "W", 0, 30000, 1, ok), tr("Apply power limits"))}
      ${limitForm("current-form", "current-limits", "/current-limits", "current_limits", (ok) =>
        numIn("import_current_a", tr("Import current limit"), l.import_current_a, "A", 2, 100, 0.1, ok) +
        numIn("export_current_a", tr("Export current limit"), l.export_current_a, "A", 2, 100, 0.1, ok), tr("Apply current limits"))}
      <form id="comp-form" data-cmd="meter-comp" hx-put="${API}/meter-compensation" hx-encoding="json" hx-swap="none">
        <div class="fields two">${numIn("w", tr("Meter compensation"), st.meter_compensation_w ?? 0, "W", -500, 500, 1, compOK)}</div>
        <p class="muted small">${esc(tr("Corrects a steady offset between the CT meter and the real flow (−500 to 500 W). Export limiting uses the corrected reading, so real export can exceed the export limit by up to this amount."))}</p>
        <button data-cmd="meter-comp" type="submit" class="cmd" ${compOK ? "" : "disabled"}>${esc(tr("Apply compensation"))}</button>
      </form>${writeNote("meter_compensation")}

      ${head(2, tr("Inverter grid port"))}
      ${limitForm("port-form", "port-limits", "/port-limits", "port_limits", (ok) =>
        numIn("load_limit_w", tr("Load limit"), l.load_limit_w, "W", 0, 65535, 1, ok) +
        numIn("generation_limit_w", tr("Generation limit"), l.generation_limit_w, "W", 0, 65535, 1, ok), tr("Apply port limits"))}
      <p class="muted small">${esc(tr("The load limit caps what the inverter draws from the switchboard — battery charging plus backup pass-through (verified 17 Nov); keep it within the inverter's supply breaker. The generation limit's effect is untested."))}</p>

      ${head(3, tr("Battery"))}
      ${limitForm("batt-form", "battery-currents", "/battery-currents", "battery_currents", (ok) =>
        numIn("max_charge_current_a", tr("Max charge current"), l.max_charge_current_a, "A", 0, 50, 0.1, ok) +
        numIn("max_discharge_current_a", tr("Max discharge current"), l.max_discharge_current_a, "A", 0, 50, 0.1, ok), tr("Apply battery currents"))}

      ${head(4, tr("Inverter output"))}
      ${limitForm("plim-form", "power-limit", "/power-limit", "power_limit", (ok) =>
        numIn("power_limit_pct", tr("Power limit"), l.power_limit_pct, esc(tr("% of rating")), 0, 100, 1, ok), tr("Apply power limit"))}

      ${head(5, tr("Backup (EPS) port"))}
      <form id="eps-form" data-cmd="eps-mode" data-confirm="epsMode" hx-put="${API}/eps-mode" hx-encoding="json" hx-swap="none">
        <div class="radio-list">${[["ups", tr("UPS mode"), tr("switches the backup circuits to the battery within milliseconds when the grid fails")],
          ["eps", tr("EPS mode"), tr("switches over more slowly when the grid fails")]].map(([v, t, d]) => `
          <label class="radio-row"><input type="radio" name="eps_mode" value="${v}" ${eps === v ? "checked" : ""} ${epsOK ? "" : "disabled"}>
            <span><strong>${esc(t)}</strong><span class="desc muted small">${esc(d)}</span></span></label>`).join("")}
        </div>
        ${eps === "disabled" ? `<p class="muted small">${esc(tr("The port is currently disabled; enabling and disabling it from here is not offered until that has been verified on this inverter."))}</p>` : ""}
        <button data-cmd="eps-mode" type="submit" class="cmd" ${epsOK ? "" : "disabled"}>${esc(tr("Apply EPS mode"))}</button>
      </form>${writeNote("eps_mode")}
    </article>`;
}

// ---------- schedule ----------

const SCHED_TEXT = {
  "self-use": tr("Self-use"), "feed-in": tr("Feed-in first"), "backup": tr("Backup"), "peak-shaving": tr("Peak shaving"),
  "power-station": tr("Power station"), "force-charge": tr("Force charge"), "force-discharge": tr("Force discharge"),
};
const FORCE_MODES = ["force-charge", "force-discharge"];

function schedKind(mode) {
  return mode === "force-charge" ? "chg" : mode === "force-discharge" ? "dis" : "base";
}

function hhmmMin(s) {
  const [h, m] = (s ?? "00:00").split(":").map(Number);
  return h * 60 + m;
}

// timeline: the enabled windows on one 24 h bar (identity by group
// number and mode word in the list below, never color alone)
function schedTimeline(groups, master) {
  const blocks = groups.filter((g) => g.enabled && !g.default).map((g) => {
    const a = hhmmMin(g.start), b = Math.max(hhmmMin(g.end), a + 1);
    // the position rides in data-*: the CSP forbids inline style attributes
    return `<div class="tl-block k-${schedKind(g.work_mode)}" data-a="${a}" data-b="${b}" title="${esc(tr("Group {n}", { n: g.index }))}: ${esc(SCHED_TEXT[g.work_mode] ?? g.work_mode)} ${esc(g.start)}–${esc(g.end)}"><span>${g.index}</span></div>`;
  }).join("");
  return `<div class="timeline${master ? "" : " off"}" aria-hidden="true">
      <div class="tl-bar">${blocks}</div>
      <div class="tl-ticks"><span>00</span><span>06</span><span>12</span><span>18</span><span>24</span></div>
    </div>`;
}

// fmtLimitW: a window's grid limit, compact ("35 kW"; ≥ 99 999 W reads
// as no limit — the cloud's 99999 / 100000 defaults)
function fmtLimitW(w) {
  if (w == null) return "—";
  if (w >= 99999) return tr("no limit");
  return (w / 1000).toLocaleString(locale(), { maximumFractionDigits: 1 }) + " kW";
}

// schedDetail: a force window's settings the FoxESS app owns (read-only
// here; saving the schedule keeps them): what happens at the target,
// grid charging (power 0 = off), PV, and the window's grid limits.
function schedDetail(g) {
  if (!FORCE_MODES.includes(g.work_mode)) return "";
  const chg = g.work_mode === "force-charge";
  const items = [];
  if (g.after_cutoff) items.push(tr("after the target: {mode}", { mode: SCHED_TEXT[g.after_cutoff] ?? g.after_cutoff }));
  if (chg) items.push(g.fd_pwr_w > 0 ? tr("charges from the grid") : tr("no charging from the grid"));
  if (g.pv != null) items.push(chg ? (g.pv ? tr("charges from solar") : tr("no charging from solar")) : (g.pv ? tr("discharges with solar") : tr("solar not used")));
  if (g.import_limit_w != null) items.push(tr("grid import {w}", { w: fmtLimitW(g.import_limit_w) }));
  if (g.export_limit_w != null) items.push(tr("export {w}", { w: fmtLimitW(g.export_limit_w) }));
  if (!items.length) return "";
  return `<p class="muted small g-detail">${items.map(esc).join(" · ")}<br><span>${esc(tr("Set in the FoxESS app; saving here keeps them."))}</span></p>`;
}

// schedDefault: the default group (the cloud's "remaining time" mode) —
// what runs outside every window: its mode and SoC limits, the time is
// always the whole day.
function schedDefault(g) {
  const i = g.index;
  const modes = SCHED_MODES.filter((m) => !FORCE_MODES.includes(m));
  return `<article class="panel default-mode" data-group="${i}">
    <input type="checkbox" name="g${i}-enabled" checked hidden>
    <input type="hidden" name="g${i}-start" value="00:00"><input type="hidden" name="g${i}-end" value="23:59">
    <input type="hidden" name="g${i}-fdsoc" value="${g.fd_soc_pct}"><input type="hidden" name="g${i}-fdpwr" value="${g.fd_pwr_w}">
    <h3>${esc(tr("Default mode"))}</h3>
    <p class="muted small">${esc(tr("Outside the windows below."))}</p>
    <label>${esc(tr("Mode"))}<select name="g${i}-mode">${modes.map((m) => `<option value="${m}" ${m === g.work_mode ? "selected" : ""}>${esc(SCHED_TEXT[m])}</option>`).join("")}</select></label>
    <div class="fields two">
      <label>${esc(tr("Max SoC from grid"))}<span class="unit-in"><input type="number" inputmode="numeric" min="10" max="100" name="g${i}-maxsoc" value="${g.max_soc_pct}"><span>%</span></span></label>
      <label>${esc(tr("Min SoC on grid"))}<span class="unit-in"><input type="number" inputmode="numeric" min="10" max="100" name="g${i}-minsoc" value="${g.min_soc_on_grid_pct}"><span>%</span></span></label>
    </div>
  </article>`;
}

function schedGroup(g) {
  const i = g.index;
  const on = g.enabled;
  const dis = on ? "" : " disabled";
  const force = FORCE_MODES.includes(g.work_mode);
  const summary = on
    ? `${esc(SCHED_TEXT[g.work_mode] ?? g.work_mode)} · <span class="num">${esc(g.start)}–${esc(g.end)}</span>`
    : `<span class="muted">${esc(tr("off"))}</span>`;
  // an unused group stays in the form (the save sends all eight), hidden
  // until "Add a window" brings it up
  return `<div class="group k-${schedKind(g.work_mode)}${on ? " on" : ""}" data-group="${i}"${on ? "" : " data-unused hidden"}>
    <input type="checkbox" switch name="g${i}-enabled" ${on ? "checked" : ""} aria-label="${esc(tr("Group {n} enabled", { n: i }))}">
    <details>
      <summary><span class="g-title">${esc(tr("Group {n}", { n: i }))}</span><span class="g-sum">${summary}</span></summary>
      <div class="fields two">
        <label>${esc(tr("Start"))}<input type="time" name="g${i}-start" value="${esc(g.start)}"${dis}></label>
        <label>${esc(tr("End"))}<input type="time" name="g${i}-end" value="${esc(g.end)}"${dis}></label>
      </div>
      <label>${esc(tr("Mode"))}<select name="g${i}-mode"${dis}>${SCHED_MODES.map((m) => `<option value="${m}" ${m === g.work_mode ? "selected" : ""}>${esc(SCHED_TEXT[m])}</option>`).join("")}</select></label>
      <div class="fields two">
        <label>${esc(tr("Max SoC from grid"))}<span class="unit-in"><input type="number" inputmode="numeric" min="10" max="100" name="g${i}-maxsoc" value="${g.max_soc_pct}"${dis}><span>%</span></span></label>
        <label>${esc(tr("Min SoC on grid"))}<span class="unit-in"><input type="number" inputmode="numeric" min="10" max="100" name="g${i}-minsoc" value="${g.min_soc_on_grid_pct}"${dis}><span>%</span></span></label>
      </div>
      <div class="fields two force"${force ? "" : " hidden"}>
        <label>${esc(tr("Target SoC"))}<span class="unit-in"><input type="number" inputmode="numeric" min="10" max="100" name="g${i}-fdsoc" value="${g.fd_soc_pct}"${dis}><span>%</span></span></label>
        <label>${esc(tr("Power"))}<span class="unit-in"><input type="number" inputmode="numeric" min="0" max="10500" step="1" name="g${i}-fdpwr" value="${g.fd_pwr_w}"${dis}><span>W</span></span></label>
      </div>
      ${schedDetail(g)}
    </details>
  </div>`;
}

async function renderSchedule() {
  if (!(await loadState())) return null;
  const sch = S.state.schedule;
  const groups = [];
  for (let i = 1; i <= 8; i++) {
    groups.push((sch?.groups ?? []).find((x) => x.index === i) ??
      { index: i, enabled: false, start: "00:00", end: "00:00", work_mode: "self-use", max_soc_pct: 100, min_soc_on_grid_pct: 10, fd_soc_pct: 10, fd_pwr_w: 0 });
  }
  const ok = writable("schedule");
  const def = groups.find((g) => g.default);
  const windows = groups.filter((g) => !g.default);
  const spare = windows.some((g) => !g.enabled);
  const shown = windows.filter((g) => g.enabled).length;
  return `
  ${pageHead(tr("Schedule"), esc(def
    ? tr("Daily time windows, and the default mode that runs outside them.")
    : tr("Daily time windows. Outside them the inverter follows its work mode.")))}
  <form id="schedule-form" data-cmd="schedule" hx-put="${API}/schedule" hx-encoding="json" hx-swap="none">
    <article class="panel">
      <label class="row"><input id="sched-enabled" name="enabled" type="checkbox" switch ${sch?.enabled ? "checked" : ""}> <strong>${esc(tr("Scheduler"))}</strong> <span class="muted small">${esc(sch?.enabled ? tr("on") : tr("off — the schedule below is ignored"))}</span></label>
      ${schedTimeline(groups, sch?.enabled)}
      <p class="legend small"><span><i class="lg k-chg"></i>${esc(tr("Force charge"))}</span><span><i class="lg k-dis"></i>${esc(tr("Force discharge"))}</span><span><i class="lg k-base"></i>${esc(tr("Other modes"))}</span>${def ? `<span>${esc(tr("Otherwise: {mode}", { mode: SCHED_TEXT[def.work_mode] ?? def.work_mode }))}</span>` : ""}</p>
    </article>
    ${def ? `<div class="groups${sch?.enabled ? "" : " master-off"}">${schedDefault(def)}</div>` : ""}
    <h3 class="sub-head">${esc(tr("Windows"))}</h3>
    <div class="groups${sch?.enabled ? "" : " master-off"}">${windows.map(schedGroup).join("")}
      ${shown ? "" : `<p class="muted small" id="sched-none">${esc(def ? tr("No windows: the default mode runs all day.") : tr("No windows: the work mode runs all day."))}</p>`}
    </div>
    <button type="button" class="ghost" data-action="sched-add" ${spare ? "" : "disabled"}>${esc(tr("Add a window"))}</button>
    <p class="muted small">${esc(tr("Windows may nest (a narrow window inside a long one) or touch, but not partly overlap. End must be after start."))}</p>
    ${writeNote("schedule")}
    <div class="savebar">
      <span id="sched-dirty" class="muted small">${esc(tr("No changes"))}</span>
      <button type="button" class="ghost" data-action="sched-reset">${esc(tr("Reset"))}</button>
      <button data-cmd="schedule" type="submit" class="cmd" ${ok ? "" : "disabled"}>${esc(tr("Save schedule"))}</button>
    </div>
  </form>`;
}

// ---------- activity (audit) ----------

const ACTION_CHIPS = [["", tr("All")], ["allow", tr("Allowed")], ["drop", tr("Blocked")], ["hold", tr("Held")]];
const ACTION_NAME = { allow: tr("allowed"), drop: tr("blocked"), hold: tr("held") };

function actionKind(a) {
  return a === "drop" ? "err" : a === "allow" ? "ok" : "warn";
}

async function renderAudit() {
  const first = S.audit.items.length === 0;
  if (first && !(await fetchAuditPage(null, true))) return null;
  const f = S.auditFilters;
  let lastDay = "";
  const items = S.audit.items.map((a) => {
    const day = dayLabel(a.ts);
    const head = day !== lastDay ? `<li class="day">${esc(day)}</li>` : "";
    lastDay = day;
    const conf = a.confirmed && a.confirmed !== "not-applicable"
      ? `<span class="chip ${a.confirmed === "yes" ? "ok" : a.confirmed === "no" ? "err" : "warn"}">${esc(a.confirmed === "yes" ? tr("confirmed") : a.confirmed === "no" ? tr("not applied") : tr("unconfirmed"))}</span>` : "";
    return `${head}<li class="item">
      <span class="item-mark ${actionKind(a.action)}" aria-hidden="true"></span>
      <div class="item-main">
        <div class="item-title">${esc(a.summary)}</div>
        <div class="item-meta muted small"><time datetime="${esc(a.ts)}" title="${esc(fmtTime(a.ts))}">${fmtAgo(a.ts)}</time> · ${esc(a.source)}${a.source_role ? " (" + esc(fmtRole(a.source_role)) + ")" : ""} · <span class="chip ${actionKind(a.action)}">${esc(ACTION_NAME[a.action] ?? a.action)}</span> ${conf}</div>
      </div>
    </li>`;
  }).join("");
  return `
  ${pageHead(tr("Activity"), esc(tr("Every command sent to the inverter and every cloud message allowed or blocked, newest first.")))}
  <form id="audit-form" class="filterbar" role="search">
    <div class="chips" role="group" aria-label="${esc(tr("Filter by action"))}">
      ${ACTION_CHIPS.map(([v, l]) => `<button type="submit" name="action" value="${v}" class="chipbtn" aria-pressed="${f.action === v}">${esc(l)}</button>`).join("")}
    </div>
    <label class="search"><span class="sr-only">${esc(tr("Source"))}</span><input type="text" name="source" value="${esc(f.source)}" placeholder="${esc(tr("Filter by source (token name or cloud)"))}"></label>
  </form>
  ${S.audit.cloudHidden ? `<p class="note muted small">${esc(tr("The cloud's individual messages, allowed or blocked, are shown to the expert and installer roles only. Sign in with such a token to see them."))}</p>` : ""}
  <ul class="list">${items || `<li class="empty muted">${esc(f.source || f.action ? tr("No activity for this filter.") : tr("No activity."))}</li>`}</ul>
  ${S.audit.exhausted ? "" : `<button type="button" class="ghost wide" data-action="audit-more">${esc(tr("Load older"))}</button>`}`;
}

// ---------- more (hub) ----------

async function renderMore() {
  if (!(await loadState())) return null;
  const id = S.state.identity ?? {};
  const devSub = id.model
    ? `${esc(id.model)} · fw ${esc(id.firmware?.master ?? "?")} · ${esc(id.verified_profile ? tr("verified") : tr("unverified model"))}`
    : esc(tr("not identified yet"));
  const link = (route, title, sub, ico) => `<li><a class="hub-link" href="#/${route}">
      <svg class="ico" viewBox="0 0 24 24" aria-hidden="true">${ico}</svg>
      <span><span class="hub-title">${esc(title)}</span><span class="hub-sub muted small">${sub}</span></span>
      <svg class="ico chev" viewBox="0 0 24 24" aria-hidden="true"><path d="m9 6 6 6-6 6"/></svg></a></li>`;
  return `
  ${pageHead(tr("More"))}
  <ul class="hub">
    ${link("device", tr("Device"), devSub, '<rect x="5" y="3" width="14" height="18" rx="2"/><path d="M9 7h6M9 11h6"/>')}
    ${link("cloud", tr("Cloud & maintenance"), esc(tr("cloud forwarding: {mode}", { mode: FORWARD_NAME()[S.forwardMode?.mode] ?? "—" })), '<path d="M7 18a4 4 0 0 1-.5-8 6 6 0 0 1 11.5 1.5A3.5 3.5 0 0 1 17.5 18z"/>')}
    ${link("setup", tr("Setup"), esc(tr("checks, battery capacity, cloud and access choices")), '<path d="m5 12 4 4 10-10"/>')}
    ${link("settings", tr("Settings"), esc(tr("language, access tokens, notifications, tuning, diagnostics")), '<circle cx="12" cy="12" r="3"/><path d="M12 3v2M12 19v2M3 12h2M19 12h2M5.6 5.6l1.4 1.4M17 17l1.4 1.4M5.6 18.4 7 17M17 7l1.4-1.4"/>')}
    ${link("report", tr("Report a problem"), esc(tr("one file for the developers, plus a prefilled issue")), '<path d="M12 3 2 21h20zM12 9v5M12 17.5v.5"/>')}
    ${link("quirks", tr("Inverter quirks"), esc(tr("compensate for known reading quirks")), '<path d="M4 12h3l2-5 3 10 2-5h6"/><circle cx="19" cy="6" r="1.5"/>')}
    ${link("messages", tr("Frame messages"), esc(tr("the raw message log (diagnostics; expert)")), '<path d="M4 6h16M4 12h16M4 18h10"/>')}
  </ul>`;
}

// msgStatus is a message's status chip: what happened to the frame, the
// full policy / routing reason in the tooltip. A plain forwarded frame
// gets none.
function msgStatus(m) {
  const r = m.reason ?? "";
  let txt = "", cls = "";
  if (m.decision === "dropped") [txt, cls] = [tr("blocked"), "err"];
  else if (m.decision === "injected") txt = tr("sent by the app") + (m.origin ? ` (${m.origin})` : "");
  else if (m.decision === "residue") [txt, cls] = [tr("partial frame"), "warn"];
  else if (r.includes("kept from the cloud")) txt = tr("kept from the cloud");
  else if (r.startsWith("mode:telemetry_only read:")) [txt, cls] = [tr("relayed (read-only)"), "ok"];
  if (!txt) return "";
  return `<span class="chip ${cls}"${r ? ` title="${esc(r)}"` : ""}>${esc(txt)}</span>`;
}

// FW_COMPONENT: the firmware components an upgrade task names
const FW_COMPONENT = { master: tr("master"), slave: tr("slave"), manager: tr("manager"), datalogger: tr("datalogger") };

// msgFirmware: a readable line for a firmware upgrade task or in-band
// firmware announcement (profile §6), "" for any other frame. The task's
// address is the file the dongle would download (a link, https only).
function msgFirmware(m) {
  let d;
  try { d = JSON.parse(m.summary ?? "null"); } catch { return ""; }
  const t = d?.firmware_task, p = d?.firmware_push;
  if (t) {
    const url = typeof t.url === "string" && t.url.startsWith("https://") ? t.url : "";
    return `<p class="small fw-task"><strong>${esc(tr("Firmware update task"))}</strong>: ${esc(tr("{component} firmware {file}, {size} bytes, CRC {crc}", {
      component: FW_COMPONENT[t.component] ?? t.component, file: t.filename, size: t.size, crc: t.crc,
    }))}${url ? `<br>${esc(tr("Download address"))}: <a href="${esc(url)}" target="_blank" rel="noopener noreferrer" class="mono">${esc(url)}</a>` : ""}</p>`;
  }
  if (p) {
    return `<p class="small fw-task"><strong>${esc(tr("Firmware push"))}</strong>: ${esc(tr("{file}, {size} bytes, CRC {crc}", { file: p.name, size: p.size, crc: p.crc }))}</p>`;
  }
  return "";
}

// ---------- device scan (F8, read-only) ----------

let scanTimer = 0;

// paintScan shows a scan's progress; done points at the report.
function paintScan(st) {
  const el = byId("scan-status");
  if (!el || !st) return;
  if (st.state === "running") {
    el.textContent = tr("Scanning… {done} of {total} reads", { done: st.blocks_done, total: st.blocks_total });
  } else if (st.state === "done") {
    el.innerHTML = esc(tr("Done: {ok} of {total} reads answered", { ok: st.blocks_ok, total: st.blocks_total })) +
      (st.blocks_failed ? esc(tr(", {n} refused by the inverter (expected for registers it does not have)", { n: st.blocks_failed })) : "") +
      `. <a href="#/report">${esc(tr("Report it"))}</a> ${esc(tr("to share the result."))}`;
  } else {
    el.textContent = "";
  }
}

async function scanPoll() {
  clearTimeout(scanTimer);
  const r = await apiFetch("/device-scan");
  if (!r.ok) return;
  const st = await r.json();
  paintScan(st);
  if (st.state === "running") scanTimer = setTimeout(scanPoll, 1000);
}

async function scanStart() {
  const r = await apiFetch("/device-scan", { method: "POST" });
  if (!r.ok && r.status !== 409) {
    const p = await problemOf(r);
    toast(`${p.code ?? "error"}: ${CODE_TEXT[p.code] ? CODE_TEXT[p.code](p) : (p.detail ?? p.title)}`, "error");
    return;
  }
  scanPoll();
}

// ---------- setup wizard (F12) ----------

const CHECK_CHIP = { ok: [tr("ok"), "ok"], warn: [tr("check"), "warn"], fail: [tr("needs attention"), "err"], pending: [tr("waiting"), "muted"] };

// CHECK_TITLE: the setup checks' titles by id (their details come from
// the server, in English)
const CHECK_TITLE = {
  inverter_connected: tr("Inverter connected"), identified: tr("Inverter identified"),
  verified: tr("Settings support"), clock: tr("Clocks agree"), battery_capacity: tr("Battery capacity"),
};

const FORWARD_CHOICES = [
  ["off", tr("Nothing"), tr("The cloud and the FoxESS app see nothing. Recommended to start.")],
  ["telemetry_only", tr("Readings only"), tr("The FoxESS app keeps showing data; the cloud cannot change anything.")],
  ["full", tr("Everything, as before"), tr("The cloud can change settings too. A firmware update still needs a maintenance window.")],
];

async function renderSetup() {
  const [sr, gr] = await Promise.all([apiFetch("/setup"), apiFetch("/settings")]);
  if (sr.status === 401) { gate(tr("the token was rejected — sign in again")); return null; }
  if (!sr.ok) {
    const p = await problemOf(sr);
    return `${pageHead(tr("Setup"))}<article class="panel"><p class="muted">${esc(p.code === "insufficient_role"
      ? tr("Setup needs an installer token (yours: {role}).", { role: p.token_role }) : tr("Setup is not available on this server."))}</p></article>`;
  }
  const s = await sr.json();
  const set = gr.ok ? await gr.json() : { settings: [] };
  const val = (k) => set.settings.find((x) => x.key === k)?.value;
  const src = (k) => set.settings.find((x) => x.key === k)?.source;
  const fwd = val("forward.mode") ?? S.forwardMode?.mode;
  const capWh = val("battery.capacity_wh");
  const modules = s.battery?.modules;
  const zone = val("site.time_zone");
  const browserZone = Intl.DateTimeFormat().resolvedOptions().timeZone ?? "";
  const zones = Intl.supportedValuesOf?.("timeZone") ?? [];
  const pick = zone ?? browserZone;
  if (pick && !zones.includes(pick)) zones.unshift(pick);

  const checks = s.checks.map((c) => {
    const [txt, cls] = CHECK_CHIP[c.status] ?? [c.status, "muted"];
    return `<li class="item">
      <div class="item-main">
        <div class="item-title">${esc(CHECK_TITLE[c.id] ?? c.title)} <span class="chip ${cls}">${esc(txt)}</span></div>
        <div class="item-meta muted small">${esc(c.detail)}</div>
        ${c.fix && c.status !== "ok" ? `<p class="small">${esc(c.fix)}</p>` : ""}
        ${c.action === "set_clock" ? `<button type="button" class="ghost" data-action="setup-set-clock">${esc(tr("Set the inverter's clock"))}</button>` : ""}
      </div></li>`;
  }).join("");
  // waiting checks need only time; a failed one needs the owner
  const allOk = s.checks.every((c) => c.status !== "fail");

  return `<div class="setup">
  ${pageHead(tr("Setup"), esc(s.completed ? tr("Setup was completed; you can change anything here at any time.") : tr("A few checks and choices to get foxesslocal running. Everything can be changed later.")))}
  <article class="panel">
    <h3>1 · ${esc(tr("Checks"))}</h3>
    <ul class="list compact">${checks}</ul>
    <button type="button" class="ghost" data-action="setup-refresh">${esc(tr("Check again"))}</button>
  </article>
  <article class="panel">
    <h3>2 · ${esc(tr("Time zone"))}</h3>
    <p class="muted small">${esc(tr("The inverter's clock has no time zone: it shows local time, and its schedule runs on it. Choose where the inverter is — this machine's own time zone does not matter. foxesslocal keeps the inverter's clock to this zone, daylight saving included."))}</p>
    <label>${esc(tr("Time zone"))}${zones.length
      ? `<select id="setup-zone">${zones.map((z) => `<option value="${esc(z)}" ${z === pick ? "selected" : ""}>${esc(z)}</option>`).join("")}</select>`
      : `<input type="text" id="setup-zone" placeholder="Australia/Melbourne" value="${esc(pick)}">`}</label>
    <p class="muted small">${esc(zone ? tr("Currently {zone} ({source}).", { zone, source: SOURCE_TEXT[src("site.time_zone")] ?? "" })
      : browserZone ? tr("Not set yet. Suggested from this browser: {zone}.", { zone: browserZone }) : tr("Not set yet."))}</p>
    <button type="button" class="cmd" data-action="setup-zone">${esc(tr("Save time zone"))}</button>
  </article>
  <article class="panel">
    <h3>3 · ${esc(tr("Battery capacity"))}</h3>
    <p class="muted small">${esc(tr("The battery's usable energy, used to show how much is stored. It is the number of battery modules times the energy of one module."))} ${esc(modules
        ? tr("The inverter reports {n} modules — for EQ4800 modules (4.66 kWh each) that is {kwh} kWh.", { n: modules, kwh: (modules * 4.66).toFixed(2) })
        : tr("An EQ4800 module is 4.66 kWh. The module count shows here once the inverter has sent its battery details."))}</p>
    <label>${esc(tr("Usable capacity (kWh)"))}<input type="number" id="setup-capacity" min="0.1" max="200" step="0.01" value="${capWh ? (capWh / 1000).toFixed(2) : ""}"></label>
    <p class="muted small">${esc(capWh ? tr("Currently {kwh} kWh ({source}).", { kwh: (capWh / 1000).toFixed(2), source: SOURCE_TEXT[src("battery.capacity_wh")] ?? "" }) : tr("Not set yet."))}</p>
    <button type="button" class="cmd" data-action="setup-capacity">${esc(tr("Save capacity"))}</button>
  </article>
  <article class="panel">
    <h3>4 · ${esc(tr("Cloud forwarding"))}</h3>
    <p class="muted small">${esc(tr("What reaches the FoxESS cloud and the FoxESS app, and whether they can change anything. The same setting as More → Cloud & maintenance; a maintenance window overrides it for a while."))}</p>
    <div class="radio-list">${FORWARD_CHOICES.map(([m, t, d]) => `
      <label class="radio-row"><input type="radio" name="setup-forward" value="${m}" ${fwd === m ? "checked" : ""}>
        <span><strong>${esc(t)}</strong><span class="desc muted small">${esc(d)}</span></span></label>`).join("")}
    </div>
    <button type="button" class="cmd" data-action="setup-forward">${esc(tr("Save cloud choice"))}</button>
  </article>
  <article class="panel">
    <h3>5 · ${esc(tr("Visitors without a token"))}</h3>
    <p class="muted small">${esc(tr("Separate from cloud forwarding: this is about people on your own network opening this web page."))}</p>
    <label class="radio-row"><input type="checkbox" id="setup-public" ${val("access.public_read_only") ? "checked" : ""}>
      <span>${esc(tr("Can see the dashboard (read only)"))}</span></label>
    <label class="radio-row"><input type="checkbox" id="setup-public-all" ${val("access.public_all_pages") ? "checked" : ""}>
      <span>${esc(tr("…and every other page (still read only)"))}</span></label>
    <p class="muted small">${esc(tr("Anyone on your network can open this page. Changes always need a token."))}</p>
    <button type="button" class="cmd" data-action="setup-access">${esc(tr("Save access"))}</button>
  </article>
  <article class="panel">
    <h3>6 · ${esc(tr("Done"))}</h3>
    <p class="muted small">${esc(tr("Each section above takes effect as soon as you save it; this changes no setting. Marking setup as done only stops this page opening by itself when an installer signs in — it stays under More → Setup."))}
      ${esc(allOk ? tr("Everything needed is in place.") : tr("Some checks still need attention: you can mark it done now and come back."))}</p>
    ${s.completed ? `<p class="small">${esc(tr("Setup is marked as done."))}</p>`
      : `<button type="button" class="cmd" data-action="setup-finish">${esc(tr("Mark setup as done"))}</button>`}
  </article></div>`;
}

const SOURCE_TEXT = { ui: tr("set here"), config: tr("from the config file"), default: tr("built-in default") };

// setupSave sends one setup change and reports the outcome.
async function setupSave(path, method, body, okMsg) {
  const r = await apiFetch(path, { method, headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  if (!r.ok) {
    const p = await problemOf(r);
    toast(tr("not saved"), "err", (CODE_TEXT[p.code] ?? (() => p.detail ?? p.title))(p));
    return false;
  }
  toast(okMsg);
  return true;
}

// saveZone stores the site time zone; when the inverter's clock
// disagrees with it (409 zone_mismatch), the owner confirms and the
// inverter's clock is set to it.
async function saveZone(z) {
  const body = JSON.stringify({ "site.time_zone": z });
  const put = (q) => apiFetch("/settings" + q, { method: "PUT", headers: { "Content-Type": "application/json" }, body });
  let r = await put("");
  if (r.status === 409) {
    const p = await problemOf(r);
    if (p.code !== "zone_mismatch") { toast(tr("not saved"), "err", p.detail ?? p.title); return false; }
    if (!(await confirmDlg(tr("The inverter's clock disagrees"), `${p.detail ?? ""}\n\n${tr("Save this time zone anyway and set the inverter's clock to it?")}`,
      tr("Save and set the clock")))) return false;
    r = await put("?confirm=zone");
    if (!r.ok) { const q = await problemOf(r); toast(tr("not saved"), "err", q.detail ?? q.title); return false; }
    toast(tr("time zone saved"));
    await setInverterClock();
    return true;
  }
  if (!r.ok) { const p = await problemOf(r); toast(tr("not saved"), "err", (CODE_TEXT[p.code] ?? (() => p.detail ?? p.title))(p)); return false; }
  toast(tr("time zone saved"));
  return true;
}

// setInverterClock sets the inverter's clock to the site's time now.
async function setInverterClock() {
  const r = await apiFetch("/clock/set", { method: "POST" });
  if (!r.ok) {
    const p = await problemOf(r);
    toast(tr("the inverter's clock was not set"), "err", (CODE_TEXT[p.code] ?? (() => p.detail ?? p.title))(p));
    return false;
  }
  const b = await r.json();
  toast(tr("inverter clock set to {time}", { time: b.set_to.replace("T", " ") }));
  return true;
}

async function setupAction(action) {
  switch (action) {
    case "setup-refresh":
      render();
      break;
    case "setup-capacity": {
      const kwh = parseFloat(byId("setup-capacity").value);
      if (!(kwh > 0)) { toast(tr("enter the usable capacity in kWh"), "warn"); return; }
      if (await setupSave("/settings", "PUT", { "battery.capacity_wh": Math.round(kwh * 1000) }, tr("battery capacity saved"))) render();
      break;
    }
    case "setup-zone": {
      const z = byId("setup-zone").value.trim();
      if (!z) { toast(tr("choose the inverter's time zone"), "warn"); return; }
      if (await saveZone(z)) render();
      break;
    }
    case "setup-set-clock": {
      const set = await apiFetch("/settings");
      const z = set.ok ? (await set.json()).settings.find((x) => x.key === "site.time_zone")?.value : null;
      if (!z) { toast(tr("choose the inverter's time zone"), "warn"); return; }
      if (!(await confirmDlg(tr("Set the inverter's clock?"),
        tr("The inverter's clock will be set to {zone} time now. Its schedule runs on this clock.", { zone: z }), tr("Set the clock")))) return;
      if (await setInverterClock()) render();
      break;
    }
    case "setup-forward": {
      const m = document.querySelector('input[name="setup-forward"]:checked')?.value;
      if (!m) { toast(tr("choose what the cloud may receive"), "warn"); return; }
      if (await setupSave("/forward-mode", "PUT", { mode: m }, tr("cloud choice saved — the inverter reconnects under it within seconds"))) {
        S.forwardMode = { mode: m };
        renderForwardBadge();
        render();
      }
      break;
    }
    case "setup-access": {
      const pub = byId("setup-public").checked;
      const all = pub && byId("setup-public-all").checked;
      if (await setupSave("/settings", "PUT", { "access.public_read_only": pub, "access.public_all_pages": all }, tr("access saved"))) {
        S.cfg = null;
        refreshMeta();
        render();
      }
      break;
    }
    case "setup-finish": {
      const r = await apiFetch("/setup/complete", { method: "POST" });
      if (!r.ok) { toast(tr("could not record setup"), "err"); return; }
      toast(tr("setup complete"));
      location.hash = "#/dashboard";
      break;
    }
  }
}

// ---------- device ----------

const LEVEL_TEXT = {
  exact: [tr("verified"), "ok"], compatible: [tr("compatible"), "warn"], tentative: [tr("tentative"), "warn"],
  pending: [tr("not read yet"), "muted"], unknown: [tr("not trusted"), "err"],
};

async function renderDevice() {
  if (!(await loadState())) return null;
  const id = S.state.identity ?? {};
  let ov = null;
  try {
    const r = await apiFetch("/write-override");
    if (r.ok) ov = await r.json();
  } catch { /* shown without the grant state */ }
  const fw = id.firmware ?? {};
  const v = (s) => esc(s || tr("not seen yet"));
  const status = id.verified_profile
    ? `<p class="note ok">${esc(tr("Verified model and firmware ({profile}): every setting here has been confirmed on this exact inverter.", { profile: id.verified_profile }))}</p>`
    : `<p class="note warn">${esc(tr("This model or firmware is not verified yet. Monitoring works; settings show whether they are available, untested or not mapped."))}</p>`;
  const rows = Object.keys(COMPAT_LABEL).filter((k) => id.compatibility?.[k]).map((k) => {
    const [txt, cls] = LEVEL_TEXT[id.compatibility[k]] ?? [id.compatibility[k], "muted"];
    return `<div><dt>${esc(COMPAT_LABEL[k])}</dt><dd><span class="chip ${cls}">${esc(txt)}</span></dd></div>`;
  }).join("");
  return `
  ${pageHead(tr("Device"))}
  <section class="stack">
    <article class="panel">
      <h3>${esc(tr("Inverter"))}</h3>
      <dl class="kv">
        <div><dt>${esc(tr("Model"))}</dt><dd>${v(id.model)}</dd></div>
        <div><dt>${esc(tr("Serial"))}</dt><dd class="num">${v(id.sn)}</dd></div>
        <div><dt>${esc(tr("Firmware"))}</dt><dd class="num">${v(fw.master)} / ${v(fw.slave)} / ${v(fw.manager)}</dd></div>
      </dl>
      ${status}
    </article>
    <article class="panel">
      <h3>${esc(tr("Confidence per feature"))}</h3>
      <dl class="kv">${rows || `<div><dt>—</dt><dd class="muted">${esc(tr("not evaluated yet"))}</dd></div>`}</dl>
      <p class="muted small">${esc(tr("Re-checked on every reading."))} <a href="${API}/compatibility" target="_blank" rel="noopener">${esc(tr("Compatibility report"))}</a></p>
    </article>
    ${id.verified_profile ? "" : `<article class="panel">${writeOverrideSection(ov)}</article>`}
    <article class="panel">
      <h3>${esc(tr("Register scan"))}</h3>
      <p class="muted small">${esc(id.verified_profile
        ? tr("Reads every register that the known maps list for this model, so a developer can check them against the inverter.")
        : tr("This model is not verified yet. A scan reads every register that the community maps list for it — read-only, nothing is changed — so a developer can check which map fits and work towards full support."))}
        ${esc(tr("It takes about ten seconds; live values keep updating. The result joins your next problem report (More → Report a problem). Installer role."))}</p>
      <button type="button" class="cmd" data-action="scan-start">${esc(tr("Scan registers"))}</button>
      <p id="scan-status" class="small" aria-live="polite"></p>
    </article>
  </section>`;
}

// writeOverrideSection: the installer's grant of untested writes for this
// exact model and firmware (C-008). Shown only for an unverified device.
function writeOverrideSection(ov) {
  if (!ov) return `<h3>${esc(tr("Write access"))}</h3><p class="muted small">${esc(tr("Not available."))}</p>`;
  const head = `<h3>${esc(tr("Write access"))}</h3>`;
  if (ov.granted) {
    return `${head}<p class="note warn">${esc(tr("Untested writes are enabled for {model} fw {fw} (by {by}).", { model: ov.device.model, fw: ov.device.firmware.master, by: ov.grant?.by ?? "?" }))}</p>
      <button type="button" class="ghost" data-cmd="write-override" data-confirm="writeOverrideRevoke" hx-delete="${API}/write-override" hx-swap="none">${esc(tr("Disable untested writes"))}</button>`;
  }
  return `${head}<p class="small">${esc(tr("Writes are limited to settings verified on this model and firmware."))}</p>
    <p class="note warn">${esc(tr("An installer can enable untested writes for this exact inverter. They may misconfigure it."))}</p>
    <button type="button" class="cmd danger" data-cmd="write-override" data-confirm="writeOverride" hx-put="${API}/write-override" hx-encoding="json" hx-vals='{"acknowledge_risk":true}' hx-swap="none">${esc(tr("Enable untested writes…"))}</button>`;
}

// feature labels (device page)
const COMPAT_LABEL = {
  live_values: tr("Live values"), settings_read: tr("Settings read"), work_mode: tr("Work mode"),
  power_limits: tr("Grid power limits"), current_limits: tr("Grid current limits"),
  meter_compensation: tr("Meter compensation"), schedule: tr("Schedule"),
};

// ---------- cloud & maintenance ----------

const FORWARD_TEXT = {
  off: [tr("Off"), tr("The inverter's cloud channel is blocked at the gate. The FoxESS app shows no live data; local monitoring and control continue.")],
  telemetry_only: [tr("Telemetry only"), tr("The cloud receives the inverter's data but cannot change anything.")],
  full: [tr("Full"), tr("The cloud has its normal access, including settings. A firmware update needs a maintenance window: anything the cloud is not known to send is held until one is open.")],
};

async function fetchMaintenance() {
  const resp = await apiFetch("/maintenance");
  if (resp.status === 401) { gate(tr("the token was rejected — sign in again")); return null; }
  if (!resp.ok) {
    const p = await problemOf(resp);
    return { error: p };
  }
  return await resp.json();
}

async function renderCloud() {
  if (!(await loadState())) return null;
  const m = await fetchMaintenance();
  if (m === null) return null;
  const mode = S.forwardMode?.mode ?? S.state.forward_mode;
  const choice = (k) => {
    const [t, d] = FORWARD_TEXT[k];
    return `<label class="choice radio"><input type="radio" name="mode" value="${k}" ${k === mode ? "checked" : ""}>
      <span class="choice-title">${esc(t)}${k === mode ? ` <span class="chip">${esc(tr("current"))}</span>` : ""}</span><span class="choice-desc">${esc(d)}</span></label>`;
  };
  let maint;
  if (m.error) {
    maint = `<p class="error">${esc(m.error.code)}: ${esc(m.error.detail ?? m.error.title)}</p>`;
  } else if (m.active) {
    maint = `<div class="countdown"><span id="maint-remaining" class="hero num" data-expires="${esc(m.expires_at)}">${Math.floor(m.remaining_s / 60)}:${String(m.remaining_s % 60).padStart(2, "0")}</span><span class="muted">${esc(tr("remaining · until {time}", { time: fmtTime(m.expires_at) }))}</span></div>
      <p class="note warn">${esc(tr("The cloud has unrestricted access until then (including firmware). \"{mode}\" is restored at the deadline.", { mode: FORWARD_NAME()[m.overridden_from] ?? m.overridden_from ?? "" }))}</p>
      ${m.capturing ? `<p class="note">${esc(tr("Recording this window's traffic (see Captures below)."))}</p>` : ""}
      <button type="button" class="cmd danger" data-cmd="maint-close" data-confirm="maintClose" hx-delete="${API}/maintenance" hx-swap="none">${esc(tr("Close window now"))}</button>`;
  } else {
    maint = `<p class="small">${esc(tr("Temporarily give the cloud full access — for example to let the FoxESS app change a setting or install firmware. Installer role; it ends on its own and does not survive a restart."))}</p>
    <form data-cmd="maint-open" hx-post="${API}/maintenance" hx-encoding="json" hx-swap="none">
      <div class="chips" role="radiogroup" aria-label="${esc(tr("Duration"))}">
        ${["15m", "30m", "1h", "2h", "4h"].map((d) => `<label class="chipradio"><input type="radio" name="duration" value="${d}" ${d === "30m" ? "checked" : ""}><span>${d}</span></label>`).join("")}
      </div>
      <label class="row"><input type="checkbox" name="capture"> <span>${esc(tr("Record the traffic"))} <span class="muted small">${esc(tr("— a byte-exact copy of everything in the window, e.g. a firmware update, kept until you delete it"))}</span></span></label>
      <button data-cmd="maint-open" type="submit" class="cmd" data-confirm="maintOpen">${esc(tr("Open window"))}</button>
    </form>`;
  }
  const caps = await fetchCaptures();
  return `
  ${pageHead(tr("Cloud & maintenance"), esc(tr("How much of the FoxESS cloud this inverter may talk to.")))}
  <section class="stack">
    <article class="panel">
      <h3>${esc(tr("Cloud forwarding"))}</h3>
      <form id="forward-form" data-cmd="forward" hx-put="${API}/forward-mode" hx-encoding="json" hx-swap="none">
        <div class="choices">${["off", "telemetry_only", "full"].map(choice).join("")}</div>
        <button data-cmd="forward" data-confirm="forward" type="submit" class="cmd">${esc(tr("Apply"))}</button>
      </form>
      <p class="muted small">${esc(tr("Installer role. Applies at once: the inverter's connection is restarted under the new mode (a few seconds without live data)."))}</p>
    </article>
    <article class="panel">
      <div class="card-head"><h3>${esc(tr("Maintenance window"))}</h3><span class="chip ${m.active ? "warn" : "muted"}">${esc(m.active ? tr("open") : tr("closed"))}</span></div>
      ${maint}
    </article>
    ${caps === null ? "" : capturesPanel(caps)}
  </section>`;
}

// fetchCaptures lists the window captures (installer; null otherwise).
async function fetchCaptures() {
  const resp = await apiFetch("/captures");
  if (!resp.ok) return null;
  return (await resp.json()).items ?? [];
}

const fmtSize = (n) => n < 1024 ? n + " B" : n < 1048576 ? (n / 1024).toFixed(1) + " KiB" : (n / 1048576).toFixed(1) + " MiB";

function capturesPanel(caps) {
  const rows = caps.map((c) => `<li class="item">
      <div class="item-main">
        <div class="item-title">${esc(fmtTime(c.started))}${c.stopped ? " – " + esc(fmtTime(c.stopped)) : ""}
          ${c.active ? `<span class="chip warn">${esc(tr("recording"))}</span>` : ""}${c.capped ? `<span class="chip warn">${esc(tr("size cap reached"))}</span>` : ""}</div>
        <div class="muted small num">${fmtSize(c.bytes)} · ${esc(c.connections === 1 ? tr("1 connection") : tr("{n} connections", { n: c.connections }))} · ${esc(c.version)}</div>
      </div>
      <button type="button" class="ghost small-btn" data-action="capture-download" data-id="${esc(c.id)}">${esc(tr("Download"))}</button>
      ${c.active ? "" : `<button type="button" class="ghost small-btn danger" data-action="capture-delete" data-id="${esc(c.id)}">${esc(tr("Delete"))}</button>`}
    </li>`).join("");
  return `<article class="panel">
      <h3>${esc(tr("Captures"))}</h3>
      <p class="small">${esc(tr("Maintenance windows opened with \"Record the traffic\". Each download is the raw traffic, both directions, in prox's capture layout. It is not de-identified: it contains your inverter's serial numbers, so keep it private."))}</p>
      ${caps.length ? `<ul class="list">${rows}</ul>` : `<p class="muted small">${esc(tr("None yet."))}</p>`}
    </article>`;
}

async function captureDownload(id) {
  const resp = await apiFetch("/captures/" + encodeURIComponent(id));
  if (!resp.ok) {
    const p = await problemOf(resp);
    toast(`${p.code ?? "error"}: ${p.detail ?? p.title}`, "error");
    return;
  }
  toast(tr("capture saved: {name}", { name: await saveBlob(resp, "foxess-capture.tar.gz") }));
}

async function captureDelete(id) {
  if (!(await confirmDlg(tr("delete this capture?"), tr("The recorded traffic is removed from this machine. Download it first if you want to keep it."), tr("delete")))) return;
  const resp = await apiFetch("/captures/" + encodeURIComponent(id), { method: "DELETE" });
  if (!resp.ok) {
    const p = await problemOf(resp);
    toast(`${p.code ?? "error"}: ${p.detail ?? p.title}`, "error");
    return;
  }
  toast(tr("capture deleted"));
  render();
}

// live countdown for an open window
function afterCloud() {
  const el = byId("maint-remaining");
  if (!el) return;
  const end = new Date(el.dataset.expires).getTime();
  S.maintTimer = setInterval(() => {
    const s = Math.max(0, Math.round((end - Date.now()) / 1000));
    el.textContent = Math.floor(s / 60) + ":" + String(s % 60).padStart(2, "0");
    if (s === 0) { clearInterval(S.maintTimer); render(); }
  }, 1000);
}

// ---------- frame messages ----------

async function renderMessages() {
  const first = S.messages.items.length === 0;
  if (first && !(await fetchMessagesPage(null, true))) return null;
  if (S.messages.denied) {
    return `${pageHead(tr("Frame messages"))}
  <p class="muted">${esc(tr("The raw message log needs the expert or installer role. Sign in with such a token to see it."))}</p>`;
  }
  const f = S.msgFilters;
  let lastDay = "";
  const items = S.messages.items.map((m) => {
    const day = dayLabel(m.ts);
    const head = day !== lastDay ? `<li class="day">${esc(day)}</li>` : "";
    lastDay = day;
    const up = m.dir === "inverter-to-cloud";
    return `${head}<li class="item">
      <span class="dir ${up ? "up" : "down"}" title="${esc(up ? tr("inverter → cloud") : tr("cloud → inverter"))}">${up ? "↑" : "↓"}</span>
      <div class="item-main">
        <div class="item-title"><span class="mono">${esc(m.frame)}</span> <span class="muted small num">${fmtBytes(m.length)}</span> ${msgStatus(m)}</div>
        <div class="item-meta muted small"><time datetime="${esc(m.ts)}">${fmtTime(m.ts)}</time>${msgFirmware(m) ? "" : m.summary ? " · " + esc(m.summary) : ""}</div>
        ${msgFirmware(m)}
        ${m.hex ? `<details class="hex"><summary>${esc(tr("raw bytes"))}</summary><pre>${esc(m.hex)}</pre></details>` : ""}
      </div>
    </li>`;
  }).join("");
  return `
  ${pageHead(tr("Frame messages"), esc(tr("The raw message log between the inverter and the cloud channel (diagnostics).")))}
  <form class="filterbar" id="messages-form">
    <div class="fields two">
      <label>${esc(tr("Direction"))}<select name="dir">
        <option value="">${esc(tr("All"))}</option>
        <option value="inverter-to-cloud" ${f.dir === "inverter-to-cloud" ? "selected" : ""}>${esc(tr("Inverter → cloud"))}</option>
        <option value="cloud-to-inverter" ${f.dir === "cloud-to-inverter" ? "selected" : ""}>${esc(tr("Cloud → inverter"))}</option>
      </select></label>
      <label>${esc(tr("Frame type"))}<select name="frame">
        <option value="">${esc(tr("All"))}</option>
        ${FRAME_TYPES.map((fr) => `<option ${f.frame === fr ? "selected" : ""}>${fr}</option>`).join("")}
      </select></label>
    </div>
    <button type="submit" class="ghost">${esc(tr("Filter"))}</button>
  </form>
  <ul class="list">${items || `<li class="empty muted">${esc(f.dir || f.frame ? tr("No frames for this filter.") : tr("No frames."))}</li>`}</ul>
  ${S.messages.exhausted ? "" : `<button type="button" class="ghost wide" data-action="messages-more">${esc(tr("Load older"))}</button>`}`;
}

// ---------- settings ----------

async function renderSettings() {
  let cfg = S.cfg;
  if (!cfg) {
    const r = await apiFetch("/config");
    if (r.status === 401) { gate(tr("the token was rejected — sign in again")); return null; }
    if (r.ok) cfg = await r.json();
    S.cfg = cfg;
  }
  let tokens = null;
  let denied = null;
  {
    const r = await apiFetch("/config/tokens");
    if (r.status === 401 && S.token) { gate(tr("the token was rejected — sign in again")); return null; }
    if (r.ok) tokens = await r.json();
    else denied = await problemOf(r);
  }
  const tokenList = tokens ? `
    <ul class="list compact">${(tokens.items ?? []).map((tk) => `
      <li class="item">
        <div class="item-main">
          <div class="item-title">${esc(tk.name)} <span class="chip">${esc(tk.role)}</span></div>
          <div class="item-meta muted small">${esc(tr("created {time}", { time: fmtTime(tk.created) }))} · ${esc(tr("last used {when}", { when: tk.last_used ? fmtAgo(tk.last_used) : tr("never") }))}</div>
        </div>
        <button type="button" class="ghost small-btn danger-text" data-cmd="token-revoke" data-name="${esc(tk.name)}" data-role="${esc(tk.role)}" data-confirm="tokenRevoke"
          hx-delete="${API}/config/tokens/${encodeURIComponent(tk.id)}" hx-swap="none">${esc(tr("Revoke"))}</button>
      </li>`).join("") || `<li class="empty muted">${esc(tr("No tokens."))}</li>`}</ul>
    <details class="sub">
      <summary>${esc(tr("Create a token"))}</summary>
      <form id="token-form" data-cmd="tokens" hx-post="${API}/config/tokens" hx-encoding="json" hx-swap="none">
        <div class="fields two">
          <label>${esc(tr("Name"))}<input type="text" name="name" required maxlength="64" pattern="[\\w.-]+" placeholder="${esc(tr("e.g. phone, home-assistant"))}"></label>
          <label>${esc(tr("Role"))}<select name="role">${ROLES.map((r) => `<option ${r === "standard" ? "selected" : ""}>${r}</option>`).join("")}</select></label>
        </div>
        <button type="submit" class="cmd" data-cmd="tokens">${esc(tr("Create"))}</button>
        <p class="muted small">${esc(tr("Read-only < standard (work mode, schedule) < expert (meter compensation, tuning) < installer (limits, cloud, tokens). The token is shown once."))}</p>
      </form>
    </details>`
    : `<p class="muted">${esc(denied?.code === "insufficient_role"
      ? tr("Token management needs the {need} role (yours: {have}).", { need: denied.required_role, have: denied.token_role })
      : denied?.status === 401 ? tr("Sign in with an installer token to manage tokens.")
      : tr("Cannot load the token list."))}</p>`;
  let upd = null;
  try {
    const r = await apiFetch("/update");
    if (r.ok) upd = await r.json();
  } catch { /* the panel shows without the update state */ }
  if (upd) refreshUpdateBadge(upd);
  let sets = null;
  try {
    const r = await apiFetch("/settings");
    if (r.ok) sets = await r.json();
  } catch { /* the panel says it cannot load */ }
  return `
  ${pageHead(tr("Settings"))}
  <section class="stack">
    ${languagePanel()}
    ${updatesPanel(upd)}
    ${notifyPanel(sets)}
    ${tuningPanel(sets)}
    <article class="panel">
      <h3>${esc(tr("Access tokens"))}</h3>
      ${tokenList}
    </article>
    <article class="panel">
      <h3>${esc(tr("Diagnostics"))}</h3>
      <p class="small">${esc(tr("Download a diagnostic bundle to help support your inverter model: recent messages and register readings, with serial numbers and dates removed. You can inspect it before sharing; nothing is uploaded."))}</p>
      <div class="fields two inline">
        <label>${esc(tr("Period"))}<select id="bundle-hours">
          <option value="6">${esc(tr("last 6 hours"))}</option><option value="24" selected>${esc(tr("last 24 hours"))}</option><option value="72">${esc(tr("last 3 days"))}</option><option value="168">${esc(tr("last 7 days"))}</option>
        </select></label>
        <button type="button" class="cmd" data-action="bundle-download">${esc(tr("Download bundle"))}</button>
      </div>
      <p class="muted small">${esc(tr("Installer role. Reporting a problem? Report a problem (under More) adds your description and the app's checks to the same file."))}</p>
    </article>
    <article class="panel">
      <h3>${esc(tr("Server"))}</h3>
      <dl class="kv">
        <div><dt>${esc(tr("Version"))}</dt><dd class="num">${esc(cfg?.version ?? "—")}</dd></div>
        <div><dt>${esc(tr("Detailed history kept"))}</dt><dd>${cfg ? cfg.raw_hours + " h" : "—"}</dd></div>
        <div><dt>${esc(tr("Hourly history kept"))}</dt><dd>${cfg ? esc(tr("{n} days", { n: cfg.bucket_days })) : "—"}</dd></div>
        <div><dt>${esc(tr("Read-only access without a token"))}</dt><dd>${esc(S.cfg?.public_access ? tr("on") : tr("off"))}</dd></div>
        <div><dt>${esc(tr("Site time zone"))}</dt><dd>${cfg ? esc(cfg.site_time_zone ?? tr("not chosen yet (Setup)")) : "—"}</dd></div>
        <div><dt>${esc(tr("Inverter clock"))}</dt><dd>${clockOffsetText(cfg?.inverter_clock_offset_s, cfg?.site_time_zone)}</dd></div>
        <div><dt>${esc(tr("This machine's time zone"))}</dt><dd>${cfg ? esc(cfg.time_zone) + " " + utcOffset(cfg.utc_offset_s) : "—"}</dd></div>
        <div><dt>${esc(tr("API description"))}</dt><dd><a href="${API}/openapi.yaml" target="_blank" rel="noopener">openapi.yaml</a></dd></div>
      </dl>
    </article>
  </section>`;
}

// NOTIFY_KEYS: the notification targets (F2) — a secret: the API shows
// the URL to installers only ("(set)" to anyone else).
const NOTIFY_KEYS = [
  ["notify.webhook_url", tr("Webhook URL"), "http://homeassistant.local:8123/api/webhook/…",
    tr("A JSON POST per notice — e.g. a Home Assistant webhook trigger.")],
  ["notify.ntfy_url", tr("ntfy topic URL"), "https://ntfy.sh/your-hard-to-guess-topic",
    tr("A push to your phone through ntfy; pick a hard-to-guess topic.")],
];

// notifyPanel: where notices go (F2) — an installer edits and tests the
// targets; anyone else sees whether each is set.
function notifyPanel(sets) {
  if (!sets) return "";
  const item = (k) => sets.settings?.find((s) => s.key === k);
  const hidden = NOTIFY_KEYS.some(([k]) => item(k)?.value === "(set)");
  const fields = NOTIFY_KEYS.map(([k, label, ph, help]) => {
    const it = item(k);
    if (hidden) {
      return `<div><dt>${esc(label)}</dt><dd>${esc(it?.value ? tr("set") : tr("not set"))}</dd></div>`;
    }
    return `<label>${esc(label)}<input type="url" id="notify-${k.split(".")[1]}" value="${esc(it?.value ?? "")}"
      placeholder="${esc(ph)}" maxlength="2048" autocomplete="off"><span class="muted small">${esc(help)}${
      it?.source === "config" ? " " + esc(tr("Set in the config file; a value saved here wins.")) : ""}</span></label>`;
  }).join("");
  return `
    <article class="panel">
      <h3>${esc(tr("Notifications"))}</h3>
      <p class="small">${esc(tr("Hear about a problem without this page open: the inverter offline for 10 minutes (and back), the gate losing its rules, a failed command or schedule, a write refused by the hourly limit, low disk, an upgrade rolled back, cloud-forwarding or maintenance-window changes, a lost grid, a restart after a power cut, and a low or falling battery (see Tuning)."))}</p>
      ${hidden ? `<dl class="kv">${fields}</dl><p class="muted small">${esc(tr("Sign in with an installer token to change or test them."))}</p>`
        : `<div class="fields stacked-fields">${fields}</div>
      <div class="row-actions">
        <button type="button" class="cmd" data-action="notify-save">${esc(tr("Save"))}</button>
        <button type="button" class="ghost" data-action="notify-test">${esc(tr("Send a test"))}</button>
      </div>
      <p class="muted small">${esc(tr("Empty turns a target off. The URLs are kept from other users: only installers see them."))}</p>`}
    </article>`;
}

// TUNING_KEYS: the expert-level knobs (owner, 2026-11-18): [key, label,
// unit, min, max, step, help].
const TUNING_KEYS = [
  ["store.write_delay_s", tr("Write delay"), "s", 0, 900, 1,
    tr("How long new readings may wait in memory before they are saved. Longer is gentler on an SD card or eMMC; a power cut loses what waits. Commands and setting changes are always saved at once. 0 saves at once.")],
  ["alerts.soc_threshold_pct", tr("Battery alert level"), "%", 0, 95, 1,
    tr("Notify when the battery is at or below this charge, or is forecast to reach it. 0 turns the battery alerts off.")],
  ["alerts.soc_horizon_min", tr("Forecast horizon"), "min", 10, 1440, 1,
    tr("How far ahead the forecast looks.")],
  ["alerts.soc_window_min", tr("Forecast window"), "min", 5, 240, 1,
    tr("How much recent history the forecast fits its line to.")],
  ["alerts.soc_p_value", tr("Forecast p-value"), "", 0.001, 0.5, 0.001,
    tr("How sure the forecast must be: 0.05 means 95 % confident. Smaller means fewer, later warnings.")],
];

// tuningPanel: the expert knobs — anyone may look; saving needs an
// expert token (the API refuses otherwise, and the toast says so).
function tuningPanel(sets) {
  if (!sets) return "";
  const item = (k) => sets.settings?.find((s) => s.key === k);
  const fields = TUNING_KEYS.map(([k, label, unit, min, max, step, help]) => {
    const it = item(k);
    return `<label>${esc(label)}${unit ? ` <span class="muted small">(${esc(unit)})</span>` : ""}
      <input type="number" id="tune-${k.replace(/\./g, "-")}" value="${esc(it?.value ?? "")}" data-orig="${esc(it?.value ?? "")}" min="${min}" max="${max}" step="${step}" inputmode="decimal">
      <span class="muted small">${esc(help)}${it?.source === "config" ? " " + esc(tr("Set in the config file; a value saved here wins.")) : ""}</span></label>`;
  }).join("");
  return `
    <article class="panel">
      <h3>${esc(tr("Tuning"))} <span class="chip muted">${esc(tr("expert"))}</span></h3>
      <div class="fields stacked-fields">${fields}</div>
      <div class="row-actions">
        <button type="button" class="cmd" data-action="tuning-save">${esc(tr("Save"))}</button>
      </div>
    </article>`;
}

// tuningSave stores the knobs that changed (empty = back to the file's
// or the default). Unchanged ones are not sent: saving them would pin
// today's default as a UI value.
async function tuningSave() {
  const body = {};
  for (const [k] of TUNING_KEYS) {
    const el = byId("tune-" + k.replace(/\./g, "-"));
    const v = el.value.trim();
    if (v === el.dataset.orig) continue;
    body[k] = v === "" ? null : Number(v);
  }
  if (Object.keys(body).length === 0) { toast(tr("nothing changed")); return; }
  if (await setupSave("/settings", "PUT", body, tr("tuning saved"))) render();
}

// notifySave stores both targets ("" = off).
async function notifySave() {
  const body = {};
  for (const [k] of NOTIFY_KEYS) body[k] = byId("notify-" + k.split(".")[1]).value.trim();
  if (await setupSave("/settings", "PUT", body, tr("notifications saved"))) render();
}

// notifyTest sends a test notice to each saved target and reports each.
async function notifyTest() {
  const r = await apiFetch("/notify/test", { method: "POST" });
  if (!r.ok) {
    const p = await problemOf(r);
    toast(tr("no test sent"), "warn", p.detail ?? p.title);
    return;
  }
  const res = (await r.json()).results ?? [];
  const bad = res.filter((x) => x.error);
  if (bad.length === 0) toast(tr("test sent to {targets}", { targets: res.map((x) => x.target).join(", ") }));
  else toast(tr("test failed"), "err", bad.map((x) => `${x.target}: ${x.error}`).join("; "));
}

// utcOffset formats a zone offset in seconds as UTC±hh:mm.
function utcOffset(s) {
  const a = Math.abs(s), h = Math.floor(a / 3600), m = Math.floor((a % 3600) / 60);
  return `UTC${s < 0 ? "−" : "+"}${String(h).padStart(2, "0")}:${String(m).padStart(2, "0")}`;
}

// clockOffsetText: the inverter's clock against this machine's (audit
// R8). Schedule windows run on this machine's clock; a large difference
// means a wrong host zone or an inverter clock to set.
function clockOffsetText(d, zone) {
  if (!zone) return esc(tr("not compared: no site time zone"));
  if (d == null) return esc(tr("not read yet"));
  const a = Math.abs(d);
  if (a < 60) return esc(tr("in step with the site's time"));
  const t = a >= 3600 ? (a / 3600).toFixed(1) + " h" : Math.round(a / 60) + " min";
  return `<span class="warn">${esc(d > 0
    ? tr("{d} ahead of the site's time — see Setup", { d: t })
    : tr("{d} behind the site's time — see Setup", { d: t }))}</span>`;
}

// languagePanel: the UI language (kept on this device; the browser's
// language until one is picked). The translations are machine-made.
function languagePanel() {
  const cur = currentLang();
  return `<article class="panel">
      <h3>${esc(tr("Language"))}</h3>
      <label>${esc(tr("Language of this page"))}<select id="lang-select">
        ${LANGS.map(([code, name]) => `<option value="${code}" ${code === cur ? "selected" : ""}>${esc(name)}</option>`).join("")}
      </select></label>
      <p class="muted small">${esc(tr("Kept on this device. The translations other than English are machine-made: corrections are welcome (one file per language, internal/web/static/i18n/ in the repository)."))}</p>
    </article>`;
}

// updatesPanel: the release check and the no-root self-upgrade (D-023).
function updatesPanel(u) {
  if (!u) return "";
  const checked = esc(u.checked_at ? tr("checked {when}", { when: fmtAgo(u.checked_at) }) : tr("not checked yet"));
  let status;
  if (u.available) {
    status = `<p class="note ok">${esc(tr("{latest} is available (you have {current}).", { latest: u.latest, current: u.current }))}${u.latest_prerelease ? ` <span class="chip warn">${esc(tr("prerelease"))}</span>` : ""} ${u.url ? `<a href="${esc(u.url)}" target="_blank" rel="noopener">${esc(tr("Release notes"))}</a>` : ""}</p>
      ${u.can_apply
        ? `<button type="button" class="cmd" data-cmd="update-apply" data-confirm="updateApply" data-to="${esc(u.latest)}" hx-post="${API}/update/apply" hx-swap="none">${esc(tr("Update now"))}</button>`
        : `<p class="muted small">${esc(updateReason(u))}</p>`}`;
  } else if (u.latest) {
    status = `<p class="small">${esc(tr("Up to date (latest release {v}).", { v: u.latest }))}</p>`;
  } else {
    const why = u.checks_enabled ? tr("No release information yet.")
      : u.reason ? updateReason(u)
      : tr("Scheduled checks are off.");
    status = `<p class="muted small">${esc(why)}</p>`;
  }
  const rolled = u.rolled_back
    ? `<p class="note warn">${esc(tr("The update to {to} did not start correctly and was rolled back to {from}.", { to: u.rolled_back.to, from: u.rolled_back.from }))}
        <button type="button" class="ghost small-btn" data-cmd="update-dismiss" hx-delete="${API}/update/rolled-back" hx-swap="none">${esc(tr("Dismiss"))}</button></p>` : "";
  const pending = u.pending && u.pending.to === u.current
    ? `<p class="muted small">${esc(tr("Running the new version; it is kept once the inverter is back with fresh readings."))}</p>` : "";
  return `<article class="panel">
      <div class="card-head"><h3>${esc(tr("Updates"))}</h3><span class="chip">${esc(u.current)}</span></div>
      ${rolled}${status}${pending}
      <p class="muted small">${checked}${u.next_check_at ? " · " + esc(tr("next check {when}", { when: fmtTime(u.next_check_at) })) : ""}${u.error ? " · " + esc(tr("last check failed: {err}", { err: u.error })) : ""} ·
        <button type="button" class="linkbtn" data-cmd="update-check" hx-post="${API}/update/check" hx-swap="none">${esc(tr("check now"))}</button></p>
      ${updateChoices(u)}
      <p class="muted small">${esc(tr("Updates are signature-checked before they are installed, and the app restarts itself (installer role). If the new version fails to start, the previous one is restored automatically."))}</p>
    </article>`;
}

// updateReason: why an update cannot be applied here, translated by its
// code (the server's English text when the code is unknown).
function updateReason(u) {
  switch (u.reason_code) {
    case "container": return tr("Running in a container: update by pulling the new image.");
    case "source_build": return tr("A source build ({v}): update by rebuilding.", { v: u.current });
    case "no_key": return tr("This build carries no release signing key.");
    case "no_repo": return tr("No release repository is configured.");
    case "unavailable": return tr("Self-upgrade is not available in this host.");
  }
  const r = u.reason ?? "";
  return r ? r.charAt(0).toUpperCase() + r.slice(1) + "." : "";
}

// UPDATE_INTERVALS: the check-interval choices, in hours.
const UPDATE_INTERVALS = [[0, tr("Never")], [6, tr("Every 6 hours")], [24, tr("Daily")], [168, tr("Weekly")], [720, tr("Every 30 days")]];

// updateChoices: the installer's update settings (saving needs an
// installer token; the API refuses otherwise and the toast says so).
function updateChoices(u) {
  const iv = u.check_interval_h ?? 168;
  const opts = UPDATE_INTERVALS.some(([h]) => h === iv) ? UPDATE_INTERVALS : [...UPDATE_INTERVALS, [iv, tr("Every {n} hours", { n: iv })]];
  return `<details class="sub update-choices">
      <summary>${esc(tr("Update choices"))}</summary>
      <label class="field">${esc(tr("Check for updates"))}<select id="upd-interval">
        ${opts.map(([h, l]) => `<option value="${h}" ${h === iv ? "selected" : ""}>${esc(l)}</option>`).join("")}
      </select></label>
      <label class="radio-row"><input type="checkbox" id="upd-pre" ${u.prerelease ? "checked" : ""}>
        <span>${esc(tr("Offer prereleases"))}<span class="desc muted small">${esc(tr("Test versions before a release. They may have bugs."))}</span></span></label>
      <label class="radio-row"><input type="checkbox" id="upd-auto" ${u.auto_apply ? "checked" : ""}>
        <span>${esc(tr("Install updates automatically"))}<span class="desc muted small">${esc(tr("Each new update is tried once, when a check finds it. If it fails to start, the previous version comes back and that update is not tried again."))}</span></span></label>
      <button type="button" class="cmd" data-action="update-choices">${esc(tr("Save"))}</button>
    </details>`;
}

// updateChoicesSave stores the three update settings.
async function updateChoicesSave() {
  const body = {
    "update.check_interval_h": Number(byId("upd-interval").value),
    "update.prerelease": byId("upd-pre").checked,
    "update.auto_apply": byId("upd-auto").checked,
  };
  if (await setupSave("/settings", "PUT", body, tr("update choices saved"))) render();
}

// bundleDownload fetches the archive with the token header and saves it.
async function bundleDownload() {
  const hours = byId("bundle-hours")?.value ?? "24";
  const resp = await apiFetch("/bundle?hours=" + encodeURIComponent(hours));
  if (!resp.ok) {
    const p = await problemOf(resp);
    toast(`${p.code ?? "error"}: ${CODE_TEXT[p.code] ? CODE_TEXT[p.code](p) : (p.detail ?? p.title)}`, "error");
    return;
  }
  const name = await saveBlob(resp, "foxess-bundle.tar.gz");
  toast(tr("diagnostic bundle saved: {name}", { name }));
}

// saveBlob saves a file response under its Content-Disposition name.
async function saveBlob(resp, fallback) {
  const name = /filename="([^"]+)"/.exec(resp.headers.get("Content-Disposition") ?? "")?.[1] ?? fallback;
  const url = URL.createObjectURL(await resp.blob());
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10000);
  return name;
}

// ---------- problem report (F8) ----------

async function renderReport() {
  if (!(await loadState())) return null;
  const id = S.state.identity ?? {};
  return `
  ${pageHead(tr("Report a problem"), esc(tr("Builds one file with your description and what this app knows about the problem, for the developers.")))}
  <article class="panel report">
    <form id="report-form" data-cmd="report">
      <label>${esc(tr("What went wrong?"))}<textarea id="report-desc" name="description" rows="6" maxlength="4000"
        placeholder="${esc(tr("What you saw, when it started, and what you expected instead."))}"></textarea></label>
      <label>${esc(tr("History to include"))}<select id="report-hours">
        ${[["6", tr("the last 6 hours")], ["24", tr("the last 24 hours")], ["72", tr("the last 3 days")], ["168", tr("the last week")]].map(([v, t]) =>
          `<option value="${v}" ${v === "24" ? "selected" : ""}>${esc(t)}</option>`).join("")}
      </select></label>
      <button type="button" class="cmd" data-action="report-download">${esc(tr("Download report"))}</button>
    </form>
    <p class="muted small">${esc(tr("The file contains your description, the app's version, settings and checks, recent errors, and the raw messages between the inverter and the cloud. Serial numbers are replaced, dates are shifted, and token names are left out — you can open it (a .tar.gz) and read everything before sharing. Nothing is sent anywhere by this page. Installer role."))}</p>
    <div id="report-next" hidden></div>
  </article>`;
}

// QUIRK_TEXT: the quirks' translated texts by id (the server's English
// is the fallback for a quirk this page does not know yet).
const QUIRK_TEXT = () => ({
  eps_dropouts: {
    title: tr("Backup-port reading dropouts"),
    symptom: tr("The backup (EPS) port reading drops by about a quarter for a reading or two, while the inverter's AC output, which includes it, stays steady. The household split on the dashboard then jumps around."),
    effect: tr("The dashboard shows the backup port as the median of its last 5 readings (about 25 s), so such single low readings are ignored. A real change shows within about 15 s."),
    seen: tr("KH10, firmware 1.73: one installation (2026-11-19)"),
  },
});

// renderQuirks: the known inverter quirks, each on or off (expert).
async function renderQuirks() {
  const qs = await loadQuirks();
  const body = qs == null
    ? `<p class="muted">${esc(tr("Cannot load the quirks."))}</p>`
    : qs.map((q) => {
      const t = QUIRK_TEXT()[q.id] ?? { title: q.title, effect: q.effect };
      const def = q.default ? tr("on") : tr("off");
      return `<article class="panel quirk">
        <div class="card-head"><h3>${esc(t.title)}</h3>
          <span class="chip ${q.enabled ? "ok" : ""}">${esc(q.enabled ? tr("on") : tr("off"))}</span></div>
        ${t.symptom ? `<p>${esc(t.symptom)}</p>` : ""}
        <p><strong>${esc(tr("When on:"))}</strong> ${esc(t.effect)}</p>
        <p class="muted small">${esc(tr("Seen on: {where}", { where: t.seen ?? q.seen }))} · ${esc(tr("default: {v}", { v: def }))} · ${esc(SOURCE_TEXT[q.source] ?? q.source)}</p>
        <label class="radio-row"><input type="checkbox" data-quirk="${esc(q.id)}" ${q.enabled ? "checked" : ""}>
          <span>${esc(tr("Compensate for this quirk"))}</span></label>
        <div class="btn-row">
          <button type="button" class="cmd" data-action="quirk-save" data-id="${esc(q.id)}">${esc(tr("Save"))}</button>
          ${q.source === "ui" ? `<button type="button" class="ghost" data-action="quirk-reset" data-id="${esc(q.id)}">${esc(tr("Back to the default"))}</button>` : ""}
        </div>
      </article>`;
    }).join("");
  return `
  ${pageHead(tr("Inverter quirks"), esc(tr("Behaviours seen on some inverters that this app can compensate for. A quirk changes only what the web pages show: the history, the API and exports keep the inverter's own values.")))}
  <section class="stack quirks">${body}</section>
  <p class="muted small">${esc(tr("New quirks start off until reports show they are common to a model or firmware. If you see one of these (or something like it) on your inverter, please say so: More → Report a problem. Changing a quirk needs the expert role."))}</p>`;
}

// quirkSave sets (on/off) or resets one quirk, then reloads the page.
async function quirkSave(id, reset) {
  const box = document.querySelector(`input[data-quirk="${CSS.escape(id)}"]`);
  const body = { ["quirks." + id]: reset ? null : !!box?.checked };
  if (await setupSave("/settings", "PUT", body, tr("quirk saved"))) render();
}

// reportIssueURL is a prefilled GitHub issue for a downloaded report.
function reportIssueURL(file, desc) {
  const base = S.cfg?.issue_url;
  if (!base) return "";
  const id = S.state?.identity ?? {};
  const fw = id.firmware ?? {};
  const title = `Problem: ${desc.split("\n")[0].slice(0, 80) || "(no description)"}`;
  const body = [
    "**What went wrong**", "", desc || "(no description)", "",
    "**Setup**", "",
    `- foxess-local: ${S.cfg?.version ?? "?"}`,
    `- Inverter: ${id.model || "not identified"}, firmware ${fw.master ?? "?"} / ${fw.slave ?? "?"} / ${fw.manager ?? "?"}` +
      (id.verified_profile ? " (verified model)" : " (not verified)"),
    `- Cloud forwarding: ${S.state?.forward_mode ?? "?"}`, "",
    `**Report file:** drag \`${file}\` into this issue to attach it.`,
  ].join("\n");
  return `${base}?title=${encodeURIComponent(title)}&body=${encodeURIComponent(body)}`;
}

async function reportDownload() {
  const desc = byId("report-desc")?.value.trim() ?? "";
  const hours = Number(byId("report-hours")?.value ?? 24);
  const resp = await apiFetch("/report", {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ description: desc, hours }),
  });
  if (!resp.ok) {
    const p = await problemOf(resp);
    toast(`${p.code ?? "error"}: ${CODE_TEXT[p.code] ? CODE_TEXT[p.code](p) : (p.detail ?? p.title)}`, "error");
    return;
  }
  const name = await saveBlob(resp, "foxess-report.tar.gz");
  toast(tr("report saved: {name}", { name }));
  const next = byId("report-next");
  if (!next) return;
  const url = reportIssueURL(name, desc);
  next.innerHTML = url
    ? `<p>${esc(tr("Next: open an issue and attach {file} to it.", { file: name }))}</p>
       <a class="cmd" href="${esc(url)}" target="_blank" rel="noopener">${esc(tr("Open a GitHub issue"))}</a>`
    : `<p>${esc(tr("Next: send {file} to the developers the way you were told to.", { file: name }))}</p>`;
  next.hidden = false;
}

// ---------- unverified-model banner ----------

function paintDeviceBanner() {
  const b = byId("banner-device");
  const id = S.state?.identity;
  if (!id?.model || id.verified_profile) {
    b.hidden = true;
    return;
  }
  b.hidden = false;
  b.innerHTML = esc(tr("Unverified inverter model: {model} fw {fw}. Monitoring works; some settings may be unavailable.", { model: id.model, fw: id.firmware?.master ?? "?" })) +
    (dashOnly() ? "" : ` <a href="#/settings">${esc(tr("Share a diagnostic bundle to help add support."))}</a>`);
}

// ---------- command body builders ----------
// The flat form→JSON commands are finalized in the htmx:before:request hook
// (BODY_SPECS); only the schedule body is nested enough to need a
// builder.

// scheduleBody reads the schedule form's named fields into the
// nested Schedule PUT body (disabled groups zero-filled).
// schedAdd brings up the next unused group as a new, enabled window.
function schedAdd() {
  const f = byId("schedule-form");
  const g = f?.querySelector(".group[data-unused][hidden]");
  if (!g) return;
  g.hidden = false;
  byId("sched-none")?.remove();
  const sw = g.querySelector('input[name$="-enabled"]');
  sw.checked = true;
  sw.dispatchEvent(new Event("change", { bubbles: true }));
  if (!f.querySelector(".group[data-unused][hidden]")) f.querySelector('[data-action="sched-add"]').disabled = true;
}

function scheduleBody(f) {
  if (!f) return { enabled: false, groups: [] };
  const groups = [];
  for (let i = 1; i <= 8; i++) {
    const q = (n) => f.querySelector(`[name="g${i}-${n}"]`);
    const enabled = q("enabled")?.checked ?? false;
    const iv = (el) => {
      const n = Number(el?.value);
      return Number.isFinite(n) ? Math.trunc(n) : 0;
    };
    groups.push({
      index: i,
      enabled,
      start: enabled ? q("start").value : "00:00",
      end: enabled ? q("end").value : "00:00",
      work_mode: enabled ? q("mode").value : "self-use",
      max_soc_pct: enabled ? iv(q("maxsoc")) : 0,
      min_soc_on_grid_pct: enabled ? iv(q("minsoc")) : 0,
      fd_soc_pct: enabled ? iv(q("fdsoc")) : 0,
      fd_pwr_w: enabled ? iv(q("fdpwr")) : 0,
    });
  }
  return { enabled: f.querySelector('[name="enabled"]')?.checked ?? false, groups };
}

// ---------- delegated interactions ----------

document.addEventListener("click", async (e) => {
  if (e.target.closest("#gate-signin")) {
    e.preventDefault();
    openLogin();
    return;
  }
  const t = e.target.closest("[data-action]");
  if (!t) return;
  switch (t.dataset.action) {
    case "confirm-cancel":
      byId("dlg-confirm").close();
      break;
    case "sign-out":
      signOut();
      break;
    case "sign-in":
      openLogin();
      break;
    case "skip":
      e.preventDefault();
      byId("view").focus();
      break;
    case "export-csv":
      if (histData) exportCSV(histData);
      else toast(tr("history not loaded yet"), "warn");
      break;
    case "export-json":
      if (histData) exportJSON(histData);
      else toast(tr("history not loaded yet"), "warn");
      break;
    case "toggle-hist-table": {
      const tbl = byId("hist-table");
      if (!tbl) break;
      tbl.hidden = !tbl.hidden;
      t.setAttribute("aria-expanded", String(!tbl.hidden));
      t.textContent = tbl.hidden ? tr("show data table") : tr("hide data table");
      if (!tbl.hidden && histData) renderHistTable(histData);
      break;
    }
    case "login-cancel":
      byId("dlg-login").close();
      break;
    case "token-copy":
      try {
        await navigator.clipboard.writeText(lastToken);
        toast(tr("token copied to clipboard"));
      } catch {
        toast(tr("clipboard blocked — select the token text and copy manually"), "warn");
      }
      break;
    case "audit-more":
      if (!S.audit.exhausted && S.audit.before) {
        if (await fetchAuditPage(S.audit.before, false)) render();
      }
      break;
    case "sched-add":
      schedAdd();
      break;
    case "sched-reset":
      render();
      break;
    case "bundle-download":
      bundleDownload();
      break;
    case "capture-download":
      captureDownload(t.dataset.id);
      break;
    case "capture-delete":
      captureDelete(t.dataset.id);
      break;
    case "notify-save":
      notifySave();
      break;
    case "tuning-save":
      tuningSave();
      break;
    case "quirk-save":
      quirkSave(t.dataset.id, false);
      break;
    case "quirk-reset":
      quirkSave(t.dataset.id, true);
      break;
    case "update-choices":
      updateChoicesSave();
      break;
    case "notify-test":
      notifyTest();
      break;
    case "report-download":
      reportDownload();
      break;
    case "scan-start":
      scanStart();
      break;
    case "setup-refresh":
    case "setup-zone":
    case "setup-set-clock":
    case "setup-capacity":
    case "setup-forward":
    case "setup-access":
    case "setup-finish":
      setupAction(t.dataset.action);
      break;
    case "messages-more":
      if (!S.messages.exhausted && S.messages.before) {
        if (await fetchMessagesPage(S.messages.before, false)) render();
      }
      break;
  }
});

// Now window switch (instant / trailing average)
document.body.addEventListener("click", (e) => {
  const b = e.target.closest("[data-window]");
  if (!b || !authed()) return;
  S.nowWindow = b.dataset.window;
  savePref("fl.nowWindow", S.nowWindow);
  S.avg = null;
  for (const x of byId("view").querySelectorAll("[data-window]")) {
    x.setAttribute("aria-pressed", String(x === b));
  }
  paintNow(S.state);
  startAvgTimer();
});

// history range switch (delegated; CSP forbids inline handlers)
document.body.addEventListener("click", (e) => {
  const b = e.target.closest("[data-range]");
  if (!b || !authed()) return;
  S.histRange = b.dataset.range;
  savePref("fl.histRange", S.histRange);
  for (const x of byId("view").querySelectorAll("[data-range]")) {
    x.setAttribute("aria-pressed", String(x === b));
  }
  const lbl = byId("range-label");
  if (lbl) lbl.textContent = RANGES.find(([r]) => r === S.histRange)?.[2] ?? "";
  renderHistory(S.histRange);
});

document.addEventListener("submit", (e) => {
  if (e.target.id === "login-form") {
    e.preventDefault();
    loginSubmit();
  } else if (e.target.id === "audit-form") {
    e.preventDefault();
    const action = e.submitter?.name === "action" ? e.submitter.value : S.auditFilters.action;
    S.auditFilters = { source: e.target.source.value.trim(), action };
    S.audit = { items: [], before: null, exhausted: false };
    render();
  } else if (e.target.id === "messages-form") {
    e.preventDefault();
    S.msgFilters = { dir: e.target.dir.value, frame: e.target.frame.value };
    S.messages = { items: [], before: null, exhausted: false };
    render();
  }
});

// schedule interactions: the master switch and a group's switch
// enable/disable its fields (disabled fields are skipped by native
// validation); the mode decides whether the force-charge fields apply;
// any edit marks the form unsaved.
document.addEventListener("change", (e) => {
  const f = byId("schedule-form");
  if (!f || !f.contains(e.target)) return;
  const dirty = byId("sched-dirty");
  if (dirty) { dirty.textContent = tr("Unsaved changes"); dirty.classList.add("warn"); }
  if (e.target.id === "sched-enabled") {
    f.querySelector(".groups")?.classList.toggle("master-off", !e.target.checked);
    return;
  }
  const g = e.target.closest(".group");
  if (!g) return;
  if (e.target.name?.endsWith("-enabled")) {
    g.classList.toggle("on", e.target.checked);
    g.querySelectorAll("details input, details select").forEach((el) => { el.disabled = !e.target.checked; });
    if (e.target.checked) g.querySelector("details").open = true;
  } else if (e.target.name?.endsWith("-mode")) {
    const force = FORCE_MODES.includes(e.target.value);
    g.querySelector(".force").hidden = !force;
    g.className = g.className.replace(/k-\w+/, "k-" + schedKind(e.target.value));
  }
});

// the UI language: kept on this device; the page reloads into it
document.addEventListener("change", (e) => {
  if (e.target.id === "lang-select") setLang(e.target.value);
});

window.addEventListener("hashchange", () => {
  if (authed()) render();
});

// ---------- init ----------

// RETRY_S: the waits, in seconds, between attempts to reach a service
// that is not answering (a restart takes from seconds to a minute); the
// last repeats.
const RETRY_S = [1, 2, 3, 5];

async function init() {
  if (!S.token) {
    // No stored token: probe public read-only access (config flag,
    // default on — spec §5.5) before falling back to the gate.
    if (!(await probePublic())) gate(tr("sign in to use foxesslocal"));
    return;
  }
  // Only a rejection (401) drops the stored token. Anything else — the
  // service restarting after an update, a reverse proxy's 502 while it
  // does — keeps it and tries again until the service answers, or until
  // a sign-in from the gate replaces the token.
  const token = S.token;
  for (let n = 0; S.token === token; n++) {
    let resp = null;
    try {
      resp = await fetch(API + "/state", { headers: { Authorization: "Bearer " + token } });
    } catch { /* not answering: retried */ }
    if (S.token !== token) return;
    if (resp?.status === 401) {
      setToken("");
      gate(tr("the stored token was rejected — sign in again"));
      return;
    }
    if (resp?.ok) {
      S.state = await resp.json();
      S.forwardMode = { mode: S.state.forward_mode };
      enterApp();
      return;
    }
    const wait = RETRY_S[Math.min(n, RETRY_S.length - 1)];
    waitScreen(tr("foxess-local is not answering yet — it may be restarting. Trying again in {s} s…", { s: wait }));
    await new Promise((r) => setTimeout(r, wait * 1000));
  }
}

// waitScreen: the gate with a status line, the stored token kept (no
// sign-in dialog; the gate's button still offers one).
function waitScreen(msg) {
  byId("app").hidden = true;
  byId("app").setAttribute("inert", "");
  byId("gate-msg").textContent = msg;
  byId("gate").hidden = false;
}

// ---------- a new version: reload into its UI ----------
// The page runs the code and templates of the release that served it.
// When the service answers as another version (an update applied from
// this page or automatically, seen when the live stream reconnects
// after the restart), the page reloads to match — after any unsaved
// edit is saved or discarded.
function checkVersion(v) {
  if (!v) return;
  if (!S.pageVersion) {
    S.pageVersion = v;
    return;
  }
  if (v === S.pageVersion || S.reloadPending) return;
  S.reloadPending = true;
  if (!editing()) {
    location.reload();
    return;
  }
  toast(tr("foxess-local is now {v}: the page reloads once your changes are saved or discarded", { v }), "warn");
  const t = setInterval(() => {
    if (editing()) return;
    clearInterval(t);
    location.reload();
  }, 2000);
}

init();
