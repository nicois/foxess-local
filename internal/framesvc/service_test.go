package framesvc

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nicois/prox/pkg/capture"
	"github.com/nicois/prox/pkg/hooks"

	"github.com/stretchr/testify/require"

	"foxesslocal/internal/frame"
	"foxesslocal/internal/gateway"
	"foxesslocal/internal/policy"
	"foxesslocal/internal/store"
)

// All store assertions run in the test's outer scope (after the bubble):
// a Flush inside the bubble would hand the store's flusher a bubble
// channel it closes from outside (fatal). The bubble ends with every flow
// torn down (endFlow), so the enqueued rows are complete when the test
// Flushes.

// Scenario 2: 150 B telemetry at the 5 s cadence reaches the store with
// zero loss and wall-true (virtual) timestamps.
func TestTelemetryCadenceZeroLoss(t *testing.T) {
	r := newRig(t, fullPosture())
	var (
		t0  time.Time
		upN int64
	)
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		t0 = time.Now()
		for i := range 12 {
			time.Sleep(5 * time.Second)
			_, err := fl.client.Write(teleFrame(t, 100, 200, 300, 400, 500, byte(i)))
			require.NoError(t, err, "tele %d", i)
		}
		r.endFlow(t, fl, upCloudEnd, int64(12*teleFrameBytes))
		upN = fl.upN
	})

	r.st.Flush()
	rows, err := r.st.TelemetryRange(t0, t0.Add(70*time.Second))
	require.NoError(t, err)
	require.Len(t, rows, 12)
	for i, row := range rows {
		require.Equal(t, i, row.Soc, "row %d", i)
		require.False(t, row.TS.Before(t0.Add(time.Duration(i+1)*5*time.Second)),
			"row %d TS %s", i, row.TS)
		if i > 0 {
			require.True(t, rows[i].TS.After(rows[i-1].TS), "row %d not ascending", i)
		}
	}
	span := rows[11].TS.Sub(rows[0].TS)
	require.GreaterOrEqual(t, span, 55*time.Second, "cadence span %s", span)
	require.Equal(t, int64(12*teleFrameBytes), upN)
}

// Scenario 3 (owner, 2026-11-18): full mode passes what the cloud is
// known to send but holds an unrecognised frame — firmware-sized frames
// here — until a maintenance window: in one chunk, the three firmware
// frames are dropped and logged with the hold reason, and only the
// Modbus write that follows reaches the client.
func TestFullModeHoldsFirmwareSized(t *testing.T) {
	r := newRig(t, fullPosture())
	write := mkFrame(t, startS2C, devS2C, envFnS2C, pduWriteWM)
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		// The inverter opens the flow with a keepalive (the relay peeks
		// the first client byte before dialing).
		_, err := fl.client.Write(keepaliveFrame(t))
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)

		chunk := append(append(append(fwChunk(t, 0xa5), fwChunk(t, 0x3c)...), fwChunk(t, 0x77)...), write...)
		_, err = upCloudEnd.Write(chunk)
		require.NoError(t, err)
		require.Equal(t, write, readExact(t, fl.client, len(write)))

		r.endFlow(t, fl, upCloudEnd, keepaliveBytes)
		rest, err := io.ReadAll(fl.client)
		require.NoError(t, err)
		require.Empty(t, rest, "no firmware byte reaches the inverter")
	})

	r.st.Flush()
	held, err := r.st.AuditFrames(decDropped, capture.DirS2C)
	require.NoError(t, err)
	require.Len(t, held, 3, "the three firmware frames are held")
	for i, row := range held {
		require.Contains(t, row.Reason, "mode:full hold:", "row %d", i)
		require.Contains(t, row.Reason, "maintenance window", "row %d", i)
	}
	fwd, err := r.st.AuditFrames(decForwarded, capture.DirS2C)
	require.NoError(t, err)
	require.Len(t, fwd, 1, "the write is forwarded")
	require.Equal(t, reasonFull, fwd[0].Reason)
}

