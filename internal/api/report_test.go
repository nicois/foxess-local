package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"foxesslocal/internal/quirks"
)

const (
	pathReport = "/api/v1/report"
	fieldDescr = "description"
)

// tarFiles reads a tar.gz response body into name → contents.
func tarFiles(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		c, _ := io.ReadAll(tr)
		out[h.Name] = c
	}
}

// TestProblemReport (F8): POST /report (installer) returns the
// de-identified bundle plus report.json — the tester's description and
// the app's facts (version, device, forward mode, settings, setup checks)
// — named after the model; it is audited; a too-long description and a
// bad window are refused.
func TestProblemReport(t *testing.T) {
	h := newHarness(t)
	withSettings(t, h)
	h.seedTele(testBase, 1000, -50, 900, 950, 0, 90, 2)
	h.seedFrame(testBase, dirC2S, "snap01", decisionForwarded, snap01Fixture("1.73", "1.03", "1.71", "0.00"), nil)
	// a cloud frame full held: the event that outlives its frame row
	h.st.RecordAudit(testBase, "cloud-drop", "1794820938317-1: mode:full hold:kind=other")

	if rr := h.do(t, httpPost, pathReport, RoleExpert, map[string]any{fieldDescr: "x"}); rr.Code != http.StatusForbidden {
		t.Fatalf("expert: %d", rr.Code)
	}
	checkProblem(t, h.do(t, httpPost, pathReport, RoleInstaller, map[string]any{fieldDescr: strings.Repeat("x", 4001)}), http.StatusBadRequest, "invalid_value")
	checkProblem(t, h.do(t, httpPost, pathReport, RoleInstaller, map[string]any{fieldDescr: "x", "hours": 0}), http.StatusBadRequest, "invalid_value")

	rr := h.do(t, httpPost, pathReport, RoleInstaller, map[string]any{fieldDescr: "The dashboard freezes at night.", "hours": 6})
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != contentTypeGzip ||
		!strings.Contains(rr.Header().Get("Content-Disposition"), "foxess-report-KH10") {
		t.Fatalf("report: %d %v %s", rr.Code, rr.Header(), rr.Body.String())
	}
	files := tarFiles(t, rr.Body.Bytes())
	var rep struct {
		Description string         `json:"description"`
		Facts       map[string]any `json:"facts"`
	}
	if err := json.Unmarshal(files["report.json"], &rep); err != nil {
		t.Fatalf("report.json: %v (files %v)", err, files)
	}
	if !strings.Contains(string(files["report.json"]), "mode:full hold:kind=other") {
		t.Error("the cloud-drop event is not in the report")
	}
	if rep.Description != "The dashboard freezes at night." {
		t.Errorf("description %q", rep.Description)
	}
	for _, k := range []string{"app_version", "model", "firmware", "forward_mode", "settings", "setup_checks", "online"} {
		if _, ok := rep.Facts[k]; !ok {
			t.Errorf("facts lack %s: %v", k, rep.Facts)
		}
	}
	if _, ok := files["frames.jsonl"]; !ok {
		t.Error("the bundle files are missing")
	}
	h.st.Flush()
	rows, err := h.st.QueryAudit(testBase.Add(-48*3600e9), "report-export")
	if err != nil || len(rows) != 1 {
		t.Fatalf("audit: %v %v", rows, err)
	}
}

// TestConfigIssueURL: /config names where to file a report — the
// release repository's new-issue page — when one is configured.
func TestConfigIssueURL(t *testing.T) {
	h := newHarness(t)
	h.srv.info.IssueURL = "https://github.com/owner/foxess-local/issues/new"
	var cfg map[string]any
	decode(t, h.do(t, httpGet, "/api/v1/config", RoleReadOnly, nil), &cfg)
	if cfg["issue_url"] != "https://github.com/owner/foxess-local/issues/new" {
		t.Fatalf("issue_url %v", cfg["issue_url"])
	}
}

// TestReportFactsQuirks (2026-11-19): a problem report says which
// inverter quirks were on, so a field report of a quirk can be read
// against what the reporter's dashboard showed.
func TestReportFactsQuirks(t *testing.T) {
	h := newHarness(t)
	set := withSettings(t, h)
	if err := set.Set(quirks.Key(quirks.EPSDropouts), "true", "test"); err != nil {
		t.Fatal(err)
	}
	facts, err := h.srv.reportFacts()
	if err != nil {
		t.Fatal(err)
	}
	q, _ := facts["quirks"].(map[string]bool)
	if !q[quirks.EPSDropouts] {
		t.Fatalf("facts quirks: %#v", facts["quirks"])
	}
}
