package qmi

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// exampleIMEI is a placeholder (the well-known documentation IMEI,
// Luhn-valid), never a real device's.
const exampleIMEI = "490154203237518"

// fakeModem answers QMUX requests the way the UZ801's modem did on
// 2026-11-23/24: an unsolicited CTL indication (0x0027) first, then
// the replies.
type fakeModem struct {
	mu       sync.Mutex
	out      chan []byte
	closed   chan struct{}
	once     sync.Once
	imei     string
	dmsErr   uint16 // a non-zero QMI error on the DMS reply
	silent   bool   // never answer
	released []byte // the CTL release request's TLV 0x01
	sentIndi bool
}

func newFakeModem(t *testing.T, imei string) *fakeModem {
	t.Helper()
	m := &fakeModem{out: make(chan []byte, 16), closed: make(chan struct{}), imei: imei}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func (m *fakeModem) Read(p []byte) (int, error) {
	select {
	case f := <-m.out:
		return copy(p, f), nil
	case <-m.closed:
		return 0, io.EOF
	}
}

func (m *fakeModem) Close() error {
	m.once.Do(func() { close(m.closed) })
	return nil
}

func (m *fakeModem) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.silent {
		return len(p), nil
	}
	if len(p) < 6 || p[0] != qmuxMarker {
		return 0, errors.New("fake: not a qmux frame")
	}
	svc, cid, sdu := p[4], p[5], p[6:]
	if svc == svcCTL {
		return len(p), m.ctlReply(sdu)
	}
	if svc == svcDMS {
		m.dmsReply(cid, sdu)
	}
	return len(p), nil
}

func (m *fakeModem) ctlReply(sdu []byte) error {
	tid := sdu[1]
	msg := binary.LittleEndian.Uint16(sdu[2:])
	tl, err := parseTLVs(sdu[6:])
	if err != nil {
		return err
	}
	if !m.sentIndi {
		m.sentIndi = true
		m.out <- frame(svcCTL, 0, ctlSDU(ctlIndication, 0, 0x0027, nil))
	}
	switch msg {
	case ctlGetClientID:
		m.out <- frame(svcCTL, 0, ctlSDU(ctlResponse, tid, msg,
			append(tlv(tlvResult, []byte{0, 0, 0, 0}), tlv(0x01, []byte{tl[0x01][0], 7})...)))
	case ctlReleaseClientID:
		m.released = tl[0x01]
		m.out <- frame(svcCTL, 0, ctlSDU(ctlResponse, tid, msg, tlv(tlvResult, []byte{0, 0, 0, 0})))
	}
	return nil
}

func (m *fakeModem) dmsReply(cid byte, sdu []byte) {
	tid := binary.LittleEndian.Uint16(sdu[1:])
	msg := binary.LittleEndian.Uint16(sdu[3:])
	if msg != dmsGetSerials {
		return
	}
	// an indication with the same message id must be skipped
	m.out <- frame(svcDMS, cid, svcSDU(svcIndication, 0, msg, nil))
	res := make([]byte, 4)
	if m.dmsErr != 0 {
		binary.LittleEndian.PutUint16(res[0:], 1)
		binary.LittleEndian.PutUint16(res[2:], m.dmsErr)
	}
	body := tlv(tlvResult, res)
	if m.dmsErr == 0 {
		body = append(body, tlv(0x10, []byte("0"))...)
		body = append(body, tlv(tlvIMEI, []byte(m.imei))...)
	}
	m.out <- frame(svcDMS, cid, svcSDU(svcResponse, tid, msg, body))
}

func TestReadIMEI(t *testing.T) {
	m := newFakeModem(t, exampleIMEI)
	got, err := ReadIMEI(context.Background(), m)
	if err != nil {
		t.Fatalf("ReadIMEI: %v", err)
	}
	if got.Digits() != exampleIMEI {
		t.Fatalf("IMEI %q, want %q", got.Digits(), exampleIMEI)
	}
	if string(m.released) != string([]byte{svcDMS, 7}) {
		t.Fatalf("release TLV %v, want DMS client 7", m.released)
	}
}

func TestReadIMEIErrorResult(t *testing.T) {
	m := newFakeModem(t, exampleIMEI)
	m.dmsErr = 0x0003 // QMI_ERR_INTERNAL
	_, err := ReadIMEI(context.Background(), m)
	if err == nil || !strings.Contains(err.Error(), "error 3") {
		t.Fatalf("err %v, want the QMI error", err)
	}
	if m.released == nil {
		t.Fatal("client id not released after an error")
	}
}

func TestReadIMEITimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newFakeModem(t, exampleIMEI)
		m.silent = true
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := ReadIMEI(ctx, m)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err %v, want deadline exceeded", err)
		}
	})
}

func TestReadIMEIRejectsPlaceholders(t *testing.T) {
	for _, bad := range []string{
		"000000000000000", // Luhn-valid, but what an erased modem reports
		"0",
		"49015420323751",   // 14 digits
		"490154203237519",  // check digit wrong
		"49015420323751x",  // not digits
		"4901542032375180", // 16 digits
	} {
		m := newFakeModem(t, bad)
		_, err := ReadIMEI(context.Background(), m)
		if !errors.Is(err, ErrNoIMEI) {
			t.Errorf("%q: err %v, want ErrNoIMEI", bad, err)
			continue
		}
		if strings.Contains(err.Error(), bad) {
			t.Errorf("%q: the error text carries the value: %v", bad, err)
		}
	}
}

func TestParse(t *testing.T) {
	if _, err := Parse(exampleIMEI); err != nil {
		t.Fatalf("Parse(valid): %v", err)
	}
	if _, err := Parse(" " + exampleIMEI); !errors.Is(err, ErrNoIMEI) {
		t.Fatalf("Parse(padded): %v", err)
	}
}

// TestIMEINeverPrinted: the formatted forms show at most the last four
// digits, so a log line or an error wrap can't leak it.
func TestIMEINeverPrinted(t *testing.T) {
	i, err := Parse(exampleIMEI)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{i.String(), i.GoString()} {
		if strings.Contains(s, exampleIMEI[:11]) {
			t.Fatalf("formatted IMEI %q shows more than the last four digits", s)
		}
		if !strings.Contains(s, "*******7518") {
			t.Fatalf("formatted IMEI %q, want the last four digits", s)
		}
	}
}

func TestMatchesLast(t *testing.T) {
	i, err := Parse(exampleIMEI)
	if err != nil {
		t.Fatal(err)
	}
	if !i.MatchesLast("3237518") {
		t.Fatal("the last 7 digits don't match")
	}
	for _, s := range []string{"3237519", "237518", "", "32375180", "323751x"} {
		if i.MatchesLast(s) {
			t.Errorf("%q matched", s)
		}
	}
}