// Scenario 4: the telemetry_only posture denies the upstream direction
// entirely — every s2c frame in the chunk is dropped and logged with
// the stable deny reason naming the frame; the client receives zero
// s2c bytes (there is no allowlist, and nothing is held).
func TestTelemetryOnlyRelaysReads(t *testing.T) {
	r := newRig(t, policy.Posture{Mode: policy.ModeTelemetryOnly})
	var acked bool
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		_, err := fl.client.Write(keepaliveFrame(t))
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)

		// The cloud sends read, write, read: the reads reach the
		// inverter, the write never does.
		read1 := mbreadS2C(t)
		write1 := mkFrame(t, startS2C, devS2C, envFnS2C, pduWriteWM)
		read2 := mkFrame(t, startS2C, devS2C, envFnS2C, pduReadLim)
		chunk := append(append(append([]byte{}, read1...), write1...), read2...)
		_, err = upCloudEnd.Write(chunk)
		require.NoError(t, err)
		require.Equal(t, append(append([]byte{}, read1...), read2...), readExact(t, fl.client, 2*modbusFrameBytes))

		// Our command goes out while the cloud's reads are unanswered.
		time.Sleep(3 * time.Second)
		submitted := make(chan error, 1)
		go func() {
			_, err := r.gw.Submit(context.Background(), gateway.Command{
				Kind: gateway.KindWorkMode, Params: map[gateway.Param]int{gateway.ParamMode: 1}, Origin: testOrigin,
			})
			submitted <- err
		}()
		require.Len(t, readExact(t, fl.client, modbusFrameBytes), modbusFrameBytes)
		require.NoError(t, <-submitted)

		// The inverter answers ours first, then the cloud's two reads.
		ours := mkFrame(t, startC2S, devEcho, fnData, pduWriteWM)
		ans1 := mkFrame(t, startC2S, devAns, fnData, []byte{0x01, 0x03, 0x02, 0x00, 0x01})
		ans2 := mkFrame(t, startC2S, devAns, fnData, []byte{0x01, 0x03, 0x04, 0x13, 0x88, 0x30, 0xd4})
		_, err = fl.client.Write(append(append(append([]byte{}, ours...), ans1...), ans2...))
		require.NoError(t, err)
		spinUntil(t, func() bool { return len(r.gw.Pending()) == 0 })
		acked = true

		// The cloud gets its answers, never ours.
		want := int64(keepaliveBytes + len(ans1) + len(ans2))
		r.endFlow(t, fl, upCloudEnd, want)
		require.Equal(t, append(append(keepaliveFrame(t), ans1...), ans2...), fl.upBytes)
	})
	require.True(t, acked, "our command was acked by its own answer")

	r.st.Flush()
	dropped, err := r.st.AuditFrames(decDropped, capture.DirS2C)
	require.NoError(t, err)
	require.Len(t, dropped, 1, "only the write is denied")
	require.True(t, strings.HasPrefix(dropped[0].Reason, reasonTelemetryDeny), dropped[0].Reason)
	require.Equal(t, classMbwrite, dropped[0].Class)
	fwd, err := r.st.AuditFrames(decForwarded, capture.DirS2C)
	require.NoError(t, err)
	require.Len(t, fwd, 2, "the two reads pass")
	for _, row := range fwd {
		require.True(t, strings.HasPrefix(row.Reason, reasonTelemetryRead), row.Reason)
	}
}

// Scenario: off mode (D-011) — no cloud leg at all: the flow is served
// locally (no dial, no mirror) and the inverter's frames are still
// decoded and audited. Without a ServeLocal decision from the hook chain
// the relay defaults to ServeMirror and the uplink leaks to the real
// cloud.
func TestOffModeNeverDialsTheCloud(t *testing.T) {
	r := newRigBase(t, offPosture())
	r.newService(WithServePolicy(offServePolicy{}))
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		_, err := fl.client.Write(keepaliveFrame(t))
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)
		// Settle: were the flow mirrored, the keepalive is at the cloud by
		// now.
		time.Sleep(1 * time.Second)
		require.Zero(t, atomic.LoadInt64(&fl.upN), "off mode: the c2s bytes reached the cloud")
		r.endFlow(t, fl, upCloudEnd, 0)
	})
	r.st.Flush()
	fws, aerr := r.st.AuditFrames(decForwarded, capture.DirC2S)
	require.NoError(t, aerr)
	require.Len(t, fws, 1, "off mode: the keepalive is observed and audited locally")
}

// Scenario 4b: off-mode local control — an operator command over the
// locally-served session: the inverter registers, the trio is emitted,
// and the injected command frame carries the inverter's own session
// tail (pinned from the inverter's c2s session-pattern frame — a local
// session has no upstream s2c to observe) with the gateway's default
// envelope fn. The inverter's echo acks it; nothing reaches the cloud.
//
// The session byte (device[1]) is not a constant: 0x6a in the corpus,
// 0x6b on the live inverter since 2026-11-19 (profile §3/§4). Both pin.
func TestOffModeLocalCommand(t *testing.T) {
	for _, dev := range [][4]byte{devKA, {0x06, 0x6b, 0x01, 0x19}} {
		t.Run(fmt.Sprintf("%02x", dev[1]), func(t *testing.T) { testOffModeLocalCommand(t, dev) })
	}
}

