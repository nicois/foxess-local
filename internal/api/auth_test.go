package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"foxesslocal/internal/policy"
)

// roleOp is one row of the spec's x-required-role table.
type roleOp struct {
	method string
	path   string
	role   Role
}

// roleOps is the spec's x-required-role table: every operation and its
// minimum role (api/openapi.yaml).
var roleOps = []roleOp{
	{httpGet, "/api/v1/state", RoleReadOnly},
	{httpGet, "/api/v1/history", RoleReadOnly},
	{httpGet, "/api/v1/metrics", RoleReadOnly},
	{httpGet, "/api/v1/power/average?window=5m", RoleReadOnly},
	{httpPut, "/api/v1/work-mode", RoleStandard},
	{httpPut, "/api/v1/schedule", RoleStandard},
	{httpPut, "/api/v1/meter-compensation", RoleExpert},
	{httpPut, "/api/v1/limits", RoleInstaller},
	{httpPost, "/api/v1/registers/read", RoleInstaller},
	{httpGet, pathForwardMode, RoleReadOnly},
	{httpPut, pathForwardMode, RoleInstaller},
	{httpGet, "/api/v1/audit", RoleReadOnly},
	{httpGet, "/api/v1/messages", RoleExpert}, // the raw frame log (owner, 2026-11-19)
	{httpGet, pathMaintenance, RoleReadOnly},
	{httpPost, pathMaintenance, RoleInstaller},
	{httpDelete, pathMaintenance, RoleInstaller},
	{httpGet, "/api/v1/config", RoleReadOnly},
	{httpGet, pathConfigTokens, RoleInstaller},
	{httpPost, pathConfigTokens, RoleInstaller},
	{httpDelete, "/api/v1/config/tokens/t1", RoleInstaller},
	{httpGet, "/api/v1/events", RoleReadOnly},
	{httpGet, pathSettings, RoleReadOnly},
	{httpPut, pathSettings, RoleExpert}, // installer keys: checked per key (TestSettingsExpertTuning)
	{httpGet, pathSetup, RoleInstaller},
	{httpPost, pathSetupComplete, RoleInstaller},
	{httpPost, pathReport, RoleInstaller},
	{httpGet, pathDeviceScan, RoleInstaller},
	{httpPost, "/api/v1/notify/test", RoleInstaller},
}

var roles = []Role{RoleReadOnly, RoleStandard, RoleExpert, RoleInstaller}

// bodyFor gives each operation a minimal acceptable body (the auth check
// runs before body parsing; the body just keeps the request well-formed).
func bodyFor(op roleOp) any {
	switch op.path {
	case "/api/v1/work-mode":
		return map[string]string{jsonFieldWorkMode: workModeFeedIn}
	case "/api/v1/schedule":
		return map[string]any{"enabled": false, "groups": []any{}}
	case "/api/v1/meter-compensation":
		return map[string]int{"w": 0}
	case "/api/v1/limits":
		return map[string]int{jsonFieldExportW: 100}
	case "/api/v1/registers/read":
		return map[string]int{"fn": 3, jsonFieldAddr: 31000, jsonFieldCount: 1}
	case pathForwardMode:
		return map[string]string{jsonFieldMode: forwardModeTelemetryOnly}
	case pathConfigTokens:
		return map[string]string{jsonFieldName: "x", "role": string(RoleReadOnly)}
	}
	return nil
}

// checkRoleOp drives one matrix cell (op as role): below the op's
// minimum role expect 403 insufficient_role with required_role and
// token_role; otherwise expect allow (never an auth failure).
func checkRoleOp(t *testing.T, h *harness, op roleOp, role Role) {
	t.Helper()
	if roleLevel[role] < roleLevel[op.role] {
		rr := h.do(t, op.method, op.path, role, bodyFor(op))
		p := checkProblem(t, rr, http.StatusForbidden, "insufficient_role")
		if p.RequiredRole != string(op.role) || p.TokenRole != string(role) {
			t.Errorf("%s %s as %s: required_role=%q token_role=%q, want %q/%q",
				op.method, op.path, role, p.RequiredRole, p.TokenRole, op.role, role)
		}
		return
	}
	// Allowed: the /events stream must not be driven synchronously
	// (it blocks) — the SSE client asserts the 200 start.
	if op.path == "/api/v1/events" {
		c := h.connectSSE(t, map[string]string{
			headerAuth: "Bearer " + h.tokens[role],
		})
		c.close()
		if c.status() != http.StatusOK {
			t.Errorf("%s %s as %s: SSE start %d, want 200",
				op.method, op.path, role, c.status())
		}
		return
	}
	rr := h.do(t, op.method, op.path, role, bodyFor(op))
	if rr.Code == http.StatusUnauthorized || rr.Code == http.StatusForbidden {
		t.Errorf("%s %s as %s: got %d, want allow (body: %s)",
			op.method, op.path, role, rr.Code, rr.Body.String())
	}
}

