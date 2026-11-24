package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"foxesslocal/internal/api"
)

func writeStatus(t *testing.T, path string, ts time.Time, result, detail string) {
	t.Helper()
	body := "ts=" + strconv.FormatInt(ts.Unix(), 10) + "\nresult=" + result + "\ndetail=" + detail + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestGateStatusCheck (F2): the readiness line from the gate check's
// status file — ok when the last check passed recently; a failure when
// it found rule loss; a warning when the check has stopped running;
// pending when there is no result yet (or the app cannot see the
// host's /run, in a container).
func TestGateStatusCheck(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := filepath.Join(t.TempDir(), "status")
	if c := gateStatusCheck(path, now); c.Status != api.CheckPending || c.ID != "gate" {
		t.Fatalf("no file: %+v", c)
	}
	writeStatus(t, path, now.Add(-4*time.Minute), "ok", "")
	if c := gateStatusCheck(path, now); c.Status != api.CheckOK {
		t.Fatalf("ok: %+v", c)
	}
	writeStatus(t, path, now.Add(-time.Minute), "rule-loss", "table ip foxess_gate missing")
	if c := gateStatusCheck(path, now); c.Status != api.CheckFail || c.Fix == "" {
		t.Fatalf("rule loss: %+v", c)
	}
	writeStatus(t, path, now.Add(-time.Hour), "ok", "")
	if c := gateStatusCheck(path, now); c.Status != api.CheckWarn {
		t.Fatalf("stale: %+v", c)
	}
}

// TestWatchGateStatus: each new rule-loss result is published once, as
// a gate-rule-loss system event; a passing check publishes nothing, and
// a result older than gateStaleAfter at start is not news.
func TestWatchGateStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "status")
		writeStatus(t, path, time.Now().Add(-time.Hour), "rule-loss", "old news")
		var mu sync.Mutex
		var got []api.SystemEvent
		publish := func(e api.SystemEvent) { mu.Lock(); got = append(got, e); mu.Unlock() }
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { watchGateStatus(ctx, path, publish); close(done) }()
		synctest.Wait()
		time.Sleep(gatePollEvery)
		writeStatus(t, path, time.Now(), "rule-loss", "fwmark routing rule missing")
		time.Sleep(gatePollEvery)
		synctest.Wait()
		time.Sleep(gatePollEvery) // the same result again: not news
		writeStatus(t, path, time.Now(), "ok", "")
		time.Sleep(gatePollEvery)
		synctest.Wait()
		cancel()
		<-done
		mu.Lock()
		defer mu.Unlock()
		if len(got) != 1 || got[0].Type != api.SystemGateRuleLoss || got[0].Detail != "fwmark routing rule missing (the gate was re-applied)" {
			t.Fatalf("published %+v", got)
		}
	})
}

// TestNoInverter (D-031): gate.env with an empty INVERTER_IP is an
// install without an inverter (--no-inverter); a missing file (a
// container, --app-only) or an address is not.
func TestNoInverter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gate.env")
	if noInverter(path) {
		t.Fatal("missing gate.env counted as no inverter")
	}
	for body, want := range map[string]bool{
		"# comment\nINVERTER_IP=\nINVERTER_MAC=\nTPROXY_PORT=14431\n": true,
		"INVERTER_IP=192.0.2.10\nINVERTER_MAC=\n":                     false,
		"INVERTER_IP=\nINVERTER_IP=192.0.2.10\n":                      false, // the last one wins, as in the scripts
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := noInverter(path); got != want {
			t.Errorf("%q: %v, want %v", body, got, want)
		}
	}
	if c := gateIdleCheck(); c.ID != "gate" || c.Status != api.CheckPending || c.Detail == "" {
		t.Fatalf("idle gate: %+v", c)
	}
}
