// Package mcp serves the Model Context Protocol over its streamable HTTP
// transport: a thin layer above the REST API, so an AI assistant can read
// the inverter and, with a token that allows it, run the everyday
// commands.
//
// Each tool is one API operation, built from api/openapi.yaml (its
// description and input schema), and each call is that API request made
// in-process with the caller's bearer token. The API's roles, validation,
// write budget and audit therefore apply unchanged; without a token a
// call gets what public read-only access allows. Only allowlisted
// operations are tools (tools.go): tokens, updates, maintenance windows,
// cloud forwarding and raw register reads stay out of an assistant's
// reach.
//
// The transport subset: POST with one JSON-RPC message, answered with
// application/json; notifications are 202; no server-initiated stream
// (GET is 405) and no session state.
package mcp

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
)

// Protocol versions this server speaks, newest first.
var versions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// latestVersion is the one offered to a client asking for another.
var latestVersion = versions[0]

// JSON-RPC error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// jsonrpc is the JSON-RPC version every message carries.
const jsonrpc = "2.0"

// maxMessage bounds one request body.
const maxMessage = 1 << 20

// instructions orient the assistant (sent at initialize).
const instructions = `foxess-local: local monitoring and control of a FoxESS hybrid inverter.
Power is in W: grid_w > 0 imports from the grid, < 0 exports; battery_w > 0 charges, < 0 discharges.
Start with get_state (live values, settings, schedule). Commands are confirmed by the inverter's
read-back and need a token with the right role; each setting allows at most 10 real writes an hour,
so change settings deliberately, not in loops.`

// Server is the MCP endpoint.
type Server struct {
	api     http.Handler
	version string
	tools   []tool
	byName  map[string]*tool
}

// New builds the server over the API handler and its OpenAPI document.
func New(api http.Handler, spec []byte, version string) (*Server, error) {
	tools, err := buildTools(spec)
	if err != nil {
		return nil, err
	}
	s := &Server{api: api, version: version, tools: tools, byName: map[string]*tool{}}
	for i := range s.tools {
		s.byName[s.tools[i].Name] = &s.tools[i]
	}
	return s, nil
}

// message is one JSON-RPC request or notification.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// ServeHTTP is the streamable HTTP transport.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "MCP: POST JSON-RPC messages here", http.StatusMethodNotAllowed)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "MCP: cross-origin requests are refused", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMessage))
	if err != nil {
		reply(w, response{JSONRPC: jsonrpc, ID: json.RawMessage("null"), Error: &rpcError{codeParse, err.Error()}})
		return
	}
	var m message
	if err := json.Unmarshal(body, &m); err != nil {
		reply(w, response{JSONRPC: jsonrpc, ID: json.RawMessage("null"), Error: &rpcError{codeParse, "not a JSON-RPC message: " + err.Error()}})
		return
	}
	if len(m.ID) == 0 { // a notification: nothing to answer
		w.WriteHeader(http.StatusAccepted)
		return
	}
	out := response{JSONRPC: jsonrpc, ID: m.ID}
	out.Result, out.Error = s.dispatch(r, m)
	reply(w, out)
}

// dispatch runs one request.
func (s *Server) dispatch(r *http.Request, m message) (any, *rpcError) {
	if m.JSONRPC != jsonrpc || m.Method == "" {
		return nil, &rpcError{codeInvalidRequest, "a JSON-RPC 2.0 request needs jsonrpc and method"}
	}
	switch m.Method {
	case "initialize":
		return s.initialize(m.Params), nil
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		return map[string]any{"tools": s.tools}, nil
	case "tools/call":
		return s.call(r, m.Params)
	}
	return nil, &rpcError{codeMethodNotFound, "method not found: " + m.Method}
}

// initialize answers the handshake: the client's protocol version when
// we speak it, else our latest.
func (s *Server) initialize(params json.RawMessage) any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p) // absent / malformed: offer ours
	v := latestVersion
	if slices.Contains(versions, p.ProtocolVersion) {
		v = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": v,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": "foxess-local", "title": "FoxESS local control", "version": s.version},
		"instructions":    instructions,
	}
}

// call runs one tool: the API request, its answer as the result.
func (s *Server) call(r *http.Request, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{codeInvalidParams, "tools/call: " + err.Error()}
	}
	t, ok := s.byName[p.Name]
	if !ok {
		return nil, &rpcError{codeInvalidParams, "unknown tool: " + p.Name}
	}
	status, body, err := t.invoke(r, s.api, p.Arguments)
	if err != nil {
		if _, ok := errors.AsType[argError](err); ok {
			return nil, &rpcError{codeInvalidParams, err.Error()}
		}
		return toolResult(true, err.Error()), nil
	}
	return toolResult(status >= http.StatusBadRequest, body), nil
}

// toolResult is a tools/call result: the API's answer as text, and as
// structured content when it is a JSON object.
func toolResult(isError bool, text string) map[string]any {
	out := map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
	var obj map[string]any
	if json.Unmarshal([]byte(text), &obj) == nil {
		out["structuredContent"] = obj
	}
	return out
}

// sameOrigin: a browser's Origin (when sent) names this host — the
// transport's guard against a web page driving the endpoint. Clients
// that are not browsers send none.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

func reply(w http.ResponseWriter, out response) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out) //nolint:errchkjson // the status is sent; a failed write is a client gone
}
