// Package store is the foxesslocal SQLite persistence layer: the raw-byte
// frame audit (the C-001 trail), the prox capture-stream mirror, the
// telemetry projection, commands, audit events, and
// retention.
//
// The driver is modernc.org/sqlite (pure Go — C-006: no CGO). The store
// writes through a single connection — single-writer by design (the batch
// flusher), which keeps the pure-Go driver free of SQLITE_BUSY — and the
// API's list and history queries read through a separate read-only pool:
// in WAL a reader never blocks the writer (audit R4).
//
// Timestamps are stored as fixed-width UTC TEXT (encTS/decTS, tsLayout:
// RFC3339 with a 9-digit fraction always present). The fixed width is what
// makes lexicographic comparison of the ts columns time-correct (RFC3339Nano
// varies width with precision and would misorder same-second rows); the
// range queries (TelemetryRange, QueryAudit, the maintenance reaper) rely on that.
//
// The write path is a bounded batch queue: Record* methods enqueue, one
// flusher goroutine applies batches in enqueue order (a single
// transaction per batch), flushing on the batch-size threshold, the tick
// channel (250 ms in production, injected in tests), or a Flush request.
// A full queue blocks the caller — the frame hook is the only producer
// and backpressure is the intended fail-loud behaviour for an unhealthy
// store.
package store

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"foxesslocal/internal/diskspace"

	// The pure-Go SQLite driver (modernc.org/sqlite; C-006: no CGO).
	_ "modernc.org/sqlite"
)

// Store constants.
const (
	driver   = "sqlite"
	maxConns = 1
	// readConns is the read-only pool's size (the API's queries).
	readConns = 2
	batchCap  = 4096
)

// Store is the persistence handle. All methods are safe for concurrent
// use.
type Store struct {
	db *sql.DB
	// rdb is the read-only pool the API's list and history queries use:
	// in WAL a reader never blocks the write flusher (audit R4)
	rdb *sql.DB
	now func() time.Time // injected clock (default time.Now); tests fix it

	version string // the binary that opened this store (release traceability)

	batch    chan writeItem
	flushReq chan chan struct{}
	poke     chan struct{} // one-slot flush prompt (the flusher owns it)
	tick     <-chan time.Time
	ticker   *time.Ticker
	stop     chan struct{}
	done     chan struct{}

	// writeDelay is the write delay in ns (0 = flushInterval; SetWriteDelay)
	writeDelay atomic.Int64

	// enq / applied count enqueued and applied write items; Flush hands
	// off over these atomics because the flusher may run outside the
	// caller's synctest bubble (bubble channels are fatal to cross).
	enq, applied atomic.Int64

	streamWG sync.WaitGroup // in-flight persistStream goroutines
	maintWG  sync.WaitGroup // the maintenance loop (Close waits it)

	view *view // the newest values (QueryState's source; see view.go)
	// lastPoll is the newest register-poll telemetry (unix ns; PollFresh)
	lastPoll atomic.Int64
	// readChange is the OnRegisterReadChange hook (nil = none).
	readChange atomic.Pointer[func([]int)]

	mu       sync.Mutex
	closed   bool
	lastErr  error
	chunkSeq map[string]int
}

// Open opens (or creates) the database at path stamped with the
// version of the opening binary, applies the schema migration, and
// starts the batch flusher on the production 250 ms tick.
func Open(path, version string) (*Store, error) {
	return OpenWithMaxOpen(path, maxConns, version)
}

// OpenWithMaxOpen is Open with an explicit pool size. The test seam
// (synctest): a bubble reading the store concurrently with the
// root-group flusher needs headroom so the bubble's GetConn never
// parks on the pool — database/sql hands a waiting conn through a
// channel created by the waiter, and handing it across groups is a
// fatal error.
func OpenWithMaxOpen(path string, maxOpen int, version string) (*Store, error) {
	tk := time.NewTicker(flushInterval)
	s, err := openStore(path, tk.C, maxOpen, time.Now, version)
	if err != nil {
		tk.Stop()
		return nil, err
	}
	s.ticker = tk
	return s, nil
}

