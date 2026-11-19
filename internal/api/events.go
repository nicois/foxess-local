// Package api is the self-owned REST API (C-005): the /api/v1 surface
// of api/openapi.yaml plus the SSE event stream. Auth is the named
// bearer token (roles read-only < standard < expert < installer);
// errors are RFC 7807 problem+json with a stable code; writes are
// synchronous, read-back-confirmed through the gateway (C-003).
package api

import (
	"sync"
	"time"
)

// Event kinds (the spec GET /events x-event-map; the two synthetic
// kinds resync/gap are the spec's stream-integrity signals).
const (
	EventSnapshot   = "snapshot"
	EventConfig     = "config"
	EventConnection = "connection"
	EventSystem     = "system"
	EventResync     = "resync"
	EventGap        = "gap"
)

// System event types (the spec SystemEvent.type vocabulary). The
// four maintenance types are the same strings internal/forward
// re-declares (the spec is the single source of truth).
const (
	SystemForwardModeChanged  = "forward-mode-changed"
	SystemMaintenanceOpened   = "maintenance-opened"
	SystemMaintenanceExtended = "maintenance-extended"
	SystemMaintenanceExpired  = "maintenance-expired"
	SystemMaintenanceClosed   = "maintenance-closed"
	SystemAlert               = "alert"
	// SystemGateRuleLoss: the gate check found the inverter's filtering
	// gone (and re-applied it) — from the check's status file (F2).
	SystemGateRuleLoss = "gate-rule-loss"
	// SystemUpgradeRolledBack: the upgrade guard restored the previous
	// binary (F2).
	SystemUpgradeRolledBack = "upgrade-rolled-back"
	// SystemGridLost / SystemGridRestored: the inverter left / returned
	// to the grid (its state register: off grid / EPS ↔ on grid).
	SystemGridLost     = "grid-lost"
	SystemGridRestored = "grid-restored"
	// SystemUncleanRestart: foxess-local started after an unexpected stop
	// (no clean-shutdown marker): a power cut, or a crash.
	SystemUncleanRestart = "unclean-restart"
	// SystemBatteryLow / SystemBatteryForecastLow: the battery alerts
	// (internal/socalert): at or below the alert level, or forecast to be
	// there within the horizon.
	SystemBatteryLow         = "battery-low"
	SystemBatteryForecastLow = "battery-forecast-low"
	// SystemFirmwareTask: the cloud asked the inverter to update a
	// firmware component (blocked outside a maintenance window).
	SystemFirmwareTask = "firmware-task"
)

// Audit event kinds (the store audit_events rows the API writes; the
// frame decisions forwarded/dropped are written by the framesvc
// wiring to the frames table).
const (
	auditKindCommand     = "command"
	auditKindMaintenance = "maintenance"
	auditActionAllow     = "allow"
)

// ringCap is the SSE replay window (spec: 1024 ≈ 85 min at the
// snapshot cadence).
const ringCap = 1024

// subQueueCap is one client's delivery queue (a slow client that
// overflows it receives the spec's gap signal).
const subQueueCap = 64

// Event is one bus event: a monotonic id, a kind (the SSE event:
// line), and a payload (the data: line, JSON-encoded per kind).
type Event struct {
	ID   int64
	Kind string
	Data any
}

// SnapshotEvent is one 150 B telemetry frame (~5 s): the immediate
// sensor feed (spec).
type SnapshotEvent struct {
	TS     time.Time `json:"ts"`
	Power  Power     `json:"power"`
	SocPct int       `json:"soc_pct"`
	// SocEstimatePct is the SoC estimate (D-028; Battery.SocEstimatePct);
	// null without the battery capacity.
	SocEstimatePct *float64 `json:"soc_estimate_pct"`
	BatteryState   string   `json:"battery_state"`
	// Energy is the lifetime counters, in every snapshot once read (small;
	// a subscriber need not poll /state for them).
	Energy *Energy `json:"energy,omitempty"`
}

// ConfigEvent is one confirmed register change (ours or a cloud push);
// State is the full new snapshot — the client diffs on Changed (spec).
type ConfigEvent struct {
	Changed []string `json:"changed"`
	State   State    `json:"state"`
}

// ConnectionEvent is the inverter session state (online/offline,
// spec).
type ConnectionEvent struct {
	State string `json:"state"`
}

// SystemEvent is the operational signal channel (spec): the type names
// the signal, the detail is the human-readable text.
type SystemEvent struct {
	Type   string `json:"type"`
	Detail string `json:"detail"`
}

// ResyncEvent signals an exceeded replay window: do GET /state and
// resume live (spec).
type ResyncEvent struct{}

// GapEvent signals per-client drops: do GET /state (spec).
type GapEvent struct {
	Missed int `json:"missed"`
}

// subscriber is one live client: a bounded delivery queue plus the gap
// accounting (lastID = the last event delivered; missed = drops since
// the last delivered gap).
type subscriber struct {
	id     int64
	ch     chan Event
	lastID int64
	missed int
}

// Bus is the SSE feed: a bounded replay ring plus live fan-out.
type Bus struct {
	mu     sync.Mutex
	ring   []Event
	nextID int64
	subs   map[int64]*subscriber
	subSeq int64
	max    int // subscriber cap (0 = none): SetMaxSubscribers
}

