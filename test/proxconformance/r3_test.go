package proxconformance

import "testing"

// TestR3_ServeMode is a tracked-fail: per-flow serve mode — the position's
// R3 "the upstream is optional" (ServeMirror / ServeLocal / ServeReject,
// decided per flow at runtime, with the local leg consumer-provided) ships
// in prox Phase 2, which is not merged into the pinned version. The
// Phase-2 types are intentionally not imported anywhere in this suite.
//
// When the pin moves past the Phase-2 merge, replace the skip with:
//
//  1. one flow per serve mode on the same ladder: the decision hook picks
//     ServeMirror for one flow and ServeLocal for another at runtime;
//  2. the ServeLocal flow has no upstream connection at all; every
//     downstream byte is synthesized by the hook's consumer-provided leg
//     (assert the relay never dials for that flow);
//  3. the ServeLocal client stays healthy indefinitely: N virtual seconds
//     (synctest) of c2s traffic answered only by injected bytes, no
//     upstream leg, connection still open at the end.
func TestR3_ServeMode(t *testing.T) {
	t.Skipf("tracked: Phase-2 Serving is in pinned version %s; check body pending (documented in test comment)", pinnedShort)
}
