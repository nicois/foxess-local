package api

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// specOp is the part of an OpenAPI operation the parity test checks.
type specOp struct {
	Summary  string           `yaml:"summary"`
	Role     string           `yaml:"x-required-role"` //nolint:tagliatelle // the spec's own extension key
	Security []map[string]any `yaml:"security"`
}

// loadSpec reads api/openapi.yaml: every "METHOD /path" operation.
func loadSpec(t *testing.T) map[string]specOp {
	t.Helper()
	b, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]specOp{}
	for path, ops := range doc.Paths {
		for method, node := range ops {
			switch method {
			case "get", "put", "post", "delete", "patch":
			default:
				continue
			}
			var op specOp
			if err := node.Decode(&op); err != nil {
				t.Fatalf("%s %s: %v", method, path, err)
			}
			out[strings.ToUpper(method)+" "+path] = op
		}
	}
	return out
}

// wildcard normalises a ServeMux wildcard ({id}, {rest...}) to {x}.
var wildcard = regexp.MustCompile(`\{[^}]+\}`)

// TestSpecCoversRoutes (F1): api/openapi.yaml is the API's whole
// contract. Every /api/v1 route the server registers is a spec
// operation and every spec operation is a route; each operation has a
// summary and names its access (x-required-role, or security: [] for
// the open ones).
func TestSpecCoversRoutes(t *testing.T) {
	h := newHarness(t)
	spec := loadSpec(t)
	specOps := map[string]string{} // normalised → as written
	for k := range spec {
		specOps[normRoute(k)] = k
	}
	routes := apiRoutes(h.srv.routes)
	for _, r := range routes {
		if _, ok := specOps[r]; !ok {
			t.Errorf("route %s is not in api/openapi.yaml", r)
		}
	}
	for n, k := range specOps {
		if !slices.Contains(routes, n) {
			t.Errorf("spec operation %s has no route", k)
		}
		checkOp(t, k, spec[k])
	}
}

// normRoute normalises a pattern's wildcards ({id}, {rest...}) to {x}.
func normRoute(s string) string { return wildcard.ReplaceAllString(s, "{x}") }

// apiRoutes are the registered /api/v1 routes, relative to /api/v1 (the
// root aliases /healthz, /readyz and /metrics mirror them).
func apiRoutes(registered []string) []string {
	var out []string
	for _, r := range registered {
		method, path, ok := strings.Cut(r, " ")
		if ok && strings.HasPrefix(path, "/api/v1/") {
			out = append(out, normRoute(method+" "+strings.TrimPrefix(path, "/api/v1")))
		}
	}
	return out
}

// checkOp: a summary, and the operation's access stated.
func checkOp(t *testing.T, name string, op specOp) {
	t.Helper()
	if op.Summary == "" {
		t.Errorf("%s: no summary", name)
	}
	if op.Role == "" && op.Security == nil {
		t.Errorf("%s: neither x-required-role nor security: []", name)
	}
}

// TestSpecServed (F1): the running app serves its own contract at
// /api/v1/openapi.yaml — the embedded api/openapi.yaml, byte for byte —
// without a token even when public access is off (it holds no data).
func TestSpecServed(t *testing.T) {
	h := newHarness(t)
	h.srv.info.PublicReadOnly = false
	want, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	rec := h.do(t, httpGet, "/api/v1/openapi.yaml", "", nil)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/yaml" || rec.Body.String() != string(want) {
		t.Fatalf("status %d, type %q, %d bytes (want %d)", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len(), len(want))
	}
}
