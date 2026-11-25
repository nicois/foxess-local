// Command aemo-spike is an example automation ("sidecar") for
// foxess-local: it force-discharges the battery while the NEM's 5-minute
// dispatch price for a region is high, and stops at a cut-off SoC that the inverter itself
// enforces. It runs beside foxess-local (an OpenStick is enough: one
// small static binary, a few MB of memory, one HTTP fetch every 5
// minutes) and acts only through foxess-local's REST API with its own
// token. See README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// The defaults (the flags' help says what each is).
const (
	defPowerW      = 5000
	defStartAbove  = 300.0
	defStopBelow   = 200.0
	defStartSoc    = 40
	defCutoffSoc   = 30
	defMinHold     = 10 * time.Minute
	defLease       = 30 * time.Minute
	defRenewBefore = 10 * time.Minute
	defOffset      = 40 * time.Second
	priceMaxAge    = 15 * time.Minute
	httpTimeout    = 30 * time.Second
	exitUsage      = 2
	minCutoffSoc   = 10 // the inverter's range for a force window's SoC
	maxCutoffSoc   = 100
)

// config is the command line.
type config struct {
	foxess, name, region string
	nemweb               string
	powerW               int
	offset               time.Duration
	dryRun, once         bool
	policy               Policy
}

func parseFlags(args []string) (config, error) {
	c := config{}
	fs := flag.NewFlagSet("aemo-spike", flag.ContinueOnError)
	fs.StringVar(&c.foxess, "foxess", "http://127.0.0.1:18080", "foxess-local's address")
	fs.StringVar(&c.name, "name", "aemo-spike", "the name of this sidecar's token (FOXESS_TOKEN): an override held by another token is left alone")
	fs.StringVar(&c.region, "region", "VIC1", "the NEM region (NSW1, QLD1, SA1, TAS1, VIC1)")
	fs.StringVar(&c.nemweb, "nemweb", "https://nemweb.com.au", "AEMO's NEMWeb")
	fs.IntVar(&c.powerW, "power", defPowerW, "the force discharge's power (W)")
	fs.Float64Var(&c.policy.StartAbove, "start-above", defStartAbove, "start at or above this price ($/MWh)")
	fs.Float64Var(&c.policy.StopBelow, "stop-below", defStopBelow, "once running, stop at or below this price ($/MWh)")
	fs.IntVar(&c.policy.StartSoc, "start-soc", defStartSoc, "do not start below this SoC (%)")
	fs.IntVar(&c.policy.CutoffSoc, "cutoff-soc", defCutoffSoc, "the inverter stops discharging at this SoC (%, 10..100)")
	fs.DurationVar(&c.policy.MinHold, "min-hold", defMinHold, "once started, run at least this long")
	fs.DurationVar(&c.policy.Lease, "lease", defLease, "each override's length; it lapses if this sidecar stops")
	fs.DurationVar(&c.policy.RenewBefore, "renew-before", defRenewBefore, "renew when less than this is left")
	fs.DurationVar(&c.offset, "offset", defOffset, "how long after each 5-minute boundary to look (AEMO publishes ~20 s after)")
	fs.BoolVar(&c.dryRun, "dry-run", false, "decide and log, change nothing")
	fs.BoolVar(&c.once, "once", false, "one decision, then exit")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	c.policy.PriceMaxAge = priceMaxAge
	switch {
	case c.policy.CutoffSoc < minCutoffSoc || c.policy.CutoffSoc > maxCutoffSoc:
		return c, errors.New("-cutoff-soc must be in 10..100")
	case c.policy.StartSoc <= c.policy.CutoffSoc:
		return c, errors.New("-start-soc must be above -cutoff-soc")
	case c.policy.StopBelow > c.policy.StartAbove:
		return c, errors.New("-stop-below must not be above -start-above")
	case c.policy.RenewBefore >= c.policy.Lease:
		return c, errors.New("-renew-before must be shorter than -lease")
	}
	return c, nil
}

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "aemo-spike:", err)
		os.Exit(exitUsage)
	}
	token := os.Getenv("FOXESS_TOKEN")
	if token == "" {
		slog.Error("FOXESS_TOKEN is not set: create a standard token for this sidecar (System → Access)")
		os.Exit(exitUsage)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hc := &http.Client{Timeout: httpTimeout}
	s := &sidecar{
		cfg: cfg,
		api: &Client{Base: cfg.foxess, Token: token, Client: hc},
		src: &NEMWeb{Base: cfg.nemweb, Client: hc},
	}
	for {
		s.tick(ctx, time.Now())
		if cfg.once {
			return
		}
		select {
		case <-ctx.Done():
			// an override this sidecar holds runs to its lease end: no
			// write on the way out
			return
		case <-time.After(nextTick(time.Now(), cfg.offset)):
		}
	}
}

