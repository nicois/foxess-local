// Package quirks lists inverter behaviours seen in the field that the
// app can compensate for. Each one is a setting (quirks.<id>: the config
// file's [quirks] table or the web UI's Quirks page). A quirk starts off
// until reports show it is common to a model or firmware; its default
// may then change.
//
// A quirk only changes how values are shown. Stored history, the API,
// the exports and the values sent to Home Assistant stay as the inverter
// reports them, so they can always be checked against it.
package quirks

import "strings"

// Quirk is one known inverter behaviour.
type Quirk struct {
	ID string `json:"id"`
	// Title names it; Effect is what turning it on changes; Seen is where
	// it has been observed. English (the web UI translates by ID).
	Title   string `json:"title"`
	Effect  string `json:"effect"`
	Seen    string `json:"seen"`
	Default bool   `json:"default"`
}

// EPSDropouts is the quirk where the EPS (backup) port reading drops by about a quarter in
// single readings that the AC output, which includes the port, does not
// follow (owner, 2026-11-19: 9 of 35 readings at 130–138 W against
// 174–218 W, the AC output steady at 193–209 W).
const EPSDropouts = "eps_dropouts"

// prefix is the settings and config namespace.
const prefix = "quirks."

var all = []Quirk{
	{
		ID:    EPSDropouts,
		Title: "Backup-port reading dropouts",
		Effect: "The dashboard shows the backup (EPS) port as the median of its last 5 readings " +
			"(about 25 s), so single low readings the AC output does not share are ignored.",
		Seen:    "KH10, firmware 1.73: one installation (2026-11-19)",
		Default: false,
	},
}

// All returns the known quirks, in a stable order.
func All() []Quirk { return append([]Quirk(nil), all...) }

// Key is a quirk's setting key (quirks.<id>).
func Key(id string) string { return prefix + id }

// Keys are every quirk's setting key.
func Keys() []string {
	out := make([]string, 0, len(all))
	for _, q := range all {
		out = append(out, Key(q.ID))
	}
	return out
}

// FromKey returns the quirk a setting key names.
func FromKey(key string) (Quirk, bool) {
	id, ok := strings.CutPrefix(key, prefix)
	if !ok {
		return Quirk{}, false
	}
	for _, q := range all {
		if q.ID == id {
			return q, true
		}
	}
	return Quirk{}, false
}
