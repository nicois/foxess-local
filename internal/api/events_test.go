package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// awaitSystemEvent drains the subscription until one system event of
// the wanted type arrives (any other kind or type is a failure — the
// tests' feeds are deterministic).
func awaitSystemEvent(t *testing.T, ch <-chan Event, wantType string) SystemEvent {
	t.Helper()
	for {
		ev := <-ch
		if ev.Kind != "system" {
			t.Fatalf("event: got kind %q, want system", ev.Kind)
		}
		sys, ok := ev.Data.(SystemEvent)
		if !ok {
			t.Fatalf("system payload: got %T", ev.Data)
		}
		if sys.Type == wantType {
			return sys
		}
		t.Fatalf("system event: got type %q, want %q (detail %q)", sys.Type, wantType, sys.Detail)
	}
}

// ---------- the listener-free SSE client ----------

// sseEvent is one parsed server-sent event (the id/event/data fields).
type sseEvent struct {
	id   int64
	kind string
	data string
}

// sseWriter is a fake SSE ResponseWriter: it collects the stream,
// extracts complete SSE frames (terminated by a blank line), and wakes
// readers with a condition variable — no network, no wall-clock.
type sseWriter struct {
	mu         sync.Mutex
	cond       *sync.Cond
	statusCode int
	events     []sseEvent
	pending    []byte
	raw        []byte // the whole stream as written
	header     http.Header
	ready      chan struct{} // closed on the first write (the connect barrier)
}

func newSSEWriter() *sseWriter {
	w := &sseWriter{statusCode: http.StatusOK, header: http.Header{}, ready: make(chan struct{})}
	w.cond = sync.NewCond(&w.mu)
	return w
}

// awaitReady blocks until the handler has written its first byte (the
// ": connected" comment — the subscription is registered, so a later
// publish is delivered).
func (w *sseWriter) awaitReady() { <-w.ready }

// markReady closes the ready channel once (the caller holds the lock).
func (w *sseWriter) markReady() {
	select {
	case <-w.ready:
	default:
		close(w.ready)
	}
}

func (w *sseWriter) Header() http.Header { return w.header }

func (w *sseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.markReady()
	w.pending = append(w.pending, p...)
	w.raw = append(w.raw, p...)
	for {
		idx := indexBytes(w.pending, []byte("\n\n"))
		if idx < 0 {
			break
		}
		frame := strings.TrimRight(string(w.pending[:idx]), "\n")
		w.pending = w.pending[idx+2:]
		w.events = append(w.events, parseSSEFrame(frame))
	}
	w.cond.Broadcast()
	return len(p), nil
}

func (w *sseWriter) Flush() {}

func (w *sseWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.statusCode = code
}

func (w *sseWriter) status() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.statusCode
}

// nextEvent blocks until one real SSE frame has arrived (comment frames
// like the ": connected" ready signal are skipped).
func (w *sseWriter) nextEvent() sseEvent {
	w.mu.Lock()
	defer w.mu.Unlock()
	for {
		for len(w.events) > 0 {
			ev := w.events[0]
			w.events = w.events[1:]
			if ev.kind != "" {
				return ev
			}
		}
		w.cond.Wait()
	}
}

// parseSSEFrame decodes one SSE frame (id: / event: / data: lines).
func parseSSEFrame(frame string) sseEvent {
	var ev sseEvent
	for line := range strings.SplitSeq(frame, "\n") {
		switch {
		case strings.HasPrefix(line, "id: "):
			_, _ = fmt.Sscanf(line[4:], "%d", &ev.id)
		case strings.HasPrefix(line, "event: "):
			ev.kind = line[7:]
		case strings.HasPrefix(line, "data: "):
			ev.data = line[6:]
		}
	}
	return ev
}

// indexBytes is bytes.Index over a []byte (the pending stream).
func indexBytes(b, sub []byte) int {
	for i := 0; i+len(sub) <= len(b); i++ {
		if string(b[i:i+len(sub)]) == string(sub) {
			return i
		}
	}
	return -1
}