// synthesizeRestartConnections repairs what a crash leaves behind:
// persistStream writes the connections row only at stream end, so a
// process that died mid-session leaves frame/chunk/telemetry rows
// with no connections row — invisible to the close-based reaper, which
// only looks at connection rows (the old row-ts prune reached them; this
// must not regress that). Every orphan conn_id gets a finished
// connection with closed = now (the restart moment — when the stream
// stopped being written to) so the regular close-based reap takes the
// rows once the raw window passes. Idempotent.
func (s *Store) synthesizeRestartConnections() (int, error) {
	res, err := s.db.Exec(`
		INSERT OR IGNORE INTO connections
			(id, kind, sni, client, dst, opened, closed, cause, tls_host, tls_version, tls_result, evicted_chunks, version)
		SELECT t.conn_id, 'unknown', '', '', '', MIN(t.ts), ?, 'process-restart', '', 0, '', 0, ?
		FROM (
			SELECT conn_id, ts FROM frames
			UNION
			SELECT conn_id, ts FROM chunks
			UNION
			SELECT conn_id, ts FROM telemetry
		) t
		WHERE t.conn_id NOT IN (SELECT id FROM connections)
		GROUP BY t.conn_id`, encTS(s.now()), s.version)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// openStore is Open with an injected tick channel (nil = no tick),
// pool size, and clock.
func openStore(path string, tick <-chan time.Time, maxOpen int, now func() time.Time, version string) (*Store, error) {
	// auto_vacuum first: it only takes effect on a new database (before
	// the WAL switch writes its header) or at the next VACUUM
	// (EnableIncrementalVacuum); on an existing database it is a no-op.
	dsn := "file:" + path + "?_pragma=auto_vacuum(INCREMENTAL)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(maxOpen)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}
	if err := migrate(db, path); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate %s: %w", path, err)
	}
	rdb, err := sql.Open(driver, "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: open %s read-only: %w", path, err)
	}
	rdb.SetMaxOpenConns(readConns)
	s := &Store{
		db:       db,
		rdb:      rdb,
		now:      now,
		version:  version,
		batch:    make(chan writeItem, batchCap),
		flushReq: make(chan chan struct{}),
		poke:     make(chan struct{}, 1),
		tick:     tick,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		chunkSeq: make(map[string]int),
		view:     newView(),
	}
	if err := s.seedView(); err != nil {
		_ = s.db.Close()
		return nil, fmt.Errorf("store: seed %s: %w", path, err)
	}
	if err := s.repairWrappedPV(); err != nil {
		_ = s.db.Close()
		return nil, fmt.Errorf("store: repair PV history: %w", err)
	}
	if n, err := s.synthesizeRestartConnections(); err != nil {
		_ = s.db.Close()
		return nil, fmt.Errorf("store: restart synthesis: %w", err)
	} else if n > 0 {
		slog.Info("store: synthesized connection row(s) for the crashed session(s) before this start", "count", n)
	}
	go s.flusher()
	return s, nil
}

// Close drains the write queue, stops the flusher, and closes the
// database. Idempotent. Record* after Close panics (programmer error:
// producers must stop before Close); capture streams that finish after
// Close are dropped by the same guard, so the host must quiesce the
// registry before closing the store.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	if s.ticker != nil {
		s.ticker.Stop()
	}
	ch := make(chan struct{})
	s.flushReq <- ch
	<-ch
	close(s.stop)
	<-s.done
	s.maintWG.Wait()
	_ = s.rdb.Close()
	err := s.db.Close()
	s.mu.Lock()
	if err == nil {
		err = s.lastErr
	}
	s.mu.Unlock()
	return err
}

// checkOpen panics if the store is closed; see Close.
func (s *Store) checkOpen(op string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		panic("store: " + op + " after Close")
	}
}

// Err is the first batch-write failure, nil while every write has landed
// (an upgrade is not confirmed on a store that cannot write).
func (s *Store) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// setLastErr records the first batch-write failure (Close surfaces it)
// and logs it.
func (s *Store) setLastErr(err error) {
	s.mu.Lock()
	if s.lastErr == nil {
		s.lastErr = err
	}
	s.mu.Unlock()
	slog.Error("store: batch write failed", "err", err)
}

// tsLayout is the fixed-width timestamp layout: UTC RFC3339 with the
// nanosecond fraction always present (9 digits). Fixed width is what makes
// lexicographic comparison of the ts columns time-correct for every
// precision mix: RFC3339Nano varies width with precision, so a row at
// "…T12:00:00.000000001Z" sorts BEFORE the same second's "…T12:00:00Z",
// and ts >= since would silently miss rows inside since's own second.
const tsLayout = "2006-01-02T15:04:05.000000000Z"

