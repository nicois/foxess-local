// Tests for static/js/periods.js, the diagnostics period choices. Run by
// TestPeriods (node --test). Hours throughout.
import { test } from "node:test";
import assert from "node:assert/strict";
import { bundlePeriods, reportPeriods } from "../static/js/periods.js";

const CHOICES = [6, 24, 72, 168];

test("bundle: nothing longer than the messages are kept", () => {
  assert.deepEqual(bundlePeriods(CHOICES, 72), [6, 24, 72]);
});

test("bundle: the retention itself is offered when it is not a choice", () => {
  assert.deepEqual(bundlePeriods(CHOICES, 48), [6, 24, 48]);
  assert.deepEqual(bundlePeriods(CHOICES, 4), [4]);
});

test("bundle: a long retention keeps every choice", () => {
  assert.deepEqual(bundlePeriods(CHOICES, 720), CHOICES);
});

test("report: longer periods stay (their events are kept), marked beyond", () => {
  assert.deepEqual(reportPeriods(CHOICES, 72), [
    { hours: 6, beyond: false }, { hours: 24, beyond: false },
    { hours: 72, beyond: false }, { hours: 168, beyond: true },
  ]);
});

test("report: the retention is offered when it is not a choice", () => {
  assert.deepEqual(reportPeriods(CHOICES, 48), [
    { hours: 6, beyond: false }, { hours: 24, beyond: false }, { hours: 48, beyond: false },
    { hours: 72, beyond: true }, { hours: 168, beyond: true },
  ]);
});

test("report: a long retention marks nothing", () => {
  assert.ok(reportPeriods(CHOICES, 720).every((p) => !p.beyond));
});

test("unknown retention (no config): the server default, 72 h", () => {
  assert.deepEqual(bundlePeriods(CHOICES, undefined), [6, 24, 72]);
});
