package framesvc

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/nicois/prox/pkg/capture"
	"github.com/stretchr/testify/require"
)

// wantRead9C40 / wantRead277E are the two initial cloud reads verbatim
// from the 11-15 corpus (session 1794729390376-1, s2c off=45/64,
// cutover doc §3.2): fn03 @0x9C40 x6 (the inverter's system clock) and
// fn04 @0x277E x28, at cloud offsets ~4.8 s / ~9.9 s from the 2a.
var (
	wantRead9C40 = []byte{0x7f, 0x7f, 0x11, 0x01, 0xba, 0x0b, 0x47, 0x00, 0x06, 0x01, 0x03, 0x9c, 0x40, 0x00, 0x06, 0x5f, 0x62, 0xf7, 0xf7}
	wantRead277E = []byte{0x7f, 0x7f, 0x11, 0xe6, 0xba, 0x0b, 0x47, 0x00, 0x06, 0x01, 0x04, 0x27, 0x7e, 0x00, 0x1c, 0x59, 0x74, 0xf7, 0xf7}
)

// In telemetry_only the service must inject the two initial cloud reads
// after the local trio — at the cloud's offsets from the 2a (5.8 s /
// 10.9 s, 11-15 corpus meta: read1 trio3+1.18 s, read2 read1+5.09 s). The inverter arms its push mode (the fn=64
// wrapper stream) only after receiving them (cutover doc §3.6); a
// trio-only bootstrap leaves the wrapper stream silent.
func TestTelemetryOnlyTriggerReadsAfterTrio(t *testing.T) {
	r := newRig(t, telemetryOnlyPosture())
	var (
		got     []byte
		reg2c2b []byte
	)
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		reg2a := regFrameA(t)
		_, err := fl.client.Write(reg2a)
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)

		// The trio (45 B) and the two reads (19 B each) must arrive in
		// order — the reads never arrive pre-fix, so the bounded read
		// fails the bubble fast instead of hanging it.
		got = readWithin(t, fl.client, trioBytes+2*len(wantRead9C40), 20*time.Second)
		reg2c2b = regFrames(t)[len(reg2a):]
		_, err = fl.client.Write(reg2c2b)
		require.NoError(t, err)

		r.endFlow(t, fl, upCloudEnd, int64(len(reg2a)+len(reg2c2b)))
	})
	require.Equal(t, wantRead9C40, got[trioBytes:trioBytes+len(wantRead9C40)],
		"the 0x9C40 read, verbatim corpus bytes")
	require.Equal(t, wantRead277E, got[trioBytes+len(wantRead9C40):],
		"the 0x277E read, verbatim corpus bytes")

	r.st.Flush()
	inj, err := r.st.AuditFrames(decInjected, capture.DirS2C)
	require.NoError(t, err)
	require.Len(t, inj, 5, "trio (3) + two trigger reads")
	require.Equal(t, originBootstrap, inj[0].Origin)
	require.Equal(t, originTrigread, inj[3].Origin, "read 1 origin")
	require.Equal(t, originTrigread, inj[4].Origin, "read 2 origin")
	fwd, err := r.st.AuditFrames(decForwarded, capture.DirC2S)
	require.NoError(t, err)
	require.Len(t, fwd, 3, "the 2a + the 2c/2b pair")
	var regTS time.Time
	for _, row := range fwd {
		if row.Class == classRegister {
			regTS = row.TS
		}
	}
	require.False(t, regTS.IsZero(),
		"the 2a must be the one forwarded register frame")
	off1 := inj[3].TS.Sub(regTS)
	require.True(t, off1 >= 5700*time.Millisecond && off1 <= 5900*time.Millisecond,
		"read 1 at ~5.8 s from the 2a (got %v)", off1)
	off2 := inj[4].TS.Sub(regTS)
	require.True(t, off2 >= 10800*time.Millisecond && off2 <= 11000*time.Millisecond,
		"read 2 at ~10.9 s from the 2a (got %v)", off2)
}

// A flow that closes after the trio but before the trigger reads must
// cancel the pending reads: no emitter parked in the gap (synctest
// fails the bubble on a blocked goroutine) and no spurious
// inject-error row — pre-fix, the emitter woke after the close, the
// write to the closed flow failed, and the error was audited.
func TestTriggerReadsCancelledOnFlowClose(t *testing.T) {
	r := newRig(t, telemetryOnlyPosture())
	var reg2a []byte
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		reg2a = regFrameA(t)
		_, err := fl.client.Write(reg2a)
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)

		// The trio lands (~trioDelay); close mid-gap, before the first
		// read (trioDelay + 2·trioRespGap + triggerReadGap1).
		readExact(t, fl.client, trioBytes)
		r.endFlow(t, fl, upCloudEnd, int64(len(reg2a)))
		// No drain sleep: with cancellable waits the emitter exits on
		// the flow close.
	})

	r.st.Flush()
	inj, err := r.st.AuditFrames(decInjected, capture.DirS2C)
	require.NoError(t, err)
	require.Len(t, inj, 3, "trio only: the reads were cancelled with the flow")
	for i, row := range inj {
		require.Equal(t, originBootstrap, row.Origin, "row %d", i)
	}
	evts, err := r.st.QueryAudit(time.Time{}, "inject-error")
	require.NoError(t, err)
	require.Empty(t, evts, "no inject-error rows on a clean close")
}
