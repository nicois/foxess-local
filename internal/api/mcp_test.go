package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"foxesslocal/internal/mcp"
)

// mcpCall runs one tools/call through the MCP layer over the real API.
func mcpCall(t *testing.T, s *mcp.Server, token, tool, args string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":`+args+`}}`))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var resp struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Result == nil {
		t.Fatalf("%s: %s", tool, rec.Body.String())
	}
	return resp.Result
}

// TestMCPOverAPI (the MCP service): tools reach the real API with the
// caller's token, so its roles hold — read-only reads but cannot
// command, standard commands; without a token, public read-only access
// decides.
func TestMCPOverAPI(t *testing.T) {
	h := newHarness(t)
	spec, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := mcp.New(h.h, spec, "test")
	if err != nil {
		t.Fatal(err)
	}
	if r := mcpCall(t, s, h.tokens[RoleReadOnly], "get_state", `{}`); r["isError"] != false {
		t.Fatalf("read-only get_state: %v", r)
	}
	if r := mcpCall(t, s, h.tokens[RoleReadOnly], "set_work_mode", `{"work_mode":"feed-in"}`); r["isError"] != true {
		t.Fatalf("read-only set_work_mode was allowed: %v", r)
	}
	if r := mcpCall(t, s, h.tokens[RoleStandard], "set_work_mode", `{"work_mode":"feed-in"}`); r["isError"] != false {
		t.Fatalf("standard set_work_mode: %v", r)
	}
	if r := mcpCall(t, s, "", "get_state", `{}`); r["isError"] != false {
		t.Fatalf("public get_state: %v", r)
	}
	h.srv.info.PublicReadOnly = false
	if r := mcpCall(t, s, "", "get_state", `{}`); r["isError"] != true {
		t.Fatalf("tokenless get_state with public access off: %v", r)
	}
}
