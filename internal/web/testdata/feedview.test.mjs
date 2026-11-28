// Tests for static/js/feedview.js, the live-feed state the page shows.
// Run by TestFeedView (node --test). Times are milliseconds.
import { test } from "node:test";
import assert from "node:assert/strict";
import { feedView, FEED_GRACE_MS } from "../static/js/feedview.js";

test("a reconnect within the grace never shows", () => {
  const v = feedView();
  v.set("connected", 0);
  v.set("reconnecting…", 10000);
  assert.equal(v.shown(10000), "connected");
  assert.equal(v.shown(10000 + FEED_GRACE_MS - 1), "connected");
  v.set("connected", 10000 + FEED_GRACE_MS - 1);
  assert.equal(v.shown(10000 + FEED_GRACE_MS), "connected");
  assert.equal(v.wakeAt(), null);
});

test("a degraded feed shows once the grace has passed", () => {
  const v = feedView();
  v.set("connected", 0);
  v.set("reconnecting…", 1000);
  assert.equal(v.wakeAt(), 1000 + FEED_GRACE_MS);
  assert.equal(v.shown(1000 + FEED_GRACE_MS), "reconnecting…");
});

test("the grace runs from the first degraded state, not the latest", () => {
  const v = feedView();
  v.set("connected", 0);
  v.set("disconnected", 1000);
  v.set("reconnecting…", 1500);
  assert.equal(v.shown(1000 + FEED_GRACE_MS), "reconnecting…");
});

test("before the first connection the page shows no state, then connecting", () => {
  const v = feedView();
  v.set("connecting…", 0);
  assert.equal(v.shown(0), "—");
  assert.equal(v.shown(FEED_GRACE_MS), "connecting…");
});

test("time spent hidden does not count: the grace restarts on return", () => {
  const v = feedView();
  v.set("connected", 0);
  v.set("disconnected", 1000);   // the tab was hidden: the stream paused
  v.visible(60000);              // back after a minute
  v.set("reconnecting…", 60010);
  assert.equal(v.shown(60010), "connected");
  assert.equal(v.wakeAt(), 60000 + FEED_GRACE_MS);
  v.set("connected", 60300);
  assert.equal(v.shown(60300), "connected");
});

test("visible while healthy changes nothing", () => {
  const v = feedView();
  v.set("connected", 0);
  v.visible(5000);
  assert.equal(v.shown(5000), "connected");
  assert.equal(v.wakeAt(), null);
});

test("a reset forgets the last healthy state", () => {
  const v = feedView();
  v.set("connected", 0);
  v.reset();
  assert.equal(v.shown(0), "—");
  v.set("connecting…", 100);
  assert.equal(v.shown(100), "—");
});
