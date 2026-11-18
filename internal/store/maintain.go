package store

import (
	"fmt"
	"log/slog"
	"time"
)

// MaintainConfig is the retention policy (config [retention]).
type MaintainConfig struct {
	RawHours   int // dense raw window (connection close -> reap)
	BucketDays int // 15-minute rollup horizon
	// ChunkBytesHours is the chunk payload byte window, measured from
	// connection close; 0 = off (byte capture is a development /
	// trace-contribution opt-in, never a default).
	ChunkBytesHours int
}

// MaintainReport counts one maintenance pass.
type MaintainReport struct {
	Rolled         int
	NulledBytes    int // chunk payloads cleared by ChunkBytesHours
	DeletedConns   int
	DeletedChunks  int
	DeletedFrames  int
	DeletedRaw     int // telemetry rows
	DeletedBuckets int
}

// Maintain runs one pass: roll up completed buckets, clear chunk payload
// bytes at their own horizon, then reap finished connections at the raw
// window. All connection-scoped retention is measured from close — the
// moment the stream stopped being written to: a keep-alive session can
// run for days, and the rows of a still-open session must outlive the
// raw window no matter how old they are. The pass is periodic (the
// caller's ticker); the cutoffs need no exactness.
func (s *Store) Maintain(cfg MaintainConfig) (MaintainReport, error) {
	var rep MaintainReport
	n, err := s.RollUp()
	if err != nil {
		return rep, err
	}
	rep.Rolled = n
	now := s.now()

	// The chunk payload's own horizon: finished streams' bytes are
	// cleared at close+ChunkBytesHours while the metadata rows live out
	// the full raw window. No pass runs while the knob is off.
	if cfg.ChunkBytesHours > 0 {
		if rep.NulledBytes, err = s.nullChunkBytes(encTS(now.Add(-time.Duration(cfg.ChunkBytesHours) * time.Hour))); err != nil {
			return rep, err
		}
	}

	// Finished connections (and all their child rows) at the raw window,
	// clamped by the rollup watermark: never delete a connection whose
	// data the rollup has not fully covered, and never delete anything
	// while no buckets exist (watermark 0 — un-rolled data is the only
	// copy).
	if wm, err := s.lastBucket(); err != nil {
		return rep, err
	} else if wm > 0 {
		cutoff := now.Add(-time.Duration(cfg.RawHours) * time.Hour)
		if wmEnd := time.Unix(wm, 0).UTC().Add(bucketLen); wmEnd.Before(cutoff) {
			cutoff = wmEnd // delete only what the watermark has fully covered
		}
		if rep.DeletedConns, rep.DeletedChunks, rep.DeletedFrames, rep.DeletedRaw, err = s.reapFinished(encTS(cutoff)); err != nil {
			return rep, err
		}
	}
	if rep.DeletedBuckets, err = s.deleteOldBuckets(now.Add(-time.Duration(cfg.BucketDays) * 24 * time.Hour).Unix()); err != nil {
		return rep, err
	}
	return rep, nil
}

// nullChunkBytes clears the captured payload bytes of finished
// connections closed before cutoff (the ChunkBytesHours horizon). The
// metadata rows survive; open (still-written) streams are untouched.
func (s *Store) nullChunkBytes(cutoff string) (int, error) {
	res, err := s.db.Exec(`
		UPDATE chunks SET payload = NULL
		WHERE payload IS NOT NULL
		  AND conn_id IN (SELECT id FROM connections WHERE closed != '' AND closed < ?)`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: maintain: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: maintain: %w", err)
	}
	return int(n), nil
}

func (s *Store) deleteOldBuckets(cutoffUnix int64) (int, error) {
	res, err := s.db.Exec("DELETE FROM buckets15m WHERE bucket < ?", cutoffUnix)
	if err != nil {
		return 0, fmt.Errorf("store: maintain: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: maintain: %w", err)
	}
	return int(n), nil
}

// StartMaintenance runs Maintain on a wall ticker (production only)
// until Close, which waits for a pass in progress before closing the
// database. Tests call Maintain directly with the injected s.now (C-004).
func (s *Store) StartMaintenance(interval time.Duration, cfg MaintainConfig) {
	s.maintWG.Go(func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
			}
			if rep, err := s.Maintain(cfg); err != nil {
				slog.Error("store: maintenance pass failed", "err", err)
			} else if rep.DeletedConns+rep.DeletedChunks+rep.DeletedFrames+rep.DeletedRaw+rep.DeletedBuckets+rep.NulledBytes > 0 {
				slog.Info("store: maintenance pass",
					"rolled", rep.Rolled, "nulled_bytes", rep.NulledBytes,
					"conns", rep.DeletedConns, "chunks", rep.DeletedChunks,
					"frames", rep.DeletedFrames, "raw", rep.DeletedRaw,
					"buckets", rep.DeletedBuckets)
			}
		}
	})
}
