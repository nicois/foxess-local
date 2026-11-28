package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestNewConnWaitsForWriteLock pins the DSN's pragma order: a pool
// connection opened while another holds the write lock waits for it
// (busy_timeout) instead of failing at once with SQLITE_BUSY. With
// busy_timeout last, the earlier pragmas ran without it and the
// flusher's batch was lost ("store: begin batch: database is locked",
// CI on the v0.6.0-rc.5 release run, the smoke's 4-connection pool).
//
// The lock is held for lockHold of real time: SQLite's lock is not a Go
// timer, so synctest cannot stand in. With the fault the open fails at
// once; a slow runner can only make the test pass, never fail.
func TestNewConnWaitsForWriteLock(t *testing.T) {
	const lockHold = 200 * time.Millisecond
	s, err := OpenWithMaxOpen(filepath.Join(t.TempDir(), "store.db"), 4, testVersion)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	holder, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	time.AfterFunc(lockHold, func() {
		_, err := holder.ExecContext(ctx, "COMMIT")
		_ = holder.Close()
		released <- err
	})
	c, err := s.db.Conn(ctx) // a fresh pool connection: its DSN pragmas run now
	if err != nil {
		t.Fatalf("a new connection while the write lock is held: %v", err)
	}
	_ = c.Close()
	if err := <-released; err != nil {
		t.Fatal(err)
	}
}