// nextTick is the wait until offset past the next 5-minute boundary.
func nextTick(now time.Time, offset time.Duration) time.Duration {
	const step = 5 * time.Minute
	next := now.Truncate(step).Add(offset)
	for !next.After(now) {
		next = next.Add(step)
	}
	return next.Sub(now)
}

// sidecar is the loop's state.
type sidecar struct {
	cfg config
	api *Client
	src PriceSource
}

// tick makes one decision and acts on it.
func (s *sidecar) tick(ctx context.Context, now time.Time) {
	st, err := s.api.State(ctx)
	if err != nil {
		slog.Warn("foxess-local unreachable; nothing changed", "err", err)
		return
	}
	in := Inputs{Now: now, SocPct: -1}
	if st.Online && st.Battery.SocPct != nil {
		in.SocPct = *st.Battery.SocPct
	}
	if o := st.Override; o != nil && o.Active {
		if o.Holder != s.cfg.name {
			slog.Info("the override is held by another token; left alone", "holder", o.Holder, "until", o.Until)
			return
		}
		in.Active, in.Since, in.Until = true, o.Since, o.Until
	}
	if p, err := s.src.Latest(ctx, s.cfg.region); err != nil {
		slog.Warn("no price", "err", err)
	} else {
		in.Price = &p
	}
	d := s.cfg.policy.Decide(in)
	slog.Info("decision", "action", actionName[d.Action], "reason", d.Reason, "soc", in.SocPct, "price", priceText(in.Price))
	if s.cfg.dryRun || d.Action == Hold {
		return
	}
	s.act(ctx, d, in)
}

var actionName = map[Action]string{Hold: "hold", Start: "start", Stop: "stop"}

func priceText(p *Price) string {
	if p == nil {
		return "none"
	}
	return fmt.Sprintf("%s $%.2f/MWh for %s", p.Region, p.RRP, p.At.Local().Format("15:04"))
}

// act applies a Start or Stop; a start (or a refusal) tells the owner why.
func (s *sidecar) act(ctx context.Context, d Decision, in Inputs) {
	switch d.Action {
	case Start:
		o, err := s.api.SetOverride(ctx, d.Until, s.cfg.policy.CutoffSoc, s.cfg.powerW)
		if err != nil {
			slog.Warn("override refused", "err", err)
			s.notice(ctx, "override-refused", "warning", "Force discharge not started", d.Reason+": "+err.Error())
			return
		}
		slog.Info("override", "until", o.Until, "renewal", in.Active)
		// foxess-local notifies the override's start and end itself (the
		// power, window and cut-off); this adds only what it cannot know.
		if !in.Active {
			s.notice(ctx, "price-spike", "info", "Price spike", d.Reason+".")
		}
	case Stop:
		if err := s.api.ClearOverride(ctx); err != nil {
			slog.Warn("clear refused; the override lapses at its end", "err", err)
			return
		}
		slog.Info("override cleared", "reason", d.Reason) // foxess-local notifies the end
	case Hold:
	}
}

// notice sends a notice; a failure is only logged.
func (s *sidecar) notice(ctx context.Context, event, sev, title, msg string) {
	if err := s.api.Notice(ctx, event, sev, title, msg); err != nil {
		slog.Warn("notice not sent", "err", err)
	}
}
