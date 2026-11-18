// Corpus harness: replays the real cloud-channel sessions captured by
// prox under /var/lib/prox/conns (data model: /var/lib/prox/AGENTS.md) and
// asserts that Split+Decode
//
//  1. consumes every byte of the five completed sessions (zero residual);
//     the live-at-capture session may end on a trailing partial frame,
//  2. classifies frames identically to the mon150 reference tool (c2s
//     per-class parity; s2c total-frame parity),
//  3. reproduces the frozen anchor counts of the 216 B T2 session.
//
// Each session is first copied to a temp dir (snapshot): the codec reads
// the in-memory copy while mon150 reads the on-disk copy, so the harness
// never observes two different states of the same file.
package frame

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// proxRoot is the prox capture root on the gateway host.
const proxRoot = "/var/lib/prox/conns"

// replayHorizonSec is the -to horizon for the mon150 replay: t0 + one year
// covers every frame of every session in the corpus.
const replayHorizonSec = 31536000

// corpusSessions are the six real sessions in the capture corpus
// (2026-11-04 .. 2026-11-15).
var corpusSessions = []struct {
	dir  string // path under proxRoot
	live bool   // open at capture time; the transcript may end on a partial frame
}{
	{dir: t2Session, live: false},
	{dir: sess1104b, live: false},
	{dir: sess1105a, live: false},
	{dir: sess1105b, live: false},
	{dir: sessLive, live: true},
	{dir: sess1115a, live: false},
}

// Session directories under proxRoot (capture corpus, 2026-11-04 .. 15).
const (
	t2Session = "20261104/1793772365132-1" // 216 B, frozen anchor counts
	sess1104b = "20261104/1793779365405-2"
	sess1105a = "20261105/1793840764468-1"
	sess1105b = "20261105/1793875087573-1"
	sessLive  = "20261105/1793875867821-1" // still open at capture time
	sess1115a = "20261115/1794729390376-1" // cutover session; the wrapper burst
)

// Direction stream names (the .bin files under each session dir).
const (
	dirC2S = "c2s" // inverter -> cloud
	dirS2C = "s2c" // cloud -> inverter
)

// snapshotSession copies the session's c2s.bin, s2c.bin and meta.jsonl
// (the last gives mon150 a real t0) into t.TempDir; it returns the copy's
// directory.
func snapshotSession(t *testing.T, dir string) string {
	t.Helper()
	src := filepath.Join(proxRoot, dir)
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		t.Skipf("corpus session %s not present under %s", dir, proxRoot)
	}
	dst := t.TempDir()
	for _, f := range []string{"c2s.bin", "s2c.bin", "meta.jsonl"} {
		//nolint:gosec // G304: fixed corpus file names under the prox root.
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			t.Fatalf("snapshot %s/%s: %v", dir, f, err)
		}
		//nolint:gosec // G703: the destination is this test's t.TempDir.
		if err := os.WriteFile(filepath.Join(dst, f), b, 0o600); err != nil {
			t.Fatalf("snapshot %s/%s: %v", dir, f, err)
		}
	}
	return dst
}

// mon150Summary builds the reference tool from source into t.TempDir and
// runs it in replay mode over the snapshot, then parses its "summary:"
// line into key→count. Building (rather than running the repo-root
// ./mon150) keeps the parity gate tracking the source: the committed
// binary is the operational copy, and a stale one must not mask source
// drift or fake a green result.
func mon150Summary(t *testing.T, dir string) map[string]int {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mon150")
	var buildErr bytes.Buffer
	build := exec.Command("go", "build", "-o", bin, "foxesslocal/tools/mon150") //nolint:gosec // G204: constant args
	build.Stderr = &buildErr
	if err := build.Run(); err != nil {
		t.Fatalf("build mon150: %v\n%s", err, buildErr.String())
	}
	cmd := exec.Command( //nolint:gosec // G204: all args are test constants or t.TempDir paths
		bin,
		"-file", filepath.Join(dir, "c2s.bin"),
		"-s2c", filepath.Join(dir, "s2c.bin"),
		"-from", "0", "-to", strconv.Itoa(replayHorizonSec),
	)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("mon150: %v\nstderr:\n%s", err, errb.String())
	}
	var counts map[string]int
	for line := range strings.SplitSeq(out.String(), "\n") {
		if !strings.HasPrefix(line, "summary:") {
			continue
		}
		counts = map[string]int{}
		for kv := range strings.FieldsSeq(strings.TrimPrefix(line, "summary:")) {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue // display-only token (the med~Ns median)
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				t.Fatalf("mon150 summary value %q: %v", kv, err)
			}
			counts[k] = n
		}
	}
	if counts == nil {
		t.Fatalf("mon150 printed no summary line:\n%s", out.String())
	}
	return counts
}

