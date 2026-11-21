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
		Title: "Grid down", Message: "on backup", Origin: testOrigin, Targets: 3, Next: testBase,
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
	if err != nil || len(due) != 1 || due[0].ID != id || due[0].Message != "on backup" || due[0].Origin != testOrigin || due[0].Targets != 3 || !due[0].Created.Equal(testBase) {
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

// testOrigin is an automation's token name.
const testOrigin = "aemo-spike"

// TestNotices (D-030): notices are kept newest first and paged by id;
// the list keeps noticesKeep and nothing older than noticesDays.
func TestNotices(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "s.db"), nil, maxConns, fixedClock(testBase), testVersion)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	seedNotices(t, s)
	page, err := s.Notices(0, 10)
	if err != nil || len(page) != 10 || page[0].ID <= page[1].ID || page[0].Origin != testOrigin ||
		!page[0].TS.Equal(testBase.Add((noticesKeep+1)*time.Second)) {
		t.Fatalf("first page %+v %v", page, err)
	}
	all, err := s.Notices(0, 10*noticesKeep)
	if err != nil || len(all) != noticesKeep {
		t.Fatalf("kept %d %v", len(all), err)
	}
	for _, r := range all {
		if r.Title == "Old" {
			t.Fatal("the 31-day-old notice was kept")
		}
	}
	next, err := s.Notices(page[9].ID, 10)
	if err != nil || len(next) != 10 || next[0].ID >= page[9].ID {
		t.Fatalf("next page %+v %v", next, err)
	}
}

// seedNotices adds a notice older than noticesDays, then noticesKeep+2
// a second apart.
func seedNotices(t *testing.T, s *Store) {
	t.Helper()
	old := NoticeRow{TS: testBase.AddDate(0, 0, -noticesDays-1), Event: "alert", Severity: "warning", Title: "Old", Message: "gone"}
	if err := s.NoticeAdd(old); err != nil {
		t.Fatal(err)
	}
	for i := range noticesKeep + 2 {
		r := NoticeRow{TS: testBase.Add(time.Duration(i) * time.Second), Origin: testOrigin, Event: "sidecar.price-spike", Severity: "info", Title: "Spike", Message: "m"}
		if err := s.NoticeAdd(r); err != nil {
			t.Fatal(err)
		}
	}
}
