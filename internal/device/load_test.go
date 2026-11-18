package device

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"foxesslocal/internal/gateway"
	"foxesslocal/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "d.db"), "dev")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// seedReference records the reference KH10's identity and settings
// reads (as the settings poll does).
func seedReference(st *store.Store, master uint16) {
	t0 := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	model := make([]uint16, 15)
	const name = "KH10           "
	for i := range len(name) {
		model[i] = uint16(name[i])
	}
	st.RecordRegisters(t0, 30000, model)
	st.RecordRegisters(t0, 30016, []uint16{master, 0x103, 0x171})
	st.RecordRegisters(t0, 41000, []uint16{1})
	block := make([]uint16, 40)
	for addr, v := range map[int]uint16{
		41007: 450, 41008: 450, 41010: 100, 41011: 11, 41012: 100, 41016: 100,
		41022: 12500, 41024: 5000, 41027: 560, 41028: 400, 41029: 0xFFCD, 41032: 100,
	} {
		block[addr-41007] = v
	}
	st.RecordRegisters(t0, 41007, block)
	st.Flush()
}

// TestGatewayAuthorizer: the store-backed gate allows the verified
// device, refuses a firmware bump as untested until the installer grants
// that exact identity, and refuses again once the grant is cleared.
func TestGatewayAuthorizer(t *testing.T) {
	wm := gateway.Command{Kind: gateway.KindWorkMode, Params: map[gateway.Param]int{gateway.ParamMode: 1}}

	st := openStore(t)
	authz := GatewayAuthorizer(st)
	if err := authz(wm); !errors.Is(err, ErrNotIdentified) {
		t.Fatalf("empty store: %v, want not identified", err)
	}
	seedReference(st, 0x173)
	if err := authz(wm); err != nil {
		t.Fatalf("verified KH10: %v", err)
	}

	st2 := openStore(t)
	seedReference(st2, 0x174)
	authz2 := GatewayAuthorizer(st2)
	if err := authz2(wm); !errors.Is(err, ErrWriteUnverified) {
		t.Fatalf("fw 1.74: %v, want unverified", err)
	}
	snap, err := Load(st2)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveOverride(st2, Override{Model: snap.Model, Firmware: snap.Firmware, By: "installer"}); err != nil {
		t.Fatal(err)
	}
	if err := authz2(wm); err != nil {
		t.Fatalf("fw 1.74 with the grant: %v", err)
	}
	if ov, err := LoadOverride(st2); err != nil || ov == nil || ov.By != "installer" {
		t.Fatalf("override round trip: %+v %v", ov, err)
	}
	if err := ClearOverride(st2); err != nil {
		t.Fatal(err)
	}
	if err := authz2(wm); !errors.Is(err, ErrWriteUnverified) {
		t.Fatalf("after clearing: %v, want unverified", err)
	}
}
