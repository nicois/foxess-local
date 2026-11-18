// charts.js — small-multiple time-series charts (hand-written SVG).
//
// Design (dataviz skill): one series per row, the entity's fixed color
// (CSS custom property via a class — the CSP forbids inline styles),
// 2 px lines, a ~10 % area wash to the zero baseline, solid hairline
// grid, clean y ticks, gaps where data is missing. A row may carry
// several series on one scale (e.g. temperatures: one line each, told
// apart by color, dash and name). One crosshair spans every row; the
// tooltip lists only the row under the pointer (or the focused row) and
// sits beside it, inside the chart (values lead, names follow, line
// keys). Keyboard: focus a plot, ←/→ move the crosshair, Esc clears. All
// text goes in via textContent.

import { tr, locale } from "./i18n.js";

const NS = "http://www.w3.org/2000/svg";
const ROW_H = 92;          // plot height per row (px)
const AXIS_H = 22;         // x-axis band under the last row
const GUTTER_L = 52;       // y-tick labels
const GUTTER_R = 10;
const PAD_T = 8;
const PAD_B = 8;           // room under each plot for the bottom y-tick label
const TIP_GAP = 6;         // touch: the tooltip's gap above (or below) the touched row (px)

function el(tag, attrs = {}, parent) {
  const n = document.createElementNS(NS, tag);
  for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, String(v));
  if (parent) parent.appendChild(n);
  return n;
}

function niceStep(span, target) {
  const raw = span / Math.max(1, target);
  const mag = 10 ** Math.floor(Math.log10(raw || 1));
  for (const m of [1, 2, 2.5, 5, 10]) {
    if (m * mag >= raw) return m * mag;
  }
  return 10 * mag;
}

// yScale: a domain that includes 0 (unless zero is false: a level such
// as a temperature, where 0 would flatten the variation), ~3 clean ticks.
function yScale(values, fixed, zero = true) {
  let lo = zero ? 0 : Infinity;
  let hi = zero ? 0 : -Infinity;
  for (const v of values) {
    if (v == null) continue;
    if (v < lo) lo = v;
    if (v > hi) hi = v;
  }
  if (!Number.isFinite(lo)) [lo, hi] = [0, 1];
  if (fixed) [lo, hi] = fixed;
  if (lo === hi) hi = lo + 1;
  const step = niceStep(hi - lo, 3);
  lo = Math.floor(lo / step) * step;
  hi = Math.ceil(hi / step) * step;
  const ticks = [];
  for (let t = lo; t <= hi + step / 2; t += step) ticks.push(Math.round(t * 1e6) / 1e6);
  return { lo, hi, ticks };
}

// X ticks: local-time aligned steps chosen for the span.
const X_STEPS = [
  [5 * 60, "time"], [15 * 60, "time"], [30 * 60, "time"], [3600, "time"],
  [3 * 3600, "time"], [6 * 3600, "time"], [12 * 3600, "time"],
  [86400, "day"], [2 * 86400, "day"], [7 * 86400, "day"],
];

function xTicks(t0, t1, width) {
  const maxTicks = Math.max(2, Math.floor(width / 90));
  const span = t1 - t0;
  const [step, kind] = X_STEPS.find(([s]) => span / s <= maxTicks) ?? X_STEPS[X_STEPS.length - 1];
  const out = [];
  const d = new Date(t0 * 1000);
  if (kind === "day") d.setHours(0, 0, 0, 0);
  else d.setMinutes(0, 0, 0);
  for (let t = d.getTime() / 1000; t <= t1; t += step) {
    if (t >= t0) out.push({ t, kind });
  }
  return out;
}

const fmtTick = ({ t, kind }) => {
  const d = new Date(t * 1000);
  return kind === "day"
    ? d.toLocaleDateString(locale(), { weekday: "short", day: "numeric" })
    : d.toLocaleTimeString(locale(), { hour: "2-digit", minute: "2-digit" });
};

