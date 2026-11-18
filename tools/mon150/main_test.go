package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestParseConnMeta pins the meta.jsonl contract: conn_open/conn_close ts,
// per-write bytes_s2c records, s2c_drop ranges, and the whole-file close
// summary never matching as a chunk.
func TestParseConnMeta(t *testing.T) {
	meta := strings.Join([]string{
		`{"type":"conn_open","ts":1794729390376}`,
		`{"type":"bytes_s2c","ts":1794729390384,"start":0,"end":25}`,
		`{"type":"bytes_s2c","ts":1794729394583,"start":25,"end":59}`,
		`{"type":"s2c_drop","ts":1794732101403,"start":159,"end":191}`,
		`{"type":"bytes_s2c","ts":1794732101404,"start":191,"end":381}`,
		`{"type":"bytes_s2c","ts":1794741842928,"start":0,"end":381}`,
		`{"type":"conn_close","ts":1794741842929}`,
		`not json`,
		``,
	}, "\n")
	m := parseConnMeta([]byte(meta))

	if m.open.UnixMilli() != 1794729390376 || m.close.UnixMilli() != 1794741842929 {
		t.Fatalf("open/close = %d/%d, want 1794729390376/1794741842929",
			m.open.UnixMilli(), m.close.UnixMilli())
	}
	if len(m.writes) != 4 || len(m.drops) != 1 {
		t.Fatalf("writes/drops = %d/%d, want 4/1", len(m.writes), len(m.drops))
	}

	cases := []struct {
		off  int64
		want int64 // unixmilli of the covering write, 0 = no match
	}{
		{0, 1794729390384},
		{24, 1794729390384},
		{25, 1794729394583},
		{58, 1794729394583},
		{159, 0}, // dropped range: no delivered chunk covers it
		{191, 1794732101404},
		{380, 1794732101404},
	}
	for _, c := range cases {
		ts, ok := s2cWriteAt(m, c.off, 381)
		if !ok {
			if c.want != 0 {
				t.Errorf("s2cWriteAt(%d) = no match, want ts %d", c.off, c.want)
			}
			continue
		}
		if ts.UnixMilli() != c.want {
			t.Errorf("s2cWriteAt(%d) = %d, want %d", c.off, ts.UnixMilli(), c.want)
		}
	}

	// 60 is in no real chunk: only the whole-file summary covers it, and
	// the summary must not match.
	if ts, ok := s2cWriteAt(m, 60, 381); ok {
		t.Errorf("s2cWriteAt(60) = %d, want no match (close summary)", ts.UnixMilli())
	}

	for off, want := range map[int64]bool{158: false, 159: true, 190: true, 191: false} {
		if got := s2cDroppedAt(m, off); got != want {
			t.Errorf("s2cDroppedAt(%d) = %v, want %v", off, got, want)
		}
	}
}

func TestParseConnMetaEmpty(t *testing.T) {
	m := parseConnMeta(nil)
	if !m.open.IsZero() || !m.close.IsZero() || len(m.writes) != 0 || len(m.drops) != 0 {
		t.Fatalf("empty meta = %+v, want zero value", m)
	}
}

// TestC2SClockResponseBrackets pins the c2s clock's causality contract on
// the 11-15 cutover session: the inverter's response to the first initial
// register read (device lead | 0x80) must be stamped between that read's
// meta write ts and the next read's write ts, and the first wrapper (byte
// order: before the response) before the read's write ts + 1 s. The
// end-anchored keepalive period alone stamps the handshake region one
// cadence late (the cluster keepalive lands at t0+span/N, ~30 s), so this
// is the regression guard for the first-interval anchor.
func TestC2SClockResponseBrackets(t *testing.T) {
	const dir = "/var/lib/prox/conns/20261115/1794729390376-1"

	c2sData, err := os.ReadFile(filepath.Join(dir, "c2s.bin"))
	if err != nil {
		t.Skipf("corpus session absent: %v", err)
	}

	s2cData, err := os.ReadFile(filepath.Join(dir, "s2c.bin"))
	if err != nil {
		t.Fatalf("read s2c: %v", err)
	}

	metaData, err := os.ReadFile(filepath.Join(dir, "meta.jsonl"))
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	cm := parseConnMeta(metaData)
	if cm.open.IsZero() || cm.close.IsZero() {
		t.Fatalf("meta open/close missing")
	}

	cf, _ := scan(c2sData, 0)
	sf, _ := scan(s2cData, 0)

	var kaIdx, teleIdx []int
	for i, fr := range cf {
		switch classify(fr) {
		case "tele":
			teleIdx = append(teleIdx, i)
		case "keepalive":
			kaIdx = append(kaIdx, i)
		}
	}
	ctime, _ := assignC2SClock(cf, int64(len(c2sData)), kaIdx, teleIdx, sf, cm.writes, cm.open, cm.close.Sub(cm.open))

	// The two initial register reads (cloud -> inverter, device 11..ba0b).
	var readTS [2]time.Time
	for i, dev := range [2][4]byte{{0x11, 0x01, 0xba, 0x0b}, {0x11, 0xe6, 0xba, 0x0b}} {
		var off int64
		found := false
		for _, fr := range sf {
			if fr.device == dev {
				off, found = fr.off, true
				break
			}
		}
		if !found {
			t.Fatalf("s2c read dev %x not found", dev)
		}
		ts, ok := s2cWriteAt(cm, off, int64(len(s2cData)))
		if !ok {
			t.Fatalf("no meta write covers read dev %x at off %d", dev, off)
		}
		readTS[i] = ts
	}
	if !readTS[0].Before(readTS[1]) {
		t.Fatalf("read order: %v !< %v", readTS[0], readTS[1])
	}

	// The read1 response: first c2s frame with the response-bit device.
	resp, ok := indexOfDev(cf, func(f frame) bool { return f.device == [4]byte{0x91, 0x01, 0xba, 0x0b} })
	if !ok {
		t.Fatal("c2s response dev 9101ba0b not found")
	}
	if !strings.HasPrefix(classify(cf[resp]), "cfg91_") {
		t.Fatalf("response class = %s, want cfg91_*", classify(cf[resp]))
	}
	wrap, ok := indexOfDev(cf, func(f frame) bool { return classify(f) == "wrapper" })
	if !ok {
		t.Fatal("no wrapper frame in the session")
	}

	if ctime[resp].Before(readTS[0]) {
		t.Errorf("response ts %v precedes its read write %v", ctime[resp], readTS[0])
	}
	if !ctime[resp].Before(readTS[1]) {
		t.Errorf("response ts %v not before next read write %v (handshake region misplaced)", ctime[resp], readTS[1])
	}
	if !ctime[wrap].Before(ctime[resp]) {
		t.Errorf("wrapper ts %v not before response ts %v", ctime[wrap], ctime[resp])
	}
	if !ctime[wrap].Before(readTS[0].Add(time.Second)) {
		t.Errorf("wrapper ts %v not before read write + 1s (%v)", ctime[wrap], readTS[0].Add(time.Second))
	}
}

// indexOfDev returns the index of the first cf frame matching want, or -1.
func indexOfDev(cf []frame, want func(frame) bool) (int, bool) {
	for i, f := range cf {
		if want(f) {
			return i, true
		}
	}
	return -1, false
}
