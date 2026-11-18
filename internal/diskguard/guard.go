// Package diskguard keeps foxess-local from filling its disk: hourly,
// when free space falls below the critical level, it discards the oldest
// raw data (frames, chunks, telemetry — the history survives as
// 15-minute averages) until there is room again. On a large disk this
// never fires; on a small device (a repurposed modem's flash) it is what
// stands between a long retention window and a full filesystem.
package diskguard

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"foxesslocal/internal/diskspace"
	"foxesslocal/internal/store"
)

// Store is the store surface the guard drives.
type Store interface {
	OldestRaw() (time.Time, bool, error)
	DiscardRawBefore(cutoff time.Time) (store.DiscardReport, error)
	Reclaim() error
}

// Defaults.
const (
	// DefaultEvery is the check period.
	DefaultEvery = time.Hour
	// DefaultKeepRaw is the newest raw window never discarded.
	DefaultKeepRaw = 6 * time.Hour
	// maxSteps bounds one check (each step halves the raw window).
	maxSteps = 12
	// wholeStep: a raw span shorter than this is discarded in one step.
	wholeStep = time.Hour
	// recoverFactor: discard until free space is this multiple of the
	// critical level (hysteresis: not a discard every hour).
	recoverFactor = 2
)

// Guard checks free space and discards old raw data when it is critical.
type Guard struct {
	St      Store
	Space   func() (free, total uint64, err error) // the store's filesystem
	MinFree uint64                                 // bytes; diskspace.Threshold caps it at 10 %
	KeepRaw time.Duration                          // 0 = DefaultKeepRaw
	Every   time.Duration                          // 0 = DefaultEvery
	Now     func() time.Time
	Alert   func(msg string) // the UI's alert feed (nil = log only)
}

// Result is one check's outcome.
type Result struct {
	Free, Threshold uint64
	Cutoff          time.Time // the last discard's cutoff (zero = none)
	Discarded       store.DiscardReport
}

// Run checks at once and then every Every until ctx is done.
func (g *Guard) Run(ctx context.Context) {
	every := g.Every
	if every <= 0 {
		every = DefaultEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := g.Check(); err != nil {
			slog.Error("diskguard: check failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Check runs one pass: below the critical level, discard the older half
// of the raw window (never the newest KeepRaw), reclaim, re-measure, and
// repeat until free space is recoverFactor × the level or nothing more
// may go.
func (g *Guard) Check() (Result, error) {
	free, total, err := g.Space()
	if err != nil {
		return Result{}, err
	}
	th := diskspace.Threshold(g.MinFree, total)
	res := Result{Free: free, Threshold: th}
	if free >= th {
		return res, nil
	}
	keep := g.KeepRaw
	if keep <= 0 {
		keep = DefaultKeepRaw
	}
	if err := g.discard(&res, g.Now().Add(-keep)); err != nil {
		return res, err
	}
	g.report(res, keep)
	return res, nil
}

// discard halves the raw window older than floor until free space is
// recoverFactor × the threshold or nothing more may go.
func (g *Guard) discard(res *Result, floor time.Time) error {
	for step := 0; step < maxSteps && res.Free < recoverFactor*res.Threshold; step++ {
		oldest, ok, err := g.St.OldestRaw()
		if err != nil {
			return err
		}
		if !ok || !oldest.Before(floor) {
			return nil
		}
		cutoff := floor
		if span := floor.Sub(oldest); span > wholeStep {
			cutoff = oldest.Add(span / 2) //nolint:mnd // the older half
		}
		rep, err := g.St.DiscardRawBefore(cutoff)
		if err != nil {
			return err
		}
		if err := g.St.Reclaim(); err != nil {
			return err
		}
		res.Cutoff = cutoff
		res.Discarded.Frames += rep.Frames
		res.Discarded.Chunks += rep.Chunks
		res.Discarded.Telemetry += rep.Telemetry
		if res.Free, _, err = g.Space(); err != nil {
			return err
		}
	}
	return nil
}

// report logs and alerts one check that found the disk critical.
func (g *Guard) report(res Result, keep time.Duration) {
	if !res.Cutoff.IsZero() {
		g.alert(fmt.Sprintf("disk space low: discarded raw data older than %s (%d frames, %d samples); history kept as 15-minute averages; %d MB free",
			res.Cutoff.Local().Format("2006-01-02 15:04"), res.Discarded.Frames, res.Discarded.Telemetry, res.Free/diskspace.MiB))
	}
	if res.Free < res.Threshold {
		g.alert(fmt.Sprintf("disk space critically low: %d MB free (critical below %d MB) and no more raw data can be discarded (the last %s are kept)",
			res.Free/diskspace.MiB, res.Threshold/diskspace.MiB, keep))
	}
}

func (g *Guard) alert(msg string) {
	slog.Warn("diskguard: " + msg)
	if g.Alert != nil {
		g.Alert(msg)
	}
}