func testOffModeLocalCommand(t *testing.T, devKeepalive [4]byte) {
	t.Helper()
	r := newRigBase(t, offPosture())
	r.newService(WithServePolicy(offServePolicy{}))
	var (
		devTail [4]byte
		fn      byte
		payload []byte
		done    bool
	)
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)

		// Bootstrap: the lockstep inverter sends only the 2a
		// registration (profile §3); the trio must be emitted from that
		// alone on the local leg (hook injection bypasses the relay,
		// straight to the client).
		_, err := fl.client.Write(regFrameA(t))
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)
		require.Len(t, readExact(t, fl.client, trioBytes), trioBytes)

		// The keepalive carries the session pattern (device[1], the
		// session byte) — the identity source for a local session.
		_, err = fl.client.Write(mkFrame(t, startC2S, devKeepalive, fnData, make([]byte, keepalivePayloadLen)))
		require.NoError(t, err)

		time.Sleep(3 * time.Second)

		// The first trigger read (the cloud's 0x9C40 read, +5.8 s from
		// the 2a) lands on the client leg before the command — consume
		// it; it is not the command.
		require.Len(t, readExact(t, fl.client, len(frame.TriggerReads[0])), len(frame.TriggerReads[0]))

		submitted := make(chan error, 1)
		go func() {
			_, err := r.gw.Submit(context.Background(), gateway.Command{
				Kind:   gateway.KindWorkMode,
				Params: map[gateway.Param]int{gateway.ParamMode: 1},
				Origin: testOrigin,
			})
			submitted <- err
		}()
		got := readExact(t, fl.client, modbusFrameBytes)
		require.NoError(t, <-submitted)

		frs, _ := frame.Split(got, 0)
		require.Len(t, frs, 1)
		devTail, fn, payload = frs[0].Device, frs[0].Fn, frs[0].Payload

		// The inverter's echo acks the command.
		echo := mkFrame(t, startC2S, devEcho, fnData, pduWriteWM)
		_, err = fl.client.Write(echo)
		require.NoError(t, err)
		spinUntil(t, func() bool { return len(r.gw.Pending()) == 0 })
		done = true

		r.endFlow(t, fl, upCloudEnd, 0)
	})
	require.True(t, done)
	require.Equal(t, [4]byte{0x12, devKeepalive[1], devKeepalive[2], devKeepalive[3]}, devTail,
		"device must carry the inverter's own session tail")
	require.Equal(t, byte(0x10), fn, "envelope fn: the gateway's default (no upstream s2c to observe)")
	require.Equal(t, pduWriteWM, payload)

	r.st.Flush()
	inj, err := r.st.AuditFrames(decInjected, capture.DirS2C)
	require.NoError(t, err)
	require.Len(t, inj, 5, "the trio, the first trigger read and the command write")
	for i := range 3 {
		require.Equal(t, originBootstrap, inj[i].Origin)
		require.Equal(t, classBootstrapResp, inj[i].Class)
	}
	// The trigger read and the command write are recorded at the same
	// virtual instant from different goroutines, so their row order is
	// the queue's arrival order: match them by origin, not position.
	byOrigin := map[string]frame.Kind{inj[3].Origin: inj[3].Class, inj[4].Origin: inj[4].Class}
	require.Equal(t, map[string]frame.Kind{originTrigread: classMbread, originGateway: classMbwrite}, byOrigin)
}

// Scenario 7: the s2c rebuild tiles — a wrapper frame that embeds a
// complete frame in its payload (the 451 B session-start frame wraps a
// snap02; Split reports both — mon150 parity) reaches the client exactly
// once: the rebuild emits the wrapper verbatim and does not re-emit the
// embedded frame's bytes (a double-emit would be a silent downlink
// injection). Under a maintenance window: the only posture that passes
// every kind (full holds the wrapper class, which the cloud is not known
// to send).
func TestS2CRebuildTilesEmbeddedFrame(t *testing.T) {
	r := newRig(t, policy.Posture{Mode: policy.ModeFull, Window: true})
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		_, err := fl.client.Write(keepaliveFrame(t))
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)

		inner := mkFrame(t, startS2C, devS2C, fnData, []byte{0x01, 0x02, 0x03})
		wrapper := mkFrame(t, startS2C, devS2C, 0x11, inner)
		_, err = upCloudEnd.Write(wrapper)
		require.NoError(t, err)
		got := readExact(t, fl.client, len(wrapper))
		require.Equal(t, wrapper, got, "the wrapper must reach the client verbatim")

		// No double-emit: the embedded frame's bytes must not be replayed
		// before the next upstream frame.
		marker := mbreadS2C(t)
		_, err = upCloudEnd.Write(marker)
		require.NoError(t, err)
		got2 := readExact(t, fl.client, len(marker))
		require.Equal(t, marker, got2, "embedded-frame bytes must not precede the next frame")

		r.endFlow(t, fl, upCloudEnd, keepaliveBytes)
	})
	r.st.Flush()
	fws, err := r.st.AuditFrames(decForwarded, capture.DirS2C)
	require.NoError(t, err)
	require.Len(t, fws, 3, "wrapper, embedded frame, and marker are all audited")
	require.Equal(t, []byte{0x01, 0x02, 0x03}, fws[1].Payload,
		"the embedded frame is audited (mon150 parity)")
}

