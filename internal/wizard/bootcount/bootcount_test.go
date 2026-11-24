package bootcount

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTriggered(t *testing.T) {
	s := time.Second
	for _, c := range []struct {
		name  string
		prior []time.Duration // earlier boots' recorded uptimes, oldest first
		want  bool
	}{
		{"first boot", nil, false},
		{"one short boot", []time.Duration{30 * s}, false},
		{"two short boots", []time.Duration{30 * s, 10 * s}, true},
		{"unplugged before the first record", []time.Duration{0, 0}, true},
		{"after a long boot", []time.Duration{20 * time.Minute, 30 * s}, false},
		{"long, then two short", []time.Duration{20 * time.Minute, 30 * s, 10 * s}, true},
		{"a boot reached 120 s", []time.Duration{120 * s, 10 * s}, false},
		{"two boots just under 120 s", []time.Duration{119 * s, 119 * s}, true}, // 238 s + 2×30 s still fits 5 min
	} {
		if got := triggered(c.prior); got != c.want {
			t.Errorf("%s: triggered = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestBeginAndMark(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boots")
	b := Store{Path: path}
	// three replugs: the third boot sees two short ones before it
	for i, want := range []bool{false, false, true} {
		got, err := b.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("boot %d: triggered %v, want %v", i+1, got, want)
		}
		if err := b.Mark(15 * time.Second); err != nil {
			t.Fatal(err)
		}
	}
	// the triggering boot is consumed: the next replug doesn't re-trigger
	if got, err := b.Begin(); err != nil || got {
		t.Fatalf("boot after the trigger: %v %v", got, err)
	}
	// a long boot clears it all
	if err := b.Mark(11 * time.Minute); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if got, err := b.Begin(); err != nil || got {
			t.Fatalf("after a long boot: %v %v", got, err)
		}
	}
}

// TestCorruptFileIsAFirstBoot: a torn or garbled file (power lost mid
// write is the whole point of this file) never blocks start-up.
func TestCorruptFileIsAFirstBoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boots")
	if err := os.WriteFile(path, []byte("\x00garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Store{Path: path}.Begin()
	if err != nil || got {
		t.Fatalf("corrupt file: %v %v", got, err)
	}
}