// encTS encodes a timestamp as fixed-width UTC TEXT (tsLayout); the zero
// time encodes to the empty string (the open-connection sentinel).
func encTS(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(tsLayout)
}

// decTS is the inverse of encTS; the empty string decodes to the zero
// time.
func decTS(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	return time.Parse(tsLayout, v)
}

// schemaV1 is the base migration: all seven tables plus the four declared
// indexes.
const schemaV1 = `
CREATE TABLE IF NOT EXISTS connections(
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  sni TEXT NOT NULL,
  client TEXT NOT NULL,
  dst TEXT NOT NULL,
  opened TEXT NOT NULL,
  closed TEXT NOT NULL,
  cause TEXT NOT NULL,
  tls_host TEXT NOT NULL,
  tls_version INTEGER NOT NULL,
  tls_result TEXT NOT NULL,
  evicted_chunks INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS chunks(
  conn_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  dir TEXT NOT NULL,
  ts TEXT NOT NULL,
  len INTEGER NOT NULL,
  decision TEXT NOT NULL,
  reason TEXT NOT NULL,
  origin TEXT NOT NULL,
  payload BLOB
);
CREATE TABLE IF NOT EXISTS frames(
  conn_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  dir TEXT NOT NULL,
  off INTEGER NOT NULL,
  ts TEXT NOT NULL,
  class TEXT NOT NULL,
  dev TEXT NOT NULL,
  fn INTEGER NOT NULL,
  plen INTEGER NOT NULL,
  payload BLOB NOT NULL,
  decision TEXT NOT NULL,
  reason TEXT NOT NULL,
  origin TEXT NOT NULL,
  decoded TEXT
);
CREATE INDEX IF NOT EXISTS idx_frames_dir ON frames(dir);
CREATE INDEX IF NOT EXISTS idx_frames_conn_seq ON frames(conn_id, seq);
CREATE TABLE IF NOT EXISTS telemetry(
  conn_id TEXT NOT NULL,
  ts TEXT NOT NULL,
  sol INTEGER NOT NULL,
  grid INTEGER NOT NULL,
  haus INTEGER NOT NULL,
  acport INTEGER NOT NULL,
  batt INTEGER NOT NULL,
  soc INTEGER NOT NULL,
  charge_state INTEGER NOT NULL,
  idnt_grid INTEGER NOT NULL,
  idnt_ac INTEGER NOT NULL,
  haus_dup INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_telemetry_conn_ts ON telemetry(conn_id, ts);
CREATE TABLE IF NOT EXISTS commands(
  id TEXT PRIMARY KEY,
  ts TEXT NOT NULL,
  kind TEXT NOT NULL,
  params TEXT NOT NULL,
  status TEXT NOT NULL,
  frame_seq INTEGER NOT NULL,
  ack INTEGER NOT NULL,
  error TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS audit_events(
  ts TEXT NOT NULL,
  kind TEXT NOT NULL,
  detail TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_events(ts);
`

// schemaV2 adds the 15-minute rollup table (2026-11-12 dashboard iteration).
const schemaV2 = `
CREATE TABLE IF NOT EXISTS buckets15m(
  bucket      INTEGER PRIMARY KEY,
  sol_avg INTEGER NOT NULL, sol_min INTEGER NOT NULL, sol_max INTEGER NOT NULL, sol_wh INTEGER NOT NULL,
  haus_avg INTEGER NOT NULL, haus_min INTEGER NOT NULL, haus_max INTEGER NOT NULL, haus_wh INTEGER NOT NULL,
  grid_avg INTEGER NOT NULL, grid_min INTEGER NOT NULL, grid_max INTEGER NOT NULL,
  grid_imp_wh INTEGER NOT NULL, grid_exp_wh INTEGER NOT NULL,
  batt_avg INTEGER NOT NULL, batt_min INTEGER NOT NULL, batt_max INTEGER NOT NULL,
  batt_chg_wh INTEGER NOT NULL, batt_dis_wh INTEGER NOT NULL,
  acport_avg INTEGER NOT NULL,
  soc_last INTEGER NOT NULL
);
`

// schemaV3 adds connections.version: the binary that persisted the row
// (release traceability, D-018) — stored data is attributable to a
// build; rows written before the upgrade read 'unknown'.
const schemaV3 = `
ALTER TABLE connections ADD COLUMN version TEXT NOT NULL DEFAULT 'unknown';
`