// Scenario 5: an operator command submitted at T+3 s is injected on the
// flow with the session's device tail and envelope fn, and the
// inverter's echo acks it (the command leaves the pending list).
func TestGatewayCommandAtTPlus3(t *testing.T) {
	r := newRig(t, fullPosture())
	var (
		t0      time.Time
		devTail [4]byte
		fn      byte
		payload []byte
		ackDone bool
	)
	synctest.Test(t, func(t *testing.T) {
		t0 = time.Now()
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		_, err := fl.client.Write(keepaliveFrame(t))
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)

		// Session opener: the cloud's first s2c frame pins the session
		// identity (device tail + envelope fn).
		opener := mbreadS2C(t)
		_, err = upCloudEnd.Write(opener)
		require.NoError(t, err)
		require.Equal(t, opener, readExact(t, fl.client, modbusFrameBytes))

		time.Sleep(3 * time.Second)
		submitted := make(chan error, 1)
		go func() {
			_, err := r.gw.Submit(context.Background(), gateway.Command{
				Kind:   gateway.KindWorkMode,
				Params: map[gateway.Param]int{gateway.ParamMode: 1},
				Origin: testOrigin,
			})
			submitted <- err
		}()
		got := readExact(t, fl.client, modbusFrameBytes)
		require.NoError(t, <-submitted)

		frs, _ := frame.Split(got, 0)
		require.Len(t, frs, 1)
		devTail, fn, payload = frs[0].Device, frs[0].Fn, frs[0].Payload

		// The inverter's echo acks the command.
		echo := mkFrame(t, startC2S, devEcho, fnData, pduWriteWM)
		_, err = fl.client.Write(echo)
		require.NoError(t, err)
		spinUntil(t, func() bool { return len(r.gw.Pending()) == 0 })
		ackDone = true

		// the echo answers our request: it stays local
		r.endFlow(t, fl, upCloudEnd, keepaliveBytes)
		require.Equal(t, keepaliveFrame(t), fl.upBytes)
	})
	require.True(t, ackDone)
	require.Equal(t, devS2CWrite, devTail, "a write's lead, with the session's device tail")
	require.Equal(t, byte(envFnS2C), fn, "envelope fn must be the session's")
	require.Equal(t, pduWriteWM, payload)

	r.st.Flush()
	inj, err := r.st.AuditFrames(decInjected, capture.DirS2C)
	require.NoError(t, err)
	require.Len(t, inj, 1)
	require.Equal(t, originGateway, inj[0].Origin)
	require.Equal(t, classMbwrite, inj[0].Class)
	require.Equal(t, byte(envFnS2C), inj[0].Fn)
	require.False(t, inj[0].TS.Before(t0.Add(3*time.Second)), "injected before T+3s")
	require.True(t, inj[0].TS.Before(t0.Add(10*time.Second)), "injected far after T+3s")
}

// Scenario 6: frames split across arbitrary TCP boundaries decode once —
// neither direction double-counts.
func TestSplitFrameDecodesOnce(t *testing.T) {
	r := newRig(t, fullPosture())
	var t0 time.Time
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		t0 = time.Now()

		// c2s: the 163 B tele in three 50/50/63 B chunks. The flow
		// opens on the client's first byte, so the first 50 B go first.
		tele := teleFrame(t, 1, 2, 3, 4, 5, 9)
		rest := tele
		for _, n := range []int{50, 50, 63} {
			_, err := fl.client.Write(rest[:n])
			require.NoError(t, err)
			rest = rest[n:]
		}
		require.Empty(t, rest)
		r.rec.waitOpens(t, 1)

		// s2c: the 19 B frame in three 7/5/7 B chunks.
		whole := mbreadS2C(t)
		rest = whole
		for _, n := range []int{7, 5, 7} {
			_, err := upCloudEnd.Write(rest[:n])
			require.NoError(t, err)
			rest = rest[n:]
		}
		require.Empty(t, rest)
		got := readExact(t, fl.client, modbusFrameBytes)
		require.Equal(t, whole, got)

		r.endFlow(t, fl, upCloudEnd, teleFrameBytes)
	})

	r.st.Flush()
	s2cRows, err := r.st.AuditFrames(decForwarded, capture.DirS2C)
	require.NoError(t, err)
	require.Len(t, s2cRows, 1, "the split s2c frame must decode exactly once")
	c2sRows, err := r.st.AuditFrames(decForwarded, capture.DirC2S)
	require.NoError(t, err)
	require.Len(t, c2sRows, 1, "the split c2s frame must decode exactly once")
	require.Equal(t, classTele, c2sRows[0].Class)
	tel, err := r.st.TelemetryRange(t0.Add(-time.Second), t0.Add(time.Second))
	require.NoError(t, err)
	require.Len(t, tel, 1)
	require.Equal(t, 9, tel[0].Soc)
}

