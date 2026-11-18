package frame

// TriggerReads is the two initial register reads the real cloud sends
// right after the bootstrap trio (cutover doc §3.2, 11-15 corpus
// session 1794729390376-1, s2c off=45/64 — verbatim): fn03 @0x9C40 x6
// (the inverter's system-clock registers) ~4.8 s after the 2a, then
// fn04 @0x277E x28 ~9.9 s after. The inverter arms its push mode
// (the fn=64 wrapper stream, doc §3.3) only after receiving them, so
// a local bootstrap in off/telemetry_only must inject them. Same class
// as the trio: local injection with no upstream bytes (C-001/C-002
// clean) and fixed validated reads, not operator input (C-003 holds).
// The inverter processes the read by its Modbus PDU — the corpus
// device/fn bytes are fixed, not a discriminator (the cloud stamps a
// fresh device tail per frame).
var TriggerReads = [2][]byte{
	{0x7f, 0x7f, 0x11, 0x01, 0xba, 0x0b, 0x47, 0x00, 0x06, 0x01, 0x03, 0x9c, 0x40, 0x00, 0x06, 0x5f, 0x62, 0xf7, 0xf7},
	{0x7f, 0x7f, 0x11, 0xe6, 0xba, 0x0b, 0x47, 0x00, 0x06, 0x01, 0x04, 0x27, 0x7e, 0x00, 0x1c, 0x59, 0x74, 0xf7, 0xf7},
}
