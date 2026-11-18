package framesvc

import (
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nicois/prox/pkg/capture"
	"github.com/stretchr/testify/require"

	"foxesslocal/internal/frame"
)

// Scenario 1: full mode — the real cloud owns the bootstrap. The
// inverter's registration frames are mirrored upstream and the real
// cloud's responses (session bytes distinct from the time-derived local
// parameters) are
// relayed verbatim; the service must not also inject the local trio —
// the inverter would see a duplicated triple.
func TestFullModeNoLocalTrio(t *testing.T) {
	r := newRig(t, fullPosture())
	var reg []byte
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		reg = regFrames(t)
		_, err := fl.client.Write(reg)
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)

		// The real cloud's trio reaches the client byte-exact.
		cloud := cloudTrio(t)
		_, err = upCloudEnd.Write(cloud)
		require.NoError(t, err)
		require.Equal(t, cloud, readExact(t, fl.client, len(cloud)))

		r.endFlow(t, fl, upCloudEnd, int64(len(reg)))
	})

	r.st.Flush()
	inj, err := r.st.AuditFrames(decInjected, capture.DirS2C)
	require.NoError(t, err)
	require.Empty(t, inj, "full mode: the real cloud owns the bootstrap")
	fwd, err := r.st.AuditFrames(decForwarded, capture.DirS2C)
	require.NoError(t, err)
	require.Len(t, fwd, 3, "the real cloud's trio is relayed and audited")
}

// Scenario 1b: telemetry_only bootstrap (the live stage-1b deadlock).
// The lockstep inverter (profile §3, 2026-11-08 drop experiment) sends
// only the 2a registration until a bootstrap response is delivered; in
// telemetry_only the real cloud's responses are denied (C-001), so the
// service must emit the local trio on the first 2a — waiting for the
// 2c/2b that are gated on it deadlocks the inverter in the 12 s
// registration-retry loop and no telemetry ever flows.
func TestTelemetryOnlyTrioOnFirstRegistration(t *testing.T) {
	r := newRig(t, telemetryOnlyPosture())
	var (
		reg2a   []byte
		reg2c2b []byte
		tele    []byte
		gotTrio []byte
		t0      time.Time
	)
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		reg2a = regFrameA(t)
		_, err := fl.client.Write(reg2a)
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)

		// The local trio is scheduled by the first 2a and arrives
		// ~trioDelay later, as three separate writes (the real cloud's
		// emission algorithm); readExact spans them. Its bytes must carry
		// the time-derived S/q (checked post-flush against the row's
		// emission timestamp, profile §3).
		gotTrio = readExact(t, fl.client, trioBytes)

		// The real cloud's resp1 for the mirrored 2a is observed and
		// denied (C-001) — it never reaches the client.
		_, err = upCloudEnd.Write(cloudTrio(t)[:18])
		require.NoError(t, err)

		// The inverter proceeds once the response is delivered: 2c/2b,
		// then the 5 s telemetry starts.
		reg2c2b = regFrames(t)[len(reg2a):]
		t0 = time.Now()
		tele = teleFrame(t, 100, 200, 300, 400, 500, 42)
		_, err = fl.client.Write(reg2c2b)
		require.NoError(t, err)
		_, err = fl.client.Write(tele)
		require.NoError(t, err)

		r.endFlow(t, fl, upCloudEnd, int64(len(reg2a)+len(reg2c2b)+len(tele)))
	})

	r.st.Flush()
	inj, err := r.st.AuditFrames(decInjected, capture.DirS2C)
	require.NoError(t, err)
	require.Len(t, inj, 3, "the local bootstrap trio")
	// Time-derived S/q (profile §3): S = (ts/256) % 65536 at response
	// dev[2:4], q = ts % 256 as fn — the service clock at emission.
	p := frame.TrioParamsAt(inj[0].TS)
	var wantTrio []byte
	for _, tf := range frame.BootstrapTrio(trioDev0, p) {
		wantTrio = append(wantTrio, tf...)
	}
	require.Equal(t, wantTrio, gotTrio)
	for i, row := range inj {
		require.Equal(t, originBootstrap, row.Origin, "row %d", i)
		require.Equal(t, classBootstrapResp, row.Class, "row %d", i)
		require.Equal(t, [2]byte{p.SessionBytes[0], p.SessionBytes[1]}, [2]byte{row.Dev[2], row.Dev[3]}, "row %d", i)
		require.Equal(t, p.FnBase, row.Fn, "row %d", i)
	}
	drop, err := r.st.AuditFrames(decDropped, capture.DirS2C)
	require.NoError(t, err)
	require.Len(t, drop, 1, "the real cloud's resp1 is denied (C-001)")
	require.True(t, strings.HasPrefix(drop[0].Reason, reasonTelemetryDeny), drop[0].Reason)
	fwd, err := r.st.AuditFrames(decForwarded, capture.DirC2S)
	require.NoError(t, err)
	require.Len(t, fwd, 4, "the c2s frames are mirrored upstream")
	rows, err := r.st.TelemetryRange(t0.Add(-time.Second), t0.Add(time.Minute))
	require.NoError(t, err)
	require.Len(t, rows, 1, "the telemetry row is recorded")
	require.Equal(t, 42, rows[0].Soc)
}