// sseClient is one connected /events consumer (the handler runs in its
// own goroutine; close cancels the request context).
type sseClient struct {
	rw     *sseWriter
	cancel context.CancelFunc
}

// connectSSE opens an /events stream with the given headers (auth,
// Last-Event-ID, …).
func (h *harness) connectSSE(t *testing.T, header map[string]string) *sseClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(httpGet, "http://api.test/api/v1/events", nil).WithContext(ctx)
	q := req.URL.Query()
	for k, v := range header {
		if k == queryToken {
			q.Set(queryToken, v) // the EventSource fallback is a query param
		} else {
			req.Header.Set(k, v)
		}
	}
	req.URL.RawQuery = q.Encode()
	rw := newSSEWriter()
	go h.h.ServeHTTP(rw, req)
	rw.awaitReady() // the connect barrier: the subscription is registered
	return &sseClient{rw: rw, cancel: cancel}
}

func (c *sseClient) close() { c.cancel() }

func (c *sseClient) status() int { return c.rw.status() }

func (c *sseClient) next() sseEvent { return c.rw.nextEvent() }

// ---------- the load-bearing replay test ----------

// TestSSEReplay: 30 published events; a subscriber connects (Last-Event-
// ID 0 = from the start), is dropped after event 12, and reconnects
// with Last-Event-ID: 12 — it receives exactly 13..30, in order, no
// misses, no duplicates.
func TestSSEReplay(t *testing.T) {
	h := newHarness(t)
	for i := range 30 {
		h.bus.PublishSystem(SystemEvent{Type: SystemAlert, Detail: fmt.Sprintf("e%d", i+1)})
	}
	auth := map[string]string{headerAuth: "Bearer " + h.tokens[RoleReadOnly]}

	c := h.connectSSE(t, mergeHeader(auth, map[string]string{headerLastEventID: "0"}))
	defer c.close()
	for i := range 12 {
		ev := c.next()
		if ev.id != int64(i+1) || ev.kind != EventSystem || !contains(ev.data, fmt.Sprintf(`"e%d"`, i+1)) {
			t.Fatalf("replay %d: got id=%d kind=%q data=%s, want event %d", i, ev.id, ev.kind, ev.data, i+1)
		}
	}
	c.close() // dropped after event 12

	c = h.connectSSE(t, mergeHeader(auth, map[string]string{headerLastEventID: "12"}))
	defer c.close()
	for i := range 18 {
		ev := c.next()
		want := 13 + i
		if ev.id != int64(want) || !contains(ev.data, fmt.Sprintf(`"e%d"`, want)) {
			t.Fatalf("reconnect %d: got id=%d data=%s, want event %d", i, ev.id, ev.data, want)
		}
	}
}

