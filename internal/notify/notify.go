// Package notify sends outbound notifications (F2, audit U2): the
// owner hears about a fault without the web UI open. Two targets, each
// optional: a generic JSON webhook (what a Home Assistant webhook
// trigger reads) and an ntfy topic URL (a phone push).
//
// Notices go through an outbox (owner, 2026-11-18): each is stored with
// the targets still to reach, and one background worker delivers what is
// due, retrying with backoff. In the app the outbox is the database, so a
// notice survives a restart: after a power cut, foxess-local comes back
// long before the network and the internet do, and this is when the
// owner most needs to hear about it. A notice delivered late says when it
// happened. A dedup window keeps a flapping fault from spamming.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	neturl "net/url"
	"strconv"
	"sync"
	"time"
)

// Severity orders notices (ntfy priority, the webhook's severity).
type Severity string

// The severities.
const (
	Info     Severity = "info"
	Warning  Severity = "warning"
	Critical Severity = "critical"
)

// Notice is one notification.
type Notice struct {
	Event    string // stable machine name: inverter-offline, gate-rule-loss, alert, …
	Severity Severity
	Title    string
	Message  string
}

// Targets are where notices go ("" = not configured). Read at every
// send, so a change in the UI applies to the next delivery.
type Targets struct {
	WebhookURL string
	NtfyURL    string
}

// Transport delivers one request body to url (an error for a non-2xx
// answer too).
type Transport func(ctx context.Context, url string, header http.Header, body []byte) error

// The targets as bits of Pending.Targets.
const (
	targetWebhook = 1 << iota
	targetNtfy
)

// Pending is one notice in the outbox.
type Pending struct {
	ID       int64
	Notice   Notice
	Created  time.Time // when it happened (the webhook's ts)
	Targets  int       // the targets still to reach (bits)
	Attempts int
	Next     time.Time // when it is due
}

// Outbox keeps the pending notices (the database in the app; MemOutbox
// otherwise).
type Outbox interface {
	Add(p Pending) (int64, error)
	Due(now time.Time) ([]Pending, error)
	Update(p Pending) error
	Delete(id int64) error
}

const (
	// DedupWindow suppresses a repeat of the same notice (event and
	// message).
	DedupWindow = 15 * time.Minute
	// maxAttempts bounds one notice's deliveries: about a day of retries
	// at the backoff's cap. Counted, not timed — a board without a
	// battery-backed clock can jump years when NTP syncs after a power
	// cut, and an age limit would then drop everything.
	maxAttempts = 100
	// firstRetry and maxRetry bound the backoff (doubling).
	firstRetry = 30 * time.Second
	maxRetry   = 15 * time.Minute
	// lateAfter: a notice delivered later than this says when it
	// happened.
	lateAfter = 2 * time.Minute
	// pollEvery is how often the worker looks for due notices (a new
	// notice wakes it at once).
	pollEvery = 10 * time.Second
	// sendTimeout bounds one delivery.
	sendTimeout = 10 * time.Second
	// source names the sender in the webhook body.
	source = "foxess-local"
)

// ntfyPriority and ntfyTags per severity (ntfy's 1–5 scale; tags are
// emoji shortcodes).
const (
	ntfyDefault = 3
	ntfyHigh    = 4
	ntfyMax     = 5
)

var (
	ntfyPriority = map[Severity]int{Info: ntfyDefault, Warning: ntfyHigh, Critical: ntfyMax}
	ntfyTags     = map[Severity]string{Info: "information_source", Warning: "warning", Critical: "rotating_light"}
)

// Notifier queues and delivers notices.
type Notifier struct {
	targets func() Targets
	clock   func() time.Time
	send    Transport
	outbox  Outbox

	mu   sync.Mutex
	wake chan struct{}        // made on first use (wakeup)
	sent map[string]time.Time // dedup key → last accepted
}

// Option configures a Notifier.
type Option func(*Notifier)

// WithTransport replaces the HTTP transport (tests).
func WithTransport(t Transport) Option { return func(n *Notifier) { n.send = t } }

// WithOutbox keeps the pending notices in o (the database, so they
// survive a restart); the default is in memory.
func WithOutbox(o Outbox) Option { return func(n *Notifier) { n.outbox = o } }

