package store

import (
	"context"
	"database/sql"
	"fmt"
)

// childTables are the connection-keyed tables reaped with their
// connection. audit_events and commands carry no conn_id and are not
// reaped here.
var childTables = []string{"chunks", "frames", "telemetry"}

// reapFinished deletes finished connections (closed != "" and closed <
// cutoff) and all their child rows, returning the per-table counts. The
// cutoff is chosen by Maintain (raw window, clamped by the rollup
// watermark). Open (still-written) connections are never touched:
// retention runs from close, the moment the stream stopped being written
// to.
func (s *Store) reapFinished(cutoff string) (conns, chunks, frames, tele int, err error) {
	ids, err := s.finishedConnectionIDs(cutoff)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if len(ids) == 0 {
		return 0, 0, 0, 0, nil
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("store: reap: %w", err)
	}
	for _, id := range ids {
		for _, table := range childTables {
			n, err := deleteChildRows(tx, table, id)
			if err != nil {
				_ = tx.Rollback()
				return 0, 0, 0, 0, err
			}
			switch table {
			case "chunks":
				chunks += n
			case "frames":
				frames += n
			case "telemetry":
				tele += n
			}
		}
	}
	n, err := tx.Exec("DELETE FROM connections WHERE closed != '' AND closed < ?", cutoff)
	if err != nil {
		_ = tx.Rollback()
		return 0, 0, 0, 0, fmt.Errorf("store: reap: %w", err)
	}
	affected, err := n.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return 0, 0, 0, 0, fmt.Errorf("store: reap: %w", err)
	}
	conns = int(affected)
	if err := tx.Commit(); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("store: reap: %w", err)
	}
	return conns, chunks, frames, tele, nil
}

// finishedConnectionIDs lists the finished connections closed before
// cutoff.
func (s *Store) finishedConnectionIDs(cutoff string) ([]string, error) {
	rows, err := s.db.Query(
		"SELECT id FROM connections WHERE closed != '' AND closed < ?", cutoff)
	if err != nil {
		return nil, fmt.Errorf("store: reap: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, closeRows(rows, err)
		}
		ids = append(ids, id)
	}
	return ids, closeRows(rows, rows.Err())
}

// deleteChildRows removes one connection's rows from a child table and
// returns the count.
func deleteChildRows(tx *sql.Tx, table, connID string) (int, error) {
	//nolint:gosec // G202: table comes from the fixed childTables list.
	n, err := tx.Exec(
		"DELETE FROM "+table+" WHERE conn_id = ?", connID)
	if err != nil {
		return 0, fmt.Errorf("store: reap %s: %w", table, err)
	}
	affected, err := n.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: reap %s: %w", table, err)
	}
	return int(affected), nil
}
