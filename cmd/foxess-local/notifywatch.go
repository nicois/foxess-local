package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"foxesslocal/internal/api"
	"foxesslocal/internal/frame"
	"foxesslocal/internal/framesvc"
	"foxesslocal/internal/notify"
	"foxesslocal/internal/store"
	"foxesslocal/internal/update"
)

// offlineAfter is how long the inverter must be offline before it is
// notified (a session reconnect is a few seconds; a forward-mode change
// or a restart well under a minute).
const offlineAfter = 10 * time.Minute

// The notice event names for the connection state.
const (
	noticeOffline = "inverter-offline"
	noticeOnline  = "inverter-online"
)

// systemNotices maps the system events that are notified to their
// severity and title (any other type is not). Every one is at least a
// warning (ntfy priority high): the owner asked for these to stand
// out; only "back online" and the test notice are info (ntfy default).
var systemNotices = map[string]struct {
	sev   notify.Severity
	title string
}{
	api.SystemAlert:              {notify.Warning, "foxess-local alert"},
	api.SystemGridLost:           {notify.Critical, "Grid down: the inverter is on backup"},
	api.SystemGridRestored:       {notify.Info, "Grid back"},
	api.SystemUncleanRestart:     {notify.Warning, "foxess-local restarted after an unexpected stop"},
	api.SystemBatteryLow:         {notify.Warning, "Battery low"},
	api.SystemBatteryForecastLow: {notify.Warning, "Battery running low"},
	api.SystemGateRuleLoss:       {notify.Critical, "Gate rule loss: the inverter was unfiltered"},
	api.SystemUpgradeRolledBack:  {notify.Critical, "Upgrade rolled back"},
	api.SystemMaintenanceOpened:  {notify.Warning, "Maintenance window opened: the cloud may write to the inverter"},
	api.SystemMaintenanceClosed:  {notify.Warning, "Maintenance window closed"},
	api.SystemMaintenanceExpired: {notify.Warning, "Maintenance window expired"},
	api.SystemForwardModeChanged: {notify.Warning, "Cloud forwarding changed"},
	api.SystemFirmwareTask:       {notify.Warning, "The cloud requested a firmware update"},
}

// firmwareTaskEvent describes a cloud firmware upgrade task: the
// component and file, and what happened to it. The download URL stays
// out (notices leave the LAN): it is on Activity → Raw messages.
func firmwareTaskEvent(ev framesvc.Event) api.SystemEvent {
	t := ev.FwTask
	detail := fmt.Sprintf("The cloud asked to update the inverter's %s firmware (%s, %d bytes).",
		frame.FwMCUName(t.MCU), t.Filename, t.Size)
	if ev.Decision == store.DecisionForwarded {
		detail += " Passed (maintenance window): the inverter may download and flash it, then restart."
	} else {
		detail += " Blocked: firmware passes only in a maintenance window. The file's address is under Activity → Raw messages (expert)."
	}
	return api.SystemEvent{Type: api.SystemFirmwareTask, Detail: detail}
}

// watchNotices turns bus events into notices until ctx is done (F2):
// the system events above, and the inverter offline for offlineAfter
// (then back online).
func watchNotices(ctx context.Context, events <-chan api.Event, notice func(notify.Notice)) {
	var c connWatch
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.timer:
			notice(c.fired())
		case ev, ok := <-events:
			if !ok {
				return
			}
			if n, ok := c.noticeFor(ev); ok {
				notice(n)
			}
		}
	}
}

// noticeFor is the notice an event makes now, if any.
func (c *connWatch) noticeFor(ev api.Event) (notify.Notice, bool) {
	switch d := ev.Data.(type) {
	case api.ConnectionEvent:
		return c.change(d.State)
	case api.SystemEvent:
		if n, ok := systemNotices[d.Type]; ok {
			return notify.Notice{Event: d.Type, Severity: n.sev, Title: n.title, Message: d.Detail}, true
		}
	}
	return notify.Notice{}, false
}

// connWatch debounces the connection state: offline is news only after
// offlineAfter, and back online only after an offline notice.
type connWatch struct {
	timer     <-chan time.Time // armed while offline and not yet notified
	since     time.Time        // when it went offline
	announced bool             // an offline notice went out
}

