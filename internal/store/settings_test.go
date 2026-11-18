package store

import "testing"

// TestSettings: a named setting round-trips, is replaced on re-set, and
// is absent after delete.
func TestSettings(t *testing.T) {
	s := newTestStore(t)
	if _, ok, err := s.Setting("write_override"); ok || err != nil {
		t.Fatalf("unset: ok %v err %v", ok, err)
	}
	for _, v := range []string{`{"a":1}`, `{"a":2}`} {
		if err := s.SetSetting("write_override", v); err != nil {
			t.Fatal(err)
		}
	}
	if v, ok, err := s.Setting("write_override"); !ok || err != nil || v != `{"a":2}` {
		t.Fatalf("get: %q %v %v", v, ok, err)
	}
	if err := s.DeleteSetting("write_override"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Setting("write_override"); ok {
		t.Fatal("present after delete")
	}
}
