// Package led drives the OpenStick's three on/off LEDs (design D-031,
// "The light"): red:power, green:wlan, blue:wan under /sys/class/leds.
// Blinking uses the kernel's timer trigger, so a pattern keeps running
// if the wizard stalls.
package led

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// Root is where the kernel exposes the LEDs.
const Root = "/sys/class/leds"

// Mode is one LED's state.
type Mode int

// The modes. Slow and Fast are the design's 1 s and 0.25 s blinks.
const (
	Off Mode = iota
	On
	Slow
	Fast
)

const (
	slowMs = 1000
	fastMs = 250
)

// The UZ801's LEDs and the sysfs attributes the driver writes.
const (
	Red   = "red:power"
	Green = "green:wlan"
	Blue  = "blue:wan"

	attrTrigger    = "trigger"
	attrBrightness = "brightness"
	attrDelayOn    = "delay_on"
	attrDelayOff   = "delay_off"
	trigNone       = "none"
	trigTimer      = "timer"
)

// Pattern is what all three LEDs show.
type Pattern struct {
	Red, Green, Blue Mode
}

// Driver sets patterns. It remembers each LED's mode and writes only
// the LEDs that change, so a blink isn't restarted.
type Driver struct {
	root string
	mu   sync.Mutex
	last map[string]Mode // LED name → its current mode; missing: unknown
}

// New drives the LEDs under root (Root on the stick).
func New(root string) *Driver {
	return &Driver{root: root, last: map[string]Mode{}}
}

// Set shows p. It sets every LED it can and reports those it couldn't.
func (d *Driver) Set(p Pattern) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var errs []error
	for _, l := range []struct {
		name string
		mode Mode
	}{{Red, p.Red}, {Green, p.Green}, {Blue, p.Blue}} {
		if m, ok := d.last[l.name]; ok && m == l.mode {
			continue
		}
		if err := d.set(l.name, l.mode); err != nil {
			delete(d.last, l.name)
			errs = append(errs, err)
			continue
		}
		d.last[l.name] = l.mode
	}
	return errors.Join(errs...)
}

func (d *Driver) set(name string, m Mode) error {
	dir := filepath.Join(d.root, name)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("led %s: %w", name, err)
	}
	var period int
	switch m {
	case Off, On:
		if err := write(dir, attrTrigger, trigNone); err != nil {
			return fmt.Errorf("led %s: %w", name, err)
		}
		v := "0"
		if m == On {
			v = "1"
		}
		if err := write(dir, attrBrightness, v); err != nil {
			return fmt.Errorf("led %s: %w", name, err)
		}
		return nil
	case Slow:
		period = slowMs
	case Fast:
		period = fastMs
	}
	// the delay files exist once the timer trigger is on
	for _, w := range [][2]string{{attrTrigger, trigTimer}, {attrDelayOn, strconv.Itoa(period)}, {attrDelayOff, strconv.Itoa(period)}} {
		if err := write(dir, w[0], w[1]); err != nil {
			return fmt.Errorf("led %s: %w", name, err)
		}
	}
	return nil
}

func write(dir, file, v string) error {
	// sysfs attributes: no create, no truncate semantics to rely on
	f, err := os.OpenFile(filepath.Join(dir, file), os.O_WRONLY|os.O_TRUNC, 0) //nolint:gosec // G304: a fixed attribute under the LED root
	if err != nil {
		return err
	}
	_, werr := f.WriteString(v)
	cerr := f.Close()
	return errors.Join(werr, cerr)
}
