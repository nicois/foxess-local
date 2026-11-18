package forward

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"foxesslocal/internal/gateway"
)

// TestSchedulerBackoff: a push that keeps failing is retried at 30 s,
// then 1, 2, 4, 8 and 15 minutes — not every 30 s tick — and one alert
// is raised at the third consecutive failure.
func TestSchedulerBackoff(t *testing.T) {
	st := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		start := now.Add(time.Minute).Truncate(time.Minute)
		end := start.Add(3 * time.Hour)
		refuse := gateway.WithAuthorizer(func(gateway.Command) error { return errors.New("refused") })
		gw := gateway.New(nil, time.Now, refuse)
		sched, err := NewScheduler(st, gw, time.Now, []Window{{
			Name: "night", Slot: 1, Start: packHHMM(start), End: packHHMM(end), FdSoc: 90, FdPwr: 9500,
		}})
		if err != nil {
			t.Fatal(err)
		}
		var (
			mu     sync.Mutex
			alerts []string
		)
		sched.OnAlert(func(m string) { mu.Lock(); alerts = append(alerts, m); mu.Unlock() })
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { sched.Run(ctx); close(done) }()

		sleepTo(t, start.Add(40*time.Minute))
		synctest.Wait()
		cancel()
		<-done

		st.Flush()
		rows, err := st.QueryAudit(time.Time{}, audKindSchedule)
		if err != nil {
			t.Fatal(err)
		}
		// attempts at +0, +30 s, +1.5, +3.5, +7.5, +15.5, +30.5 min
		// (the 30 s tick rounds each retry up to the next tick)
		if n := len(rows); n < 6 || n > 8 {
			t.Fatalf("%d push attempts in 40 minutes, want 6-8 (backoff), not ~80", n)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(alerts) != 1 || !strings.Contains(alerts[0], "night") {
			t.Fatalf("alerts %v, want one naming the window", alerts)
		}
	})
}