// Scenario 7: s2c starvation — with every s2c frame denied, the
// connection stays open for two virtual hours of c2s keepalives; the
// client receives zero s2c bytes and every keepalive reaches the cloud.
func TestS2CStarvationTwoVirtualHours(t *testing.T) {
	r := newRig(t, policy.Posture{Mode: policy.ModeTelemetryOnly})
	var (
		upN      int64
		droppedN int
	)
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)

		// The flow opens on the client's first byte.
		ka := keepaliveFrame(t)
		_, err := fl.client.Write(ka)
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)

		// One s2c frame at the start: a write, denied.
		_, err = upCloudEnd.Write(mkFrame(t, startS2C, devS2C, envFnS2C, pduWriteWM))
		require.NoError(t, err)

		// Two virtual hours of keepalives, 5 s apart (1439 more).
		for i := range 1439 {
			time.Sleep(5 * time.Second)
			_, err := fl.client.Write(ka)
			require.NoError(t, err, "keepalive %d", i)
		}
		r.endFlow(t, fl, upCloudEnd, int64(1440*keepaliveBytes))
		upN = fl.upN

		// The client's s2c stream is EOF: zero bytes, ever.
		rest, err := io.ReadAll(fl.client)
		require.NoError(t, err)
		require.Empty(t, rest)
	})
	require.Equal(t, int64(1440*keepaliveBytes), upN,
		"every keepalive must reach the cloud")

	r.st.Flush()
	dropped, err := r.st.AuditFrames(decDropped, capture.DirS2C)
	require.NoError(t, err)
	droppedN = len(dropped)
	require.GreaterOrEqual(t, droppedN, 1)
}

// Unit: the hook's identity and filter.
func TestFilterMatchesCloudPortOnly(t *testing.T) {
	r := newRig(t, fullPosture())
	require.Equal(t, hookName, r.svc.Name())
	require.Equal(t, hooks.Match{DPort: cloudPort}, r.svc.Filter())
}

// Unit: a non-cloud flow on the same rig is relayed untouched — the hook
// sees only 14431 (one flow-open audit, one c2s frame row, byte-exact
// passthrough on 443).
func TestNonCloudPortFlowUntouched(t *testing.T) {
	r := newRig(t, fullPosture())
	var (
		upNA, upNB int64
		t0         time.Time
	)
	synctest.Test(t, func(t *testing.T) {
		upA, upCloudA := chanpipePair(t)
		flA := r.startFlow(t, cloudPort, upA, upCloudA)
		t0 = time.Now()
		_, err := flA.client.Write(teleFrame(t, 1, 1, 1, 1, 1, 1))
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)

		upB, upCloudB := chanpipePair(t)
		flB := r.startFlow(t, altPort, upB, upCloudB)
		teleB := teleFrame(t, 2, 2, 2, 2, 2, 2)
		_, err = flB.client.Write(teleB)
		require.NoError(t, err)
		r.rec.waitOpens(t, 2)

		// The 443 flow is relayed byte-exact: the client's c2s tele
		// arrives at the cloud end untouched (there is no s2c on this
		// flow, so the bytes come from the drainer, not the client leg).
		spinUntil(t, func() bool { return atomic.LoadInt64(&flB.upN) == teleFrameBytes })
		require.Equal(t, teleB, flB.upBytes)
		_ = flB.client.Close()
		_ = upCloudB.Close()
		rest, err := io.ReadAll(flB.client)
		require.NoError(t, err)
		require.Empty(t, rest)
		flB.ended = true
		<-flB.finished
		require.NoError(t, <-flB.done)
		<-flB.upDrained
		upNB = flB.upN
		require.Equal(t, int64(teleFrameBytes), upNB)

		r.endFlow(t, flA, upCloudA, teleFrameBytes)
		upNA = flA.upN
		require.Equal(t, int64(teleFrameBytes), upNA)
		r.st.WaitStreams()
	})

	r.st.Flush()
	// The store has exactly the one c2s frame of the cloud flow.
	c2s, err := r.st.AuditFrames(decForwarded, capture.DirC2S)
	require.NoError(t, err)
	require.Len(t, c2s, 1)
	evts, err := r.st.QueryAudit(t0, auditFlowOpen)
	require.NoError(t, err)
	require.Len(t, evts, 1, "the hook must open exactly one flow")
}

