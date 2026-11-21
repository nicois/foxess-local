package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// allowed are the API operations that are tools (by operationId). Reads,
// plus the everyday commands; each still needs the role the API requires
// (work mode, schedule and the schedule override standard, meter
// compensation expert, limits installer). Not here, on purpose: tokens, updates, maintenance windows,
// cloud forwarding, settings, raw register reads, the write override.
var allowed = []string{
	"getState", "getHistory", "getPowerAverage", "listAudit", "getForwardMode",
	"getMaintenance", "getCompatibility", "getConfig", "ready", "getUpdate",
	"setWorkMode", "setSchedule", "setLimits", "setMeterCompensation",
	"getScheduleOverride", "setScheduleOverride", "clearScheduleOverride",
	"listNotices", "sendNotice",
}

// apiPrefix is where the spec's paths live (its servers entry).
const apiPrefix = "/api/v1"

// tool is one MCP tool: what tools/list shows, and the API operation.
type tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations"`

	method  string
	path    string
	query   map[string]bool // the arguments that go in the URL
	hasBody bool
}

// specDoc is the part of the OpenAPI document the tools use.
type specDoc struct {
	Paths      map[string]map[string]any `yaml:"paths"`
	Components struct {
		Schemas    map[string]any `yaml:"schemas"`
		Parameters map[string]any `yaml:"parameters"`
	} `yaml:"components"`
}

// buildTools makes the allowlisted operations into tools, in the
// allowlist's order.
func buildTools(spec []byte) ([]tool, error) {
	var doc specDoc
	if err := yaml.Unmarshal(spec, &doc); err != nil {
		return nil, fmt.Errorf("mcp: spec: %w", err)
	}
	ops, err := doc.allowedOps()
	if err != nil {
		return nil, err
	}
	out := make([]tool, 0, len(allowed))
	for _, id := range allowed {
		t, ok := ops[id]
		if !ok {
			return nil, fmt.Errorf("mcp: operation %s is not in the spec", id)
		}
		out = append(out, t)
	}
	return out, nil
}

// allowedOps builds a tool for every allowlisted operation in the spec.
func (d *specDoc) allowedOps() (map[string]tool, error) {
	ops := map[string]tool{}
	for path, item := range d.Paths {
		for method, raw := range item {
			op, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			id, _ := op["operationId"].(string)
			if !slices.Contains(allowed, id) {
				continue
			}
			t, err := d.toolOf(strings.ToUpper(method), path, id, op)
			if err != nil {
				return nil, err
			}
			ops[id] = t
		}
	}
	return ops, nil
}

// toolOf builds one tool from its operation.
func (d *specDoc) toolOf(method, path, id string, op map[string]any) (tool, error) {
	summary, _ := op["summary"].(string)
	desc, _ := op["description"].(string)
	t := tool{
		Name: snake(id), Title: summary, method: method, path: path, query: map[string]bool{},
		Description: strings.TrimSpace(strings.Join(nonEmpty(summary, strings.TrimSpace(desc)), "\n\n")),
	}
	props, required := d.queryParams(op, t.query)
	if body := d.bodySchema(op); body != nil {
		t.hasBody = true
		bp, _ := body["properties"].(map[string]any)
		for k, v := range bp {
			if _, dup := props[k]; dup {
				return tool{}, fmt.Errorf("mcp: %s: %s is both a parameter and a body field", id, k)
			}
			props[k] = v
		}
		if br, ok := body["required"].([]any); ok {
			required = append(required, br...)
		}
	}
	t.InputSchema = map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		t.InputSchema["required"] = required
	}
	read := method == http.MethodGet
	t.Annotations = map[string]any{
		"readOnlyHint": read, "openWorldHint": false,
		// a PUT sets a value: repeating it changes nothing more
		"idempotentHint": read || method == http.MethodPut, "destructiveHint": false,
	}
	return t, nil
}

// queryParams are the operation's query parameters as input-schema
// properties (marked in query), and the required ones. Headers
// (Last-Event-ID) are not tool arguments.
func (d *specDoc) queryParams(op map[string]any, query map[string]bool) (map[string]any, []any) {
	props := map[string]any{}
	var required []any
	params, _ := op["parameters"].([]any)
	for _, raw := range params {
		p, _ := d.resolve(raw).(map[string]any)
		if in, _ := p["in"].(string); in != "query" {
			continue
		}
		name, _ := p["name"].(string)
		sch, _ := d.resolve(p["schema"]).(map[string]any)
		if sch == nil {
			sch = map[string]any{}
		}
		if pd, ok := p["description"].(string); ok {
			sch["description"] = pd
		}
		props[name] = sch
		query[name] = true
		if req, _ := p["required"].(bool); req {
			required = append(required, name)
		}
	}
	return props, required
}

