// The live-feed state the page shows (owner, 2026-11-28).
//
// A degraded feed (connecting, reconnecting, disconnected) is shown only
// once it has lasted FEED_GRACE_MS. Until then the page keeps showing the
// last healthy state ("connected", or "—" before the first connection).
// Returning to a hidden tab is the common case: the stream pauses in the
// background and reconnects within a moment of the tab coming back, and
// a warning flashing up for that is noise. Time spent hidden does not
// count towards the grace: it restarts when the page becomes visible.
//
// Pure: times are passed in (ms), so tests drive it without a clock.

export const FEED_GRACE_MS = 3000;

const healthy = (s) => s === "connected" || s === "—";

export function feedView() {
  let state = "—";     // the latest state
  let lastOK = "—";    // the last healthy state
  let since = null;    // when the current degraded spell's grace started

  return {
    set(s, at) {
      state = s;
      if (healthy(s)) {
        lastOK = s;
        since = null;
      } else if (since === null) {
        since = at;
      }
    },
    // visible: the page came back to the foreground at `at`.
    visible(at) {
      if (since !== null) since = at;
    },
    // reset: signed out; the next connection is a first one.
    reset() {
      state = "—";
      lastOK = "—";
      since = null;
    },
    shown(at) {
      if (since === null || at - since >= FEED_GRACE_MS) return state;
      return lastOK;
    },
    // wakeAt: when shown() next changes on its own (null = it will not).
    wakeAt() {
      return since === null ? null : since + FEED_GRACE_MS;
    },
  };
}
