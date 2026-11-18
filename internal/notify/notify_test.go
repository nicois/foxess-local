package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// post is one recorded delivery.
type post struct {
	url    string
	header http.Header
	body   string
	at     time.Time
}

// fakeTransport records deliveries; fail makes the first n attempts fail.
type fakeTransport struct {
	mu    sync.Mutex
	posts []post
	fail  int
}

func (f *fakeTransport) send(_ context.Context, url string, h http.Header, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, post{url: url, header: h, body: string(body), at: time.Now()})
	if f.fail > 0 {
		f.fail--
		return errors.New("503 from the server")
	}
	return nil
}

func (f *fakeTransport) all() []post {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]post(nil), f.posts...)
}

const (
	hookURL = "http://ha.example:8123/api/webhook/foxess-abc"
	ntfyURL = "https://ntfy.example/foxess-topic"
)

func both() Targets { return Targets{WebhookURL: hookURL, NtfyURL: ntfyURL} }

// start runs a notifier in the bubble; the returned stop ends it.
func start(t *testing.T, targets func() Targets, f *fakeTransport) (*Notifier, func()) {
	t.Helper()
	return startWith(t, targets, f, &MemOutbox{})
}

// startWith is start over a given outbox.
func startWith(t *testing.T, targets func() Targets, f *fakeTransport, box Outbox) (*Notifier, func()) {
	t.Helper()
	n := New(targets, time.Now, WithTransport(f.send), WithOutbox(box))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.Run(ctx); close(done) }()
	return n, func() { cancel(); <-done }
}

// TestNotifyFormats (F2): one notice reaches each configured target in
// its format — the webhook as JSON (what a Home Assistant webhook
// trigger reads), ntfy as a text body with title, priority and tags.
func TestNotifyFormats(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeTransport{}
		n, stop := start(t, both, f)
		n.Notify(Notice{Event: "inverter-offline", Severity: Critical, Title: offTitle, Message: offMsg})
		synctest.Wait()
		stop()
		ps := f.all()
		if len(ps) != 2 {
			t.Fatalf("posts %+v", ps)
		}
		for _, p := range ps {
			if p.url == hookURL {
				checkWebhook(t, p)
			} else {
				checkNtfy(t, p)
			}
		}
	})
}

const (
	offTitle = "Inverter offline"
	offMsg   = "no readings for 10 min"
)

func checkWebhook(t *testing.T, hook post) {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal([]byte(hook.body), &body); err != nil || hook.header.Get("Content-Type") != "application/json" {
		t.Fatalf("webhook body %q: %v", hook.body, err)
	}
	if body["event"] != "inverter-offline" || body["severity"] != "critical" || body["title"] != offTitle ||
		body["message"] != offMsg || body["source"] != "foxess-local" || body["ts"] == "" {
		t.Fatalf("webhook fields %+v", body)
	}
}

func checkNtfy(t *testing.T, ntfy post) {
	t.Helper()
	if ntfy.url != ntfyURL || ntfy.body != offMsg || ntfy.header.Get("Title") != offTitle ||
		ntfy.header.Get("Priority") != "5" || ntfy.header.Get("Tags") == "" {
		t.Fatalf("ntfy %+v", ntfy)
	}
}

// TestNotifyNoTargets: with nothing configured a notice is dropped
// quietly, and a target set later (in the UI) applies to the next one.
func TestNotifyNoTargets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeTransport{}
		var mu sync.Mutex
		tg := Targets{}
		n, stop := start(t, func() Targets { mu.Lock(); defer mu.Unlock(); return tg }, f)
		n.Notify(Notice{Event: "a", Severity: Warning, Title: "A", Message: "one"})
		synctest.Wait()
		mu.Lock()
		tg = Targets{NtfyURL: ntfyURL}
		mu.Unlock()
		n.Notify(Notice{Event: "b", Severity: Info, Title: "B", Message: "two"})
		synctest.Wait()
		stop()
		if ps := f.all(); len(ps) != 1 || ps[0].body != "two" || ps[0].header.Get("Priority") != "3" {
			t.Fatalf("posts %+v", ps)
		}
	})
}

// TestNotifyDedup: the same notice again within DedupWindow is
// suppressed (a flapping fault does not spam); after it, it is sent.
func TestNotifyDedup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeTransport{}
		n, stop := start(t, func() Targets { return Targets{NtfyURL: ntfyURL} }, f)
		nt := Notice{Event: "alert", Severity: Warning, Title: "Alert", Message: "disk low"}
		n.Notify(nt)
		synctest.Wait()
		time.Sleep(time.Minute)
		n.Notify(nt)
		n.Notify(Notice{Event: "alert", Severity: Warning, Title: "Alert", Message: "another"})
		synctest.Wait()
		time.Sleep(DedupWindow)
		n.Notify(nt)
		synctest.Wait()
		stop()
		var bodies []string
		for _, p := range f.all() {
			bodies = append(bodies, p.body)
		}
		if strings.Join(bodies, ",") != "disk low,another,disk low" {
			t.Fatalf("sent %v", bodies)
		}
	})
}