// Unit: a mid-frame partial left in the reassembler at close is audited
// as a residue row (the stream ended mid-frame).
func TestReasmResidueAuditedOnClose(t *testing.T) {
	r := newRig(t, fullPosture())
	var partial []byte
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		_, err := fl.client.Write(teleFrame(t, 1, 2, 3, 4, 5, 1))
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)
		// A 50 B mid-frame partial (no start marker: the reassembler
		// retains it verbatim).
		partial = make([]byte, 50)
		for i := range partial {
			partial[i] = byte(i)
		}
		_, err = fl.client.Write(partial)
		require.NoError(t, err)

		// bytes that cannot start a frame pass through at once (as
		// before the c2s rebuild); the reassembler still audits them as
		// residue at close
		r.endFlow(t, fl, upCloudEnd, teleFrameBytes+50)
		require.Equal(t, append(teleFrame(t, 1, 2, 3, 4, 5, 1), partial...), fl.upBytes)
	})

	r.st.Flush()
	rows, err := r.st.AuditFrames(decResidue, capture.DirC2S)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, partial, rows[0].Payload)
	require.Equal(t, 50, rows[0].Plen)
}

// schedReadResp builds one inverter-originated fn03 read-response echo
// frame: 01 03 <bc> <data> (the response carries no address; the gateway
// correlates by request order, profile §5).
func schedReadResp(t *testing.T, cnt int, vals []uint16) []byte {
	t.Helper()
	p := make([]byte, 3+2*cnt)
	p[0], p[1] = 0x01, 0x03
	//nolint:gosec // G115: byte count = 2*registers, bounded by blockSlotCnt.
	p[2] = byte(2 * cnt)
	for i := range vals {
		p[3+2*i] = byte(vals[i] >> 8)
		p[4+2*i] = byte(vals[i] & 0xff)
	}
	return mkFrame(t, startC2S, dev91, fnData, p)
}

// schedWriteAck builds one inverter-originated fn10 write-ACK echo frame
// (6 B PDU: 01 10 <addr> <cnt>; the KH10 omits the trailing 00, profile
// §5).
func schedWriteAck(t *testing.T, addr, cnt int) []byte {
	t.Helper()
	//nolint:gosec // G115: Modbus register address/count, bounded by the protocol.
	a, c := uint16(addr), uint16(cnt)
	p := []byte{0x01, 0x10, byte(a >> 8), byte(a & 0xff), byte(c >> 8), byte(c & 0xff)}
	return mkFrame(t, startC2S, dev91, fnData, p)
}

// s2cFn03 builds the expected injected fn03 read request (the session's
// device tail + envelope fn; the service rewrites the gateway's
// placeholder on injection).
func s2cFn03(t *testing.T, addr, cnt int) []byte {
	t.Helper()
	//nolint:gosec // G115: Modbus register address/count, bounded by the protocol.
	a, c := uint16(addr), uint16(cnt)
	p := []byte{0x01, 0x03, byte(a >> 8), byte(a & 0xff), byte(c >> 8), byte(c & 0xff)}
	return mkFrame(t, startS2C, devS2C, envFnS2C, p)
}

// s2cFn10 builds the expected injected fn10 write-multiple frame:
// 01 10 <addr> <cnt> <bc> <data>.
func s2cFn10(t *testing.T, addr, cnt int, vals []uint16) []byte {
	t.Helper()
	//nolint:gosec // G115: Modbus register address/count, bounded by the protocol.
	a, c := uint16(addr), uint16(cnt)
	p := make([]byte, 7+2*cnt)
	p[0], p[1] = 0x01, 0x10
	p[2], p[3] = byte(a>>8), byte(a&0xff)
	p[4], p[5] = byte(c>>8), byte(c&0xff)
	//nolint:gosec // G115: byte count = 2*registers, bounded by blockSlotCnt.
	p[6] = byte(2 * cnt)
	for i := range vals {
		p[7+2*i] = byte(vals[i] >> 8)
		p[8+2*i] = byte(vals[i] & 0xff)
	}
	return mkFrame(t, startS2C, devS2CWrite, envFnS2C, p)
}

