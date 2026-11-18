package quirks

import "testing"

// TestRegistry: every quirk has a unique id, its texts, and a key that
// maps back to it; unknown keys do not.
func TestRegistry(t *testing.T) {
	seen := map[string]bool{}
	for _, q := range All() {
		if q.ID == "" || q.Title == "" || q.Effect == "" || q.Seen == "" || seen[q.ID] {
			t.Errorf("quirk %+v", q)
		}
		seen[q.ID] = true
		if got, ok := FromKey(Key(q.ID)); !ok || got.ID != q.ID {
			t.Errorf("FromKey(%s) = %+v %v", Key(q.ID), got, ok)
		}
	}
	for _, k := range []string{"quirks.nope", EPSDropouts, "update.prerelease"} {
		if _, ok := FromKey(k); ok {
			t.Errorf("FromKey(%q) matched", k)
		}
	}
	if q, _ := FromKey(Key(EPSDropouts)); q.Default {
		t.Error("eps_dropouts must default off until other installations report it")
	}
}