// schemaV4 adds registers: the latest value read from each inverter
// holding register (the settings poll — work mode, limits, meter
// compensation, scheduler). One row per address, replaced on each read.
const schemaV4 = `
CREATE TABLE IF NOT EXISTS registers(
  addr  INTEGER PRIMARY KEY,
  value INTEGER NOT NULL,
  ts    TEXT NOT NULL
);
`

// schemaV5 adds settings: small named operator settings that must
// survive restarts (the C-008 write override).
const schemaV5 = `
CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY, value TEXT NOT NULL);
`

// schemaV6 indexes the newest-first reads (FramesPage): one range per
// (dir, decision) slice, or per class, read from its newest end, so a
// page costs its size whether the filter is common or rare — a ts-first
// index made a rare filter walk the whole log (1.2 M frames for 30 days).
// (dir, decision, ts) supersedes the v1 dir index. telemetry(ts) serves
// the history windows and the newest sample at open.
const schemaV6 = `
DROP INDEX IF EXISTS idx_frames_dir;
CREATE INDEX IF NOT EXISTS idx_frames_dir_dec_ts ON frames(dir, decision, ts);
CREATE INDEX IF NOT EXISTS idx_frames_class_ts ON frames(class, ts);
CREATE INDEX IF NOT EXISTS idx_telemetry_ts ON telemetry(ts);
`

// schemaV7 stores the EPS (backup / UPS) port power with each telemetry
// sample (the register poll's 31047/31048; 0 for the 150 B frame).
const schemaV7 = `
ALTER TABLE telemetry ADD COLUMN eps INTEGER NOT NULL DEFAULT 0;
`

// schemaV8 un-wraps PV power stored by the unsigned decode of the KH
// PV power registers (fixed 2026-11-17): a string reading a few watts
// negative at dawn or dusk was stored as ~65 535 W. No KH sum reaches
// 32 768 W, so every such value is a wrap — 65 536 per wrapped string —
// and the un-wrapped total (a few W of standby draw) is stored as 0, as
// the fixed decode now reports it. The buckets built from them are
// recomputed at open (repairWrappedPV).
const schemaV8 = `
UPDATE telemetry SET sol = MAX(0, sol - 65536 * ((sol + 32768) / 65536)) WHERE sol >= 32768;
`

// schemaV9 stores the register poll's temperatures and BMS charge limit
// (tenths) with each sample and their 15-minute averages; NULL where a
// sample carries none (the 150 B frame).
const schemaV9 = `
ALTER TABLE telemetry ADD COLUMN t_batt INTEGER;
ALTER TABLE telemetry ADD COLUMN t_inv INTEGER;
ALTER TABLE telemetry ADD COLUMN t_amb INTEGER;
ALTER TABLE telemetry ADD COLUMN bms_chg INTEGER;
ALTER TABLE buckets15m ADD COLUMN t_batt_avg INTEGER;
ALTER TABLE buckets15m ADD COLUMN t_inv_avg INTEGER;
ALTER TABLE buckets15m ADD COLUMN t_amb_avg INTEGER;
ALTER TABLE buckets15m ADD COLUMN bms_chg_avg INTEGER;
`

// schemaV10 stores the BMS's lowest / highest cell temperature (0.1 °C,
// from the ~5-minute snap61, held on the live samples while fresh) and
// their 15-minute averages.
const schemaV10 = `
ALTER TABLE telemetry ADD COLUMN t_cell_min INTEGER;
ALTER TABLE telemetry ADD COLUMN t_cell_max INTEGER;
ALTER TABLE buckets15m ADD COLUMN t_cell_min_avg INTEGER;
ALTER TABLE buckets15m ADD COLUMN t_cell_max_avg INTEGER;
`

// schemaV11 is the notification outbox (owner, 2026-11-18): notices
// waiting for delivery survive a restart — after a power cut the network
// and the internet come back long after foxess-local does.
const schemaV11 = `
CREATE TABLE IF NOT EXISTS notify_outbox (
  id       INTEGER PRIMARY KEY,
  created  TEXT NOT NULL,
  event    TEXT NOT NULL,
  severity TEXT NOT NULL,
  title    TEXT NOT NULL,
  message  TEXT NOT NULL,
  targets  INTEGER NOT NULL,
  attempts INTEGER NOT NULL,
  next_try TEXT NOT NULL
);
`

// wrappedPVMin is the bucket sol_max that can only come from a wrapped
// PV reading.
const wrappedPVMin = 32768

