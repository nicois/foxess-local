package frame

import "fmt"

// KH lifetime energy totals (foxess_modbus KH_PRE133 / KH_133 maps,
// 2026-11-19, for the foxess-control local backend): one holding-register
// block, 32-bit values high word first, 0.1 kWh. Each total is followed
// by a 16-bit "today" word, which is not decoded.
const (
	KHEnergyAddr  = 32000 // 32000..32023
	KHEnergyCount = 24
)

// The totals' offsets in the block.
const (
	khEnPV         = 0  // 32000/32001 Solar Generation Total
	khEnCharge     = 3  // 32003/32004 Battery Charge Total
	khEnDischarge  = 6  // 32006/32007 Battery Discharge Total
	khEnFeedIn     = 9  // 32009/32010 Feed-in Total
	khEnGridImport = 12 // 32012/32013 Grid Consumption Total
	khEnYield      = 15 // 32015/32016 Yield Total (inverter AC output)
	khEnInput      = 18 // 32018/32019 Input Energy Total
	khEnLoad       = 21 // 32021/32022 Load Energy Total
)

// KHEnergy is one read of the lifetime totals, in 0.1 kWh.
type KHEnergy struct {
	PV, Charge, Discharge, FeedIn, GridImport, Yield, Input, Load uint32
}

// ParseKHEnergy decodes the KHEnergyAddr block.
func ParseKHEnergy(regs []uint16) (KHEnergy, error) {
	if len(regs) != KHEnergyCount {
		return KHEnergy{}, fmt.Errorf("frame: ParseKHEnergy: want %d registers, got %d", KHEnergyCount, len(regs))
	}
	u32 := func(off int) uint32 { return uint32(regs[off])<<16 | uint32(regs[off+1]) }
	return KHEnergy{
		PV: u32(khEnPV), Charge: u32(khEnCharge), Discharge: u32(khEnDischarge),
		FeedIn: u32(khEnFeedIn), GridImport: u32(khEnGridImport), Yield: u32(khEnYield),
		Input: u32(khEnInput), Load: u32(khEnLoad),
	}, nil
}