// Scenario: TCP coalescing — a schedule push whose 13 read responses
// arrive in ONE client write and whose 13 write ACKs arrive in another
// single write (the inverter coalesces on a LAN hop; the reassembler
// returns every complete frame in a chunk). Every echo must be acked in
// stream order: a missed echo wedges the in-flight slot, and the
// read-modify-write push must carry the read values (the preserved
// registers round-trip through the pushed blocks).
func TestSchedulePushCoalescedEchoes(t *testing.T) {
	r := newRig(t, fullPosture())
	var err error
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		_, err = fl.client.Write(keepaliveFrame(t))
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)

		// Session opener pins the identity (device tail + envelope fn).
		_, err = upCloudEnd.Write(mbreadS2C(t))
		require.NoError(t, err)
		require.Equal(t, mbreadS2C(t), readExact(t, fl.client, modbusFrameBytes))

		// Settle: the gateway's flow slot is set once the relay is ready.
		// A Submit before the flow exists silently queues (TTL); the read
		// requests inject once the flow flushes (virtual time, no wall clock).
		time.Sleep(3 * time.Second)

		bases := schedBases()
		require.Len(t, bases, 13)

		submitted := make(chan error, 1)
		go func() {
			_, serr := r.gw.Submit(context.Background(), gateway.Command{
				Kind:   gateway.KindSchedule,
				Params: map[gateway.Param]int{gateway.ParamEnabled: 1},
				Origin: testOrigin,
			})
			submitted <- serr
		}()

		// The 13 read requests (fn03, 19 B each) are injected on the
		// flow.
		reqs := schedReadReqs(t)
		require.Equal(t, reqs, readExact(t, fl.client, len(reqs)))
		require.NoError(t, <-submitted)

		// The inverter's current state: recognizable preserved values
		// (48010 +8/+9 of two slots, mirror 48090 r0/r1/r3/r7); the
		// rest zero, except curWM3 (48010 r23) = 1 — the work mode the
		// cloud leaves on group 3; the cleared push writes 1 too, so no
		// r28 latch.
		vals48010 := make([]uint16, 80)
		vals48010[8], vals48010[9], vals48010[48] = 0x1111, 0x2222, 0x3333
		vals48010[23] = 1
		vals48090 := make([]uint16, 80)
		vals48090[0], vals48090[1], vals48090[3], vals48090[7] = 0x4444, 0x5555, 0x7777, 0x6666

		// ALL 13 read responses coalesced into ONE client write.
		var resp []byte
		resp = append(resp, schedReadResp(t, 10, make([]uint16, 10))...)
		resp = append(resp, schedReadResp(t, 80, vals48010)...)
		resp = append(resp, schedReadResp(t, 80, vals48090)...)
		for range 10 {
			resp = append(resp, schedReadResp(t, 80, make([]uint16, 80))...)
		}
		_, err = fl.client.Write(resp)
		require.NoError(t, err)

		// The write phase: 13 fn10 frames, read-modify-written from the
		// read — 48000 gets enabled=1, the preserved registers
		// round-trip, and the eight cleared groups go out in the cloud's
		// unused-group shape (48010 +3/+4/+5 = 1 / 100:10 / 10; the
		// mirrors' +4/+5/+6 forced to 100:10 / 10 / 0).
		mode := make([]uint16, 10)
		mode[0] = 1
		wantWrites := append(s2cFn10(t, 48000, 10, mode), s2cFn10(t, 48010, 80, unusedTable(vals48010, true))...)
		wantWrites = append(wantWrites, s2cFn10(t, 48090, 80, unusedTable(vals48090, false))...)
		for _, b := range bases[3:] {
			wantWrites = append(wantWrites, s2cFn10(t, b, 80, unusedTable(make([]uint16, 80), false))...)
		}
		require.Equal(t, wantWrites, readExact(t, fl.client, len(wantWrites)))

		// ALL 13 write ACKs coalesced into ONE client write.
		acks := schedWriteAcks(t)
		_, err = fl.client.Write(acks)
		require.NoError(t, err)

		spinUntil(t, func() bool { return len(r.gw.Pending()) == 0 })

		// every answer was to our requests: none reaches the cloud
		r.endFlow(t, fl, upCloudEnd, keepaliveBytes)
	})
	require.NoError(t, err)

	r.st.Flush()
	// The injected read requests and write phase audit as gateway-originated
	// Modbus frames (13 reads, then the 13 fn10 push frames).
	inj, aerr := r.st.AuditFrames(decInjected, capture.DirS2C)
	require.NoError(t, aerr)
	requireAuditClasses(t, inj, 13)
}

