package sim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"foxesslocal/internal/gateway"
	"foxesslocal/internal/store"
)

// Work-mode wire enums (profile §5 table 3-11).
const (
	wmSelfUse        = 1
	wmFeedin         = 2
	wmBackup         = 3
	wmPeakShaving    = 4
	wmPowerStation   = 5
	wmForceCharge    = 6
	wmForceDischarge = 7
)

// Work-mode string ↔ wire enum (the integration's WorkMode StrEnum is the
// five-name subset of the seven wire enums).
var workModeToWire = map[string]int{
	"SelfUse":        wmSelfUse,
	"Feedin":         wmFeedin,
	"Backup":         wmBackup,
	"PeakShaving":    wmPeakShaving,
	"PowerStation":   wmPowerStation,
	"ForceCharge":    wmForceCharge,
	"ForceDischarge": wmForceDischarge,
}

var wireToWorkMode = map[int]string{
	wmSelfUse:        "SelfUse",
	wmFeedin:         "Feedin",
	wmBackup:         "Backup",
	wmPeakShaving:    "PeakShaving",
	wmPowerStation:   "PowerStation",
	wmForceCharge:    "ForceCharge",
	wmForceDischarge: "ForceDischarge",
}

// workModeName decodes a wire enum to the integration's names (unknown →
// "Invalid"; the integration's parser turns that into None).
func workModeName(wm int) string {
	if n, ok := wireToWorkMode[wm]; ok {
		return n
	}
	return "Invalid"
}

// cloudGroup is the integration's schedule group (the ScheduleGroup
// TypedDict; the JSON names are the cloud contract).
type cloudGroup struct {
	Enable       int    `json:"enable"`
	StartHour    int    `json:"startHour"`
	StartMinute  int    `json:"startMinute"`
	EndHour      int    `json:"endHour"`
	EndMinute    int    `json:"endMinute"`
	WorkMode     string `json:"workMode"`
	MinSocOnGrid int    `json:"minSocOnGrid"`
	FdSoc        int    `json:"fdSoc"`
	FdPwr        int    `json:"fdPwr"`
}

type schedulerResult struct {
	Enable     int          `json:"enable"`
	Groups     []cloudGroup `json:"groups"`
	Properties any          `json:"properties"`
}

// handleSchedulerGet returns the stored 48010 table in the cloud shape:
// eight groups, enable derived from the slots, wire enums decoded to
// names, properties present (cloud shape; the integration ignores it).
// No stored schedule → result null (the integration normalises it).
func (s *Server) handleSchedulerGet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceSN string `json:"deviceSN"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEnvelope(w, http.StatusOK, errEnvelope(errnoInvalidValue, "invalid request"))
		return
	}
	st, err := s.state()
	if err != nil {
		writeEnvelope(w, http.StatusOK, errEnvelope(errnoInvalidValue, "store: "+err.Error()))
		return
	}
	if st.SchedTS.IsZero() {
		writeEnvelope(w, http.StatusOK, okEnvelope(nil))
		return
	}
	groups := make([]cloudGroup, 0, len(st.Schedule))
	enabled := opDisable
	for _, sl := range st.Schedule {
		g := cloudGroup{
			StartHour:    sl.StartH,
			StartMinute:  sl.StartM,
			EndHour:      sl.EndH,
			EndMinute:    sl.EndM,
			WorkMode:     workModeName(sl.WorkMode),
			MinSocOnGrid: sl.MinSoc,
			FdSoc:        sl.FDSoc,
			FdPwr:        sl.FDPower,
			Enable:       opDisable,
		}
		if sl.Enabled {
			g.Enable = opEnable
			enabled = opEnable
		}
		groups = append(groups, g)
	}
	writeEnvelope(w, http.StatusOK, okEnvelope(schedulerResult{
		Enable:     enabled,
		Groups:     groups,
		Properties: struct{}{}, // cloud shape; the integration does not read it
	}))
}

// handleSchedulerEnable maps the groups onto the gateway's KindSchedule
// command and waits for the verdict synchronously (the gateway is the
// sole upstream→inverter path, C-003). Packed HHMM fields, the wire
// work-mode enum, and the last-observed maxSoC (the gateway's
// read-modify-write replaces all 80 registers, so an unset maxSoC would
// clobber the operator's value).
func (s *Server) handleSchedulerEnable(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceSN string       `json:"deviceSN"`
		Groups   []cloudGroup `json:"groups"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEnvelope(w, http.StatusOK, errEnvelope(errnoInvalidValue, "invalid request"))
		return
	}
	if len(req.Groups) > groupBound {
		writeEnvelope(w, http.StatusOK, errEnvelope(errnoInvalidValue, "more than 8 groups"))
		return
	}
	st, _ := s.state() // tolerates an empty store (maxSoC defaults)
	params, err := s.scheduleParams(req.Groups, st.Schedule, !st.SchedTS.IsZero())
	if err != nil {
		writeEnvelope(w, http.StatusOK, errEnvelope(errnoInvalidValue, err.Error()))
		return
	}
	s.submitSchedule(r.Context(), w, params)
}