// countKinds runs Split+Decode over one direction stream and returns the
// per-Kind counts, the consumed byte count and the total frame count.
func countKinds(t *testing.T, data []byte) (map[Kind]int, int, int) {
	t.Helper()
	frames, consumed := Split(data, 0)
	counts := map[Kind]int{}
	for _, f := range frames {
		k, _ := Decode(f)
		counts[k]++
	}
	return counts, consumed, len(frames)
}

// checkResidual asserts that every byte was consumed, except that a live
// session may end on a trailing partial frame (a tail starting with a
// frame start marker, or shorter than one start marker).
func checkResidual(t *testing.T, label string, data []byte, consumed int, live bool) {
	t.Helper()
	tail := data[consumed:]
	if len(tail) == 0 {
		return
	}
	if !live {
		t.Fatalf("%s: %d unconsumed bytes at end of completed stream", label, len(tail))
	}
	if len(tail) >= 2 && !bytes.Equal(tail[:2], start7e) && !bytes.Equal(tail[:2], start7f) {
		t.Fatalf("%s: %d-byte live tail is not a partial frame: %x", label, len(tail), tail)
	}
	t.Logf("%s: trailing partial frame of %d B (live session)", label, len(tail))
}

// mapMon150C2S folds mon150's c2s summary keys onto our Kind names:
// cfg91_<size> → cfg91, other_<size> → other, the rest are identity.
func mapMon150C2S(counts map[string]int) map[Kind]int {
	out := map[Kind]int{}
	for k, n := range counts {
		if strings.HasPrefix(k, "s2c.") {
			continue
		}
		name := k
		if strings.HasPrefix(k, "cfg91_") {
			name = "cfg91"
		}
		if strings.HasPrefix(k, "other_") {
			name = "other"
		}
		out[Kind(name)] += n
	}
	return out
}

// mon150S2CTotal returns the sum of mon150's s2c.* counts. mon150 bundles
// our bootstrapresp+serialquery+other into s2c.other; s2c.read == mbread;
// so s2c gets total-frame parity, not per-Kind parity.
func mon150S2CTotal(counts map[string]int) int {
	total := 0
	for k, n := range counts {
		if strings.HasPrefix(k, "s2c.") {
			total += n
		}
	}
	return total
}

