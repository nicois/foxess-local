package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

var tokenValueRe = regexp.MustCompile(`^fl_[0-9a-f]{64}$`)

// TestTokenLifecycle: create (the plaintext exactly once) → list → use
// → revoke → the revoked token 401s.
func TestTokenLifecycle(t *testing.T) {
	h := newHarness(t)
	// The revoked-token 401 below must not be masked by the public
	// read-only admission (flag on admits tokenless reads as 200).
	h.srv.info.PublicReadOnly = false
	rr := h.do(t, httpPost, pathConfigTokens, RoleInstaller,
		map[string]string{jsonFieldName: testTokenName, jsonFieldRole: string(RoleStandard)})
	wantStatus(t, rr, http.StatusCreated)
	var created tokenCreatedBody
	decodeJSON(t, rr, &created)
	if !tokenValueRe.MatchString(created.Token) {
		t.Fatalf("token: got %q, want fl_ + 64 hex", created.Token)
	}
	if created.Name != testTokenName || created.Role != string(RoleStandard) || created.ID == "" {
		t.Fatalf("created: got %+v", created)
	}
	// The plaintext works immediately.
	rr = h.doAuth(t, httpGet, "/api/v1/state", created.Token, nil)
	wantStatus(t, rr, http.StatusOK)
	// The list carries no secrets.
	rr = h.do(t, httpGet, pathConfigTokens, RoleInstaller, nil)
	var list struct {
		Items []tokenInfoBody `json:"items"`
	}
	decodeJSON(t, rr, &list)
	found := false
	for _, tk := range list.Items {
		if tk.ID == created.ID {
			found = true
			if tk.Name != testTokenName || tk.Role != string(RoleStandard) {
				t.Fatalf("list entry: got %+v", tk)
			}
		}
	}
	if !found {
		t.Fatalf("list: token %s not found in %+v", created.ID, list.Items)
	}
	if body := rr.Body.String(); contains(body, created.Token) {
		t.Fatalf("list leaked the plaintext token")
	}
	// Revoke: 204, then the token is 401 and the id is gone (404).
	rr = h.do(t, httpDelete, pathConfigTokens+"/"+created.ID, RoleInstaller, nil)
	wantStatus(t, rr, http.StatusNoContent)
	rr = h.doAuth(t, httpGet, "/api/v1/state", created.Token, nil)
	checkProblem(t, rr, http.StatusUnauthorized, "unauthorized")
	rr = h.do(t, httpDelete, pathConfigTokens+"/"+created.ID, RoleInstaller, nil)
	checkProblem(t, rr, http.StatusNotFound, "not_found")
}

// TestTokenHashPersisted: the backing file stores the SHA-256 hash,
// never the plaintext (atomic rewrite, hash-prefixed hex).
func TestTokenHashPersisted(t *testing.T) {
	t.Parallel()
	clk := &vclock{now: testBase}
	path := filepath.Join(t.TempDir(), "tokens.json")
	ts, err := NewTokenStore(path, clk.get)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}
	_, plaintext, err := ts.Create(testTokenName, RoleStandard)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	//nolint:gosec // G304: the path is the harness's own temp file.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tokens file: %v", err)
	}
	if contains(string(data), plaintext) {
		t.Fatalf("tokens file contains the plaintext: %s", data)
	}
	if !contains(string(data), sha256Hex(plaintext)) {
		t.Fatalf("tokens file misses the hash: %s", data)
	}
	// A reopened store authenticates the same token (hash-only).
	ts2, err := NewTokenStore(path, clk.get)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	tk, ok := ts2.Lookup(plaintext)
	if !ok || tk.Role != RoleStandard {
		t.Fatalf("reopen Lookup: got %+v ok=%v", tk, ok)
	}
	// In-memory mode: no file, same behaviour.
	mem, err := NewTokenStore("", clk.get)
	if err != nil {
		t.Fatalf("NewTokenStore in-memory: %v", err)
	}
	if _, p2, err := mem.Create("b", RoleExpert); err != nil || !tokenValueRe.MatchString(p2) {
		t.Fatalf("in-memory Create: %v", err)
	}
}

// TestTokenValidation: a duplicate name or an unknown role is 400
// invalid_value with the field named.
func TestTokenValidation(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, httpPost, pathConfigTokens, RoleInstaller,
		map[string]string{jsonFieldName: "tok-installer", jsonFieldRole: string(RoleExpert)})
	checkProblem(t, rr, http.StatusBadRequest, "invalid_value")
	rr = h.do(t, httpPost, pathConfigTokens, RoleInstaller,
		map[string]string{jsonFieldName: "fresh", jsonFieldRole: "superuser"})
	p := checkProblem(t, rr, http.StatusBadRequest, "invalid_value")
	if len(p.Errors) == 0 || p.Errors[0].Field != jsonFieldRole {
		t.Fatalf("bad role: got errors %+v, want field role", p.Errors)
	}
	rr = h.do(t, httpPost, pathConfigTokens, RoleInstaller,
		map[string]string{jsonFieldName: "fresh"})
	p = checkProblem(t, rr, http.StatusBadRequest, "invalid_value")
	if len(p.Errors) == 0 || p.Errors[0].Field != jsonFieldRole {
		t.Fatalf("missing role: got errors %+v, want field role", p.Errors)
	}
}

// TestTokenRoles: token management is installer-only (the matrix covers
// the 403s; this pins the 401/403 split on the token surface).
func TestTokenRoles(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, httpGet, pathConfigTokens, RoleExpert, nil)
	p := checkProblem(t, rr, http.StatusForbidden, "insufficient_role")
	if p.RequiredRole != string(RoleInstaller) || p.TokenRole != string(RoleExpert) {
		t.Fatalf("roles: got %q/%q, want installer/expert", p.RequiredRole, p.TokenRole)
	}
}

// tokenCreatedBody mirrors the spec's TokenCreated schema.
type tokenCreatedBody struct {
	Token   string    `json:"token"`
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Role    string    `json:"role"`
	Created time.Time `json:"created"`
}

// tokenInfoBody mirrors the spec's TokenInfo schema.
type tokenInfoBody struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Role     string     `json:"role"`
	Created  time.Time  `json:"created"`
	LastUsed *time.Time `json:"last_used"`
}

// sha256Hex is the store's hash form (plain lowercase hex).
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestTokenUsageFlushed (audit S11): last_used survives a restart when
// the service flushes usage on shutdown.
func TestTokenUsageFlushed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	ts, err := NewTokenStore(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	_, plain, err := ts.Create("phone", RoleReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ts.Lookup(plain); !ok {
		t.Fatal("lookup failed")
	}
	if err := ts.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	again, err := NewTokenStore(path, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if l := again.List(); len(l) != 1 || l[0].LastUsed == nil || !l[0].LastUsed.Equal(now) {
		t.Fatalf("after reload: %+v", l)
	}
}
