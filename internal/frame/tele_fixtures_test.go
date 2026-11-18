package frame

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Frozen 150 B tele fixtures, derived 2026-11-07 from mon150 replay output
// on the two completed 11-05 sessions (session dir + c2s byte offset,
// KNOWN line values). ParseTele must reproduce them exactly.
type teleFixture struct {
	dir     string // session under proxRoot
	off     int64  // c2s byte offset of the frame start
	sol     int32
	grid    int32
	haus    int32
	acport  int32
	batt    int32
	hausDup int
	soc     byte
}

var teleFixtures = []teleFixture{
	// 20261105/1793840764468-1 c2s off=679 (first tele, charging @ ~11 kW):
	// mon150: sol=1676 grid=+10601 haus=1278 (dup=1278) acport=-9323 batt=+10999 soc=60
	{dir: sess1105a, off: 679, sol: 1676, grid: 10601, haus: 1278, acport: -9323, batt: 10999, hausDup: 1278, soc: 60},
	// 20261105/1793840764468-1 c2s off=469083 (batt idle at 100%):
	// mon150: sol=3487 grid=-2760 haus=727 (dup=727) acport=+3487 batt=+0 soc=100
	{dir: sess1105a, off: 469083, sol: 3487, grid: -2760, haus: 727, acport: 3487, batt: 0, hausDup: 727, soc: 100},
	// 20261105/1793875087573-1 c2s off=9395 (night, discharge):
	// mon150: sol=0 grid=-40 haus=562 (dup=562) acport=+602 batt=-602 soc=62
	{dir: sess1105b, off: 9395, sol: 0, grid: -40, haus: 562, acport: 602, batt: -602, hausDup: 562, soc: 62},
}