// bodySchema is the operation's JSON request body schema (an object),
// or nil.
func (d *specDoc) bodySchema(op map[string]any) map[string]any {
	rb, _ := op["requestBody"].(map[string]any)
	content, _ := rb["content"].(map[string]any)
	js, _ := content["application/json"].(map[string]any)
	sch, _ := d.resolve(js["schema"]).(map[string]any)
	return sch
}

// maxDepth bounds $ref resolution (the spec has no cycles; a guard).
const maxDepth = 32

// resolve inlines $refs (components/schemas, components/parameters),
// drops read-only properties (a GET's output, refused as input) and the
// OpenAPI-only keywords, leaving plain JSON Schema.
func (d *specDoc) resolve(v any) any { return d.resolveAt(v, 0) }

func (d *specDoc) resolveAt(v any, depth int) any {
	if depth > maxDepth {
		return map[string]any{}
	}
	switch x := v.(type) {
	case map[string]any:
		if ref, ok := x["$ref"].(string); ok {
			return d.resolveAt(d.lookup(ref), depth+1)
		}
		out := map[string]any{}
		for k, val := range x {
			if strings.HasPrefix(k, "x-") || k == "readOnly" || k == "example" {
				continue
			}
			out[k] = d.resolveAt(val, depth+1)
		}
		dropReadOnly(out, x)
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = d.resolveAt(e, depth+1)
		}
		return out
	}
	return v
}

// dropReadOnly removes the properties marked readOnly in the source
// schema (and from required).
func dropReadOnly(out, src map[string]any) {
	sp, _ := src["properties"].(map[string]any)
	op, _ := out["properties"].(map[string]any)
	for name, raw := range sp {
		if p, ok := raw.(map[string]any); ok && p["readOnly"] == true {
			delete(op, name)
		}
	}
	if req, ok := out["required"].([]any); ok {
		kept := req[:0]
		for _, r := range req {
			if name, _ := r.(string); op[name] != nil {
				kept = append(kept, r)
			}
		}
		out["required"] = kept
	}
}

// lookup finds a local $ref.
func (d *specDoc) lookup(ref string) any {
	switch {
	case strings.HasPrefix(ref, "#/components/schemas/"):
		return d.Components.Schemas[strings.TrimPrefix(ref, "#/components/schemas/")]
	case strings.HasPrefix(ref, "#/components/parameters/"):
		return d.Components.Parameters[strings.TrimPrefix(ref, "#/components/parameters/")]
	}
	return map[string]any{}
}

// argError is a tool call's bad arguments (a JSON-RPC invalid-params
// error, not a tool result).
type argError struct{ msg string }

func (e argError) Error() string { return e.msg }

// invoke makes the API request for a call: query arguments in the URL,
// the rest as the JSON body, the caller's bearer token on it.
func (t *tool) invoke(r *http.Request, api http.Handler, args map[string]any) (int, string, error) {
	q := url.Values{}
	body := map[string]any{}
	for k, v := range args {
		switch {
		case t.query[k]:
			q.Set(k, argText(v))
		case t.hasBody:
			body[k] = v
		default:
			return 0, "", argError{fmt.Sprintf("%s takes no argument %q", t.Name, k)}
		}
	}
	target := apiPrefix + t.path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	var rd *bytes.Reader
	if t.hasBody {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, "", argError{err.Error()}
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(r.Context(), t.method, target, rd)
	if err != nil {
		return 0, "", err
	}
	if t.hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	if a := r.Header.Get("Authorization"); a != "" {
		req.Header.Set("Authorization", a)
	}
	req.RemoteAddr, req.Host = r.RemoteAddr, r.Host
	rec := &recorder{header: http.Header{}, status: http.StatusOK}
	api.ServeHTTP(rec, req)
	return rec.status, strings.TrimSpace(rec.body.String()), nil
}

// argText renders a query argument (numbers without a trailing ".0").
func argText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// recorder captures the API's answer.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
	wrote  bool
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.body.Write(b)
}

// snake turns an operationId into a tool name: getPowerAverage →
// get_power_average.
func snake(id string) string {
	var b strings.Builder
	for i, c := range id {
		if unicode.IsUpper(c) {
			if i > 0 {
				b.WriteByte('_')
			}
			c = unicode.ToLower(c)
		}
		b.WriteRune(c)
	}
	return b.String()
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
