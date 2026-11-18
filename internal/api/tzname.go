package api

import (
	"os"
	"strings"
	"time"
)

// zoneSources are where the host's zone name can be found (tests point
// them elsewhere).
type zoneSources struct {
	tzfile    string // Debian's /etc/timezone
	localtime string // the /etc/localtime link into zoneinfo
}

var hostZoneSources = zoneSources{tzfile: "/etc/timezone", localtime: "/etc/localtime"}

// ianaZone is the host's IANA time-zone name (e.g. Australia/Sydney), or
// nil when it cannot be told: the TZ variable, else /etc/timezone, else
// the /etc/localtime symlink's target under zoneinfo. A name the time
// package cannot load is not reported.
func ianaZone(src zoneSources, tz string) *string {
	try := func(name string) *string {
		name = strings.TrimSpace(strings.TrimPrefix(name, ":"))
		if name == "" || strings.HasPrefix(name, "/") {
			return nil
		}
		if _, err := time.LoadLocation(name); err != nil {
			return nil
		}
		return &name
	}
	if z := try(tz); z != nil {
		return z
	}
	if b, err := os.ReadFile(src.tzfile); err == nil {
		if z := try(string(b)); z != nil {
			return z
		}
	}
	if target, err := os.Readlink(src.localtime); err == nil {
		if _, after, ok := strings.Cut(target, "zoneinfo/"); ok {
			return try(after)
		}
	}
	return nil
}
