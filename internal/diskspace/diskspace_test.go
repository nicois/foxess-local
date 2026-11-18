package diskspace

import "testing"

// TestThreshold: the critical level is the configured floor, capped at a
// tenth of the filesystem (a 256 MB flash must not be "critical" forever).
func TestThreshold(t *testing.T) {
	const mb = 1 << 20
	cases := []struct{ minFree, total, want uint64 }{
		{200 * mb, 32 << 30, 200 * mb},      // a big disk: the floor
		{200 * mb, 256 * mb, 256 * mb / 10}, // small flash: 10 %
		{0, 32 << 30, 0},
	}
	for _, c := range cases {
		if got := Threshold(c.minFree, c.total); got != c.want {
			t.Errorf("Threshold(%d, %d) = %d, want %d", c.minFree, c.total, got, c.want)
		}
	}
}

// TestFree: the filesystem holding a directory reports space.
func TestFree(t *testing.T) {
	free, total, err := Free(t.TempDir())
	if err != nil {
		t.Skipf("statfs unsupported here: %v", err)
	}
	if total == 0 || free > total {
		t.Fatalf("free %d total %d", free, total)
	}
}