// TestSSESnapshotsNotReplayed (owner, 2026-11-21): a snapshot only says
// what the values are now, so the replay ring keeps the discrete events
// (config, connection, system) and no snapshots. A connection ends its
// replay with the latest snapshot — a reconnect after an hour gets one,
// not ~700, and a fresh connect has the current values at once.
func TestSSESnapshotsNotReplayed(t *testing.T) {
	h := newHarness(t)
	snap := func(soc int) { h.bus.PublishSnapshot(SnapshotEvent{SocPct: soc}) }
	snap(41)
	h.bus.PublishSystem(SystemEvent{Type: SystemAlert, Detail: "e1"}) // 1
	snap(42)
	h.bus.PublishConfig(ConfigEvent{Changed: []string{SectionWorkMode}}) // 2
	snap(43)
	h.bus.PublishSystem(SystemEvent{Type: SystemAlert, Detail: "e2"}) // 3
	snap(44)
	auth := map[string]string{headerAuth: "Bearer " + h.tokens[RoleReadOnly]}

	c := h.connectSSE(t, mergeHeader(auth, map[string]string{headerLastEventID: "0"}))
	var got []string
	for range 4 {
		ev := c.next()
		got = append(got, fmt.Sprintf("%s/%d", ev.kind, ev.id))
		if ev.kind == EventSnapshot && !contains(ev.data, `"soc_pct":44`) {
			t.Fatalf("snapshot: got %s, want the latest (44)", ev.data)
		}
	}
	c.close()
	// the latest snapshot carries the newest event's id: the resume point
	if fmt.Sprint(got) != "[system/1 config/2 system/3 snapshot/3]" {
		t.Fatalf("replay: got %v, want the discrete events then the latest snapshot", got)
	}

	// a resume from a discrete event's id replays only what followed it
	c = h.connectSSE(t, mergeHeader(auth, map[string]string{headerLastEventID: "2"}))
	if ev := c.next(); ev.kind != EventSystem || ev.id != 3 {
		t.Fatalf("resume from 2: got %s/%d, want system/3", ev.kind, ev.id)
	}
	if ev := c.next(); ev.kind != EventSnapshot || ev.id != 3 {
		t.Fatalf("resume from 2: got %s/%d, want the latest snapshot (id 3)", ev.kind, ev.id)
	}
	c.close()

	// a fresh connect (no Last-Event-ID): the latest snapshot, then live
	c = h.connectSSE(t, auth)
	defer c.close()
	if ev := c.next(); ev.kind != EventSnapshot || !contains(ev.data, `"soc_pct":44`) {
		t.Fatalf("fresh connect: got %s %s, want the latest snapshot", ev.kind, ev.data)
	}
	h.bus.PublishSystem(SystemEvent{Type: SystemAlert, Detail: "live"})
	if ev := c.next(); ev.kind != EventSystem || ev.id != 4 {
		t.Fatalf("live: got %s/%d, want system/4", ev.kind, ev.id)
	}
}

// TestSSESnapshotCarriesLastID (owner, 2026-11-25): a snapshot carries,
// as its id, the last discrete event this client was sent (0 before
// any) — never one past it. A page that has only seen snapshots since it
// connected (the usual case between schedule changes) still resumes
// from its place: without an id, the SSE extension's background pause
// reconnected with no Last-Event-ID, and a window's start published
// while the tab was hidden was lost ("Running now" stayed stale).
func TestSSESnapshotCarriesLastID(t *testing.T) {
	h := newHarness(t)
	auth := map[string]string{headerAuth: "Bearer " + h.tokens[RoleReadOnly]}
	h.bus.PublishSnapshot(SnapshotEvent{SocPct: 41})
	c := h.connectSSE(t, auth)
	if ev := c.next(); ev.kind != EventSnapshot || ev.id != 0 {
		t.Fatalf("connect, no events yet: %s/%d, want snapshot/0", ev.kind, ev.id)
	}
	c.close()
	h.bus.PublishSystem(SystemEvent{Type: SystemAlert, Detail: "e1"})
	c = h.connectSSE(t, auth) // a fresh page: no Last-Event-ID
	defer c.close()
	if ev := c.next(); ev.kind != EventSnapshot || ev.id != 1 {
		t.Fatalf("connect after e1: %s/%d, want snapshot/1", ev.kind, ev.id)
	}
	h.bus.PublishSnapshot(SnapshotEvent{SocPct: 42})
	if ev := c.next(); ev.kind != EventSnapshot || ev.id != 1 {
		t.Fatalf("live snapshot: %s/%d, want snapshot/1", ev.kind, ev.id)
	}
	h.bus.PublishSystem(SystemEvent{Type: SystemAlert, Detail: "e2"})
	if ev := c.next(); ev.kind != EventSystem || ev.id != 2 {
		t.Fatalf("live e2: %s/%d, want system/2", ev.kind, ev.id)
	}
	h.bus.PublishSnapshot(SnapshotEvent{SocPct: 43})
	if ev := c.next(); ev.id != 2 {
		t.Fatalf("snapshot after e2: id %d, want 2", ev.id)
	}
}