// The local trio must follow the real cloud's emission algorithm
// (2026-11-15 live capture, prox session 1794729390376-1): the first
// bootstrap write lands seconds after the 2a (the cloud's processing
// latency — 2.5 s live, 5–8 s in the 2026-11-11 corpus), and the three
// responses are separate writes 255–327 ms apart. A +0 ms single-write
// trio is not picked up by the inverter (32 h of 12 s re-registration
// loops, 2026-11-15 live).
func TestTrioArrivesDelayedAndSegmented(t *testing.T) {
	r := newRig(t, telemetryOnlyPosture())
	var reg2a []byte
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		reg2a = regFrameA(t)
		_, err := fl.client.Write(reg2a)
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)

		// The responses arrive as three separate writes (18/13/14 B) —
		// not one 45 B burst (trioBytes).
		buf := make([]byte, 64)
		n, err := fl.client.Read(buf)
		require.NoError(t, err)
		require.Equal(t, 18, n, "resp1: its own write")
		n, err = fl.client.Read(buf)
		require.NoError(t, err)
		require.Equal(t, 13, n, "resp2: its own write")
		n, err = fl.client.Read(buf)
		require.NoError(t, err)
		require.Equal(t, 14, n, "resp3: its own write")

		r.endFlow(t, fl, upCloudEnd, int64(len(reg2a)))
	})

	r.st.Flush()
	inj, err := r.st.AuditFrames(decInjected, capture.DirS2C)
	require.NoError(t, err)
	require.Len(t, inj, 3, "the local bootstrap trio")
	fwd, err := r.st.AuditFrames(decForwarded, capture.DirC2S)
	require.NoError(t, err)
	require.Len(t, fwd, 1, "the 2a")
	lag := inj[0].TS.Sub(fwd[0].TS)
	require.GreaterOrEqual(t, lag, 3*time.Second,
		"the first write must lag the 2a by ~trioDelay (got %v)", lag)
	require.LessOrEqual(t, lag, 5*time.Second,
		"the first write must lag the 2a by ~trioDelay (got %v)", lag)
}

