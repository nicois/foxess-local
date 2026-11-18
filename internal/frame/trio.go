package frame

import "time"

// Bootstrap response layout constants (profile §3, 2026-11-07 structure).
const (
	trioBodyHdr = 7   // device(4) + fn(1) + plen(2)
	trioTime    = 256 // seconds per S-counter tick (profile §3)
)

// TrioParams is a session's bootstrap-response parameter set. The values
// the real cloud stamps are time-derived (TrioParamsAt, profile §3):
// S is a 256-second counter, FnBase the generation second mod 256;
// the observed per-response fn deltas (0/0/+1, 0/+1/+1, 0/0/0) are
// second boundaries falling between frame generations. The inverter
// rejects a stale S (12 s re-registration loop), so a local trio
// derives them from the clock at emission, not fixed bytes.
type TrioParams struct {
	SessionBytes [2]byte // device[2:4] of every response (per-session)
	FnBase       byte    // response fn = FnBase + Deltas[i]
	Deltas       [3]byte // per-response fn offset
}

// BootstrapTrio generates the three s2c bootstrap responses for a session's
// three c2s registration frames. reqDev0 is the first device byte of each
// request, in request order (registration first). Response i:
//
//	7e7e | [reqDev0[i]|0x80, 0x6a, S, S] | FnBase+Deltas[i] | plen u16 BE |
//	payload | crc16(dev4+fn+plen+payload) LE | e7e7
//
// r1: plen 0005, payload 01 01 01 00 00 (18 B); r2: plen 0000 (13 B);
// r3: plen 0001, payload 01 (14 B).
func BootstrapTrio(reqDev0 [3]byte, p TrioParams) [3][]byte {
	var out [3][]byte
	term := termByte(startE)
	for i := range trioPayloads {
		pl := trioPayloads[i]
		plen := plenBytes(len(pl))
		body := make([]byte, 0, trioBodyHdr+len(pl))
		body = append(body, reqDev0[i]|respBit, SessionDev1, p.SessionBytes[0], p.SessionBytes[1])
		body = append(body, p.FnBase+p.Deltas[i], plen[0], plen[1])
		body = append(body, pl...)
		crc := crcBytes(CRC16(body))
		raw := make([]byte, 0, frameOverhead+len(pl))
		raw = append(raw, startE, startE)
		raw = append(raw, body...)
		raw = append(raw, crc[0], crc[1])
		raw = append(raw, term, term)
		out[i] = raw
	}
	return out
}

// TrioParamsAt derives the bootstrap-response parameters the real cloud
// stamps on a fresh session (profile §3, 11-11 corpus, 11/11 sessions):
// S — the response device[2:4] bytes — is a 256-second time counter,
// (unix/256) % 65536, and q — each response's fn byte — is the generation
// second mod 256. Deltas stay 0/0/0: all three responses are generated at
// the same instant, and (q, q, q) is an observed pattern.
func TrioParamsAt(now time.Time) TrioParams {
	u := now.Unix()
	s := uint16(u / trioTime) //nolint:gosec // G115: mod 65536 by design
	return TrioParams{
		SessionBytes: [2]byte{byte(s >> 8), byte(s)}, //nolint:gosec,mnd // G115: 16-bit value
		FnBase:       byte(u % trioTime),             //nolint:gosec // G115: mod 256 by design
		Deltas:       [3]byte{0, 0, 0},
	}
}
