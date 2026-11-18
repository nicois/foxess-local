package framesvc

// Golden-transaction suite: a registered set of (input bytes → response
// bytes) exchanges extracted from the prox corpus (testdata/transactions/,
// see its README), each verified to reproduce byte-exact under a named
// policy through the rig, with the service clock pinned to the session's
// conn_open instant so time-derived bytes (the trio stamp) carry the
// baseline's values. New supported behavior ratchets in as new
// transactions get registered; the corpus stays the source, never the
// test base (no full-session replay, no count parity).

import (
	"cmp"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nicois/prox/pkg/capture"
	"github.com/stretchr/testify/require"

	"foxesslocal/internal/frame"
	"foxesslocal/internal/policy"
	"foxesslocal/internal/store"
)

// txJitter bounds the timing asserts for rows whose emission offset is
// fixed by the service constants (trioDelay + the gaps): the audit TS is
// the pinned clock at emission, so virtual scheduling noise
// (microsecond-scale) is all the window must absorb.
const txJitter = 10 * time.Millisecond

// txImmediate bounds response rows (forward/drop at frame completion) to
// a coarse window around their driving input.
const txImmediate = 200 * time.Millisecond

type txInput struct {
	Leg  string `json:"leg"` // "c2s" | "s2c"
	File string `json:"file"`
	AtUs int64  `json:"at_us"`
}

type txWant struct {
	Leg  string `json:"leg"` // "client"
	File string `json:"file"`
	// AfterInput is the input after which the client want is released
	// (the chanpipe directions are unbuffered queues: a pass-through s2c
	// response occupies the relay until it is read from the client leg,
	// so the driver reads each client want right after the input that
	// releases it). Up wants need no input.
	AfterInput int `json:"after_input"`
}

type txAudit struct {
	Decision     string     `json:"decision"`
	Dir          string     `json:"dir"` // "c2s" | "s2c"
	Class        frame.Kind `json:"class"`
	Origin       string     `json:"origin"`
	ReasonPrefix string     `json:"reason_prefix"`
	RelTo        int        `json:"rel_to"` // the input the window is relative to
}

type txCase struct {
	Session   string    `json:"session"`
	Policy    string    `json:"policy"` // "off" | "telemetry_only" | "full"
	EpochMs   int64     `json:"epoch_ms"`
	Inputs    []txInput `json:"inputs"`
	Want      []txWant  `json:"want"`
	Audit     []txAudit `json:"audit"`
	SilenceMs int       `json:"silence_ms"`
	Note      string    `json:"note"`
	Extract   string    `json:"extract"`
}

func loadTxCase(t *testing.T, dir string) txCase {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "case.json")) //nolint:gosec // G304: a fixed testdata name
	require.NoError(t, err)
	var c txCase
	require.NoError(t, json.Unmarshal(b, &c))
	return c
}

// readSilence asserts no bytes arrive on c within d (virtual).
func readSilence(t *testing.T, c net.Conn, d time.Duration) {
	t.Helper()
	ch := make(chan int, 1)
	go func() {
		var b [1]byte
		n, _ := c.Read(b[:])
		ch <- n
	}()
	select {
	case n := <-ch:
		t.Fatalf("readSilence: %d byte(s) arrived within %v; wanted none", n, d)
	case <-time.After(d):
	}
}

func TestGoldenTransactions(t *testing.T) {
	dirs, err := os.ReadDir(filepath.Join("testdata", "transactions"))
	require.NoError(t, err)
	var names []string
	for _, d := range dirs {
		if d.IsDir() {
			names = append(names, d.Name())
		}
	}
	sort.Strings(names)
	require.NotEmpty(t, names, "no golden transaction cases")
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			runGoldenTransaction(t, name)
		})
	}
}

func runGoldenTransaction(t *testing.T, name string) {
	t.Helper()
	dir := filepath.Join("testdata", "transactions", name)
	c := loadTxCase(t, dir)

	var posture policy.Posture
	switch c.Policy {
	case "off":
		posture = offPosture()
	case "telemetry_only":
		posture = telemetryOnlyPosture()
	case "full":
		posture = fullPosture()
	default:
		t.Fatalf("policy: unknown %q", c.Policy)
	}
	epoch := time.Unix(0, c.EpochMs*int64(time.Millisecond))

	r := newRigBase(t, posture)
	var (
		fl       *flowLegs
		inTS     []time.Time
		upExpect []byte // the mirrored c2s bytes: the cloud-leg expectation
	)
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl = r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		// The pinned virtual epoch: the service clock runs on the
		// bubble's virtual time from the corpus session's conn_open
		// instant (C-004 — no wall clock), so time-derived bytes are
		// stamped exactly as the baseline stamped them.
		start := time.Now()
		r.clock = func() time.Time { return epoch.Add(time.Since(start)) }
		if c.Policy == "off" {
			r.newService(WithServePolicy(offServePolicy{}))
		} else {
			r.newService()
		}
		inTS, upExpect = driveTxInputs(t, dir, c, epoch, start, posture, fl, upCloudEnd)
		if c.SilenceMs > 0 {
			readSilence(t, fl.client, time.Duration(c.SilenceMs)*time.Millisecond)
		}
		r.endFlow(t, fl, upCloudEnd, int64(len(upExpect)))
	})

	// The mirror is identity: the cloud leg must carry exactly the c2s
	// inputs, in drive order (off mode never dials: nothing arrives).
	require.Equal(t, upExpect, fl.upBytes, "the cloud leg (c2s) bytes")

	r.st.Flush()
	assertTxAudit(t, r, c, inTS)
}

