package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"foxesslocal/internal/update"
)

// fakeUpdates scripts the update service.
type fakeUpdates struct {
	view     update.View
	applyErr error
	applied  int
	cleared  int
}

func (f *fakeUpdates) View() update.View                    { return f.view }
func (f *fakeUpdates) CheckNow(context.Context) update.View { f.view.Latest = "v0.2.0"; return f.view }
func (f *fakeUpdates) Apply(context.Context) error          { f.applied++; return f.applyErr }
func (f *fakeUpdates) ClearFailed() error                   { f.cleared++; return nil }

// TestUpdateEndpoints: anyone signed in sees the update state; only an
// installer checks, applies (202, audited) or dismisses a rollback;
// each failure kind has its own code.
func TestUpdateEndpoints(t *testing.T) {
	h := newHarness(t)
	f := &fakeUpdates{view: update.View{Current: "v0.1.0", CanApply: true, Available: true}}
	WithUpdate(f)(h.srv)

	rr := h.do(t, httpGet, "/api/v1/update", RoleReadOnly, nil)
	var v update.View
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &v) != nil || v.Current != "v0.1.0" {
		t.Fatalf("GET: %d %s", rr.Code, rr.Body.String())
	}
	if rr := h.do(t, httpPost, "/api/v1/update/apply", RoleExpert, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("expert apply: %d", rr.Code)
	}
	if rr := h.do(t, httpPost, "/api/v1/update/check", RoleInstaller, nil); rr.Code != http.StatusOK {
		t.Fatalf("check: %d", rr.Code)
	}
	if rr := h.do(t, httpPost, "/api/v1/update/apply", RoleInstaller, nil); rr.Code != http.StatusAccepted || f.applied != 1 {
		t.Fatalf("apply: %d %s", rr.Code, rr.Body.String())
	}
	h.st.Flush()
	if ev, _ := h.st.QueryAudit(testBase.Add(-24*time.Hour), "update"); len(ev) != 1 {
		t.Fatalf("audit: %v", ev)
	}
	for err, code := range map[error]string{
		&update.ReasonError{Reason: "container"}: "update_not_possible",
		update.ErrUpToDate:                       "update_not_available",
		update.ErrChecksum:                       "update_verification_failed",
		update.ErrBadSignature:                   "update_verification_failed",
	} {
		f.applyErr = err
		rr := h.do(t, httpPost, "/api/v1/update/apply", RoleInstaller, nil)
		var p problem
		_ = json.Unmarshal(rr.Body.Bytes(), &p)
		if p.Code != code {
			t.Errorf("%v: %d %s, want %s", err, rr.Code, p.Code, code)
		}
	}
	if rr := h.do(t, httpDelete, "/api/v1/update/rolled-back", RoleInstaller, nil); rr.Code != http.StatusNoContent || f.cleared != 1 {
		t.Fatalf("dismiss: %d", rr.Code)
	}
}
