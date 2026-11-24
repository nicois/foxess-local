package led

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSysfs is a /sys/class/leds with the UZ801's three LEDs. Writing
// "timer" to a trigger makes delay_on / delay_off appear, as the
// kernel does; the fake creates them up front, and the test checks
// the order of writes through the files' final contents.
func fakeSysfs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, n := range []string{Red, Green, Blue} {
		d := filepath.Join(root, n)
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
		for f, v := range map[string]string{attrTrigger: "[none] timer heartbeat", attrBrightness: "0", attrDelayOn: "500", attrDelayOff: "500"} {
			if err := os.WriteFile(filepath.Join(d, f), []byte(v), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func read(t *testing.T, root, led, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, led, file)) //nolint:gosec // G304: the test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestSet(t *testing.T) {
	root := fakeSysfs(t)
	d := New(root)
	if err := d.Set(Pattern{Red: Slow, Green: On, Blue: Off}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	for _, c := range []struct{ led, file, want string }{
		{Red, attrTrigger, trigTimer},
		{Red, attrDelayOn, "1000"},
		{Red, attrDelayOff, "1000"},
		{Green, attrTrigger, trigNone},
		{Green, attrBrightness, "1"},
		{Blue, attrTrigger, trigNone},
		{Blue, attrBrightness, "0"},
	} {
		if got := read(t, root, c.led, c.file); got != c.want {
			t.Errorf("%s/%s = %q, want %q", c.led, c.file, got, c.want)
		}
	}
	if err := d.Set(Pattern{Red: Off, Green: Fast, Blue: Off}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := read(t, root, Red, attrTrigger); got != trigNone {
		t.Errorf("red trigger %q after Off, want none", got)
	}
	if got := read(t, root, Green, attrDelayOn); got != "250" {
		t.Errorf("green delay_on %q, want 250", got)
	}
}

// TestSetUnchangedLeavesBlinkAlone: re-setting the same pattern writes
// nothing, so a blink keeps its phase instead of restarting.
func TestSetUnchangedLeavesBlinkAlone(t *testing.T) {
	root := fakeSysfs(t)
	d := New(root)
	p := Pattern{Red: Slow}
	if err := d.Set(p); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, Red, attrTrigger)
	if err := os.WriteFile(marker, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Set(p); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, Red, attrTrigger); got != "untouched" {
		t.Fatalf("an unchanged pattern was rewritten: trigger %q", got)
	}
}

// TestSetMissingLED: a stick without an LED reports it; the others
// are still set.
func TestSetMissingLED(t *testing.T) {
	root := fakeSysfs(t)
	if err := os.RemoveAll(filepath.Join(root, Blue)); err != nil {
		t.Fatal(err)
	}
	err := New(root).Set(Pattern{Green: On, Blue: On})
	if err == nil || !strings.Contains(err.Error(), Blue) {
		t.Fatalf("err %v, want one naming blue:wan", err)
	}
	if got := read(t, root, Green, attrBrightness); got != "1" {
		t.Fatalf("green not set when blue is missing: %q", got)
	}
}