// TestAuthRoleMatrix walks every operation × every token role: allow
// (some 2xx — the operation succeeds, or fails on its own body, never on
// auth) or 403 insufficient_role carrying required_role and token_role.
func TestAuthRoleMatrix(t *testing.T) {
	for _, op := range roleOps {
		// A fresh harness per op: the matrix's token CRUD ops mutate the
		// store (POST creates a token, DELETE /t1 revokes the read-only
		// fixture), which would poison the fixture tokens for every op
		// that follows (the /events 401 the matrix must not see).
		h := newHarness(t)
		for _, role := range roles {
			checkRoleOp(t, h, op, role)
		}
	}
}

// TestAuthMissingAndBadToken: no token and a bad token are 401
// problem+json (code unauthorized) on every operation, including the
// ?token= form.
func TestAuthMissingAndBadToken(t *testing.T) {
	h := newHarness(t)
	// The 401 auth path: with the flag on, a missing/bad token on a
	// read-only endpoint is admitted as the public session (200 —
	// public_test.go).
	h.srv.info.PublicReadOnly = false
	for _, op := range roleOps {
		rr := h.do(t, op.method, op.path, "", bodyFor(op))
		checkProblem(t, rr, http.StatusUnauthorized, "unauthorized")

		bad := httptest.NewRequest(op.method, "http://api.test"+op.path, nil)
		bad.Header.Set(headerAuth, "Bearer fl_deadbeef")
		rr = httptest.NewRecorder()
		h.h.ServeHTTP(rr, bad)
		checkProblem(t, rr, http.StatusUnauthorized, "unauthorized")

		q := op.path
		if strings.Contains(q, "?") {
			q += "&token=fl_deadbeef"
		} else {
			q += "?token=fl_deadbeef"
		}
		rr = h.do(t, op.method, q, "", bodyFor(op))
		checkProblem(t, rr, http.StatusUnauthorized, "unauthorized")
	}
}

// TestAuthQueryToken: ?token= is accepted on GET /events only — the
// EventSource cannot send a header (audit S4: a query-string token
// lands in browser history, proxy logs and shell history, so no other
// endpoint takes one; the stream's own test is TestSSEAuthQueryToken).
func TestAuthQueryToken(t *testing.T) {
	h := newHarness(t)
	h.srv.info.PublicReadOnly = false
	rr := h.do(t, httpGet, "/api/v1/state?token="+h.tokens[RoleReadOnly], "", nil)
	checkProblem(t, rr, http.StatusUnauthorized, "unauthorized")
	rr = h.do(t, httpDelete, "/api/v1/config/tokens/missing?token="+h.tokens[RoleInstaller], "", nil)
	checkProblem(t, rr, http.StatusUnauthorized, "unauthorized")
}

// TestAuthWrongScheme: a non-Bearer Authorization scheme is 401.
func TestAuthWrongScheme(t *testing.T) {
	h := newHarness(t)
	// The 401 auth path: with the flag on, a non-Bearer scheme on a
	// read-only endpoint is admitted as the public session (200).
	h.srv.info.PublicReadOnly = false
	req := httptest.NewRequest(httpGet, "http://api.test/api/v1/state", nil)
	req.Header.Set(headerAuth, "Basic "+h.tokens[RoleReadOnly])
	rr := httptest.NewRecorder()
	h.h.ServeHTTP(rr, req)
	checkProblem(t, rr, http.StatusUnauthorized, "unauthorized")
}

// TestHealthz: unauthenticated liveness, no secret data.
func TestHealthz(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/healthz", "/api/v1/healthz"} {
		rr := h.do(t, httpGet, path, "", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: got %d, want 200", path, rr.Code)
		}
		var body map[string]string
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s body: %v", path, err)
		}
		if body["status"] != "ok" {
			t.Fatalf("%s: status=%q, want ok", path, body["status"])
		}
	}
}

// TestForwardModeSeamDefaults: the config-backed seam starts at the
// given mode and the API reflects it.
func TestForwardModeSeamDefaults(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, httpGet, pathForwardMode, RoleReadOnly, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /forward-mode: got %d, want 200", rr.Code)
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Mode != string(policy.ModeTelemetryOnly) {
		t.Fatalf("mode: got %q, want telemetry_only", body.Mode)
	}
}