func assertSameCounts(t *testing.T, label string, want, got map[Kind]int) {
	t.Helper()
	keys := map[Kind]bool{}
	for k := range want {
		keys[k] = true
	}
	for k := range got {
		keys[k] = true
	}
	ks := make([]Kind, 0, len(keys))
	for k := range keys {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	for _, k := range ks {
		if want[k] != got[k] {
			t.Errorf("%s: kind %s: got %d, want %d", label, k, got[k], want[k])
		}
	}
}

// Frozen anchors for the 216 B T2 session (mon150-verified 2026-11-07):
// c2s 428 frames = keepalive 233, rssi 23, register 1, other 2, wrapper 1,
// cfg91 9,
// snap01 26, snap02 27, snap04 27, snap60 26, snap61 26, snap65 27, tele 0;
// s2c 12 frames = bootstrapresp 3, mbread 9.
const (
	t2C2STotal    = 428
	t2Keepalive   = 233
	t2RSSI        = 23
	t2Register    = 1
	t2Other       = 2
	t2Wrapper     = 1 // the 451 B fn=64 wrapper frame (mon150: wrapper)
	t2Cfg91       = 9
	t2Snap01      = 26
	t2Snap02      = 27
	t2Snap04      = 27
	t2Snap60      = 26
	t2Snap61      = 26
	t2Snap65      = 27
	t2S2CTotal    = 12
	t2BootstrResp = 3
	t2Mbread      = 9
)

func assertFrozenT2(t *testing.T, c2s map[Kind]int, c2sTotal int, s2c map[Kind]int, s2cTotal int) {
	t.Helper()
	wantC2S := map[Kind]int{
		KindKeepalive:  t2Keepalive,
		KindRSSI:       t2RSSI,
		KindRegister:   t2Register,
		KindOther:      t2Other,
		KindWrapper:    t2Wrapper,
		KindCfg91:      t2Cfg91,
		SnapKind(0x01): t2Snap01,
		SnapKind(0x02): t2Snap02,
		SnapKind(0x04): t2Snap04,
		SnapKind(0x60): t2Snap60,
		SnapKind(0x61): t2Snap61,
		SnapKind(0x65): t2Snap65,
	}
	if c2sTotal != t2C2STotal {
		t.Errorf("t2 c2s total: got %d, want %d", c2sTotal, t2C2STotal)
	}
	if c2s[KindTele] != 0 {
		t.Errorf("t2 c2s tele: got %d, want 0", c2s[KindTele])
	}
	assertSameCounts(t, "t2 c2s", wantC2S, c2s)
	wantS2C := map[Kind]int{
		KindBootstrapResp: t2BootstrResp,
		KindMbread:        t2Mbread,
	}
	if s2cTotal != t2S2CTotal {
		t.Errorf("t2 s2c total: got %d, want %d", s2cTotal, t2S2CTotal)
	}
	assertSameCounts(t, "t2 s2c", wantS2C, s2c)
}

// TestCorpusParity is the red-first acceptance harness: it must pass on all
// five sessions before the implementation is considered correct.
func TestCorpusParity(t *testing.T) {
	for _, s := range corpusSessions {
		t.Run(s.dir, func(t *testing.T) {
			dir := snapshotSession(t, s.dir)
			m150 := mon150Summary(t, dir)

			//nolint:gosec // G304: this test's own t.TempDir snapshot.
			c2sData, err := os.ReadFile(filepath.Join(dir, "c2s.bin"))
			if err != nil {
				t.Fatalf("read c2s snapshot: %v", err)
			}
			c2sCounts, c2sConsumed, c2sTotal := countKinds(t, c2sData)
			checkResidual(t, dirC2S, c2sData, c2sConsumed, s.live)
			assertSameCounts(t, dirC2S, mapMon150C2S(m150), c2sCounts)

			//nolint:gosec // G304: this test's own t.TempDir snapshot.
			s2cData, err := os.ReadFile(filepath.Join(dir, "s2c.bin"))
			if err != nil {
				t.Fatalf("read s2c snapshot: %v", err)
			}
			s2cCounts, s2cConsumed, s2cTotal := countKinds(t, s2cData)
			checkResidual(t, dirS2C, s2cData, s2cConsumed, s.live)
			if s2cTotal != mon150S2CTotal(m150) {
				t.Errorf("s2c total: got %d, want %d (mon150)", s2cTotal, mon150S2CTotal(m150))
			}

			t.Logf("c2s: %d frames %v", c2sTotal, c2sCounts)
			t.Logf("s2c: %d frames %v", s2cTotal, s2cCounts)

			if s.dir == t2Session {
				assertFrozenT2(t, c2sCounts, c2sTotal, s2cCounts, s2cTotal)
			}
		})
	}
}
