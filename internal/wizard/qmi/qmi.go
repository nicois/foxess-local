// Package qmi reads the stick modem's IMEI over QMI (design D-031, "The
// IMEI as proof of physical access"). The modem's control device
// (/dev/wwan0qmi0, root 0600 once ModemManager is masked) carries raw
// QMUX frames, one per read: a CTL request for a DMS client id, the DMS
// "get device serial numbers" request, and the release of the client
// id. Checked on a UZ801 v3.0, 2026-11-23/24.
//
// The IMEI is a secret of sorts: nothing in this package formats it in
// full, and errors never carry it.
package qmi

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// QMUX framing.
const (
	qmuxMarker = 0x01
	qmuxHeader = 6 // marker, length (2), flags, service, client id

	svcCTL = 0x00
	svcDMS = 0x02

	ctlGetClientID     = 0x0022
	ctlReleaseClientID = 0x0023
	dmsGetSerials      = 0x0025

	// the SDU control flags of a reply: CTL and services number them
	// differently.
	ctlResponse   = 0x01
	ctlIndication = 0x02
	svcResponse   = 0x02
	svcIndication = 0x04

	ctlSDUHeader = 6 // flags, transaction (1), message (2), length (2)
	svcSDUHeader = 7 // flags, transaction (2), message (2), length (2)
	tlvHeader    = 3 // type, length (2)

	tlvResult   = 0x02 // result (2), error (2)
	tlvClientID = 0x01 // service, client id
	tlvIMEI     = 0x11

	resultLen   = 4
	clientIDLen = 2

	readBuf = 4096
)

// imeiLen is an IMEI's length: an 8-digit model code (TAC), a 6-digit
// serial and a Luhn check digit.
const imeiLen = 15

// LastDigits is how many trailing digits an operator types to prove
// they had the cover off: the whole serial and the check digit.
const LastDigits = 7

// shownDigits is how many trailing digits the formatted forms show.
const shownDigits = 4

// ErrNoIMEI is a modem that reports no usable IMEI: missing, the wrong
// length, not digits, a failed check digit, or all zeros (what an
// erased modem calibration reports).
var ErrNoIMEI = errors.New("qmi: the modem reports no valid IMEI")

// IMEI is a validated IMEI. Its formatted forms show only the last four
// digits.
type IMEI struct{ digits string }

// Parse validates s as an IMEI.
func Parse(s string) (IMEI, error) {
	if len(s) != imeiLen || strings.Trim(s, "0") == "" || !luhn(s) {
		return IMEI{}, ErrNoIMEI
	}
	return IMEI{digits: s}, nil
}

// Digits is the full IMEI. Callers compare it; they never log it.
func (i IMEI) Digits() string { return i.digits }

// String shows the last four digits only.
func (i IMEI) String() string {
	if len(i.digits) < shownDigits {
		return "(none)"
	}
	return strings.Repeat("*", len(i.digits)-shownDigits) + i.digits[len(i.digits)-shownDigits:]
}

// GoString keeps %#v as masked as %v.
func (i IMEI) GoString() string { return "qmi.IMEI(" + i.String() + ")" }

// MatchesLast reports whether s is exactly the IMEI's last LastDigits
// digits, compared in constant time.
func (i IMEI) MatchesLast(s string) bool {
	if len(s) != LastDigits || len(i.digits) != imeiLen {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(s), []byte(i.digits[imeiLen-LastDigits:])) == 1
}

