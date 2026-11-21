package main

import (
	"fmt"
	"time"
)

// Policy is when to force-discharge. Change it here: the rest of the
// sidecar is plumbing.
type Policy struct {
	StartAbove  float64       // start at or above this price ($/MWh)
	StopBelow   float64       // once running, stop at or below this price
	StartSoc    int           // do not start below this SoC (%)
	CutoffSoc   int           // the inverter stops discharging here (fd_soc_pct)
	MinHold     time.Duration // once started, run at least this long
	Lease       time.Duration // each override's length
	RenewBefore time.Duration // renew when less than this is left
	PriceMaxAge time.Duration // an older price counts as none
}

// Inputs is what one decision sees.
type Inputs struct {
	Now    time.Time
	Price  *Price // nil: no price
	SocPct int    // -1: unknown

	Active bool      // this sidecar holds the override
	Since  time.Time // when it started
	Until  time.Time // when it ends
}

// Action is what to do.
type Action int

// The actions.
const (
	Hold  Action = iota // nothing to change
	Start               // start, or renew, the override until Decision.Until
	Stop                // end the override now
)

// Decision is one decision and why.
type Decision struct {
	Action Action
	Until  time.Time
	Reason string
}

// Decide is the policy, a pure function of its inputs.
func (p Policy) Decide(in Inputs) Decision {
	price := in.Price
	if price != nil && in.Now.Sub(price.At) > p.PriceMaxAge {
		price = nil
	}
	want, why := p.want(in, price)
	switch {
	case want && !in.Active:
		return Decision{Action: Start, Until: p.leaseEnd(in.Now), Reason: why}
	case want && in.Until.Sub(in.Now) < p.RenewBefore:
		return Decision{Action: Start, Until: p.leaseEnd(in.Now), Reason: "renew: " + why}
	case !want && in.Active:
		return Decision{Action: Stop, Reason: why}
	}
	return Decision{Action: Hold, Reason: why}
}

// want reports whether a discharge should be running now.
func (p Policy) want(in Inputs, price *Price) (bool, string) {
	switch {
	case in.SocPct < 0:
		return false, "SoC unknown"
	case in.SocPct <= p.CutoffSoc:
		return false, fmt.Sprintf("SoC %d %% at the cut-off", in.SocPct)
	case !in.Active && in.SocPct < p.StartSoc:
		return false, fmt.Sprintf("SoC %d %% below %d %%", in.SocPct, p.StartSoc)
	case price == nil:
		return false, "no current price"
	case price.RRP >= p.StartAbove:
		return true, fmt.Sprintf("%s at $%.0f/MWh", price.Region, price.RRP)
	case in.Active && in.Now.Sub(in.Since) < p.MinHold:
		return true, "minimum hold"
	case in.Active && price.RRP > p.StopBelow:
		return true, fmt.Sprintf("%s still at $%.0f/MWh", price.Region, price.RRP)
	}
	return false, fmt.Sprintf("%s at $%.0f/MWh", price.Region, price.RRP)
}

// leaseEnd is now + Lease, rounded up to a 5-minute boundary (renewals
// that land on the same minute are not written to the inverter).
func (p Policy) leaseEnd(now time.Time) time.Time {
	const step = 5 * time.Minute
	end := now.Add(p.Lease)
	if t := end.Truncate(step); t.Before(end) {
		return t.Add(step)
	}
	return end
}
