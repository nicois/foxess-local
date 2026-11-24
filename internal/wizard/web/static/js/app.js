// The wizard's page: what the stick is doing, what to do next, and the
// home Wi-Fi setup. It polls /api/status every 5 s (which also tells the
// wizard an operator is here, so the hotspot stays open).
import { LANGS, setLang, currentLang, tr } from "./i18n.js";

const byId = (id) => document.getElementById(id);
const POLL_MS = 5000;
let last = null;
let pendingJoin = null;

const sel = byId("lang");
for (const [code, name] of LANGS) sel.add(new Option(name, code, false, code === currentLang()));
sel.addEventListener("change", () => setLang(sel.value));

async function post(path, body) {
  return fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Wizard": "1" },
    body: JSON.stringify(body ?? {}),
  });
}

function title(s) {
  switch (s.state) {
    case "unconfigured": return tr("Connect this stick to your Wi-Fi");
    case "joining": return tr("Joining your Wi-Fi network…");
    case "normal": return tr("All is well");
    case "degraded": return tr("Something needs attention");
    case "home_lost": return tr("Looking for your Wi-Fi network");
    case "hotspot": return tr("Choose your Wi-Fi network");
    case "awaiting_inverter": return tr("Waiting for the inverter");
    default: return s.state;
  }
}

function body(s) {
  switch (s.state) {
    case "unconfigured": return tr("Choose your home network and enter its password. Only 2.4 GHz networks can appear.");
    case "joining": return tr("This hotspot is off while the stick tries your network. Watch the light: solid green means it worked; blinking red means rejoin this hotspot to see why.");
    case "normal": return tr("The stick is on your Wi-Fi and foxess-local is receiving data from the inverter.");
    case "degraded": return "";
    case "home_lost": return tr("The stick keeps trying. If your Wi-Fi name or password changed, unplug and replug the stick three times within five minutes, waiting for the light each time, then join its hotspot.");
    case "hotspot": return tr("The stick has opened its own hotspot so you can set it up. Choose your home network below.");
    case "awaiting_inverter": return tr("The stick is on your Wi-Fi, but the inverter is not sending to it yet. In the inverter's data logger settings, set a fixed IP address with this stick ({address}) as its gateway.", { address: s.address ?? "?" });
    default: return "";
  }
}

function problem(p) {
  switch (p) {
    case "app_down": return tr("foxess-local has stopped. The stick restarts it by itself; if this stays, check its log.");
    case "no_inverter": return tr("No readings from the inverter for 10 minutes. Is the inverter on, and is its data logger on your Wi-Fi?");
    default: return p;
  }
}

function joinText(j) {
  if (j.ok) return tr("Joined {ssid}.", { ssid: j.ssid });
  switch (j.reason) {
    case "wrong_password": return tr("{ssid} refused the password.", { ssid: j.ssid });
    case "not_found": return tr("{ssid} is not in range.", { ssid: j.ssid });
    default: return tr("Could not join {ssid}.", { ssid: j.ssid });
  }
}

function render(s) {
  last = s;
  byId("login").hidden = true;
  byId("status").hidden = false;
  byId("status-title").textContent = title(s);
  byId("status-body").textContent = body(s);
  const probs = byId("problems");
  probs.replaceChildren(...s.problems.map((p) => Object.assign(document.createElement("li"), { textContent: problem(p) })));
  const jr = byId("join-result");
  jr.hidden = !s.join;
  if (s.join) {
    jr.textContent = joinText(s.join);
    jr.className = s.join.ok ? "ok" : "error";
  }
  const link = byId("foxess-link");
  link.hidden = !s.foxess_url;
  if (s.foxess_url) link.href = s.foxess_url;
  const wantNetworks = s.on_hotspot || s.state === "unconfigured" || s.state === "hotspot";
  byId("networks").hidden = !wantNetworks;
  byId("hotspot").hidden = s.on_hotspot || !["normal", "degraded", "awaiting_inverter"].includes(s.state);
  renderNetworks(s.networks);
}

function renderNetworks(nets) {
  const list = byId("network-list");
  byId("no-networks").hidden = nets.length > 0;
  list.replaceChildren(...nets.map((n) => {
    const b = document.createElement("button");
    b.type = "button";
    const name = document.createElement("span");
    name.textContent = (n.secured ? "🔒 " : "") + n.ssid;
    const sig = document.createElement("span");
    sig.textContent = tr("signal {n}%", { n: n.signal });
    b.append(name, sig);
    b.addEventListener("click", () => {
      byId("join-ssid").value = n.ssid;
      byId("join-password").focus();
    });
    const li = document.createElement("li");
    li.append(b);
    return li;
  }));
}

function confirm(text, go) {
  byId("confirm-text").textContent = text;
  byId("confirm").hidden = false;
  byId("confirm").scrollIntoView({ behavior: "smooth" });
  byId("confirm-go").onclick = () => { byId("confirm").hidden = true; go(); };
  byId("confirm-cancel").onclick = () => { byId("confirm").hidden = true; };
}

byId("join-form").addEventListener("submit", (e) => {
  e.preventDefault();
  const ssid = byId("join-ssid").value.trim();
  const password = byId("join-password").value;
  if (password && (password.length < 8 || password.length > 63)) {
    byId("join-result").hidden = false;
    byId("join-result").className = "error";
    byId("join-result").textContent = tr("A Wi-Fi password has 8 to 63 characters.");
    return;
  }
  pendingJoin = { ssid, password };
  const url = `http://${last?.host ?? "foxess.local"}:18081`;
  confirm(tr("The stick will now leave this hotspot and join {ssid}. If its light turns solid green, join {ssid} again on this device and open {url}. If the light blinks red, rejoin this hotspot: this page will say what went wrong.", { ssid, url }), async () => {
    const r = await post("api/join", pendingJoin);
    if (r.status === 400) {
      byId("join-result").hidden = false;
      byId("join-result").className = "error";
      byId("join-result").textContent = tr("That network name or password can't be used.");
    }
  });
});

byId("rescan").addEventListener("click", () => post("api/rescan"));

byId("open-hotspot").addEventListener("click", () => {
  confirm(tr("The stick leaves your Wi-Fi for at least 5 minutes and opens its own hotspot. The inverter loses its connection meanwhile."), () => post("api/hotspot"));
});

byId("login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const r = await post("api/login", { password: byId("login-password").value });
  const err = byId("login-error");
  if (r.status === 204) { err.hidden = true; poll(); return; }
  err.hidden = false;
  if (r.status === 429) {
    const v = await r.json().catch(() => ({}));
    err.textContent = tr("Too many attempts. Try again in {minutes} min.", { minutes: Math.ceil((v.retry_after_s ?? 60) / 60) });
  } else {
    err.textContent = tr("Wrong password.");
  }
});

async function poll() {
  try {
    const r = await fetch("api/status", { cache: "no-store" });
    if (r.status === 401) {
      byId("login").hidden = false;
      byId("status").hidden = byId("networks").hidden = byId("hotspot").hidden = true;
      return;
    }
    if (r.ok) render(await r.json());
  } catch { /* the hotspot is down for a moment: keep the last view */ }
}

poll();
setInterval(poll, POLL_MS);
