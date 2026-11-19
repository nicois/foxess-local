package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"foxesslocal/internal/gateway"
	"foxesslocal/internal/siteclock"
)

// clockOrigin tags the automatic clock writes at the gateway.
const clockOrigin = "clock-sync"

// clockWait bounds one clock write's wait for the inverter's echo.
const clockWait = 15 * time.Second

// audKindClock is the clock correction's audit kind.
const audKindClock = "clock"

// siteZone is the site's time zone (settings), nil until chosen.
func (h *handle) siteZone() *time.Location {
	if h.set == nil {
		return nil
	}
	return h.set.Get().SiteZone
}

// clockReading is the newest reading of the inverter's clock.
func (h *handle) clockReading() ([6]int, time.Time, bool) {
	return siteclock.ReadingFrom(h.st.RegisterRead)
}

// wallNow is the inverter's wall time now (the forward windows' clock).
func (h *handle) wallNow() (time.Time, bool) {
	rtc, at, ok := h.clockReading()
	return siteclock.WallNow(h.siteZone(), rtc, at, ok, h.clk())
}

// clockSyncer keeps the inverter's clock to the site's time (owner,
// 2026-11-19, option b): every write through the gateway (C-003, C-008).
func (h *handle) clockSyncer() *siteclock.Syncer {
	return &siteclock.Syncer{
		Zone:       h.siteZone,
		Reading:    h.clockReading,
		HostSynced: siteclock.HostSynced,
		Online:     h.gw.HasFlow,
		Set:        h.setClock,
		Audit:      func(d string) { h.st.RecordAudit(h.clk(), audKindClock, d) },
		Clock:      h.clk,
	}
}

// setClock writes one wall time and waits for the inverter's echo.
func (h *handle) setClock(ctx context.Context, wall [6]int) error {
	params := map[gateway.Param]int{}
	for i, k := range []gateway.Param{
		gateway.ParamYear, gateway.ParamMonth, gateway.ParamDay,
		gateway.ParamHour, gateway.ParamMinute, gateway.ParamSecond,
	} {
		params[k] = wall[i]
	}
	id, err := h.gw.Submit(ctx, gateway.Command{Kind: gateway.KindClock, Origin: clockOrigin, Params: params})
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, clockWait)
	defer cancel()
	res, err := h.gw.Wait(wctx, id)
	switch {
	case err != nil:
		return fmt.Errorf("no echo: %w", err)
	case res.Status != gateway.StatusAcked:
		return errors.New(string(res.Status) + " " + res.Detail)
	}
	return nil
}
