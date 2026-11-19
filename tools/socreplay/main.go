// Command socreplay replays the stored telemetry through the SoC
// estimate (internal/socest, D-028) to validate it against a real
// inverter. It is a development tool, not shipped.
//
// It opens the database read-only, feeds every telemetry row to a fresh
// socest.Estimator in order (the rows are exactly what the live process
// observed), and reports:
//
//   - invariant violations: the estimate outside its tick's bucket or
//     0–100, not finite, or different from an independent recomputation
//     of the integration (a bug in the estimator, or this tool);
//   - per tick (a change of the reported SoC): how much of the
//     integration the bucket clamp removed in the interval before it
//     ("pinned": the estimate ran ahead and waited at the edge) and at the
//     tick itself ("jump": + = the estimate lagged and was pulled forward
//     into the new bucket, − = it was pushed back), in % of charge;
//   - anomalies: ticks against the battery flow (a BMS recalibration, or
//     a reading lag around zero power) — data, not estimator bugs;
//   - a summary per direction, split into ticks that continue the
//     previous tick's direction and the first tick after a reversal.
//
// Reading the summary: with a rounding BMS and a calibrated estimate both
// figures are ~0. A calibration error shows on every tick (pinned when
// the estimate runs fast, jumps when it runs slow). A BMS that reports
// with hysteresis shows after reversals (about twice the hysteresis). A
// truncating BMS cannot be seen: it is a constant half-percent offset,
// which fits the bucket in both directions.
//
// Run it as the database owner (foxess) on the gateway:
//
//	GOOS=linux GOARCH=arm64 go build -o socreplay ./tools/socreplay
//	sudo -u foxess ./socreplay -since 48h
package main

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	_ "modernc.org/sqlite"

	"foxesslocal/internal/socest"
)

// tsLayout is the store's fixed-width timestamp layout.
const tsLayout = "2006-01-02T15:04:05.000000000Z"

// errViolations: the replay found invariant violations (exit 1).
var errViolations = errors.New("invariant violations")

type options struct {
	db, since, until, csv string
	capWh                 int
	learned, verbose      bool
}

func main() {
	var o options
	flag.StringVar(&o.db, "db", "/var/lib/foxess-local/foxess.db", "the foxess-local database (opened read-only)")
	flag.StringVar(&o.since, "since", "72h", "start: RFC3339, or a duration before now (e.g. 48h)")
	flag.StringVar(&o.until, "until", "0s", "end: RFC3339, or a duration before now")
	flag.IntVar(&o.capWh, "capacity", 0, "battery capacity, Wh (0 = the one set in the UI)")
	flag.BoolVar(&o.learned, "learned", false, "start from the stored energy per percent (socest.learned) instead of learning from nothing")
	flag.StringVar(&o.csv, "csv", "", "also write every row (ts, raw, batt W, estimate) to this CSV file")
	flag.BoolVar(&o.verbose, "v", false, "list every tick")
	flag.Parse()
	if err := run(o, time.Now()); err != nil {
		log.Fatal(err)
	}
}

func run(o options, now time.Time) error {
	since, err := parseWhen(o.since, now)
	if err != nil {
		return fmt.Errorf("-since: %w", err)
	}
	until, err := parseWhen(o.until, now)
	if err != nil {
		return fmt.Errorf("-until: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+o.db+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	capWh, learned, err := inputs(db, o)
	if err != nil {
		return err
	}
	rows, err := load(db, since, until)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return errors.New("no telemetry in the range")
	}
	res := replay(rows, capWh, learned)
	fmt.Printf("%s → %s\n", rows[0].TS.UTC().Format(time.RFC3339), rows[len(rows)-1].TS.UTC().Format(time.RFC3339))
	if err := report(os.Stdout, res, capWh, o.verbose); err != nil {
		return err
	}
	if o.csv != "" {
		if err := writeCSV(o.csv, rows, res.Est); err != nil {
			return err
		}
	}
	if len(res.Violations) > 0 {
		return errViolations
	}
	return nil
}

// inputs are the capacity (the flag, else the stored setting) and the
// learnt energy per percent to start from (nil = from nothing).
func inputs(db *sql.DB, o options) (int, *socest.Learned, error) {
	capWh := o.capWh
	if capWh == 0 {
		// the UI's value (settings stores it as ui.<key>); a capacity set
		// only in config.toml is not in the database
		v, ok := setting(db, "ui.battery.capacity_wh")
		if !ok {
			return 0, nil, errors.New("no capacity: set -capacity (the database has no ui.battery.capacity_wh)")
		}
		var err error
		if capWh, err = strconv.Atoi(v); err != nil {
			return 0, nil, fmt.Errorf("ui.battery.capacity_wh %q: %w", v, err)
		}
	}
	if !o.learned {
		return capWh, nil, nil
	}
	v, ok := setting(db, "socest.learned")
	if !ok {
		return 0, nil, errors.New("-learned: the database has no socest.learned")
	}
	var l socest.Learned
	if err := json.Unmarshal([]byte(v), &l); err != nil {
		return 0, nil, fmt.Errorf("socest.learned: %w", err)
	}
	return capWh, &l, nil
}

// load reads the telemetry rows in [since, until), in stored order.
func load(db *sql.DB, since, until time.Time) ([]row, error) {
	rs, err := db.Query(`SELECT ts, soc, batt FROM telemetry WHERE ts >= ? AND ts < ? ORDER BY ts, rowid`,
		since.UTC().Format(tsLayout), until.UTC().Format(tsLayout))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rs.Close() }()
	var out []row
	for rs.Next() {
		var (
			ts string
			r  row
		)
		if err := rs.Scan(&ts, &r.Soc, &r.Batt); err != nil {
			return nil, err
		}
		if r.TS, err = time.Parse(time.RFC3339Nano, ts); err != nil {
			return nil, fmt.Errorf("ts %q: %w", ts, err)
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

func setting(db *sql.DB, key string) (string, bool) {
	var v string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v); err != nil {
		return "", false
	}
	return v, true
}

// parseWhen is a time (RFC3339) or a duration before now.
func parseWhen(s string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	return time.Parse(time.RFC3339, s)
}

func writeCSV(path string, rows []row, est []float64) error {
	f, err := os.Create(path) //nolint:gosec // G304: the operator's -csv path
	if err != nil {
		return err
	}
	cw := csv.NewWriter(f)
	_ = cw.Write([]string{"ts", "soc_raw", "batt_w", "soc_estimate"})
	for i, r := range rows {
		_ = cw.Write([]string{r.TS.UTC().Format(time.RFC3339Nano), strconv.Itoa(r.Soc), strconv.Itoa(r.Batt), strconv.FormatFloat(est[i], 'f', 4, 64)})
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
