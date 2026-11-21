// The dashboard's state of charge between readings (owner, 2026-11-21).
//
// The estimate arrives every ~5 s, in 0.01 % steps. Between readings the
// value is carried forward at the rate the battery power gives (W over
// the capacity) — not the rate between two readings, which the steps
// make wrong: four unchanged readings and a 0.01 % step are not
// "0.01 % per 5 s". It is carried for at most SOC_EXTRAP_MS (one
// reading interval, with slack); the power-based rate leaves out the
// estimator's learnt factor, an error of thousandths of a percent over
// one interval.
//
// It never goes back against the direction the battery last moved: when
// a reading lands behind the displayed value, the display holds until
// the carry from the new reading catches up. Only a turn of direction
// (charging after discharging) or a correction of SOC_SNAP_PCT or more
// (a BMS anchor) moves it back.
//
// Pure: times are passed in (ms), so tests drive it without a clock.

export const SOC_EXTRAP_MS = 6000;
export const SOC_IDLE_W = 50;    // below this battery power the SoC does not move
export const SOC_SNAP_PCT = 0.25; // a reading this far behind the display is a correction

const MS_PER_H = 3600000;

export function socView() {
  let r = null;      // the last reading: {est, at, rate (%/ms), key}
  let shown = null;  // the last value returned
  let dir = 0;       // the direction the battery last moved (−1, +1), 0 = never

  return {
    // reading takes one estimate (%, null = none), the battery power (W,
    // + = charging), the capacity (Wh), a key naming the reading (its
    // timestamp: the same reading again does not restart the carry) and
    // the local time it arrived.
    reading(est, battW, capWh, key, now) {
      if (est == null) {
        r = null;
        shown = null;
        return;
      }
      if (r && r.key === key) return;
      const w = battW ?? 0;
      const rate = capWh > 0 && Math.abs(w) >= SOC_IDLE_W ? (w / capWh) * 100 / MS_PER_H : 0;
      if (rate !== 0) dir = Math.sign(rate);
      r = { est, at: now, rate, key };
    },

    // value is the SoC to display at local time now (null: none).
    value(now) {
      if (!r) return null;
      let v = r.est + r.rate * Math.min(Math.max(now - r.at, 0), SOC_EXTRAP_MS);
      // ahead of the reading in the direction of travel: hold
      if (shown != null && dir !== 0 && (v - shown) * dir < 0 && Math.abs(v - shown) < SOC_SNAP_PCT) v = shown;
      v = Math.max(0, Math.min(100, v));
      shown = v;
      return v;
    },
  };
}
