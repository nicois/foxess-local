package store

import (
	"fmt"
	"time"
)

// OutboxRow is one notice waiting for delivery (internal/notify's
// pending notice; the host adapts the two).
type OutboxRow struct {
	ID                              int64
	Created                         time.Time
	Event, Severity, Title, Message string
	Targets                         int // bit set of the targets still to reach
	Attempts                        int
	Next                            time.Time
}

// OutboxAdd stores a new pending notice and returns its id. Written at
// once (not through the batch queue): a notice is rare and must survive
// a power cut that follows it.
func (s *Store) OutboxAdd(r OutboxRow) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO notify_outbox
		(created, event, severity, title, message, targets, attempts, next_try)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		encTS(r.Created), r.Event, r.Severity, r.Title, r.Message, r.Targets, r.Attempts, encTS(r.Next))
	if err != nil {
		return 0, fmt.Errorf("store: outbox add: %w", err)
	}
	return res.LastInsertId()
}

// OutboxDue lists the pending notices due at now, oldest first.
func (s *Store) OutboxDue(now time.Time) ([]OutboxRow, error) {
	rows, err := s.db.Query(`SELECT id, created, event, severity, title, message, targets, attempts, next_try
		FROM notify_outbox WHERE next_try <= ? ORDER BY id`, encTS(now))
	if err != nil {
		return nil, fmt.Errorf("store: outbox due: %w", err)
	}
	var out []OutboxRow
	for rows.Next() {
		var (
			r             OutboxRow
			created, next string
		)
		if err := rows.Scan(&r.ID, &created, &r.Event, &r.Severity, &r.Title, &r.Message, &r.Targets, &r.Attempts, &next); err != nil {
			return nil, closeRows(rows, err)
		}
		if err := setTS(&r.Created, created); err != nil {
			return nil, closeRows(rows, err)
		}
		if err := setTS(&r.Next, next); err != nil {
			return nil, closeRows(rows, err)
		}
		out = append(out, r)
	}
	return out, closeRows(rows, rows.Err())
}

// OutboxUpdate records a delivery attempt (the targets left, the
// attempts and the next try).
func (s *Store) OutboxUpdate(r OutboxRow) error {
	_, err := s.db.Exec(`UPDATE notify_outbox SET targets = ?, attempts = ?, next_try = ? WHERE id = ?`,
		r.Targets, r.Attempts, encTS(r.Next), r.ID)
	if err != nil {
		return fmt.Errorf("store: outbox update: %w", err)
	}
	return nil
}

// OutboxDelete removes a delivered (or abandoned) notice.
func (s *Store) OutboxDelete(id int64) error {
	if _, err := s.db.Exec(`DELETE FROM notify_outbox WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: outbox delete: %w", err)
	}
	return nil
}
