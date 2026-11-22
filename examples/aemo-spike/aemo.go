package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// Price is one region's dispatch price for one 5-minute interval.
type Price struct {
	Region string
	RRP    float64   // $/MWh
	At     time.Time // the interval's end (SETTLEMENTDATE)
}

// PriceSource gives the latest price for a region. Swap it for another
// feed (a retailer's API, Amber, a forecast) to adapt this sidecar.
type PriceSource interface {
	Latest(ctx context.Context, region string) (Price, error)
}

// nemTime is the NEM's market time: AEST all year (UTC+10, no daylight
// saving).
var nemTime = time.FixedZone("NEM", int((10 * time.Hour).Seconds()))

// NEMWeb reads AEMO's public 5-minute dispatch reports
// (Reports/Current/DispatchIS_Reports): a ~125 KB directory listing,
// then the newest ~21 KB zip, only when it is new.
type NEMWeb struct {
	Base   string // https://nemweb.com.au
	Client *http.Client

	lastFile string
	last     map[string]Price
}

const (
	csvLimit     = 16 << 20 // bounds one report's CSV (~170 KB)
	dispatchDir  = "/Reports/Current/DispatchIS_Reports/"
	lineBuf      = 64 << 10         // the listing's first line buffer
	listingLimit = 4 << 20          // bounds the listing read
	retryPeriod  = 5 * time.Second  // how often to recheck during the window
	retryWindow  = 30 * time.Second // how long to keep looking for new pricing data
	userAgent    = "foxess-local-aemo-spike-example"
	zipLimit     = 2 << 20 // bounds one report
)

var dispatchFileRe = regexp.MustCompile(`PUBLIC_DISPATCHIS_\d{12}_\d{16}\.zip`)

// Latest returns region's price from the newest dispatch report.
func (n *NEMWeb) Latest(ctx context.Context, region string) (Price, error) {
	ctx, cancelFunction := context.WithTimeout(ctx, retryWindow)
	defer cancelFunction()
	var name string
	var err error
	for {
		name, err = n.newestFile(ctx)
		if err != nil {
			return Price{}, err
		}
		if name != n.lastFile {
			break
		}
		slog.Info("new pricing not yet available", "newest filename", name)
		if err = sleep(ctx, retryPeriod); err != nil {
			return Price{}, err
		}
	}
	body, err := n.get(ctx, dispatchDir+name, zipLimit)
	if err != nil {
		return Price{}, err
	}
	prices, err := parseDispatchZip(body)
	if err != nil {
		return Price{}, fmt.Errorf("%s: %w", name, err)
	}
	n.lastFile, n.last = name, prices
	p, ok := n.last[region]
	if !ok {
		return Price{}, fmt.Errorf("%s: no price for %s", n.lastFile, region)
	}
	return p, nil
}

// newestFile scans the listing for the last (newest) report name.
func (n *NEMWeb) newestFile(ctx context.Context) (string, error) {
	body, err := n.get(ctx, dispatchDir, listingLimit)
	if err != nil {
		return "", err
	}
	newest := ""
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, lineBuf), listingLimit)
	for sc.Scan() {
		for _, m := range dispatchFileRe.FindAll(sc.Bytes(), -1) {
			if s := string(m); s > newest { // the names sort by time
				newest = s
			}
		}
	}
	if newest == "" {
		return "", errors.New("nemweb: no dispatch report in the listing")
	}
	return newest, sc.Err()
}

func (n *NEMWeb) get(ctx context.Context, path string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.Base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := n.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("nemweb: %s: HTTP %d", path, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// parseDispatchZip reads the DISPATCH PRICE table of one report: each
// region's RRP for the interval, the physical run (INTERVENTION 0).
func parseDispatchZip(b []byte) (map[string]Price, error) {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, err
	}
	if len(zr.File) != 1 {
		return nil, fmt.Errorf("%d files in the report, want 1", len(zr.File))
	}
	f, err := zr.File[0].Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return parseDispatchCSV(io.LimitReader(f, csvLimit))
}

// parseDispatchCSV reads AEMO's MMS CSV: "I" rows name a table's
// columns, "D" rows carry its data; only DISPATCH PRICE is kept.
func parseDispatchCSV(r io.Reader) (map[string]Price, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // each table has its own width
	cr.ReuseRecord = true
	var col map[string]int
	out := map[string]Price{}
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if col, err = priceRecord(rec, col, out); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no DISPATCH PRICE rows")
	}
	return out, nil
}

// priceRecord takes one record: a DISPATCH PRICE header ("I") sets the
// columns, a data row ("D") adds its region's price to out.
func priceRecord(rec []string, col map[string]int, out map[string]Price) (map[string]int, error) {
	if len(rec) < 4 || rec[1] != "DISPATCH" || rec[2] != "PRICE" {
		return col, nil
	}
	switch rec[0] {
	case "I":
		col = map[string]int{}
		for i, name := range rec {
			col[name] = i
		}
	case "D":
		p, ok, err := priceRow(rec, col)
		if err != nil {
			return col, err
		}
		if ok {
			out[p.Region] = p
		}
	}
	return col, nil
}

// priceRow is one DISPATCH PRICE data row (ok false: an intervention
// run, not the physical one).
func priceRow(rec []string, col map[string]int) (Price, bool, error) {
	field := func(name string) (string, error) {
		i, ok := col[name]
		if !ok || i >= len(rec) {
			return "", fmt.Errorf("DISPATCH PRICE: no %s column", name)
		}
		return rec[i], nil
	}
	var vals [4]string
	for i, name := range []string{"REGIONID", "RRP", "SETTLEMENTDATE", "INTERVENTION"} {
		v, err := field(name)
		if err != nil {
			return Price{}, false, err
		}
		vals[i] = v
	}
	if vals[3] != "0" {
		return Price{}, false, nil
	}
	rrp, err := strconv.ParseFloat(vals[1], 64)
	if err != nil {
		return Price{}, false, fmt.Errorf("RRP %q: %w", vals[1], err)
	}
	at, err := time.ParseInLocation("2006/01/02 15:04:05", vals[2], nemTime)
	if err != nil {
		return Price{}, false, fmt.Errorf("SETTLEMENTDATE %q: %w", vals[2], err)
	}
	return Price{Region: vals[0], RRP: rrp, At: at}, true, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
