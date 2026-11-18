package sim

import (
	"github.com/nicois/prox/pkg/capture"

	"foxesslocal/internal/frame"
	"foxesslocal/internal/identity"
	"foxesslocal/internal/store"
)

// decisionForwarded is the framesvc decision every c2s frame is recorded
// under (the gate's policy log; the audit filter is safe on all classes).
const decisionForwarded = store.DecisionForwarded

// Identity snapshot class names (the frame codec's class registry).
const (
	classSnap01 = frame.KindSnap01
	classSnap60 = frame.KindSnap60
)

// deviceID is the inverter identity decoded from the identity snapshots
// (snap01 device ID, snap60 battery module list).
type deviceID struct {
	SN         string
	Model      string
	Firmware   []string
	BatterySNs []string
}

// deviceID returns the latest observed identity, cached once found (the
// SN and module list are static per device; an empty result is not
// cached so the first snapshot burst still resolves it).
func (s *Server) deviceID() deviceID {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	if s.id != nil {
		return *s.id
	}
	rows, err := s.st.AuditFrames(decisionForwarded, capture.DirC2S)
	if err != nil {
		return deviceID{}
	}
	var snap01, snap60 []byte
	for _, r := range rows { // ts-ordered: the last row per class wins
		// frame.Kind is open-ended (SnapKind): the known classes, then the default
		//exhaustive:ignore
		switch r.Class {
		case classSnap01:
			snap01 = r.Payload
		case classSnap60:
			snap60 = r.Payload
		}
	}
	d := decodeIdentity(snap01, snap60)
	if d.SN == "" {
		return d
	}
	s.id = &d
	return d
}

// decodeIdentity decodes the two identity snapshots (the shared
// identity package — mon150 devinfo layout).
func decodeIdentity(p01, p60 []byte) deviceID {
	d := identity.FromSnapshots(p01, p60)
	return deviceID{SN: d.SN, Model: d.Model, Firmware: d.Versions, BatterySNs: d.BatterySNs}
}
