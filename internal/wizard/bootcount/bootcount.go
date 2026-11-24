// Package bootcount turns three unplug/replug cycles within five
// minutes into a hotspot request (design D-031, "When the hotspot
// opens"). The stick has no battery-backed clock, so it counts short
// boots: each boot's uptime is recorded on flash as it passes 10, 30,
// 60 and 120 s. Three replugs make two short boots; the third boot
// after them triggers.
package bootcount

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Path is where the records live.
const Path = "/var/lib/foxess-wizard/boots"

// Marks are the uptimes at which the loop records the current boot.
var Marks = []time.Duration{10 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute}

const (
	shortBoot = 2 * time.Minute // a boot whose record stays below this is short
	bootTime  = 30 * time.Second
	window    = 5 * time.Minute
	keep      = 3 // records kept: the two before the current boot, and it
	fileMode  = 0o600
)

// Store keeps the records in one small file, replaced atomically.
type Store struct{ Path string }

// Begin records a new boot and reports whether it is the trigger. A
// triggering boot is recorded as long, so one sequence triggers once.
func (s Store) Begin() (bool, error) {
	prior := s.read()
	trig := triggered(prior)
	rec := time.Duration(0)
	if trig {
		rec = shortBoot
	}
	prior = append(prior, rec)
	if len(prior) > keep {
		prior = prior[len(prior)-keep:]
	}
	return trig, s.write(prior)
}

// Mark records the current boot's uptime (the last record).
func (s Store) Mark(uptime time.Duration) error {
	recs := s.read()
	if len(recs) == 0 {
		recs = []time.Duration{0}
	}
	if recs[len(recs)-1] < uptime {
		recs[len(recs)-1] = uptime
	}
	return s.write(recs)
}

// triggered: the last two earlier boots were short and fit, with their
// boot time, in the window.
func triggered(prior []time.Duration) bool {
	if len(prior) < 2 {
		return false
	}
	a, b := prior[len(prior)-2], prior[len(prior)-1]
	return a < shortBoot && b < shortBoot && a+b+2*bootTime <= window
}

// read returns the records, oldest first; a missing or garbled file is
// no records.
func (s Store) read() []time.Duration {
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return nil
	}
	var recs []time.Duration
	for f := range strings.FieldsSeq(string(b)) {
		n, perr := strconv.ParseInt(f, 10, 64)
		if perr != nil || n < 0 {
			return nil
		}
		recs = append(recs, time.Duration(n)*time.Second)
	}
	return recs
}

// write replaces the file durably: write, fsync, rename, fsync the
// directory. Power loss is what this file is about.
func (s Store) write(recs []time.Duration) error {
	var b strings.Builder
	for _, r := range recs {
		b.WriteString(strconv.FormatInt(int64(r/time.Second), 10))
		b.WriteByte('\n')
	}
	dir := filepath.Dir(s.Path)
	tmp := s.Path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fileMode) //nolint:gosec // G304: the store's fixed path
	if err != nil {
		return fmt.Errorf("bootcount: %w", err)
	}
	_, werr := f.WriteString(b.String())
	serr := f.Sync()
	if err := errors.Join(werr, serr, f.Close()); err != nil {
		return fmt.Errorf("bootcount: %w", err)
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		return fmt.Errorf("bootcount: %w", err)
	}
	d, err := os.Open(dir) //nolint:gosec // G304: the store's own directory
	if err != nil {
		return fmt.Errorf("bootcount: %w", err)
	}
	return errors.Join(d.Sync(), d.Close())
}
