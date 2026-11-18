package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The disk-pressure path (internal/diskguard): discard the oldest raw
// data whatever its session, and give the space back to the filesystem.
// The retention pass (Maintain) only reaps finished sessions, so a
// long-lived session's rows grow until it closes; on a small device the
// disk can fill first.

// autoVacuumIncremental is PRAGMA auto_vacuum's INCREMENTAL mode: freed
// pages are returned to the filesystem by PRAGMA incremental_vacuum
// (with NONE they are only reused, and the file never shrinks).
const autoVacuumIncremental = 2

// discardBatch bounds each delete transaction: a large delete in one
// transaction grows the WAL by the deleted pages — disk the device does
// not have when this runs.
const discardBatch = 5000

// vacuumHeadroom is the free space EnableIncrementalVacuum keeps beyond
// the rewrite's own need (a VACUUM writes a full copy of the database).
const vacuumHeadroom = 64 << 20

// DiscardReport counts one discard.
type DiscardReport struct {
	Frames, Chunks, Telemetry int
}

// Per-table batched deletes (fixed statements: the table is never a
// parameter).
const (
	discardFramesSQL    = `DELETE FROM frames WHERE rowid IN (SELECT rowid FROM frames WHERE ts < ? LIMIT ?)`
	discardChunksSQL    = `DELETE FROM chunks WHERE rowid IN (SELECT rowid FROM chunks WHERE ts < ? LIMIT ?)`
	discardTelemetrySQL = `DELETE FROM telemetry WHERE rowid IN (SELECT rowid FROM telemetry WHERE ts < ? LIMIT ?)`
)

// OldestRaw is the timestamp of the oldest raw row (frame, chunk or
// telemetry sample); ok is false when there is none. Rows arrive in time
// order, so the first row of each table is its oldest.
func (s *Store) OldestRaw() (time.Time, bool, error) {
	var oldest time.Time
	for _, q := range []string{
		`SELECT ts FROM frames ORDER BY rowid LIMIT 1`,
		`SELECT ts FROM chunks ORDER BY rowid LIMIT 1`,
		`SELECT MIN(ts) FROM telemetry`,
	} {
		var tsText sql.NullString
		err := s.db.QueryRow(q).Scan(&tsText)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !tsText.Valid) {
			continue
		}
		if err != nil {
			return time.Time{}, false, fmt.Errorf("store: oldest raw: %w", err)
		}
		ts, err := decTS(tsText.String)
		if err != nil {
			return time.Time{}, false, err
		}
		if oldest.IsZero() || ts.Before(oldest) {
			oldest = ts
		}
	}
	return oldest, !oldest.IsZero(), nil
}

// DiscardRawBefore deletes the raw rows older than cutoff — frames,
// chunks and telemetry, open sessions included. It rolls up first and
// never deletes telemetry the rollup has not covered, so the history
// survives as 15-minute buckets. Deletes run in bounded batches.
func (s *Store) DiscardRawBefore(cutoff time.Time) (DiscardReport, error) {
	var rep DiscardReport
	if _, err := s.RollUp(); err != nil {
		return rep, err
	}
	wm, err := s.lastBucket()
	if err != nil {
		return rep, err
	}
	var teleCut time.Time // zero: nothing rolled up, keep every sample
	if wm > 0 {
		teleCut = cutoff
		if wmEnd := time.Unix(wm, 0).UTC().Add(bucketLen); wmEnd.Before(teleCut) {
			teleCut = wmEnd
		}
	}
	c := encTS(cutoff)
	if rep.Frames, err = s.deleteBatched(discardFramesSQL, c); err != nil {
		return rep, err
	}
	if rep.Chunks, err = s.deleteBatched(discardChunksSQL, c); err != nil {
		return rep, err
	}
	if !teleCut.IsZero() {
		if rep.Telemetry, err = s.deleteBatched(discardTelemetrySQL, encTS(teleCut)); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// deleteBatched runs one batched delete to completion, checkpointing the
// WAL between batches so it does not grow with the delete.
func (s *Store) deleteBatched(q, cutoff string) (int, error) {
	total := 0
	for {
		res, err := s.db.Exec(q, cutoff, discardBatch)
		if err != nil {
			return total, fmt.Errorf("store: discard: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("store: discard: %w", err)
		}
		total += int(n)
		if n < discardBatch {
			return total, nil
		}
		if _, err := s.db.Exec(`PRAGMA wal_checkpoint(PASSIVE)`); err != nil {
			return total, fmt.Errorf("store: discard checkpoint: %w", err)
		}
	}
}

// Reclaim gives freed space back to the filesystem: it truncates the WAL
// and, with incremental auto-vacuum, releases the free pages.
func (s *Store) Reclaim() error {
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("store: reclaim checkpoint: %w", err)
	}
	mode, err := s.autoVacuum()
	if err != nil {
		return err
	}
	if mode == autoVacuumIncremental {
		// the pragma frees one page per step: drain it as a query (an
		// Exec steps it once)
		rows, err := s.db.Query(`PRAGMA incremental_vacuum`)
		if err != nil {
			return fmt.Errorf("store: incremental vacuum: %w", err)
		}
		for rows.Next() {
		}
		if err := closeRows(rows, rows.Err()); err != nil {
			return fmt.Errorf("store: incremental vacuum: %w", err)
		}
		if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			return fmt.Errorf("store: reclaim checkpoint: %w", err)
		}
	}
	return nil
}

// EnableIncrementalVacuum converts a database created without
// auto-vacuum (before 2026-11-17) when free bytes leave room for the
// rewrite (twice the database plus headroom); it reports whether it
// converted. A VACUUM rewrites the whole file, so it runs at startup
// only, never under disk pressure.
func (s *Store) EnableIncrementalVacuum(free uint64) (bool, error) {
	mode, err := s.autoVacuum()
	if err != nil || mode == autoVacuumIncremental {
		return false, err
	}
	var pages, size uint64
	if err := s.db.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		return false, err
	}
	if err := s.db.QueryRow(`PRAGMA page_size`).Scan(&size); err != nil {
		return false, err
	}
	if free < 2*pages*size+vacuumHeadroom {
		return false, nil
	}
	if _, err := s.db.Exec(`PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
		return false, fmt.Errorf("store: auto_vacuum: %w", err)
	}
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return false, fmt.Errorf("store: vacuum: %w", err)
	}
	// in WAL mode the rewrite lands in the WAL: fold it back now
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return true, fmt.Errorf("store: vacuum checkpoint: %w", err)
	}
	return true, nil
}

func (s *Store) autoVacuum() (int, error) {
	var mode int
	if err := s.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return 0, fmt.Errorf("store: auto_vacuum: %w", err)
	}
	return mode, nil
}
