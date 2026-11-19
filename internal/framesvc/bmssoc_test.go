package framesvc

import (
	"encoding/binary"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
)

// snap61Payload is a 626 B snap61 carrying the SoC (tag 1905 [6:8]) and
// the BMS SoC in 0.1 % (tag 1908 [4:6]), little-endian, in their TLV
// slots (profile §4).
func snap61Payload(socPct, bmsDeci uint16) []byte {
	const tlvBase, stride = 40, 12
	p := make([]byte, 626)
	put := func(slot int, tag uint16, off int, v uint16) {
		e := tlvBase + slot*stride
		binary.BigEndian.PutUint16(p[e:], tag)
		binary.LittleEndian.PutUint16(p[e+2+off:], v)
	}
	put(16, 0x1905, 6, socPct)
	put(19, 0x1908, 4, bmsDeci)
	return p
}

// TestObserverBMSSoc (profile §4 1908, 2026-11-20): an inverter snap61
// carrying the BMS's 0.1 % SoC is observed as EventBMSSoc (the SoC
// estimate anchors to it), stamped with its arrival; one whose tag
// disagrees with its whole-percent SoC is not.
func TestObserverBMSSoc(t *testing.T) {
	r := newRigBase(t, fullPosture())
	col := &evCollector{}
	r.newService(WithObserver(col.add))
	devSnap61 := [4]byte{0x61, 0x00, 0x00, 0x00}

	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		_, err := fl.client.Write(regFrames(t))
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)
		cloud := cloudTrio(t)
		_, err = upCloudEnd.Write(cloud)
		require.NoError(t, err)
		require.Equal(t, cloud, readExact(t, fl.client, len(cloud)))

		bad := mkFrame(t, startC2S, devSnap61, fnData, snap61Payload(68, 679))
		good := mkFrame(t, startC2S, devSnap61, fnData, snap61Payload(67, 679))
		_, err = fl.client.Write(append(bad, good...))
		require.NoError(t, err)
		ev := col.wait(t, EventBMSSoc)
		require.Equal(t, 679, ev.BMSSocDeciPct)
		require.False(t, ev.TS.IsZero())

		r.endFlow(t, fl, upCloudEnd, int64(len(regFrames(t))+len(bad)+len(good)))
	})
	n := 0
	for _, k := range col.kinds() {
		if k == EventBMSSoc {
			n++
		}
	}
	require.Equal(t, 1, n, "only the snapshot whose tags agree")
}