// New builds a notifier; Run delivers.
func New(targets func() Targets, clock func() time.Time, opts ...Option) *Notifier {
	n := &Notifier{
		targets: targets, clock: clock, send: httpTransport(&http.Client{Timeout: sendTimeout}),
		outbox: &MemOutbox{}, sent: map[string]time.Time{},
	}
	for _, o := range opts {
		o(n)
	}
	return n
}

// wakeup returns the worker's wake channel, making it on first use: a
// goroutine waiting on a channel made outside its synctest bubble never
// lets the bubble's virtual time advance (the e2e tests build the app
// before the bubble starts).
func (n *Notifier) wakeup() chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.wake == nil {
		n.wake = make(chan struct{}, 1)
	}
	return n.wake
}

// Notify stores a notice for delivery (never blocks on the network):
// dropped when nothing is configured, or when the same notice went out
// within DedupWindow.
func (n *Notifier) Notify(nt Notice) {
	mask := n.targetMask()
	if mask == 0 {
		return
	}
	now := n.clock()
	if !n.fresh(nt, now) {
		return
	}
	if _, err := n.outbox.Add(Pending{Notice: nt, Created: now, Targets: mask, Next: now}); err != nil {
		slog.Error("notify: notice not stored", "event", nt.Event, "err", err)
		return
	}
	select {
	case n.wakeup() <- struct{}{}:
	default:
	}
}

// fresh records a notice for dedup and reports whether it is news.
func (n *Notifier) fresh(nt Notice, now time.Time) bool {
	key := nt.Event + "\x00" + nt.Message
	n.mu.Lock()
	defer n.mu.Unlock()
	if last, ok := n.sent[key]; ok && now.Sub(last) < DedupWindow {
		return false
	}
	n.sent[key] = now
	for k, at := range n.sent { // bounded: expired keys go
		if now.Sub(at) >= DedupWindow {
			delete(n.sent, k)
		}
	}
	return true
}

// targetMask is the configured targets as bits.
func (n *Notifier) targetMask() int {
	t := n.targets()
	mask := 0
	if t.WebhookURL != "" {
		mask |= targetWebhook
	}
	if t.NtfyURL != "" {
		mask |= targetNtfy
	}
	return mask
}

// Run delivers due notices until ctx is done: at start (what a previous
// run left), when a notice arrives, and every pollEvery.
func (n *Notifier) Run(ctx context.Context) {
	wake := n.wakeup()
	for {
		n.deliverDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-time.After(pollEvery):
		}
	}
}

// deliverDue attempts every due notice once.
func (n *Notifier) deliverDue(ctx context.Context) {
	now := n.clock()
	due, err := n.outbox.Due(now)
	if err != nil {
		slog.Error("notify: outbox", "err", err)
		return
	}
	for _, p := range due {
		if ctx.Err() != nil {
			return
		}
		n.attempt(ctx, p, now)
	}
}

// attempt delivers one notice to the targets it still needs and records
// the outcome: done, retried later, or abandoned.
func (n *Notifier) attempt(ctx context.Context, p Pending, now time.Time) {
	p.Targets &= n.targetMask() // a target turned off since: not retried
	for _, r := range n.requests(n.delivered(p, now), p.Created, p.Targets) {
		if err := n.post(ctx, r); err != nil {
			slog.Warn("notify: delivery failed", "target", r.kind, "event", p.Notice.Event, "attempt", p.Attempts+1, "err", err)
			continue
		}
		p.Targets &^= r.bit
	}
	if p.Targets == 0 {
		n.done(p.ID)
		return
	}
	p.Attempts++
	if p.Attempts >= maxAttempts {
		slog.Error("notify: giving up on a notice", "event", p.Notice.Event, "attempts", p.Attempts)
		n.done(p.ID)
		return
	}
	p.Next = now.Add(backoff(p.Attempts))
	if err := n.outbox.Update(p); err != nil {
		slog.Error("notify: outbox", "err", err)
	}
}

func (n *Notifier) done(id int64) {
	if err := n.outbox.Delete(id); err != nil {
		slog.Error("notify: outbox", "err", err)
	}
}

// delivered is the notice as sent now: one delivered late says when it
// happened.
func (n *Notifier) delivered(p Pending, now time.Time) Notice {
	nt := p.Notice
	if now.Sub(p.Created) > lateAfter {
		nt.Message += fmt.Sprintf(" (This happened at %s; delivered late.)", p.Created.Local().Format("Mon 2 Jan 15:04"))
	}
	return nt
}