// TestNotifyRetry (owner, 2026-11-18): a failed delivery is retried
// with backoff (firstRetry, doubling) until it lands; one delivered late
// says when it happened.
func TestNotifyRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeTransport{fail: 3}
		n, stop := start(t, func() Targets { return Targets{NtfyURL: ntfyURL} }, f)
		t0 := time.Now()
		n.Notify(Notice{Event: "x", Severity: Warning, Title: "X", Message: "retry me"})
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		stop()
		ps := f.all()
		if len(ps) != 4 {
			t.Fatalf("%d attempts, want 4", len(ps))
		}
		if gap := ps[2].at.Sub(ps[1].at); gap < 2*firstRetry {
			t.Fatalf("second retry after %v, want the doubled backoff", gap)
		}
		if !strings.Contains(ps[3].body, "delivered late") || ps[3].at.Sub(t0) < lateAfter {
			t.Fatalf("late delivery: %q at %v", ps[3].body, ps[3].at.Sub(t0))
		}
	})
}

// TestNotifySurvivesRestart: the outbox outlives the notifier — after a
// power cut the next run delivers what the last one could not (the
// internet came back long after foxess-local).
func TestNotifySurvivesRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		box := &MemOutbox{}
		down := &fakeTransport{fail: 1 << 30}
		ctx, cancel := context.WithCancel(context.Background())
		n := New(func() Targets { return Targets{NtfyURL: ntfyURL} }, time.Now, WithTransport(down.send), WithOutbox(box))
		done := make(chan struct{})
		go func() { n.Run(ctx); close(done) }()
		n.Notify(Notice{Event: "grid-lost", Severity: Critical, Title: "Grid down", Message: "on backup"})
		synctest.Wait()
		cancel()
		<-done
		if box.Len() != 1 {
			t.Fatalf("outbox %d, want the undelivered notice", box.Len())
		}
		time.Sleep(time.Hour) // the restart, and the wait for the internet
		up := &fakeTransport{}
		_, stop := startWith(t, func() Targets { return Targets{NtfyURL: ntfyURL} }, up, box)
		synctest.Wait()
		stop()
		if ps := up.all(); len(ps) != 1 || !strings.HasPrefix(ps[0].body, "on backup") || box.Len() != 0 {
			t.Fatalf("after the restart: %+v, outbox %d", ps, box.Len())
		}
	})
}

// TestNotifyPartial: a target that took the notice is not sent it
// again while the other is retried.
func TestNotifyPartial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		hookTries, ntfyTries := 0, 0
		send := func(_ context.Context, url string, _ http.Header, _ []byte) error {
			mu.Lock()
			defer mu.Unlock()
			if url == hookURL {
				hookTries++
				return nil
			}
			ntfyTries++
			if ntfyTries < 3 {
				return errors.New("down")
			}
			return nil
		}
		n := New(both, time.Now, WithTransport(send))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { n.Run(ctx); close(done) }()
		n.Notify(Notice{Event: "a", Severity: Warning, Title: "A", Message: "m"})
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		cancel()
		<-done
		mu.Lock()
		defer mu.Unlock()
		if hookTries != 1 || ntfyTries != 3 {
			t.Fatalf("webhook %d, ntfy %d (want 1 and 3)", hookTries, ntfyTries)
		}
	})
}

// TestNotifyGivesUp: a target that never answers is abandoned after
// maxAttempts (counted, not timed: a board without a battery-backed
// clock can jump years at NTP sync).
func TestNotifyGivesUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		box := &MemOutbox{}
		f := &fakeTransport{fail: 1 << 30}
		_, stop := startWith(t, func() Targets { return Targets{NtfyURL: ntfyURL} }, f, box)
		n2 := New(func() Targets { return Targets{NtfyURL: ntfyURL} }, time.Now, WithOutbox(box))
		n2.Notify(Notice{Event: "y", Severity: Warning, Title: "Y", Message: "never lands"})
		time.Sleep(time.Duration(maxAttempts+1) * maxRetry)
		synctest.Wait()
		stop()
		if got := len(f.all()); got != maxAttempts || box.Len() != 0 {
			t.Fatalf("%d attempts, outbox %d (want %d and 0)", got, box.Len(), maxAttempts)
		}
	})
}

// TestNotifyTest: Test sends one test notice to each target now and
// reports each result; with none configured it says so.
func TestNotifyTest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeTransport{fail: 1}
		n := New(both, time.Now, WithTransport(f.send))
		res := n.Test(context.Background())
		if len(res) != 2 || (res[0].Err == "") == (res[1].Err == "") {
			t.Fatalf("results %+v", res)
		}
		if res := New(func() Targets { return Targets{} }, time.Now).Test(context.Background()); len(res) != 0 {
			t.Fatalf("no targets: %+v", res)
		}
	})
}
