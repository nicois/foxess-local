// Command loadgen drives the foxess-local API with the requests the web
// UI makes and reports latency percentiles per endpoint — for comparing
// builds and devices, and as the load while a CPU or heap profile is
// captured (go tool pprof against the app's [debug] pprof_listen, or
// devui -pprof).
//
//	go run ./tools/loadgen -base http://127.0.0.1:18999 -token fl_… -dur 30s -c 4
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"sync"
	"text/tabwriter"
	"time"
)

// request is one UI request and its relative weight (the dashboard is
// the page people keep open; the rest are visits).
type request struct {
	path   string
	weight int
}

var mix = []request{
	{"/api/v1/state", 20},
	{"/api/v1/power/average?window=5m", 10},
	{"/api/v1/power/average?window=30m", 4},
	{"/api/v1/history?range=1h", 4},
	{"/api/v1/history?range=24h", 6},
	{"/api/v1/history?range=7d", 2},
	{"/api/v1/history?range=30d", 1},
	{"/api/v1/config", 3},
	{"/api/v1/forward-mode", 2},
	{"/api/v1/compatibility", 1},
	{"/api/v1/audit", 2},
	{"/api/v1/audit?source=cloud&action=drop", 1},
	{"/api/v1/messages", 2},
	{"/api/v1/messages?frame=modbus-write", 1},
	{"/api/v1/messages?dir=cloud-to-inverter&frame=modbus-read", 1},
}

type sample struct {
	path string
	d    time.Duration
	ok   bool
}

func main() {
	base := flag.String("base", "http://127.0.0.1:18999", "API base URL")
	token := flag.String("token", os.Getenv("FL_TOKEN"), "bearer token (default $FL_TOKEN; empty = tokenless)")
	dur := flag.Duration("dur", 30*time.Second, "run time")
	conc := flag.Int("c", 4, "concurrent clients")
	flag.Parse()

	total := 0
	for _, r := range mix {
		total += r.weight
	}
	ctx, cancel := context.WithTimeout(context.Background(), *dur)
	defer cancel()
	var (
		mu  sync.Mutex
		all []sample
		wg  sync.WaitGroup
	)
	client := &http.Client{Timeout: 30 * time.Second}
	for w := range *conc {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w), 7)) //nolint:gosec // G404: request mix, not security
			var local []sample
			for ctx.Err() == nil {
				path := pick(rng, total)
				local = append(local, do(ctx, client, *base+path, *token, path))
			}
			mu.Lock()
			all = append(all, local...)
			mu.Unlock()
		})
	}
	wg.Wait()
	report(all, *dur)
}

func pick(rng *rand.Rand, total int) string {
	n := rng.IntN(total)
	for _, r := range mix {
		if n < r.weight {
			return r.path
		}
		n -= r.weight
	}
	return mix[0].path
}

func do(ctx context.Context, c *http.Client, url, token, path string) sample {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		log.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	t0 := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return sample{path: path, d: time.Since(t0), ok: ctx.Err() != nil}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return sample{path: path, d: time.Since(t0), ok: resp.StatusCode == http.StatusOK}
}

func report(all []sample, dur time.Duration) {
	by := map[string][]time.Duration{}
	errs := map[string]int{}
	for _, s := range all {
		by[s.path] = append(by[s.path], s.d)
		if !s.ok {
			errs[s.path]++
		}
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	_, _ = fmt.Fprintln(tw, "endpoint\tn\terr\tp50\tp90\tp99\tmax\t")
	for _, r := range mix {
		ds := by[r.path]
		if len(ds) == 0 {
			continue
		}
		slices.Sort(ds)
		q := func(p float64) time.Duration { return ds[int(p*float64(len(ds)-1))].Round(time.Microsecond) }
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%v\t%v\t%v\t%v\t\n", r.path, len(ds), errs[r.path], q(0.5), q(0.9), q(0.99), ds[len(ds)-1].Round(time.Microsecond))
	}
	_ = tw.Flush()
	fmt.Printf("%d requests in %v (%.0f/s)\n", len(all), dur, float64(len(all))/dur.Seconds())
}