// repairWrappedPV recomputes every 15-minute bucket that still carries a
// wrapped PV maximum from its raw samples (un-wrapped by schema v8); a
// bucket whose raw samples are gone is left as it is.
func (s *Store) repairWrappedPV() error {
	rows, err := s.db.Query(`SELECT bucket FROM buckets15m WHERE sol_max >= ?`, wrappedPVMin)
	if err != nil {
		return err
	}
	var buckets []int64
	for rows.Next() {
		var b int64
		if err := rows.Scan(&b); err != nil {
			_ = rows.Close()
			return err
		}
		buckets = append(buckets, b)
	}
	if err := closeRows(rows, rows.Err()); err != nil {
		return err // an iteration error must not truncate the repair silently
	}
	fixed := 0
	for _, b := range buckets {
		from := time.Unix(b, 0).UTC()
		a, ok, err := s.windowAgg(from, from.Add(bucketLen))
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if _, err := s.db.Exec(upsertSQL, upsertArgs(b, a)...); err != nil {
			return err
		}
		fixed++
	}
	if fixed > 0 {
		slog.Info("store: recomputed history buckets with wrapped PV readings", "buckets", fixed)
	}
	return nil
}

// migrate applies the schema, guarded by schema_migrations; idempotent on
// an existing database. Each version applies only when the recorded
// version is below it, so a v1 database upgrades in place.
func migrate(db *sql.DB, path string) error {
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER NOT NULL)"); err != nil {
		return err
	}
	var n int
	if err := db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&n); err != nil {
		return err
	}
	migrations := []struct {
		version int
		ddl     string
	}{
		{1, schemaV1},
		{2, schemaV2},
		{3, schemaV3},
		{4, schemaV4},
		{5, schemaV5},
		{6, schemaV6},
		{7, schemaV7},
		{8, schemaV8},
		{9, schemaV9},
		{10, schemaV10},
		{11, schemaV11},
	}
	latest := migrations[len(migrations)-1].version
	if n > latest {
		// a newer binary wrote it (this one is a rollback): running old
		// code against a newer schema is never safe (install review G3)
		return fmt.Errorf("store: the database is schema v%d, newer than this version knows (v%d): "+
			"it was written by a newer foxess-local — run that version again, or restore %s.pre-v%d", n, latest, path, n)
	}
	if n > 0 && n < latest {
		snapshotBeforeMigrating(db, path, latest)
	}
	for _, m := range migrations {
		if n < m.version {
			if err := migrateStep(db, m.version, m.ddl); err != nil {
				return err
			}
		}
	}
	return nil
}

// migrateStep applies one migration and records its version in one
// transaction: a step either lands whole or not at all.
func migrateStep(db *sql.DB, version int, ddl string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: migration %d: %w", version, err)
	}
	if _, err := tx.Exec(ddl); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: migration %d: %w", version, err)
	}
	if _, err := tx.Exec("INSERT INTO schema_migrations(version) VALUES (?)", version); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: record migration %d: %w", version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: migration %d: %w", version, err)
	}
	return nil
}

// snapshotHeadroom is the free space kept beyond a pre-migration copy.
const snapshotHeadroom = 64 << 20

// snapshotBeforeMigrating keeps a copy of the database as it was before a
// schema change (<path>.pre-v<latest>; earlier copies replaced), when the
// disk has room for it: a rollback to the previous binary then has its
// schema to return to (install review G3). Best effort — a missing copy
// never blocks the start.
func snapshotBeforeMigrating(db *sql.DB, path string, latest int) {
	snap := fmt.Sprintf("%s.pre-v%d", path, latest)
	var pages, size uint64
	if db.QueryRow(`PRAGMA page_count`).Scan(&pages) != nil || db.QueryRow(`PRAGMA page_size`).Scan(&size) != nil {
		return
	}
	if free, _, err := diskspace.Free(filepath.Dir(path)); err == nil && free < pages*size+snapshotHeadroom {
		slog.Warn("store: not enough free space for a copy before migrating", "need_mb", (pages*size+snapshotHeadroom)/diskspace.MiB)
		return
	}
	old, _ := filepath.Glob(path + ".pre-v*")
	for _, f := range old {
		_ = os.Remove(f)
	}
	if _, err := db.Exec(`VACUUM INTO ?`, snap); err != nil {
		slog.Warn("store: copy before migrating failed", "err", err)
		return
	}
	slog.Info("store: kept a copy of the database before migrating", "copy", snap)
}
