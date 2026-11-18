package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"foxesslocal/internal/api"
)

// gateStatusPath is where check-gate.sh (root, every 5 minutes) leaves
// its result; world-readable, so the app (and doctor) can read it.
const gateStatusPath = "/run/foxess-gate/status"

const (
	// gateStaleAfter: a result older than this means the 5-minute check
	// has stopped running.
	gateStaleAfter = 15 * time.Minute
	// gatePollEvery is how often the app reads the result.
	gatePollEvery = time.Minute
	// gateRuleLoss is check-gate.sh's failing result.
	gateRuleLoss = "rule-loss"
)

// gateStatus is one check's result.
type gateStatus struct {
	TS     time.Time
	Result string // ok | rule-loss
	Detail string
}

// readGateStatus parses the status file (ts=, result=, detail= lines).
func readGateStatus(path string) (gateStatus, error) {
	f, err := os.Open(path) //nolint:gosec // G304: a fixed path (tests pass a temp file)
	if err != nil {
		return gateStatus{}, err
	}
	defer func() { _ = f.Close() }()
	var g gateStatus
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), "=")
		switch k {
		case "ts":
			sec, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return gateStatus{}, fmt.Errorf("gate status: ts %q", v)
			}
			g.TS = time.Unix(sec, 0)
		case "result":
			g.Result = v
		case "detail":
			g.Detail = v
		}
	}
	if g.TS.IsZero() || g.Result == "" {
		return gateStatus{}, fmt.Errorf("gate status: incomplete %s", path)
	}
	return g, sc.Err()
}

// gateStatusCheck is the readiness line for the egress gate.
func gateStatusCheck(path string, now time.Time) api.Check {
	c := api.Check{ID: "gate", Title: "Egress gate"}
	g, err := readGateStatus(path)
	switch {
	case err != nil:
		c.Status = api.CheckPending
		c.Detail = "no gate check result yet (the check runs every 5 minutes; a container cannot see the host's)"
	case now.Sub(g.TS) > gateStaleAfter:
		c.Status = api.CheckWarn
		c.Detail = fmt.Sprintf("the last gate check ran %s ago: the 5-minute check has stopped", now.Sub(g.TS).Round(time.Minute))
		c.Fix = "Start it: `sudo systemctl enable --now foxess-gate-check.timer`."
	case g.Result == gateRuleLoss:
		c.Status = api.CheckFail
		c.Detail = "the last check found rule loss (" + g.Detail + ") and re-applied the gate"
		c.Fix = "See `journalctl -t foxess-gate -n 20`; the next check (within 5 minutes) confirms the re-apply."
	default:
		c.Status = api.CheckOK
		c.Detail = fmt.Sprintf("the rules were in place %s ago", now.Sub(g.TS).Round(time.Minute))
	}
	return c
}

// watchGateStatus publishes each new rule-loss result once (F2) until
// ctx is done. A result older than gateStaleAfter when the app starts is
// not news.
func watchGateStatus(ctx context.Context, path string, publish func(api.SystemEvent)) {
	seen := time.Now().Add(-gateStaleAfter)
	for {
		if g, err := readGateStatus(path); err == nil && g.TS.After(seen) {
			seen = g.TS
			if g.Result == gateRuleLoss {
				publish(api.SystemEvent{Type: api.SystemGateRuleLoss, Detail: g.Detail + " (the gate was re-applied)"})
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(gatePollEvery):
		}
	}
}