// The inverter re-requests the bootstrap with a fresh 2a every 12 s
// until it is established, and the real cloud answers every request
// (the trio fn steps +12 per retransmission, 2026-11-08 captures).
// The local trio must re-emit on each 2a — not just the first per
// connection — or the inverter loops in the 12 s registration retry
// forever.
func TestTrioReemittedOnEachRegistration(t *testing.T) {
	r := newRig(t, telemetryOnlyPosture())
	var (
		reg2a   []byte
		gotTrio []byte
		got2    []byte
	)
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		reg2a = regFrameA(t)
		_, err := fl.client.Write(reg2a)
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)
		// The full bootstrap sequence: the trio plus the two trigger
		// reads (the inverter's push-mode arming).
		const seq = trioBytes + 2*19 // the trio + the two 19 B trigger reads
		gotTrio = readWithin(t, fl.client, seq, 20*time.Second)

		// The inverter never establishes (simulated): it re-registers
		// on the 12 s cadence and a fresh sequence must follow.
		time.Sleep(12 * time.Second)
		_, err = fl.client.Write(reg2a)
		require.NoError(t, err)
		got2 = readWithin(t, fl.client, seq, 20*time.Second)

		r.endFlow(t, fl, upCloudEnd, int64(len(reg2a))*2)
	})
	require.NotEqual(t, gotTrio, got2, "the re-emit carries fresh time-derived S/q")
	require.Equal(t, wantRead9C40, gotTrio[trioBytes:trioBytes+len(wantRead9C40)])
	require.Equal(t, wantRead277E, gotTrio[trioBytes+len(wantRead9C40):])
	require.Equal(t, wantRead9C40, got2[trioBytes:trioBytes+len(wantRead9C40)])
	require.Equal(t, wantRead277E, got2[trioBytes+len(wantRead9C40):])

	r.st.Flush()
	inj, err := r.st.AuditFrames(decInjected, capture.DirS2C)
	require.NoError(t, err)
	require.Len(t, inj, 10, "two bootstrap sequences (trio + reads) per 2a")
	wantOrigin := []string{originBootstrap, originBootstrap, originBootstrap, originTrigread, originTrigread, originBootstrap, originBootstrap, originBootstrap, originTrigread, originTrigread}
	for i, row := range inj {
		require.Equal(t, wantOrigin[i], row.Origin, "row %d", i)
	}
	p := frame.TrioParamsAt(inj[5].TS)
	var want2 []byte
	for _, tf := range frame.BootstrapTrio(trioDev0, p) {
		want2 = append(want2, tf...)
	}
	want2 = append(want2, wantRead9C40...)
	want2 = append(want2, wantRead277E...)
	require.Equal(t, want2, got2)
}

// A flow that closes before the trio's delay elapses must not emit:
// the inverter is gone, and the pending emission's goroutine must find
// the closed flow and exit cleanly (synctest fails the bubble on a
// leaked goroutine).
func TestTrioPendingFlowClose(t *testing.T) {
	r := newRig(t, telemetryOnlyPosture())
	var reg2a []byte
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		reg2a = regFrameA(t)
		_, err := fl.client.Write(reg2a)
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)

		// Close before the trio's delay elapses.
		r.endFlow(t, fl, upCloudEnd, int64(len(reg2a)))
	})

	r.st.Flush()
	inj, err := r.st.AuditFrames(decInjected, capture.DirS2C)
	require.NoError(t, err)
	require.Empty(t, inj, "no trio onto a closed flow")
}

// TestQuiesceStopsEmitters (DEF-008): Quiesce ends a pending bootstrap
// emitter at once — the flow still open, the delay not elapsed — and
// waits for it, so nothing writes to the store after the host closes it
// (a 2a within trioDelay of shutdown panicked the process on stop).
func TestQuiesceStopsEmitters(t *testing.T) {
	r := newRig(t, telemetryOnlyPosture())
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		reg2a := regFrameA(t)
		_, err := fl.client.Write(reg2a)
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)

		done := make(chan struct{})
		go func() { r.svc.Quiesce(); close(done) }()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("Quiesce did not return with an emitter pending (it must not wait out trioDelay)")
		}
		time.Sleep(2 * trioDelay) // past the delay: the stopped emitter stays stopped
		synctest.Wait()

		r.endFlow(t, fl, upCloudEnd, int64(len(reg2a)))
	})
	r.st.Flush()
	inj, err := r.st.AuditFrames(decInjected, capture.DirS2C)
	require.NoError(t, err)
	require.Empty(t, inj, "no trio after Quiesce")
}