// TestSSEResumeAfterSnapshotsOnly: a page that saw only snapshots,
// paused while an event was published, resumes from the snapshot's id
// and is sent the event it missed.
func TestSSEResumeAfterSnapshotsOnly(t *testing.T) {
	h := newHarness(t)
	auth := map[string]string{headerAuth: "Bearer " + h.tokens[RoleReadOnly]}
	h.bus.PublishSystem(SystemEvent{Type: SystemAlert, Detail: "e1"})
	h.bus.PublishSnapshot(SnapshotEvent{SocPct: 41})
	c := h.connectSSE(t, auth)
	last := c.next().id
	c.close() // the tab goes to the background
	h.bus.PublishConfig(ConfigEvent{Changed: []string{SectionActiveMode}})
	c = h.connectSSE(t, mergeHeader(auth, map[string]string{headerLastEventID: fmt.Sprint(last)}))
	defer c.close()
	if ev := c.next(); ev.kind != EventConfig {
		t.Fatalf("resumed from %d: got %s, want the missed config event", last, ev.kind)
	}
}

// TestSSEGapIgnoresSnapshots: a slow client that misses snapshots has
// lost nothing (the next one supersedes them): no gap signal. Missed
// discrete events still raise one (TestSSEGap).
func TestSSEGapIgnoresSnapshots(t *testing.T) {
	h := newHarness(t)
	sub := h.bus.Subscribe()
	defer sub.Close()
	for i := range 3 * subQueueCap {
		h.bus.PublishSnapshot(SnapshotEvent{SocPct: i % 100})
	}
	for range subQueueCap {
		<-sub.Events()
	}
	h.bus.PublishSystem(SystemEvent{Type: SystemAlert, Detail: "after"})
	if ev := <-sub.Events(); ev.Kind != EventSystem {
		t.Fatalf("after dropped snapshots: got %s %+v, want the system event (no gap)", ev.Kind, ev.Data)
	}
}

// TestSSEResync: a Last-Event-ID older than the ring's oldest entry
// (1024 entries) gets the spec's resync signal instead of a replay.
func TestSSEResync(t *testing.T) {
	h := newHarness(t)
	for i := range 1050 {
		h.bus.PublishSystem(SystemEvent{Type: SystemAlert, Detail: fmt.Sprintf("e%d", i+1)})
	}
	// The ring (1024) holds ids 27..1050; 5 is older than the oldest.
	c := h.connectSSE(t, mergeHeader(map[string]string{
		headerAuth: "Bearer " + h.tokens[RoleReadOnly],
	}, map[string]string{headerLastEventID: "5"}))
	defer c.close()
	ev := c.next()
	if ev.kind != "resync" {
		t.Fatalf("resync: got kind %q (id=%d), want resync", ev.kind, ev.id)
	}
	if ev.id != 1050 {
		t.Fatalf("resync: got id=%d, want the ring's last id 1050", ev.id)
	}
}

// TestSSEResyncFreshServer: a Last-Event-ID against an empty ring (a
// restart) also gets the resync signal.
func TestSSEResyncFreshServer(t *testing.T) {
	h := newHarness(t)
	c := h.connectSSE(t, mergeHeader(map[string]string{
		headerAuth: "Bearer " + h.tokens[RoleReadOnly],
	}, map[string]string{headerLastEventID: "42"}))
	defer c.close()
	ev := c.next()
	if ev.kind != "resync" || ev.id != 0 {
		t.Fatalf("resync: got kind=%q id=%d, want resync id 0", ev.kind, ev.id)
	}
}

// TestSSELive: a fresh connection (no Last-Event-ID) is live-only; a
// publish after the connect arrives, with the id of the newest event
// before it (its resume point).
func TestSSELive(t *testing.T) {
	h := newHarness(t)
	h.bus.PublishSystem(SystemEvent{Type: SystemAlert, Detail: "before"})
	c := h.connectSSE(t, map[string]string{headerAuth: "Bearer " + h.tokens[RoleReadOnly]})
	defer c.close()
	h.bus.PublishSnapshot(SnapshotEvent{TS: testBase, SocPct: 42, BatteryState: "idle"})
	ev := c.next()
	if ev.kind != "snapshot" || ev.id != 1 || !contains(ev.data, `"soc_pct":42`) {
		t.Fatalf("live: got kind=%q id=%d data=%s, want a snapshot with id 1", ev.kind, ev.id, ev.data)
	}
	// behind nginx (the TLS route): the stream must not be buffered
	if got := c.rw.header.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering %q, want no", got)
	}
}

