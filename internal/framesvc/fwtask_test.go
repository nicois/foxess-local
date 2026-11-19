package framesvc

import (
	"encoding/json"
	"testing"
	"testing/synctest"
	"time"

	"foxesslocal/internal/frame"

	"github.com/nicois/prox/pkg/capture"
	"github.com/stretchr/testify/require"
)

// fwTaskPayload is a cloud upgrade task (profile §6) with a placeholder
// URL.
var fwTaskPayload = frame.JSONMsg(0x17, []byte(`{"task_type":"upgrade","data":{"mcu_type":"A","notify_url":"https://example.invalid/status","url":"https://example.invalid/fw/1","filename":"10-300-11057-37_K10_Manager_V1.71.bin","crc":59102,"size":764904}}`))

// TestFirmwareTaskObserved (owner, 2026-11-19): a cloud upgrade task is
// dropped in telemetry_only, recorded with a readable decode (the
// Messages page shows it) and observed, so the host can say that the
// cloud asked for a firmware update and what happened to it.
func TestFirmwareTaskObserved(t *testing.T) {
	r := newRigBase(t, telemetryOnlyPosture())
	col := &evCollector{}
	r.newService(WithObserver(col.add))
	var ev Event
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		_, err := fl.client.Write(keepaliveFrame(t))
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)
		_, err = upCloudEnd.Write(mkFrame(t, startS2C, devS2C, envFnS2C, fwTaskPayload))
		require.NoError(t, err)
		ev = col.wait(t, EventFirmwareTask)
		time.Sleep(time.Second)
		r.endFlow(t, fl, upCloudEnd, keepaliveBytes)
	})
	require.NotNil(t, ev.FwTask)
	require.Equal(t, "A", ev.FwTask.MCU)
	require.Equal(t, "10-300-11057-37_K10_Manager_V1.71.bin", ev.FwTask.Filename)
	require.Equal(t, "dropped", string(ev.Decision))

	r.st.Flush()
	dropped, err := r.st.AuditFrames(decDropped, capture.DirS2C)
	require.NoError(t, err)
	require.Len(t, dropped, 1)
	var dec struct {
		FirmwareTask struct {
			Component string `json:"component"`
			Filename  string `json:"filename"`
			Size      int    `json:"size"`
			CRC       int    `json:"crc"`
			URL       string `json:"url"`
		} `json:"firmware_task"`
	}
	require.NoError(t, json.Unmarshal(dropped[0].Decoded, &dec), string(dropped[0].Decoded))
	require.Equal(t, "manager", dec.FirmwareTask.Component)
	require.Equal(t, 764904, dec.FirmwareTask.Size)
	require.Equal(t, 59102, dec.FirmwareTask.CRC)
	require.Equal(t, "https://example.invalid/fw/1", dec.FirmwareTask.URL)
}
