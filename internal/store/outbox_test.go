package store

import (
	"path/filepath"
	"testing"
	"time"
)

// TestOutboxSurvivesReopen (owner, 2026-11-18): a pending notice is in
// the database, so it outlives the process (a power cut); due, updated
// and deleted like the notifier needs.
func TestOutboxSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	s, err := openStore(path, nil, maxConns, fixedClock(testBase), testVersion)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.OutboxAdd(OutboxRow{
		Created: testBase, Event: "grid-lost", Severity: "critical",
		Title: "Grid down", Message: "on backup", Targets: 3, Next: testBase,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openStore(path, nil, maxConns, fixedClock(testBase), testVersion)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	due, err := s.OutboxDue(testBase)
	if err != nil || len(due) != 1 || due[0].ID != id || due[0].Message != "on backup" || due[0].Targets != 3 || !due[0].Created.Equal(testBase) {
		t.Fatalf("after reopen: %+v %v", due, err)
	}
	checkOutboxCycle(t, s, due[0])
}

// checkOutboxCycle: an update defers the notice; a delete removes it.
func checkOutboxCycle(t *testing.T, s *Store, row OutboxRow) {
	t.Helper()
	row.Targets, row.Attempts, row.Next = 2, 1, testBase.Add(time.Minute)
	if err := s.OutboxUpdate(row); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.OutboxDue(testBase); len(d) != 0 {
		t.Fatalf("not due yet: %+v", d)
	}
	if d, _ := s.OutboxDue(testBase.Add(time.Minute)); len(d) != 1 || d[0].Targets != 2 || d[0].Attempts != 1 {
		t.Fatalf("due again: %+v", d)
	}
	if err := s.OutboxDelete(row.ID); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.OutboxDue(testBase.Add(time.Hour)); len(d) != 0 {
		t.Fatalf("deleted: %+v", d)
	}
}