// change applies a connection event; ok when it is a notice now.
func (c *connWatch) change(state string) (notify.Notice, bool) {
	switch state {
	case "offline":
		if c.timer == nil && !c.announced {
			c.since, c.timer = time.Now(), time.After(offlineAfter)
		}
	case "online":
		was := c.announced
		c.timer, c.announced = nil, false
		if was {
			return notify.Notice{
				Event: noticeOnline, Severity: notify.Info, Title: "Inverter back online",
				Message: fmt.Sprintf("The inverter is back online after %s.", time.Since(c.since).Round(time.Minute)),
			}, true
		}
	}
	return notify.Notice{}, false
}

// fired is the offline notice when the timer runs out.
func (c *connWatch) fired() notify.Notice {
	c.timer, c.announced = nil, true
	return notify.Notice{
		Event: noticeOffline, Severity: notify.Critical, Title: "Inverter offline",
		Message: fmt.Sprintf("No readings from the inverter for %s.", offlineAfter),
	}
}

// rollbackAnnounced is the store key naming the last rollback announced
// (a restart must not repeat it while the guard's marker stays).
const rollbackAnnounced = "notify.rollback_announced"

// rollbackNotice is the system event for a rolled-back upgrade, and the
// value that marks it announced; ok is false without one, or when last
// already names it.
func rollbackNotice(failed *update.Marker, last string) (api.SystemEvent, string, bool) {
	if failed == nil {
		return api.SystemEvent{}, "", false
	}
	key := failed.From + " → " + failed.To
	if key == last {
		return api.SystemEvent{}, "", false
	}
	return api.SystemEvent{
		Type:   api.SystemUpgradeRolledBack,
		Detail: fmt.Sprintf("the upgrade to %s did not come up, so %s was restored", failed.To, failed.From),
	}, key, true
}

// notifyTargets are the live targets (a UI value wins over the file).
func (h *handle) notifyTargets() notify.Targets {
	switch {
	case h.set != nil:
		v := h.set.Get()
		return notify.Targets{WebhookURL: v.NotifyWebhook, NtfyURL: v.NotifyNtfy}
	case h.cfg != nil:
		return notify.Targets{WebhookURL: h.cfg.Notify.WebhookURL, NtfyURL: h.cfg.Notify.NtfyURL}
	}
	return notify.Targets{}
}

// startNotify runs the notifier, its bus watcher and the gate-status
// poller, and announces a rolled-back upgrade (F2).
func (h *handle) startNotify(ctx context.Context) {
	if h.notifier == nil {
		return
	}
	go h.notifier.Run(ctx)
	sub := h.bus.Subscribe() // before the announcement below
	go func() {
		defer sub.Close()
		watchNotices(ctx, sub.Events(), h.notifier.Notify)
	}()
	go watchGateStatus(ctx, gateStatusPath, func(e api.SystemEvent) { h.bus.PublishSystem(e) })
	h.announceRestart()
	if h.upd == nil {
		return
	}
	last, _, err := h.st.Setting(rollbackAnnounced)
	if err != nil {
		return
	}
	if ev, key, ok := rollbackNotice(h.upd.View().Failed, last); ok {
		h.bus.PublishSystem(ev)
		if err := h.st.SetSetting(rollbackAnnounced, key); err != nil {
			slog.Warn("notify: could not record the rollback announcement", "err", err)
		}
	}
}

// The inverter states the grid watch reads (31027 — foxess_modbus:
// 0 self test, 1 waiting, 2 checking, 3 on grid, 4 off grid / EPS,
// 5 / 6 faults).
const (
	invStateOnGrid  = 3
	invStateOffGrid = 4
)

// gridWatch turns the inverter's state into grid-lost / grid-restored
// events (owner, 2026-11-18). A grid already down when the app starts is
// news too: the gateway host may itself have lost power with the grid.
type gridWatch struct {
	mu        sync.Mutex
	seen      bool      // a state has been observed since start
	lost      bool      // a grid-lost event went out, not yet restored
	lostSince time.Time // when it was first seen down
}

