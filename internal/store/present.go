package store

import (
	"fmt"

	"foxesslocal/internal/frame"
)

// presentMinW is the reading that shows a source exists: the
// dashboard's idle threshold (standby noise is not a source).
const presentMinW = 30

// Present tells whether the site has each optional power source at all,
// so the web UI can leave out one it lacks (issue #2: a KH10 with only
// AC-coupled solar shows no DC solar; most sites have no second CT).
type Present struct {
	// PV: the PV strings have generated (DC solar).
	PV bool
	// Meter2: the second CT (meter 2) has measured power.
	Meter2 bool
}

// Present reports the sources seen since the history began: in a live
// sample, or (from before this start) in a stored 15-minute bucket. A
// source once seen stays present.
func (s *Store) Present() Present {
	return Present{PV: s.pvSeen.Load(), Meter2: s.meter2Seen.Load()}
}

// notePresent marks the sources a live sample shows.
func (s *Store) notePresent(t frame.Tele) {
	if t.Sol >= presentMinW {
		s.pvSeen.Store(true)
	}
	if m := t.Meter2; m != nil && (*m >= presentMinW || *m <= -presentMinW) {
		s.meter2Seen.Store(true)
	}
}

// seedPresent reads the sources the stored buckets show (two
// EXISTS probes on the small rollup table, once per start).
func (s *Store) seedPresent() error {
	for _, q := range []struct {
		where string
		seen  interface{ Store(bool) }
	}{
		{"sol_max >= ?", &s.pvSeen},
		{"ABS(meter2_avg) >= ?", &s.meter2Seen},
	} {
		var ok bool

		if err := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM buckets15m WHERE "+q.where+")", presentMinW).Scan(&ok); err != nil {
			return fmt.Errorf("present: %w", err)
		}
		if ok {
			q.seen.Store(true)
		}
	}
	return nil
}