func luhn(s string) bool {
	sum := 0
	for k := range len(s) {
		d := int(s[k] - '0')
		if d < 0 || d > 9 {
			return false
		}
		if k%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}

// ReadIMEI asks the modem on dev for its IMEI. It closes dev when ctx
// ends first (the only way to unblock a pending read). The caller opens
// a fresh device for each call and closes it afterwards, which also
// ends the read loop.
func ReadIMEI(ctx context.Context, dev io.ReadWriteCloser) (IMEI, error) {
	c := newClient(dev)
	defer close(c.done)
	stop := context.AfterFunc(ctx, func() { _ = dev.Close() })
	defer stop()
	imei, err := c.readIMEI()
	if ctx.Err() != nil {
		return IMEI{}, fmt.Errorf("qmi: %w", ctx.Err())
	}
	return imei, err
}

type client struct {
	dev    io.ReadWriter
	frames chan []byte
	rerr   chan error
	done   chan struct{} // closed when the request is over
	ctlTID byte          // CTL transactions are one byte
	svcTID uint16        // service transactions two
}

func newClient(dev io.ReadWriter) *client {
	c := &client{dev: dev, frames: make(chan []byte), rerr: make(chan error, 1), done: make(chan struct{})}
	go c.readLoop()
	return c
}

// readLoop hands each frame to the request waiting for it. It ends on
// the first read error (the device closed); a frame that arrives once
// the request is over is dropped.
func (c *client) readLoop() {
	for {
		b := make([]byte, readBuf)
		n, err := c.dev.Read(b)
		if err != nil {
			c.rerr <- err
			return
		}
		select {
		case c.frames <- b[:n]:
		case <-c.done:
		}
	}
}

func (c *client) readIMEI() (IMEI, error) {
	cid, err := c.getClientID(svcDMS)
	if err != nil {
		return IMEI{}, err
	}
	imei, err := c.getSerials(cid)
	if rerr := c.releaseClientID(svcDMS, cid); err == nil && rerr != nil {
		return IMEI{}, rerr
	}
	return imei, err
}

func (c *client) getClientID(svc byte) (byte, error) {
	tl, err := c.ctl(ctlGetClientID, tlv(tlvClientID, []byte{svc}))
	if err != nil {
		return 0, err
	}
	v := tl[tlvClientID]
	if len(v) != clientIDLen || v[0] != svc {
		return 0, errors.New("qmi: get client id: malformed reply")
	}
	return v[1], nil
}

func (c *client) releaseClientID(svc, cid byte) error {
	_, err := c.ctl(ctlReleaseClientID, tlv(tlvClientID, []byte{svc, cid}))
	return err
}

func (c *client) getSerials(cid byte) (IMEI, error) {
	c.svcTID++
	tid := c.svcTID
	if _, err := c.dev.Write(frame(svcDMS, cid, svcSDU(0, tid, dmsGetSerials, nil))); err != nil {
		return IMEI{}, fmt.Errorf("qmi: write: %w", err)
	}
	tl, err := c.await(func(f []byte) (tlvs, bool, error) {
		if f[4] != svcDMS || f[5] != cid {
			return nil, false, nil
		}
		return matchSDU(f[qmuxHeader:], svcSDUHeader, svcResponse, uint32(tid), dmsGetSerials)
	})
	if err != nil {
		return IMEI{}, err
	}
	return Parse(string(tl[tlvIMEI]))
}

func (c *client) ctl(msg uint16, payload []byte) (tlvs, error) {
	c.ctlTID++
	tid := c.ctlTID
	if _, err := c.dev.Write(frame(svcCTL, 0, ctlSDU(0, tid, msg, payload))); err != nil {
		return nil, fmt.Errorf("qmi: write: %w", err)
	}
	return c.await(func(f []byte) (tlvs, bool, error) {
		if f[4] != svcCTL {
			return nil, false, nil
		}
		return matchSDU(f[qmuxHeader:], ctlSDUHeader, ctlResponse, uint32(tid), msg)
	})
}

// await reads frames until match accepts one; it skips indications and
// other services' traffic.
func (c *client) await(match func([]byte) (tlvs, bool, error)) (tlvs, error) {
	for {
		select {
		case f := <-c.frames:
			if len(f) < qmuxHeader || f[0] != qmuxMarker {
				continue
			}
			tl, ok, err := match(f)
			if err != nil {
				return nil, err
			}
			if ok {
				return tl, nil
			}
		case err := <-c.rerr:
			c.rerr <- err
			return nil, fmt.Errorf("qmi: read: %w", err)
		}
	}
}

// matchSDU checks an SDU's flags, transaction and message, then its
// result TLV.
func matchSDU(sdu []byte, header int, flags byte, tid uint32, msg uint16) (tlvs, bool, error) {
	if len(sdu) < header || sdu[0] != flags {
		return nil, false, nil
	}
	var gotTID uint32
	var gotMsg uint16
	if header == ctlSDUHeader {
		gotTID, gotMsg = uint32(sdu[1]), binary.LittleEndian.Uint16(sdu[2:])
	} else {
		gotTID, gotMsg = uint32(binary.LittleEndian.Uint16(sdu[1:])), binary.LittleEndian.Uint16(sdu[3:])
	}
	if gotTID != tid || gotMsg != msg {
		return nil, false, nil
	}
	tl, err := parseTLVs(sdu[header:])
	if err != nil {
		return nil, false, err
	}
	r := tl[tlvResult]
	if len(r) != resultLen {
		return nil, false, fmt.Errorf("qmi: message 0x%04x: no result", msg)
	}
	if res := binary.LittleEndian.Uint16(r[0:]); res != 0 {
		return nil, false, fmt.Errorf("qmi: message 0x%04x: error %d", msg, binary.LittleEndian.Uint16(r[2:]))
	}
	return tl, true, nil
}

type tlvs map[byte][]byte

func parseTLVs(b []byte) (tlvs, error) {
	t := tlvs{}
	for len(b) > 0 {
		if len(b) < tlvHeader {
			return nil, errors.New("qmi: short TLV")
		}
		n := int(binary.LittleEndian.Uint16(b[1:]))
		if len(b) < tlvHeader+n {
			return nil, errors.New("qmi: TLV overruns the message")
		}
		t[b[0]] = b[tlvHeader : tlvHeader+n]
		b = b[tlvHeader+n:]
	}
	return t, nil
}

func tlv(typ byte, v []byte) []byte {
	out := make([]byte, tlvHeader, tlvHeader+len(v))
	out[0] = typ
	binary.LittleEndian.PutUint16(out[1:], uint16(len(v))) //nolint:gosec // G115: QMI TLVs here are a few bytes
	return append(out, v...)
}

// frame wraps an SDU in a QMUX header; the length counts everything
// after the marker.
func frame(svc, cid byte, sdu []byte) []byte {
	out := make([]byte, qmuxHeader, qmuxHeader+len(sdu))
	out[0] = qmuxMarker
	binary.LittleEndian.PutUint16(out[1:], uint16(qmuxHeader-1+len(sdu))) //nolint:gosec // G115: a few bytes
	out[4], out[5] = svc, cid
	return append(out, sdu...)
}

func ctlSDU(flags, tid byte, msg uint16, payload []byte) []byte {
	out := make([]byte, ctlSDUHeader, ctlSDUHeader+len(payload))
	out[0], out[1] = flags, tid
	binary.LittleEndian.PutUint16(out[2:], msg)
	binary.LittleEndian.PutUint16(out[4:], uint16(len(payload))) //nolint:gosec // G115: a few bytes
	return append(out, payload...)
}

func svcSDU(flags byte, tid, msg uint16, payload []byte) []byte {
	out := make([]byte, svcSDUHeader, svcSDUHeader+len(payload))
	out[0] = flags
	binary.LittleEndian.PutUint16(out[1:], tid)
	binary.LittleEndian.PutUint16(out[3:], msg)
	binary.LittleEndian.PutUint16(out[5:], uint16(len(payload))) //nolint:gosec // G115: a few bytes
	return append(out, payload...)
}