// backoff is the wait after the n-th failed attempt: firstRetry doubling,
// capped at maxRetry.
func backoff(attempts int) time.Duration {
	d := firstRetry
	for range attempts - 1 {
		d *= 2
		if d >= maxRetry {
			return maxRetry
		}
	}
	return d
}

// Result is one target's test outcome.
type Result struct {
	Target string `json:"target"` // webhook | ntfy
	Err    string `json:"error,omitempty"`
}

// Test sends a test notice to every configured target now (no outbox,
// no dedup, no retry) and reports each outcome.
func (n *Notifier) Test(ctx context.Context) []Result {
	nt := Notice{
		Event: "test", Severity: Info, Title: "foxess-local test",
		Message: "Notifications from foxess-local reach you here.",
	}
	out := []Result{}
	for _, r := range n.requests(nt, n.clock(), n.targetMask()) {
		res := Result{Target: r.kind}
		if err := n.post(ctx, r); err != nil {
			res.Err = err.Error()
		}
		out = append(out, res)
	}
	return out
}

// request is one target's delivery.
type request struct {
	kind   string
	bit    int
	url    string
	header http.Header
	body   []byte
}

// requests are the deliveries of nt to the targets in mask.
func (n *Notifier) requests(nt Notice, ts time.Time, mask int) []request {
	t := n.targets()
	var out []request
	if t.WebhookURL != "" && mask&targetWebhook != 0 {
		body, _ := json.Marshal(map[string]string{ //nolint:errchkjson // a map of strings always marshals
			"source": source, "event": nt.Event, "severity": string(nt.Severity),
			"title": nt.Title, "message": nt.Message, "ts": ts.UTC().Format(time.RFC3339),
		})
		out = append(out, request{
			kind: "webhook", bit: targetWebhook, url: t.WebhookURL,
			header: http.Header{"Content-Type": {"application/json"}}, body: body,
		})
	}
	if t.NtfyURL != "" && mask&targetNtfy != 0 {
		h := http.Header{
			"Content-Type": {"text/plain; charset=utf-8"},
			"Title":        {nt.Title},
			"Priority":     {strconv.Itoa(ntfyPriority[nt.Severity])},
			"Tags":         {ntfyTags[nt.Severity]},
		}
		out = append(out, request{kind: "ntfy", bit: targetNtfy, url: t.NtfyURL, header: h, body: []byte(nt.Message)})
	}
	return out
}

func (n *Notifier) post(ctx context.Context, r request) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	return n.send(ctx, r.url, r.header, r.body)
}

// httpTransport POSTs with client; a non-2xx answer is an error.
func httpTransport(client *http.Client) Transport {
	return func(ctx context.Context, url string, header http.Header, body []byte) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header = header.Clone()
		resp, err := client.Do(req)
		if err != nil {
			// the URL carries the secret (an ntfy topic, a webhook id):
			// report the cause without it
			if ue, ok := errors.AsType[*neturl.Error](err); ok {
				return ue.Err
			}
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return nil
	}
}

// MemOutbox is an in-memory Outbox (tests, devui): lost at exit.
type MemOutbox struct {
	mu     sync.Mutex
	nextID int64
	items  []Pending
}

// Add stores p.
func (m *MemOutbox) Add(p Pending) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	p.ID = m.nextID
	m.items = append(m.items, p)
	return p.ID, nil
}

// Due lists the notices due at now, oldest first.
func (m *MemOutbox) Due(now time.Time) ([]Pending, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Pending
	for _, p := range m.items {
		if !p.Next.After(now) {
			out = append(out, p)
		}
	}
	return out, nil
}

// Update replaces p.
func (m *MemOutbox) Update(p Pending) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.items {
		if m.items[i].ID == p.ID {
			m.items[i] = p
		}
	}
	return nil
}

// Delete removes the notice id.
func (m *MemOutbox) Delete(id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.items {
		if m.items[i].ID == id {
			m.items = append(m.items[:i], m.items[i+1:]...)
			return nil
		}
	}
	return nil
}

// Len is the number of pending notices.
func (m *MemOutbox) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.items)
}