const fmtTipTime = (t, step) => {
  const d = new Date(t * 1000);
  const opts = step >= 3600
    ? { weekday: "short", day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" }
    : { hour: "2-digit", minute: "2-digit" };
  return d.toLocaleString(locale(), opts);
};

// pathFor: the line (gaps split it), the area wash to the zero line, and
// the isolated points (a one-sample run has no length to stroke — it gets
// a marker instead, or a sparse range would look empty).
function pathFor(vals, x, y, y0) {
  let line = "";
  let area = "";
  const dots = [];
  let run = [];
  const flush = () => {
    if (run.length === 0) return;
    if (run.length === 1) {
      dots.push(run[0]);
      run = [];
      return;
    }
    line += run.map(([px, py], i) => (i ? "L" : "M") + px.toFixed(1) + " " + py.toFixed(1)).join("");
    area += "M" + run[0][0].toFixed(1) + " " + y0.toFixed(1) +
      run.map(([px, py]) => "L" + px.toFixed(1) + " " + py.toFixed(1)).join("") +
      "L" + run[run.length - 1][0].toFixed(1) + " " + y0.toFixed(1) + "Z";
    run = [];
  };
  vals.forEach((v, i) => {
    if (v == null) { flush(); return; }
    run.push([x(i), y(v)]);
  });
  flush();
  return { line, area, dots };
}

// summarize: the plot's text alternative (WCAG 1.1.1) — latest, low and
// high over the range; the data table carries every value.
function summarize(row, rangeLabel) {
  const ss = seriesOf(row);
  if (ss.length > 1) return ss.map((s) => summarize({ ...row, title: s.title, values: s.values, series: null }, rangeLabel)).join(" ");
  let last = null;
  let lo = Infinity;
  let hi = -Infinity;
  for (const v of row.values) {
    if (v == null) continue;
    last = v;
    lo = Math.min(lo, v);
    hi = Math.max(hi, v);
  }
  const title = rangeLabel ? tr("{title} over the {range}", { title: row.title, range: rangeLabel }) : row.title;
  if (last == null) return tr("{title}: no data.", { title });
  return tr("{title}: latest {last}, lowest {lo}, highest {hi}. Arrow keys step through the values.", {
    title, last: row.fmt(last, false), lo: row.fmt(lo, false), hi: row.fmt(hi, false),
  });
}

// seriesOf: a row's series (a plain row is one series).
function seriesOf(row) {
  return row.series ?? [{ title: row.title, cls: row.cls, values: row.values }];
}

// smallMultiples renders rows into root and keeps them sized to it.
// data: { ts: [unix s], step: seconds, rows: [{ key, title, cls, unit,
//   values, fmt(v), fixed?: [lo, hi], legend?: string,
//   area?: false (a level such as SoC: line only, no wash),
//   zero?: false (the scale need not include 0),
//   series?: [{ title, cls, values, dash? }] (several lines, one scale) }] }
// Returns a disposer.
export function smallMultiples(root, data) {
  let idx = null; // crosshair position (index into ts)
  let rowIdx = 0; // the row the tooltip describes (pointer / focus)
  let touch = false; // the pointer is a finger
  let parts = [];
  const tip = document.createElement("div");
  tip.className = "sm-tip";
  tip.hidden = true;
  tip.setAttribute("aria-hidden", "true"); // the live region below speaks for it
  // keyboard readout for screen readers (pointer moves stay silent)
  const live = document.createElement("div");
  live.className = "sr-only";
  live.setAttribute("aria-live", "polite");
  let fromKeyboard = false;

  const hasData = data.rows.some((r) => seriesOf(r).some((s) => s.values.some((v) => v != null)));

  function draw() {
    root.replaceChildren();
    parts = [];
    if (!hasData) {
      const p = document.createElement("p");
      p.className = "sm-empty muted";
      p.textContent = tr("No data in this range yet.");
      root.appendChild(p);
      return;
    }
    const width = Math.max(240, root.clientWidth);
    const n = data.ts.length;
    const plotW = width - GUTTER_L - GUTTER_R;
    const x = (i) => GUTTER_L + (n <= 1 ? plotW / 2 : (i / (n - 1)) * plotW);
    const ticksX = xTicks(data.ts[0], data.ts[n - 1], plotW);

    data.rows.forEach((row, ri) => {
      const last = ri === data.rows.length - 1;
      const wrap = document.createElement("div");
      wrap.className = "sm-row";
      const head = document.createElement("div");
      head.className = "sm-head";
      const key = document.createElement("span");
      key.className = "sm-key " + row.cls;
      key.setAttribute("aria-hidden", "true");
      const title = document.createElement("span");
      title.className = "sm-title";
      title.textContent = row.title;
      const legend = document.createElement("span");
      legend.className = "sm-legend muted";
      if (row.series) {
        key.hidden = true; // each series has its own key below
        for (const s of row.series) {
          const k = document.createElement("span");
          k.className = "sm-key " + s.cls + (s.dash ? " dash" : "");
          k.setAttribute("aria-hidden", "true");
          const n = document.createElement("span");
          n.className = "sm-series-name";
          n.textContent = s.title;
          legend.append(k, n);
        }
      } else {
        legend.textContent = row.legend ?? "";
      }
      head.append(key, title, legend);
      wrap.appendChild(head);

      const h = ROW_H + PAD_T + PAD_B + (last ? AXIS_H : 0);
      const svg = el("svg", {
        class: "sm-plot", width, height: h, viewBox: `0 0 ${width} ${h}`,
        role: "img", tabindex: 0,
        "aria-label": summarize(row, data.rangeLabel),
      });
      const series = seriesOf(row);
      const sc = yScale(series.flatMap((s) => s.values), row.fixed, row.zero !== false);
      const y = (v) => PAD_T + (1 - (v - sc.lo) / (sc.hi - sc.lo)) * ROW_H;
      // grid + y ticks
      for (const t of sc.ticks) {
        const gy = y(t);
        el("line", { class: t === 0 ? "sm-zero" : "sm-grid", x1: GUTTER_L, x2: width - GUTTER_R, y1: gy, y2: gy }, svg);
        const lab = el("text", { class: "sm-tick", x: GUTTER_L - 6, y: gy + 4, "text-anchor": "end" }, svg);
        lab.textContent = row.fmt(t, true);
      }
      // x ticks (vertical hairlines on every row, labels on the last)
      for (const tk of ticksX) {
        const i = (tk.t - data.ts[0]) / data.step;
        const gx = x(i);
        el("line", { class: "sm-grid", x1: gx, x2: gx, y1: PAD_T, y2: PAD_T + ROW_H }, svg);
        if (last) {
          const anchor = gx > width - GUTTER_R - 30 ? "end" : "middle";
          const lab = el("text", { class: "sm-tick", x: Math.min(gx, width - 2), y: PAD_T + ROW_H + PAD_B + 14, "text-anchor": anchor }, svg);
          lab.textContent = fmtTick(tk);
        }
      }
      const y0 = y(Math.max(sc.lo, Math.min(0, sc.hi)));
      for (const s of series) {
        const { line, area, dots } = pathFor(s.values, x, y, y0);
        if (row.area !== false && series.length === 1) el("path", { class: "sm-area " + s.cls, d: area }, svg);
        el("path", { class: "sm-line " + s.cls + (s.dash ? " dash" : ""), d: line }, svg);
        for (const [px, py] of dots) el("circle", { class: "sm-pt " + s.cls, cx: px, cy: py, r: 3 }, svg);
      }
      const xh = el("line", { class: "sm-xhair", x1: 0, x2: 0, y1: PAD_T, y2: PAD_T + ROW_H, visibility: "hidden" }, svg);
      const dots = series.map((s) => el("circle", { class: "sm-dot " + s.cls, r: 4, cx: 0, cy: 0, visibility: "hidden" }, svg));
      wrap.appendChild(svg);
      root.appendChild(wrap);
      parts.push({ row, series, svg, wrap, xh, dots, x, y });
    });
    root.append(tip, live);
    paintCrosshair();
  }

  function paintCrosshair() {
    if (idx == null) {
      tip.hidden = true;
      for (const p of parts) {
        p.xh.setAttribute("visibility", "hidden");
        for (const d of p.dots) d.setAttribute("visibility", "hidden");
      }
      return;
    }
    for (const p of parts) {
      const px = p.x(idx);
      p.xh.setAttribute("x1", px);
      p.xh.setAttribute("x2", px);
      p.xh.setAttribute("visibility", "visible");
      p.series.forEach((s, i) => {
        const v = s.values[idx];
        const d = p.dots[i];
        if (v == null) {
          d.setAttribute("visibility", "hidden");
        } else {
          d.setAttribute("cx", px);
          d.setAttribute("cy", p.y(v));
          d.setAttribute("visibility", "visible");
        }
      });
    }
    // the tooltip: only the row under the pointer (or focused)
    const p = parts[Math.min(rowIdx, parts.length - 1)];
    const lines = p.series.map((s) => [s, s.values[idx]]);
    tip.replaceChildren();
    const when = document.createElement("div");
    when.className = "sm-tip-time";
    when.textContent = fmtTipTime(data.ts[idx], data.step);
    tip.appendChild(when);
    for (const [s, v] of lines) {
      const r = document.createElement("div");
      r.className = "sm-tip-row";
      const k = document.createElement("span");
      k.className = "sm-key " + s.cls + (s.dash ? " dash" : "");
      const val = document.createElement("strong");
      val.textContent = v == null ? "—" : p.row.fmt(v, false);
      const name = document.createElement("span");
      name.className = "muted";
      name.textContent = s.title;
      r.append(k, val, name);
      tip.appendChild(r);
    }
    tip.hidden = false;
    if (fromKeyboard) {
      live.textContent = fmtTipTime(data.ts[idx], data.step) + ": " +
        lines.map(([s, v]) => s.title + " " + (v == null ? tr("no data") : p.row.fmt(v, false))).join(", ");
    }
    placeTip(p);
  }

  // placeTip: beside the crosshair (the side with room), level with the
  // described row, clamped inside the chart area — never off screen. On
  // touch it goes above the touched row (below it when the viewport has
  // no room above): the finger is inside the row's plot, so the hand
  // does not cover it. It is anchored to the row, not to the finger's y:
  // a vertical drag scrolls the page off the main thread, so a finger-y
  // measured against the chart's (not yet scrolled) position would move
  // the box twice — once by the measurement, once with the scroll.
  function placeTip(p) {
    const px = p.x(idx);
    const w = root.clientWidth;
    if (touch) {
      tip.style.left = Math.max(0, Math.min(px - tip.offsetWidth / 2, w - tip.offsetWidth)) + "px";
      tip.style.right = "";
      const visTop = -root.getBoundingClientRect().top; // the viewport's top edge, in root's frame
      let top = p.wrap.offsetTop - TIP_GAP - tip.offsetHeight;
      if (top < visTop) top = p.wrap.offsetTop + p.wrap.offsetHeight + TIP_GAP;
      tip.style.top = top + "px";
      return;
    }
    const left = px <= w / 2;
    tip.classList.toggle("left", !left);
    tip.dataset.x = String(px);
    // position via CSSOM (allowed under style-src 'self')
    tip.style.left = "";
    tip.style.right = "";
    const tw = tip.offsetWidth;
    if (left) tip.style.left = Math.min(px + 12, Math.max(0, w - tw)) + "px";
    else tip.style.right = Math.min(w - px + 12, Math.max(0, w - tw)) + "px";
    const maxTop = Math.max(0, root.clientHeight - tip.offsetHeight);
    // the plot's top (below the row's title and legend), in root's frame
    const plotTop = p.wrap.offsetTop + (p.svg.getBoundingClientRect().top - p.wrap.getBoundingClientRect().top);
    tip.style.top = Math.min(plotTop + PAD_T, maxTop) + "px";
  }

  // rowOf: the index of the row a pointer / focus target belongs to
  function rowOf(target) {
    const svg = target.closest?.(".sm-plot");
    const i = parts.findIndex((q) => q.svg === svg);
    return i < 0 ? rowIdx : i;
  }

  function nearest(clientX) {
    const p = parts[0];
    if (!p) return null;
    const box = p.svg.getBoundingClientRect();
    const n = data.ts.length;
    const plotW = box.width - GUTTER_L - GUTTER_R;
    const f = (clientX - box.left - GUTTER_L) / plotW;
    return Math.max(0, Math.min(n - 1, Math.round(f * (n - 1))));
  }

  const onMove = (e) => {
    if (!e.target.closest?.(".sm-plot")) return;
    fromKeyboard = false;
    touch = e.pointerType === "touch";
    rowIdx = rowOf(e.target);
    idx = nearest(e.clientX);
    paintCrosshair();
  };
  // a lifted finger leaves the readout up (a tap elsewhere moves focus
  // away and clears it)
  const onLeave = (e) => { if (e?.pointerType === "touch") return; idx = null; paintCrosshair(); };
  const onKey = (e) => {
    if (!e.target.closest?.(".sm-plot")) return;
    rowIdx = rowOf(e.target);
    touch = false;
    const n = data.ts.length;
    if (e.key === "ArrowRight") idx = Math.min(n - 1, (idx ?? -1) + 1);
    else if (e.key === "ArrowLeft") idx = Math.max(0, (idx ?? n) - 1);
    else if (e.key === "Escape") idx = null;
    else return;
    e.preventDefault();
    fromKeyboard = true;
    paintCrosshair();
  };
  root.addEventListener("pointermove", onMove);
  root.addEventListener("pointerdown", onMove); // a tap, without a move
  root.addEventListener("pointerleave", onLeave);
  root.addEventListener("keydown", onKey);
  root.addEventListener("focusout", (e) => { if (!root.contains(e.relatedTarget)) onLeave(); });

  let lastW = 0;
  let raf = 0;
  const ro = new ResizeObserver(() => {
    const w = root.clientWidth;
    if (w === lastW) return;
    lastW = w;
    cancelAnimationFrame(raf);
    raf = requestAnimationFrame(draw);
  });
  ro.observe(root);
  lastW = root.clientWidth;
  draw();

  return () => {
    ro.disconnect();
    cancelAnimationFrame(raf);
    root.removeEventListener("pointermove", onMove);
    root.removeEventListener("pointerdown", onMove);
    root.removeEventListener("pointerleave", onLeave);
    root.removeEventListener("keydown", onKey);
  };
}