// schedBases is the schedule-push block set: 48000/48010 (the 10-register
// mode block and the 80-register table) + the eleven ×80 mirrors
// 48090…48890 — 13 blocks.
func schedBases() []int {
	bases := []int{48000, 48010}
	for b := 48090; b <= 48890; b += 80 {
		bases = append(bases, b)
	}
	return bases
}

// schedReadReqs builds the 13 injected fn03 read requests (10 registers
// for the first block, 80 for the rest).
func schedReadReqs(t *testing.T) []byte {
	t.Helper()
	var reqs []byte
	for i, b := range schedBases() {
		cnt := 10
		if i > 0 {
			cnt = 80
		}
		reqs = append(reqs, s2cFn03(t, b, cnt)...)
	}
	return reqs
}

// schedWriteAcks builds the 13 inverter-originated fn10 write-ACK echoes.
func schedWriteAcks(t *testing.T) []byte {
	t.Helper()
	var acks []byte
	for i, b := range schedBases() {
		cnt := 10
		if i > 0 {
			cnt = 80
		}
		acks = append(acks, schedWriteAck(t, b, cnt)...)
	}
	return acks
}

// requireAuditClasses asserts the injected s2c audit rows are the
// gateway's read requests followed by the fn10 push frames.
func requireAuditClasses(t *testing.T, rows []store.FrameRecord, reads int) {
	t.Helper()
	require.Len(t, rows, reads+reads)
	for i, row := range rows {
		require.Equal(t, originGateway, row.Origin, "row %d", i)
		cls := classMbwrite
		if i < reads {
			cls = classMbread
		}
		require.Equal(t, cls, row.Class, "row %d", i)
	}
}

// unusedTable overlays the cloud's unused-group shape on a ×80 block:
// +4 = 100:10, +5 = 10, +6 = 0 on every group, plus +3 = 1 on the 48010
// table (the mirrors carry only +4/+5/+6 from the command).
func unusedTable(vals []uint16, table bool) []uint16 {
	out := append([]uint16(nil), vals...)
	for k := range 8 {
		b := 10 * k
		if table {
			out[b+3] = 1
		}
		out[b+4], out[b+5], out[b+6] = 100<<8|10, 10, 0
	}
	return out
}

// TestReconnectClosesActiveFlow: a forward-mode change ends the live
// session at its next chunk, so the inverter reconnects at once under the
// new mode (the serve decision is made per connection — R3). The audit
// says why.
func TestReconnectClosesActiveFlow(t *testing.T) {
	r := newRig(t, policy.Posture{Mode: policy.ModeTelemetryOnly})
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		_, err := fl.client.Write(keepaliveFrame(t))
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)
		spinUntil(t, func() bool { return atomic.LoadInt64(&fl.upN) == keepaliveBytes })

		r.svc.Reconnect("forward mode changed to full")
		_, _ = fl.client.Write(keepaliveFrame(t)) // the next chunk ends it
		<-fl.finished
		fl.ended = true
		if err := <-fl.done; err != nil {
			t.Logf("relay ended: %v (expected: the hook closed it)", err)
		}
		_ = upCloudEnd.Close()
		<-fl.upDrained
		r.st.WaitStreams()
	})
	r.st.Flush()
	rows, err := r.st.QueryAudit(time.Time{}, "reconnect")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Contains(t, rows[0].Detail, "forward mode changed to full")
}

// TestCaptureSeesRawChunks (owner, 2026-11-18): the capture hook sees
// every chunk as it arrived, both directions, before reassembly and
// policy — here a frame full mode holds still reaches the capture (a
// firmware push recorded during a window is the point).
func TestCaptureSeesRawChunks(t *testing.T) {
	var mu sync.Mutex
	var c2s, s2c []byte
	r := newRigBase(t, fullPosture())
	r.newService(WithCapture(func(_ string, down bool, data []byte) {
		mu.Lock()
		defer mu.Unlock()
		if down {
			s2c = append(s2c, data...)
		} else {
			c2s = append(c2s, data...)
		}
	}))
	fw := fwChunk(t, 0xa5)
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		_, err := fl.client.Write(keepaliveFrame(t))
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)
		_, err = upCloudEnd.Write(fw)
		require.NoError(t, err)
		r.endFlow(t, fl, upCloudEnd, keepaliveBytes)
	})
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, keepaliveFrame(t), c2s)
	require.Equal(t, fw, s2c, "the held firmware chunk is captured verbatim")
}
