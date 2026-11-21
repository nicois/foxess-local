// Tests for static/js/socview.js, the dashboard's SoC between readings.
// Run by TestSocView (node --test). Times are milliseconds; readings
// come every 5 s and the dashboard asks for the value every 250 ms.
import { test } from "node:test";
import assert from "node:assert/strict";
import { socView, SOC_EXTRAP_MS, SOC_SNAP_PCT } from "../static/js/socview.js";

const CAP = 41940; // Wh
const TICK = 250;
const EVERY = 5000;

// run feeds readings [{est, w}] every 5 s and returns every displayed
// value, ticking every 250 ms from each reading to the next.
function run(readings, { cap = CAP, from = 0 } = {}) {
  const v = socView();
  const out = [];
  readings.forEach((r, i) => {
    const at = from + i * EVERY;
    v.reading(r.est, r.w, cap, "r" + i, at);
    for (let t = at; t < at + EVERY; t += TICK) out.push(v.value(t));
  });
  return out;
}

const never = (xs, cmp, msg) => {
  for (let i = 1; i < xs.length; i++) {
    assert.ok(cmp(xs[i], xs[i - 1]), `${msg}: step ${i}: ${xs[i - 1]} → ${xs[i]}`);
  }
};
const nonIncreasing = (xs, msg) => never(xs, (b, a) => b <= a + 1e-12, msg);
const nonDecreasing = (xs, msg) => never(xs, (b, a) => b >= a - 1e-12, msg);

// %/5 s at a battery power (W, + = charging)
const per5s = (w) => (w / CAP) * 100 * (EVERY / 3600000);

test("steady discharge: smooth, never up", () => {
  const w = -2600;
  const readings = Array.from({ length: 8 }, (_, i) => ({ est: 90 + per5s(w) * i, w }));
  const xs = run(readings);
  nonIncreasing(xs, "steady discharge");
  // it moves between readings, not only at them
  assert.ok(xs[1] < xs[0] && xs[19] < xs[10], "the value moves between readings");
});

// The first defect (owner, 2026-11-21): readings in 0.01 % steps. Four
// unchanged readings then a step down is not "0.01 % per 5 s".
test("a 0.01 % step after a long hold is not a rate", () => {
  const w = -100; // ~0.00033 %/5 s
  const xs = run([
    { est: 90.0, w }, { est: 90.0, w }, { est: 90.0, w }, { est: 90.0, w },
    { est: 89.99, w }, { est: 89.99, w },
  ]);
  // after the step, the next 10 s move about as far as 100 W moves it
  const after = xs.slice(4 * 20);
  assert.ok(after[0] - after.at(-1) < 0.002, `fell ${after[0] - after.at(-1)} % in 10 s at 100 W`);
  nonIncreasing(xs, "slow discharge");
});

// The second defect (owner, 2026-11-21): the display ran ahead, the next
// reading landed behind it (or did not move: the estimate held at a
// bound), and the display went back up.
test("a reading behind the display: hold, never back up", () => {
  const w = -3000;
  // a step, then an unchanged reading (the display is past it by then)
  nonIncreasing(run([
    { est: 50.0, w }, { est: 49.99, w }, { est: 49.99, w }, { est: 49.98, w }, { est: 49.98, w },
  ]), "step then hold");
  const xs = run([
    { est: 50.0, w }, { est: 50.0, w }, { est: 50.0, w }, // the estimate held
    { est: 49.99, w }, { est: 49.98, w },
  ]);
  nonIncreasing(xs, "held estimate");
  // and it does not run away from a held estimate: at most one
  // interval's movement ahead of it
  assert.ok(Math.min(...xs) >= 49.98 + per5s(w) * (SOC_EXTRAP_MS / EVERY) - 1e-9, `ran to ${Math.min(...xs)}`);
});

test("going idle after a discharge: never back up", () => {
  const xs = run([
    { est: 70.0, w: -3000 }, { est: 69.99, w: -3000 },
    { est: 69.99, w: 0 }, { est: 69.99, w: 10 }, { est: 69.99, w: -5 },
  ]);
  nonIncreasing(xs, "discharge then idle");
});

test("turning to charge: it rises from then on", () => {
  const xs = run([
    { est: 60.0, w: -3000 }, { est: 59.99, w: -3000 },
    { est: 59.99, w: 3000 }, { est: 60.0, w: 3000 }, { est: 60.01, w: 3000 },
  ]);
  nonIncreasing(xs.slice(0, 40), "discharging");
  nonDecreasing(xs.slice(40), "charging");
});

test("a correction against the direction is shown at once", () => {
  const v = socView();
  v.reading(50.0, -3000, CAP, "a", 0);
  v.value(4000);
  v.reading(50.0 + SOC_SNAP_PCT + 0.1, -3000, CAP, "b", 5000); // the BMS anchor
  assert.ok(v.value(5000) >= 50.0 + SOC_SNAP_PCT, `got ${v.value(5000)}`);
});

test("no new reading: it stops after SOC_EXTRAP_MS", () => {
  const v = socView();
  v.reading(80.0, -5000, CAP, "a", 0);
  const at = (t) => v.value(t);
  assert.equal(at(SOC_EXTRAP_MS), at(60000));
  const most = -per5s(-5000) * (SOC_EXTRAP_MS / EVERY);
  assert.ok(80.0 - at(60000) <= most + 1e-9, `fell ${80.0 - at(60000)} %, at most ${most}`);
  assert.ok(80.0 - at(60000) > 0, "it did move");
});

test("the same reading again does not restart the carry", () => {
  const v = socView();
  v.reading(80.0, -5000, CAP, "a", 0);
  const before = v.value(4000);
  v.reading(80.0, -5000, CAP, "a", 4000); // a repaint of the same reading
  assert.ok(v.value(4000) <= before);
});

test("no estimate, or no capacity", () => {
  const v = socView();
  assert.equal(v.value(0), null);
  v.reading(null, -3000, CAP, "a", 0);
  assert.equal(v.value(1000), null);
  v.reading(55.5, -3000, 0, "b", 0); // capacity unknown: the reading as is
  assert.equal(v.value(4000), 55.5);
});
