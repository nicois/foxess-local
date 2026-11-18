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
