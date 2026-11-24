// The wizard's page: what the stick is doing, what to do next, and the
// home Wi-Fi setup. It polls /api/status every 5 s (which also tells the
// wizard an operator is here, so the hotspot stays open).
import { LANGS, setLang, currentLang, tr } from "./i18n.js";

const byId = (id) => document.getElementById(id);
const POLL_MS = 5000;
let last = null;
let pendingJoin = null;
let joinSentAt = 0; // ms: when the stick accepted the join (0: none pending)

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

// showAfterJoin replaces the page with what to do next. It shows before
// the join is sent (asking), because the phone closes its sign-in window
// the moment the hotspot goes; after it, it stays in a real browser.
function showAfterJoin(ssid, asking) {
  const host = (last?.host ?? "foxess.local").replace(/\.local$/, "");
  const url = `http://${host}.local:18081/`;
  byId("after-join-title").textContent = asking ? tr("Before you connect") : tr("Joining your Wi-Fi network…");
  byId("after-join-text").textContent = asking
    ? tr("When you press Connect, the stick leaves this hotspot to join {ssid}, and this page closes. Note the address below first: it is also on the stick's label. Joining takes up to a minute. Then watch its light:", { ssid })
    : tr("The stick has left this hotspot to join {ssid}. That takes up to a minute. Then watch its light:", { ssid });
  byId("after-join-ask").hidden = !asking;
  byId("after-join-green").textContent = tr("Solid green: it worked. Put this device back on {ssid} and open:", { ssid });
  const a = byId("after-join-link");
  a.href = url;
  a.textContent = url;
  byId("after-join-red").textContent = tr("Blinking red: it could not join. Rejoin the {hotspot} Wi-Fi network and this page will say why.", { hotspot: host });
  byId("after-join-note").textContent = tr("If that address does not open, find {host} in your router's list of devices and open its address with :18081 at the end.", { host });
  for (const id of ["status", "networks", "hotspot", "confirm"]) byId(id).hidden = true;
  byId("after-join").hidden = false;
  window.scrollTo(0, 0);
}

function render(s) {
  last = s;
  byId("login").hidden = true;
  if (!joinSentAt && !byId("after-join").hidden) return; // asking before a join
  if (joinSentAt) {
    // a result newer than the join this page sent: back on the hotspot
    const fresh = s.join && s.join.age_s * 1000 <= Date.now() - joinSentAt;
    if (!fresh) return;
    joinSentAt = 0;
    byId("after-join").hidden = true;
  }
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
  showAfterJoin(ssid, true);
});

byId("after-join-go").addEventListener("click", async () => {
  byId("after-join-ask").hidden = true;
  let r;
  try {
    r = await post("api/join", pendingJoin);
  } catch {
    r = { status: 202 }; // the hotspot went before the answer came: it was taken
  }
  if (r.status === 202) {
    joinSentAt = Date.now();
    showAfterJoin(pendingJoin.ssid, false);
    return;
  }
  leaveAfterJoin();
  if (r.status === 400) {
    byId("join-result").hidden = false;
    byId("join-result").className = "error";
    byId("join-result").textContent = tr("That network name or password can't be used.");
  }
});

byId("after-join-cancel").addEventListener("click", leaveAfterJoin);

// leaveAfterJoin puts the page back as it was before Connect.
function leaveAfterJoin() {
  byId("after-join").hidden = true;
  for (const id of ["status", "networks", "hotspot"]) byId(id).hidden = false;
  if (last) render(last);
}

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