// observe takes one live sample's state (nil: not reported) and returns
// the event it makes, if any.
func (g *gridWatch) observe(state *int, now time.Time) (api.SystemEvent, bool) {
	if state == nil {
		return api.SystemEvent{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	first := !g.seen
	g.seen = true
	switch {
	case *state == invStateOffGrid && !g.lost:
		g.lost, g.lostSince = true, now
		detail := "The inverter reports off grid: the backup circuits run on the battery."
		if first {
			detail += " Seen at start: the grid was already down (foxess-local may have restarted after losing power)."
		}
		return api.SystemEvent{Type: api.SystemGridLost, Detail: detail}, true
	case *state == invStateOnGrid && g.lost:
		g.lost = false
		return api.SystemEvent{
			Type:   api.SystemGridRestored,
			Detail: fmt.Sprintf("The inverter is back on the grid after %s.", fmtOutage(now.Sub(g.lostSince))),
		}, true
	}
	return api.SystemEvent{}, false
}

// fmtOutage renders an outage length for a notice ("30 min", "2 h 5 min").
func fmtOutage(d time.Duration) string {
	const perHour = 60
	m := int(d.Round(time.Minute) / time.Minute)
	if m < perHour {
		return fmt.Sprintf("%d min", m)
	}
	return fmt.Sprintf("%d h %d min", m/perHour, m%perHour)
}

// The lifecycle markers (store settings, not UI settings).
const (
	// cleanStopKey is written by a clean shutdown and consumed at start.
	cleanStopKey = "lifecycle.clean_stop"
	// restartAnnouncedKey names the outage last announced (its last
	// reading's time), so a restart loop announces it once.
	restartAnnouncedKey = "lifecycle.restart_announced"
)

// restartNotice decides, at start, whether the last stop was unexpected
// (owner, 2026-11-18: the gateway host losing power in a grid outage is
// the main case): no clean-stop marker, and data from before. It returns
// the event and the outage's key; ok is false for a clean stop, a first
// start, or an outage already announced.
func restartNotice(clean bool, lastData time.Time, announced string, now time.Time) (api.SystemEvent, string, bool) {
	if clean || lastData.IsZero() {
		return api.SystemEvent{}, "", false
	}
	key := lastData.UTC().Format(time.RFC3339)
	if key == announced {
		return api.SystemEvent{}, "", false
	}
	detail := "foxess-local stopped without shutting down (a power cut?) and has started again. The last reading before the stop was at " +
		lastData.Local().Format("Mon 2 Jan 15:04")
	if now.Before(lastData) {
		detail += "; this machine's clock is not set yet, so how long it was down is unknown."
	} else {
		detail += fmt.Sprintf(" (%s ago).", fmtOutage(now.Sub(lastData)))
	}
	return api.SystemEvent{Type: api.SystemUncleanRestart, Detail: detail}, key, true
}

// announceRestart publishes the unexpected-stop event at start (the
// outbox delivers it whenever the network allows).
func (h *handle) announceRestart() {
	_, clean, err := h.st.Setting(cleanStopKey)
	if err != nil {
		return
	}
	if clean {
		if err := h.st.DeleteSetting(cleanStopKey); err != nil {
			slog.Warn("lifecycle: clean-stop marker not cleared", "err", err)
		}
	}
	qs, err := h.st.QueryState()
	if err != nil {
		return
	}
	announced, _, err := h.st.Setting(restartAnnouncedKey)
	if err != nil {
		return
	}
	if ev, key, ok := restartNotice(clean, qs.TeleTS, announced, h.clk()); ok {
		h.bus.PublishSystem(ev)
		if err := h.st.SetSetting(restartAnnouncedKey, key); err != nil {
			slog.Warn("lifecycle: restart announcement not recorded", "err", err)
		}
	}
}

// storeOutbox keeps the notifier's outbox in the database (so a notice
// survives a restart).
type storeOutbox struct{ st *store.Store }

func (o storeOutbox) Add(p notify.Pending) (int64, error) { return o.st.OutboxAdd(toRow(p)) }

func (o storeOutbox) Due(now time.Time) ([]notify.Pending, error) {
	rows, err := o.st.OutboxDue(now)
	if err != nil {
		return nil, err
	}
	out := make([]notify.Pending, 0, len(rows))
	for _, r := range rows {
		out = append(out, notify.Pending{
			ID: r.ID, Created: r.Created, Targets: r.Targets, Attempts: r.Attempts, Next: r.Next,
			Notice: notify.Notice{Event: r.Event, Severity: notify.Severity(r.Severity), Title: r.Title, Message: r.Message},
		})
	}
	return out, nil
}

func (o storeOutbox) Update(p notify.Pending) error { return o.st.OutboxUpdate(toRow(p)) }

func (o storeOutbox) Delete(id int64) error { return o.st.OutboxDelete(id) }

func toRow(p notify.Pending) store.OutboxRow {
	return store.OutboxRow{
		ID: p.ID, Created: p.Created, Event: p.Notice.Event, Severity: string(p.Notice.Severity),
		Title: p.Notice.Title, Message: p.Notice.Message, Targets: p.Targets, Attempts: p.Attempts, Next: p.Next,
	}
}
