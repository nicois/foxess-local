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
	Origin                          string // an automation's token name ("" = foxess-local)
	Targets                         int    // bit set of the targets still to reach
	Attempts                        int
	Next                            time.Time
}

// OutboxAdd stores a new pending notice and returns its id. Written at
// once (not through the batch queue): a notice is rare and must survive
// a power cut that follows it.
func (s *Store) OutboxAdd(r OutboxRow) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO notify_outbox
		(created, event, severity, title, message, origin, targets, attempts, next_try)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		encTS(r.Created), r.Event, r.Severity, r.Title, r.Message, r.Origin, r.Targets, r.Attempts, encTS(r.Next))
	if err != nil {
		return 0, fmt.Errorf("store: outbox add: %w", err)
	}
	return res.LastInsertId()
}

// OutboxDue lists the pending notices due at now, oldest first.
func (s *Store) OutboxDue(now time.Time) ([]OutboxRow, error) {
	rows, err := s.db.Query(`SELECT id, created, event, severity, title, message, origin, targets, attempts, next_try
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
		if err := rows.Scan(&r.ID, &created, &r.Event, &r.Severity, &r.Title, &r.Message, &r.Origin, &r.Targets, &r.Attempts, &next); err != nil {
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

// NoticeRow is one notice as kept for the web UI's list (D-030).
type NoticeRow struct {
	ID                              int64
	TS                              time.Time
	Origin                          string // an automation's token name ("" = foxess-local)
	Event, Severity, Title, Message string
}

// Notice retention: the newest noticesKeep, none older than noticesDays.
const (
	noticesKeep = 500
	noticesDays = 30
)

// NoticeAdd keeps one notice and prunes the list (written at once: rare,
// and the list is what the owner reads after an outage).
func (s *Store) NoticeAdd(r NoticeRow) error {
	if _, err := s.db.Exec(`INSERT INTO notices (ts, origin, event, severity, title, message) VALUES (?, ?, ?, ?, ?, ?)`,
		encTS(r.TS), r.Origin, r.Event, r.Severity, r.Title, r.Message); err != nil {
		return fmt.Errorf("store: notice add: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM notices WHERE ts < ? OR id <= (SELECT MAX(id) FROM notices) - ?`,
		encTS(r.TS.AddDate(0, 0, -noticesDays)), noticesKeep); err != nil {
		return fmt.Errorf("store: notice prune: %w", err)
	}
	return nil
}

// Notices lists the kept notices before id (0: the newest), newest
// first, at most limit.
func (s *Store) Notices(before int64, limit int) ([]NoticeRow, error) {
	q := `SELECT id, ts, origin, event, severity, title, message FROM notices`
	args := []any{}
	if before > 0 {
		q += ` WHERE id < ?`
		args = append(args, before)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: notices: %w", err)
	}
	out := []NoticeRow{}
	for rows.Next() {
		var (
			r  NoticeRow
			ts string
		)
		if err := rows.Scan(&r.ID, &ts, &r.Origin, &r.Event, &r.Severity, &r.Title, &r.Message); err != nil {
			return nil, closeRows(rows, err)
		}
		if err := setTS(&r.TS, ts); err != nil {
			return nil, closeRows(rows, err)
		}
		out = append(out, r)
	}
	return out, closeRows(rows, rows.Err())
}
