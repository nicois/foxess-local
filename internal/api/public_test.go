package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Public read-only access (spec §5.5): the config flag, default on —
// unauthenticated reads pass, unauthenticated writes 401 (never 403),
// flag off → unauthenticated reads 401 while tokened reads still pass.

func TestPublicAccessDefaultOn(t *testing.T) {
	h := newHarness(t)
	// role "" = unauthenticated (h.do); harness default: flag on.
	if rec := h.do(t, "GET", "/api/v1/state", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("unauthenticated /state: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.do(t, "GET", "/api/v1/history?range=1h", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("unauthenticated /history: %d %s", rec.Code, rec.Body.String())
	}
	var cfgView struct {
		PublicAccess bool `json:"public_access"`
	}
	rec := h.do(t, "GET", "/api/v1/config", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unauthenticated /config: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfgView); err != nil || !cfgView.PublicAccess {
		t.Fatalf("config view: %+v err=%v, want public_access true", cfgView, err)
	}
}

func TestPublicAccessWriteDenied(t *testing.T) {
	h := newHarness(t)
	rec := h.do(t, httpPut, "/api/v1/work-mode", "", map[string]string{jsonFieldWorkMode: workModeFeedIn})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated write: %d, want 401", rec.Code)
	}
}

func TestPublicAccessOff(t *testing.T) {
	h := newHarness(t)
	h.srv.info.PublicReadOnly = false // the config flag, off
	if rec := h.do(t, "GET", "/api/v1/state", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("read with flag off: %d, want 401", rec.Code)
	}
	// Bearer tokens are untouched — the flag only admits tokenless reads.
	if rec := h.do(t, "GET", "/api/v1/state", RoleReadOnly, nil); rec.Code != http.StatusOK {
		t.Fatalf("read-only token with flag off: %d, want 200", rec.Code)
	}
}

// dashboardReads are the GETs the dashboard needs: a tokenless session
// is admitted to these whenever public_read_only is on.
var dashboardReads = []string{
	"/api/v1/state",
	"/api/v1/history?range=1h",
	"/api/v1/power/average?window=5m",
	"/api/v1/config",
	"/api/v1/forward-mode",
}

// otherReads are the read-only GETs behind the other pages: a tokenless
// session reaches them only with public_all_pages on.
var otherReads = []string{
	"/api/v1/compatibility",
	"/api/v1/write-override",
	"/api/v1/update",
	"/api/v1/audit",
	"/api/v1/maintenance",
}

// TestPublicDashboardOnlyByDefault: [access] public_all_pages defaults
// off — a tokenless session reads the dashboard's endpoints and gets 401
// on the rest; tokens are unaffected.
func TestPublicDashboardOnlyByDefault(t *testing.T) {
	h := newHarness(t)
	for _, p := range dashboardReads {
		if rec := h.do(t, "GET", p, "", nil); rec.Code != http.StatusOK {
			t.Errorf("tokenless %s: %d, want 200", p, rec.Code)
		}
	}
	for _, p := range otherReads {
		rec := h.do(t, "GET", p, "", nil)
		checkProblem(t, rec, http.StatusUnauthorized, "unauthorized")
		if rec := h.do(t, "GET", p, RoleReadOnly, nil); rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Errorf("read-only token %s: %d, want admitted", p, rec.Code)
		}
	}
	var cfgView struct {
		PublicAllPages *bool `json:"public_all_pages"`
	}
	rec := h.do(t, "GET", "/api/v1/config", "", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &cfgView); err != nil || cfgView.PublicAllPages == nil || *cfgView.PublicAllPages {
		t.Fatalf("config view public_all_pages: %s err=%v, want false", rec.Body.String(), err)
	}
}

// TestPublicAllPages: with the flag on, a tokenless session reads every
// read-only endpoint (still never a write).
func TestPublicAllPages(t *testing.T) {
	h := newHarness(t)
	h.srv.info.PublicAllPages = true
	for _, p := range append(append([]string{}, dashboardReads...), otherReads...) {
		if rec := h.do(t, "GET", p, "", nil); rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Errorf("tokenless %s with all pages: %d, want admitted", p, rec.Code)
		}
	}
	if rec := h.do(t, httpPut, "/api/v1/work-mode", "", map[string]string{jsonFieldWorkMode: workModeFeedIn}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("tokenless write: %d, want 401", rec.Code)
	}
}

// TestPublicAllPagesNeedsPublicReadOnly: public_read_only off shuts every
// tokenless read, whatever public_all_pages says.
func TestPublicAllPagesNeedsPublicReadOnly(t *testing.T) {
	h := newHarness(t)
	h.srv.info.PublicReadOnly = false
	h.srv.info.PublicAllPages = true
	for _, p := range append(append([]string{}, dashboardReads...), otherReads...) {
		if rec := h.do(t, "GET", p, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("tokenless %s with public reads off: %d, want 401", p, rec.Code)
		}
	}
}
