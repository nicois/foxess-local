// The diagnostics period choices (issue #1): what a period can hold
// depends on the retention. Messages (frames) are kept retention.raw_hours
// after their connection closed; events are kept longer.
//
// - The diagnostic bundle holds messages and registers only: a period
//   longer than the retention would promise messages that are gone, so
//   it is not offered.
// - A problem report also carries the events, which a longer period
//   does reach: it stays, marked beyond, so its label can say the
//   messages stop at the retention.
//
// Where the retention is not one of the choices it is offered itself
// (all the messages there are). Pure: the labels are the caller's.

// DEFAULT_KEPT_HOURS is the server's default raw_hours, used while
// /config is unknown.
export const DEFAULT_KEPT_HOURS = 72;

// withKept adds the retention to the choices when it falls inside them
// (a longer one is past the longest period the server takes).
function withKept(choices, kept) {
  const all = choices.includes(kept) || kept > Math.max(...choices) ? [...choices] : [...choices, kept];
  return all.sort((a, b) => a - b);
}

// bundlePeriods: the choices no longer than the retention, plus the
// retention itself.
export function bundlePeriods(choices, keptHours) {
  const kept = keptHours || DEFAULT_KEPT_HOURS;
  return withKept(choices, kept).filter((h) => h <= kept);
}

// reportPeriods: every choice, plus the retention; beyond marks a period whose messages stop at the retention.
export function reportPeriods(choices, keptHours) {
  const kept = keptHours || DEFAULT_KEPT_HOURS;
  return withKept(choices, kept).map((h) => ({ hours: h, beyond: h > kept }));
}
