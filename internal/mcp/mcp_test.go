package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
)

// apiCall is one request the fake API received.
type apiCall struct {
	method, path, query, auth, body string
}

// fakeAPI answers every request with status and body, recording it.
type fakeAPI struct {
	calls  []apiCall
	status int
	body   string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.calls = append(f.calls, apiCall{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), string(b)})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(f.status)
	_, _ = io.WriteString(w, f.body)
}

func newServer(t *testing.T, api http.Handler) *Server {
	t.Helper()
	spec, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(api, spec, "v-test")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// rpc posts one JSON-RPC message and decodes the response.
func rpc(t *testing.T, s *Server, auth, msg string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(msg))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("response %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, out
}

func result(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	r, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", resp)
	}
	return r
}

// TestInitialize: the handshake names the server and offers tools; a
// supported protocol version is echoed, an unknown one answered with
// ours.
func TestInitialize(t *testing.T) {
	s := newServer(t, &fakeAPI{status: 200, body: "{}"})
	_, resp := rpc(t, s, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	r := result(t, resp)
	if r["protocolVersion"] != "2025-03-26" || resp["id"] != float64(1) {
		t.Fatalf("initialize: %v", resp)
	}
	info, _ := r["serverInfo"].(map[string]any)
	caps, _ := r["capabilities"].(map[string]any)
	if info["name"] != "foxess-local" || info["version"] != "v-test" || caps["tools"] == nil || r["instructions"] == "" {
		t.Fatalf("initialize result: %v", r)
	}
	_, resp = rpc(t, s, "", `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if result(t, resp)["protocolVersion"] != latestVersion {
		t.Fatalf("unknown version: %v", resp)
	}
}

// TestToolsList: the allowlisted operations, named from their
// operationId, with descriptions and input schemas from the spec — the
// read-only fields a GET returns are not inputs (the API refuses
// unknown fields), and the dangerous operations are not tools at all.
func TestToolsList(t *testing.T) {
	s := newServer(t, &fakeAPI{status: 200, body: "{}"})
	_, resp := rpc(t, s, "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools, _ := result(t, resp)["tools"].([]any)
	byName := map[string]map[string]any{}
	for _, x := range tools {
		tm, _ := x.(map[string]any)
		name, _ := tm["name"].(string)
		byName[name] = tm
	}
	for _, want := range []string{"get_state", "get_history", "set_work_mode", "set_schedule"} {
		if byName[want] == nil {
			t.Errorf("no %s tool", want)
		}
	}
	for _, never := range []string{"create_token", "apply_update", "set_forward_mode", "open_maintenance", "read_registers"} {
		if byName[never] != nil {
			t.Errorf("%s must not be a tool", never)
		}
	}
	if d, _ := byName["get_state"]["description"].(string); d == "" {
		t.Error("get_state has no description")
	}
	hist, _ := byName["get_history"]["inputSchema"].(map[string]any)
	props, _ := hist["properties"].(map[string]any)
	req, _ := hist["required"].([]any)
	if props["range"] == nil || !slices.Contains(req, any("range")) {
		t.Errorf("get_history schema: %v", hist)
	}
	sched, err := json.Marshal(byName["set_schedule"]["inputSchema"])
	if err != nil {
		t.Fatal(err)
	}
	for _, ro := range []string{`"after_cutoff"`, `"import_limit_w"`, `"$ref"`} {
		if bytes.Contains(sched, []byte(ro)) {
			t.Errorf("set_schedule schema carries %s: %s", ro, sched)
		}
	}
	ann, _ := byName["get_state"]["annotations"].(map[string]any)
	if ann["readOnlyHint"] != true {
		t.Errorf("get_state annotations: %v", ann)
	}
}

// TestToolsCall: a tool call is the API request — query parameters in
// the URL, the rest as the JSON body, the caller's bearer token passed
// on (the API enforces roles, validation and the write budget) — and
// the API's answer is the result; an error status is a tool error.
func TestToolsCall(t *testing.T) {
	api := &fakeAPI{status: 200, body: `{"sol_wh":1}`}
	s := newServer(t, api)
	_, resp := rpc(t, s, "Bearer fl_x", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_history","arguments":{"range":"24h"}}}`)
	r := result(t, resp)
	if c := api.calls[0]; c.method != http.MethodGet || c.path != "/api/v1/history" || c.query != "range=24h" || c.auth != "Bearer fl_x" {
		t.Fatalf("call %+v", c)
	}
	checkTextResult(t, r, `{"sol_wh":1}`)

	rpc(t, s, "", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"set_work_mode","arguments":{"work_mode":"feed-in"}}}`)
	if c := api.calls[1]; c.method != http.MethodPut || c.path != "/api/v1/work-mode" || c.body != `{"work_mode":"feed-in"}` || c.auth != "" {
		t.Fatalf("call %+v", c)
	}

	api.status, api.body = http.StatusForbidden, `{"code":"insufficient_role"}`
	_, resp = rpc(t, s, "Bearer fl_ro", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"set_work_mode","arguments":{"work_mode":"self-use"}}}`)
	if r := result(t, resp); r["isError"] != true {
		t.Fatalf("403 not a tool error: %v", r)
	}

	_, resp = rpc(t, s, "", `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"create_token","arguments":{}}}`)
	if e, _ := resp["error"].(map[string]any); e == nil || e["code"] != float64(codeInvalidParams) {
		t.Fatalf("unknown tool: %v", resp)
	}
}

// checkTextResult: a successful result carrying the API's answer as
// text and as structured content.
func checkTextResult(t *testing.T, r map[string]any, text string) {
	t.Helper()
	content, _ := r["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content %v", r)
	}
	first, _ := content[0].(map[string]any)
	if r["isError"] == true || first["type"] != "text" || first["text"] != text || r["structuredContent"] == nil {
		t.Fatalf("result %v", r)
	}
}

// TestTransport: notifications are 202 without a body; only POST is
// served (no server-initiated stream); a malformed message is a JSON-RPC
// parse error; an unknown method is method-not-found; ping answers.
func TestTransport(t *testing.T) {
	s := newServer(t, &fakeAPI{status: 200, body: "{}"})
	if code, _ := rpc(t, s, "", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != http.StatusAccepted {
		t.Fatalf("notification: %d", code)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", rec.Code)
	}
	_, resp := rpc(t, s, "", `{not json`)
	if e, _ := resp["error"].(map[string]any); e == nil || e["code"] != float64(codeParse) {
		t.Fatalf("parse error: %v", resp)
	}
	_, resp = rpc(t, s, "", `{"jsonrpc":"2.0","id":7,"method":"resources/list"}`)
	if e, _ := resp["error"].(map[string]any); e == nil || e["code"] != float64(codeMethodNotFound) {
		t.Fatalf("unknown method: %v", resp)
	}
	_, resp = rpc(t, s, "", `{"jsonrpc":"2.0","id":8,"method":"ping"}`)
	if resp["result"] == nil {
		t.Fatalf("ping: %v", resp)
	}
}

// TestOrigin: a browser request from another site is refused (the MCP
// transport's DNS-rebinding / cross-site guard); same-origin and
// non-browser clients (no Origin) are served.
func TestOrigin(t *testing.T) {
	s := newServer(t, &fakeAPI{status: 200, body: "{}"})
	for origin, want := range map[string]int{
		"https://evil.example":    http.StatusForbidden,
		"http://foxess.lan:18080": http.StatusOK,
		"":                        http.StatusOK,
	} {
		req := httptest.NewRequest(http.MethodPost, "http://foxess.lan:18080/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Origin %q: %d, want %d", origin, rec.Code, want)
		}
	}
}