// scheduleParams builds the gateway param map; it validates the group
// shape (sim-level: hour/minute ranges, work-mode names — the gateway
// validates soc/pwr bounds and the window order).
func (s *Server) scheduleParams(groups []cloudGroup, stored []store.Slot, storedAny bool) (map[gateway.Param]int, error) {
	params := map[gateway.Param]int{}
	enabled := opDisable
	for k, g := range groups {
		idx := k + slotBase
		key := func(f gateway.SlotField) gateway.Param { return gateway.SlotParam(idx, f) }
		if g.Enable != opEnable {
			params[key(gateway.SlotEnabled)] = opDisable
			continue
		}
		enabled = opEnable
		if err := checkWindow(g); err != nil {
			return nil, fmt.Errorf("group %d: %w", idx, err)
		}
		wm, ok := workModeToWire[g.WorkMode]
		if !ok {
			return nil, fmt.Errorf("group %d: unknown workMode %q", idx, g.WorkMode)
		}
		params[key(gateway.SlotEnabled)] = opEnable
		params[key(gateway.SlotStart)] = g.StartHour<<8 | g.StartMinute
		params[key(gateway.SlotEnd)] = g.EndHour<<8 | g.EndMinute
		params[key(gateway.SlotWorkMode)] = wm
		params[key(gateway.SlotMinSoc)] = g.MinSocOnGrid
		params[key(gateway.SlotFdSoc)] = g.FdSoc
		params[key(gateway.SlotFdPwr)] = g.FdPwr
		params[key(gateway.SlotMaxSoc)] = storedMaxSoc(idx, stored, storedAny)
	}
	params[gateway.ParamEnabled] = enabled
	return params, nil
}

// storedMaxSoc preserves the last-observed maxSoC for an enabled slot
// (default 100 when nothing is stored).
func storedMaxSoc(idx int, stored []store.Slot, storedAny bool) int {
	if storedAny && idx <= len(stored) && stored[idx-slotBase].MaxSoc > opDisable {
		return stored[idx-slotBase].MaxSoc
	}
	return defaultMaxSoc
}

// checkWindow validates the hour/minute fields (the gateway checks the
// packed window order; the sim checks the field ranges first so the
// error names the group).
func checkWindow(g cloudGroup) error {
	if g.StartHour < opDisable || g.StartHour > hourBound ||
		g.StartMinute < opDisable || g.StartMinute > minuteBound ||
		g.EndHour < opDisable || g.EndHour > hourBound ||
		g.EndMinute < opDisable || g.EndMinute > minuteBound {
		return errors.New("hour must be 0-23 and minute 0-59")
	}
	return nil
}

// submitSchedule submits the KindSchedule command and maps the Wait
// verdict onto the errno envelope.
func (s *Server) submitSchedule(ctx context.Context, w http.ResponseWriter, params map[gateway.Param]int) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), waitTimeout)
	defer cancel()
	cmdID, err := s.gw.Submit(ctx, gateway.Command{
		Kind:   gateway.KindSchedule,
		Params: params,
		Origin: "sim",
	})
	if err != nil {
		if errors.Is(err, gateway.ErrInFlight) {
			writeEnvelope(w, http.StatusOK, errEnvelope(errnoInvalidValue, "a command is already in flight"))
			return
		}
		writeEnvelope(w, http.StatusOK, errEnvelope(errnoInvalidValue, err.Error()))
		return
	}
	res, err := s.gw.Wait(ctx, cmdID)
	if err != nil {
		// The wait window elapsed: the command stays queued until the
		// gateway's TTL (no inverter flow is active) — reported, not retried.
		writeEnvelope(w, http.StatusOK, errEnvelope(errnoInvalidValue,
			"command queued, not confirmed within the wait window (no inverter flow active?)"))
		return
	}
	switch res.Status {
	case gateway.StatusAcked:
		writeEnvelope(w, http.StatusOK, okEnvelope(nil))
	case gateway.StatusFailed:
		writeEnvelope(w, http.StatusOK, errEnvelope(errnoInvalidValue, "command failed: "+res.Detail))
	case gateway.StatusExpired:
		writeEnvelope(w, http.StatusOK, errEnvelope(errnoInvalidValue, "command expired in the queue"))
	case gateway.StatusQueued, gateway.StatusInFlight: // Wait blocks until a terminal status
		writeEnvelope(w, http.StatusOK, errEnvelope(errnoInvalidValue, "command status: "+string(res.Status)))
	}
}