// SetMaxSubscribers caps concurrent subscribers (0 = no cap): a LAN
// device must not hold unbounded streams open (audit S6).
func (b *Bus) SetMaxSubscribers(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.max = n
}

// NewBus creates an empty event bus.
func NewBus() *Bus {
	return &Bus{subs: make(map[int64]*subscriber)}
}

// PublishSnapshot publishes one live-value snapshot.
func (b *Bus) PublishSnapshot(e SnapshotEvent) Event { return b.publish(EventSnapshot, e) }

// PublishConfig publishes a confirmed configuration change.
func (b *Bus) PublishConfig(e ConfigEvent) Event { return b.publish(EventConfig, e) }

// PublishConnection publishes an inverter session state change.
func (b *Bus) PublishConnection(e ConnectionEvent) Event { return b.publish(EventConnection, e) }

// PublishSystem publishes an operational signal (an alert, a maintenance
// or forward-mode change, …).
func (b *Bus) PublishSystem(e SystemEvent) Event { return b.publish(EventSystem, e) }

// publish appends one event to the ring and fans it out to the
// subscribers (non-blocking: an over-full client's queue drops,
// surfaced as the gap signal). The typed Publish* methods are the only
// way in, so a kind always carries its own payload type (the SSE
// stream and the notifier both rely on it).
func (b *Bus) publish(kind string, data any) Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	ev := Event{ID: b.nextID, Kind: kind, Data: data}
	if len(b.ring) == ringCap {
		b.ring = append(b.ring[1:], ev)
	} else {
		b.ring = append(b.ring, ev)
	}
	for _, s := range b.subs {
		b.deliverLocked(s, ev)
	}
	return ev
}

// deliverLocked fans one event out to one subscriber, flushing a
// pending gap first (the gap carries the running missed count, id =
// the client's last confirmed baseline so a reconnect replays exactly
// the rest).
func (b *Bus) deliverLocked(s *subscriber, ev Event) {
	if s.missed > 0 {
		select {
		case s.ch <- gapEvent(s):
			s.missed = 0
		default:
		}
	}
	select {
	case s.ch <- ev:
		s.lastID = ev.ID
	default:
		s.missed++
	}
}

// gapEvent snapshots the subscriber's pending gap.
func gapEvent(s *subscriber) Event {
	return Event{ID: s.lastID, Kind: EventGap, Data: GapEvent{Missed: s.missed}}
}

// Subscription is one connected client of the bus.
type Subscription struct {
	b   *Bus
	sub *subscriber
}

// Events returns the live event feed (a gap signal arrives on drops).
func (s *Subscription) Events() <-chan Event {
	return s.sub.ch
}

// Close detaches the subscription (the per-connection cleanup).
func (s *Subscription) Close() {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	delete(s.b.subs, s.sub.id)
}

// Subscribe opens a live feed without a replay (task 10's wiring and
// the tests use it directly).
func (b *Bus) Subscribe() *Subscription {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.newSubscriberLocked()
	return &Subscription{b: b, sub: s}
}

// Connect atomically registers a subscription — nil when the subscriber
// cap is reached (SetMaxSubscribers) — and captures the replay
// window (a publish interleaved between the two would be missed or
// duplicated): wantReplay returns the events with id > replayFrom, or
// the resync signal when the window is exceeded (replayFrom older than
// the ring's oldest, or a non-zero id against an empty ring).
func (b *Bus) Connect(replayFrom int64, wantReplay bool) (*Subscription, []Event, bool, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.max > 0 && len(b.subs) >= b.max {
		return nil, nil, false, 0 // full: SetMaxSubscribers
	}
	sub := b.newSubscriberLocked()
	var replay []Event
	var resync bool
	var resyncID int64
	if wantReplay {
		replay, resync, resyncID = b.replayWindowLocked(replayFrom)
	}
	return &Subscription{b: b, sub: sub}, replay, resync, resyncID
}

// replayWindowLocked computes the replay or resync for one connecting
// client (the caller holds the bus lock): the events with id >
// replayFrom, or the resync signal when the window is exceeded
// (replayFrom older than the ring's oldest, or a non-zero id against
// an empty ring).
func (b *Bus) replayWindowLocked(replayFrom int64) ([]Event, bool, int64) {
	var replay []Event
	if len(b.ring) == 0 {
		if replayFrom > 0 {
			return nil, true, 0
		}
		return replay, false, 0
	}
	if replayFrom > 0 && replayFrom < b.ring[0].ID {
		return nil, true, b.ring[len(b.ring)-1].ID
	}
	for i := range b.ring {
		if b.ring[i].ID > replayFrom {
			replay = append(replay, b.ring[i])
		}
	}
	return replay, false, 0
}

// newSubscriberLocked appends one subscriber (the caller holds the
// bus lock).
func (b *Bus) newSubscriberLocked() *subscriber {
	b.subSeq++
	s := &subscriber{id: b.subSeq, ch: make(chan Event, subQueueCap)}
	b.subs[s.id] = s
	return s
}