// frameAt returns the corpus frame whose start byte is at off.
func frameAt(t *testing.T, dir string, which string, off int64) Frame {
	t.Helper()
	dst := snapshotSession(t, dir)
	//nolint:gosec // G304: this test's own t.TempDir snapshot.
	data, err := os.ReadFile(filepath.Join(dst, which+".bin"))
	if err != nil {
		t.Fatalf("read %s: %v", which, err)
	}
	frames, _ := Split(data, 0)
	for _, f := range frames {
		if f.Off == off {
			return f
		}
	}
	t.Fatalf("no frame at %s offset %d in %s", which, off, dir)
	return Frame{}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// TestTeleFixtures pins ParseTele against the frozen mon150 values.
func TestTeleFixtures(t *testing.T) {
	for _, fx := range teleFixtures {
		t.Run(fx.dir+"@"+strconv.FormatInt(fx.off, 10), func(t *testing.T) {
			f := frameAt(t, fx.dir, dirC2S, fx.off)
			if k, _ := Decode(f); k != KindTele {
				t.Fatalf("kind: got %s, want tele", k)
			}
			tele, err := ParseTele(f.Payload)
			if err != nil {
				t.Fatalf("ParseTele: %v", err)
			}
			checkTeleFixture(t, fx, tele)
		})
	}
}

// checkTeleFixture asserts one parsed tele against its frozen fixture.
func checkTeleFixture(t *testing.T, fx teleFixture, tele Tele) {
	t.Helper()
	got := [5]int32{tele.Sol, tele.Grid, tele.Haus, tele.Acport, tele.Batt}
	want := [5]int32{fx.sol, fx.grid, fx.haus, fx.acport, fx.batt}
	if got != want {
		t.Errorf("power: got %v, want %v", got, want)
	}
	if tele.HausDup != fx.hausDup {
		t.Errorf("hausDup: got %d, want %d", tele.HausDup, fx.hausDup)
	}
	if tele.Soc != fx.soc {
		t.Errorf("soc: got %d, want %d", tele.Soc, fx.soc)
	}
	if tele.IDGrid != 0 || tele.IDAcport != 0 {
		t.Errorf("identities: IDGrid=%d IDAcport=%d, want 0/0", tele.IDGrid, tele.IDAcport)
	}
}

// teleIdentitySessions are the 11-05 sessions of the corpus. The recon
// established both power identities as exact at the frame instant (on its
// ~200 frame sample); the full 11-05 corpus shows 0.7-2.1% of tele frames
// with CRC-valid device-side deviations (mostly ±1 W metering roundings,
// plus one 3 W and one 54 W transient). Completed-session deviations are
// frozen in frozenDeviations.
var teleIdentitySessions = []struct {
	dir  string
	live bool
}{
	{dir: sess1105a, live: false},
	{dir: sess1105b, live: false},
	{dir: sessLive, live: true},
}

// identitySanityBandW is the per-frame sanity band. The observed
// device-side deviations (2026-11-07, 28.5k tele frames) are 594 x ±1 W
// (the metering channels round independently), one 3 W and one 54 W
// transient (soc 24) — all CRC-valid device frames. 100 W leaves headroom
// for transients, while a decode error typically produces >=100 W
// differences on most frames.
const identitySanityBandW = 100

// minIdentityExactRate is the minimum share of tele frames whose both
// identities are exactly 0/0. The corpus shows 97.9% exact (2.1% deviate,
// mostly ±1 W metering roundings); 95 leaves headroom for the device's
// rare metering transients while still catching a broken decode (wrong
// offsets would make most frames deviate).
const minIdentityExactRate = 95

// frozenDeviations are the c2s offsets of the tele frames in the completed
// 11-05 sessions whose identities are not exactly 0/0 (CRC-valid: exactly
// as the device transmitted; 41 frames, uniformly IDGrid +1 W / IDAcport
// -1 W, all at soc 100 discharging). Frozen 2026-11-07.
var frozenDeviations = map[string][]int64{
	sess1105a: {
		465868, 466031, 466194, 466505, 466668,
		643981, 644144, 644307, 661448, 661611,
		908721, 911588, 911751, 911914, 912077,
		912240, 912403, 912714, 912877, 913040,
		913203, 913366, 913558, 913869, 914032,
		914195, 929906, 930069, 930706, 930869,
		931032, 931195, 931358, 931733, 932044,
		932207, 932370, 932533, 932859, 933170,
		937680,
	},
}

// TestTeleIdentityAllFrames asserts both identities over ALL tele frames of
// the 11-05 corpus sessions:
//   - every frame is within the per-frame sanity band (decode error guard);
//   - at least minIdentityExactRate percent of frames are exactly 0/0;
//   - completed sessions: the deviating offsets are exactly the frozen set.
func TestTeleIdentityAllFrames(t *testing.T) {
	for _, s := range teleIdentitySessions {
		t.Run(s.dir, func(t *testing.T) {
			n, exact, dev := scanIdentity(t, snapshotSession(t, s.dir))
			if n == 0 {
				t.Fatalf("no tele frames found")
			}
			exactPct := 100 * exact / n
			if exactPct < minIdentityExactRate {
				t.Fatalf("exact identities: %d of %d frames (%d%%), want >= %d%%",
					exact, n, exactPct, minIdentityExactRate)
			}
			if !s.live {
				assertFrozenDeviations(t, s.dir, dev)
			}
			t.Logf("%d tele frames: %d exact (%d%%), %d deviating: %v", n, exact, exactPct, len(dev), dev)
		})
	}
}

// scanIdentity classifies every tele frame of a session snapshot's c2s
// stream:
// it returns the frame count, the exact-identity count and the deviating
// offsets; a deviation beyond the sanity band fails the test.
func scanIdentity(t *testing.T, dst string) (n, exact int, dev []int64) {
	t.Helper()
	//nolint:gosec // G304: this test's own t.TempDir snapshot.
	data, err := os.ReadFile(filepath.Join(dst, dirC2S+".bin"))
	if err != nil {
		t.Fatalf("read c2s: %v", err)
	}
	frames, _ := Split(data, 0)
	for _, f := range frames {
		if k, _ := Decode(f); k != KindTele {
			continue
		}
		tele, err := ParseTele(f.Payload)
		if err != nil {
			t.Fatalf("off=%d ParseTele: %v", f.Off, err)
		}
		n++
		switch {
		case tele.IDGrid == 0 && tele.IDAcport == 0:
			exact++
		case abs(tele.IDGrid) <= identitySanityBandW && abs(tele.IDAcport) <= identitySanityBandW:
			dev = append(dev, f.Off)
		default:
			t.Fatalf("off=%d: identities IDGrid=%d IDAcport=%d outside the %d W sanity band",
				f.Off, tele.IDGrid, tele.IDAcport, identitySanityBandW)
		}
	}
	return n, exact, dev
}

// assertFrozenDeviations checks a completed session's deviating offsets
// against the frozen set.
func assertFrozenDeviations(t *testing.T, dir string, dev []int64) {
	t.Helper()
	want := frozenDeviations[dir]
	if len(dev) != len(want) {
		t.Fatalf("deviating frames: got %v, want %v", dev, want)
	}
	for i := range want {
		if dev[i] != want[i] {
			t.Fatalf("deviating frames: got %v, want %v", dev, want)
		}
	}
}