// TestSSEGap: a client that does not drain overflows its queue and
// receives the spec's gap signal with the missed count (the pending gap
// flushes on the next publish once the queue has room).
func TestSSEGap(t *testing.T) {
	h := newHarness(t)
	sub := h.bus.Subscribe()
	defer sub.Close()
	for i := range 100 {
		h.bus.PublishSystem(SystemEvent{Type: SystemAlert, Detail: fmt.Sprintf("g%d", i)})
	}
	var sawGap bool
	for i := range 200 {
		ev := <-sub.Events()
		if ev.Kind == "gap" {
			sawGap = true
			gap, ok := ev.Data.(GapEvent)
			if !ok || gap.Missed <= 0 {
				t.Fatalf("gap: got %T %+v, want a missed count", ev.Data, ev.Data)
			}
			break
		}
		// Consume while publishing: the queue drains, the next publish
		// flushes the pending gap before its event.
		if i%10 == 0 {
			h.bus.PublishSystem(SystemEvent{Type: SystemAlert, Detail: "flush"})
		}
	}
	if !sawGap {
		t.Fatalf("gap: no gap signal after 300 publishes against a slow client")
	}
}

// TestSSEAuthQueryToken: ?token= authenticates the stream (the
// EventSource path) — and a bad token is 401.
func TestSSEAuthQueryToken(t *testing.T) {
	h := newHarness(t)
	// The bad-token 401 below: with the flag on, a bad token on a
	// read-only endpoint is admitted as the public session (200).
	h.srv.info.PublicReadOnly = false
	c := h.connectSSE(t, map[string]string{queryToken: h.tokens[RoleReadOnly]})
	defer c.close()
	h.bus.PublishSystem(SystemEvent{Type: SystemAlert, Detail: "via-token"})
	ev := c.next()
	if ev.kind != EventSystem || !contains(ev.data, "via-token") {
		t.Fatalf("?token= stream: got kind=%q data=%s", ev.kind, ev.data)
	}

	c2 := h.connectSSE(t, map[string]string{queryToken: "fl_bad"})
	c2.close()
	if c2.status() != http.StatusUnauthorized {
		t.Fatalf("?token= bad: got status %d, want 401", c2.status())
	}
}

// mergeHeader merges two header maps (the second wins).
func mergeHeader(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	maps.Copy(out, a)
	maps.Copy(out, b)
	return out
}

// TestBusSubscriberCap (audit S6): with a cap set, connects past it are
// refused until a subscriber leaves; the stream answers 503.
func TestBusSubscriberCap(t *testing.T) {
	b := NewBus()
	b.SetMaxSubscribers(2)
	s1, _, _, _ := b.Connect(0, false)
	s2, _, _, _ := b.Connect(0, false)
	if s1 == nil || s2 == nil {
		t.Fatal("connects under the cap refused")
	}
	if s3, _, _, _ := b.Connect(0, false); s3 != nil {
		t.Fatal("a third subscriber past the cap of 2")
	}
	s1.Close()
	if s4, _, _, _ := b.Connect(0, false); s4 == nil {
		t.Fatal("refused after a subscriber left")
	}

	h := newHarness(t)
	h.bus.SetMaxSubscribers(0)
	h.srv.bus.SetMaxSubscribers(1)
	held, _, _, _ := h.srv.bus.Connect(0, false)
	defer held.Close()
	rr := h.do(t, httpGet, "/api/v1/events", RoleReadOnly, nil)
	checkProblem(t, rr, http.StatusServiceUnavailable, "too_many_streams")
	if rr.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After on the refused stream")
	}
}