// driveTxInputs writes each case input at its drive offset and reads the
// client wants released by each input, in order. inTS is stamped on the
// pinned clock's timeline (the bubble's own virtual clock starts at a
// fixed 2000 anchor, so epoch + offset, not start + offset); upExpect
// accumulates the mirrored c2s bytes (the cloud-leg expectation).
func driveTxInputs(t *testing.T, dir string, c txCase, epoch, start time.Time, posture policy.Posture, fl *flowLegs, upCloudEnd net.Conn) ([]time.Time, []byte) {
	t.Helper()
	inTS := make([]time.Time, len(c.Inputs))
	for i, in := range c.Inputs {
		inTS[i] = epoch.Add(time.Duration(in.AtUs) * time.Microsecond)
	}
	// Client wants are released by the input named in AfterInput.
	afterInput := map[int][]int{}
	for wi, w := range c.Want {
		if w.Leg != "client" {
			t.Fatalf("want leg: unknown %q", w.Leg)
		}
		afterInput[w.AfterInput] = append(afterInput[w.AfterInput], wi)
	}
	var upExpect []byte
	ordered := make([]int, len(c.Inputs))
	for i := range ordered {
		ordered[i] = i
	}
	slices.SortFunc(ordered, func(a, b int) int {
		return cmp.Compare(c.Inputs[a].AtUs, c.Inputs[b].AtUs)
	})
	for _, i := range ordered {
		in := c.Inputs[i]
		deadline := start.Add(time.Duration(in.AtUs) * time.Microsecond)
		if d := time.Until(deadline); d > 0 {
			time.Sleep(d)
		}
		b, err := os.ReadFile(filepath.Join(dir, in.File)) //nolint:gosec // G304: a testdata name from case.json
		require.NoError(t, err)
		switch in.Leg {
		case "c2s":
			_, err = fl.client.Write(b)
			require.NoError(t, err)
			if posture.Mode != policy.ModeOff {
				upExpect = append(upExpect, b...)
			}
		case "s2c":
			_, err = upCloudEnd.Write(b)
			require.NoError(t, err)
		default:
			t.Fatalf("input leg: unknown %q", in.Leg)
		}
		for _, wi := range afterInput[i] {
			w := c.Want[wi]
			b, err := os.ReadFile(filepath.Join(dir, w.File)) //nolint:gosec // G304: a testdata name from case.json
			require.NoError(t, err)
			require.Equal(t, b, readExact(t, fl.client, len(b)),
				"the client leg must carry "+w.File+" byte-exact")
		}
	}
	return inTS, upExpect
}

// rowWindow returns the expected TS offset of an audit row from its
// driving input plus the tolerance: injection offsets come from the
// service's own emission constants (n = the row's index within its
// origin); every other row is bounded to a coarse window around the
// input.
func rowWindow(a txAudit, n int) (off, win time.Duration) {
	switch a.Origin {
	case originBootstrap:
		return trioDelay + time.Duration(n)*trioRespGap, txJitter
	case originTrigread:
		return trioDelay + 2*trioRespGap + triggerReadGap1 + time.Duration(n)*triggerReadGap2, txJitter
	default:
		return 0, txImmediate
	}
}

// assertTxAudit matches each audit expectation, in order, against the
// store rows for its (decision, dir) and checks the fields plus the
// emission/processing time against the pinned clock. Injection offsets
// come from the service's own constants; every other row is bounded to
// a coarse window around its driving input.
func assertTxAudit(t *testing.T, r *rig, c txCase, inTS []time.Time) {
	t.Helper()
	counts := map[string]int{}
	for _, a := range c.Audit {
		counts[a.Decision+"/"+a.Dir]++
	}
	next := map[string]int{}
	originN := map[string]int{}
	for i, a := range c.Audit {
		var dirn capture.Dir
		switch a.Dir {
		case "c2s":
			dirn = capture.DirC2S
		case "s2c":
			dirn = capture.DirS2C
		default:
			t.Fatalf("audit dir: unknown %q", a.Dir)
		}
		all, err := r.st.AuditFrames(store.Decision(a.Decision), dirn)
		require.NoError(t, err)
		key := a.Decision + "/" + a.Dir
		require.Len(t, all, counts[key],
			"%s rows: got %d, want %d", key, len(all), counts[key])
		row := all[next[key]]
		next[key]++
		if a.Class != "" {
			require.Equal(t, a.Class, row.Class, "row %d", i)
		}
		if a.Origin != "" {
			require.Equal(t, a.Origin, row.Origin, "row %d", i)
		}
		if a.ReasonPrefix != "" {
			require.True(t, strings.HasPrefix(row.Reason, a.ReasonPrefix),
				"row %d: reason %q must carry the baseline verdict prefix", i, row.Reason)
		}
		off, win := rowWindow(a, originN[a.Origin])
		originN[a.Origin]++
		rel := row.TS.Sub(inTS[a.RelTo])
		require.True(t, rel >= off-win && rel <= off+win,
			"row %d (%s/%s): %v from its input, want %v±%v", i, a.Decision, a.Dir, rel, off, win)
	}
}
