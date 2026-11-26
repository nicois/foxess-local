package api

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestBundleExport: GET /bundle (installer) streams a gzip archive named
// after the model, the SN never appears in it, and the export is
// audited; other roles are refused and a bad window is a 400.
func TestBundleExport(t *testing.T) {
	h := newHarness(t)
	h.seedTele(testBase, 1000, -50, 900, 950, 0, 90, 2)
	h.seedFrame(testBase, dirC2S, "snap01", decisionForwarded, snap01Fixture("1.73", "1.03", "1.71", "0.00"), nil)

	if rr := h.do(t, httpGet, "/api/v1/bundle", RoleExpert, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("expert: %d", rr.Code)
	}
	if rr := h.do(t, httpGet, "/api/v1/bundle?hours=0", RoleInstaller, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("hours=0: %d", rr.Code)
	}
	rr := h.do(t, httpGet, "/api/v1/bundle?hours=48", RoleInstaller, nil)
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != contentTypeGzip ||
		!strings.Contains(rr.Header().Get("Content-Disposition"), "KH10") {
		t.Fatalf("export: %d %v", rr.Code, rr.Header())
	}
	zr, err := gzip.NewReader(bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	if _, err := raw.ReadFrom(zr); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw.Bytes(), []byte("TESTSN000000001")) {
		t.Fatal("SN in the bundle")
	}
	// the manifest states the frame retention (the server's raw_hours)
	if !bytes.Contains(raw.Bytes(), []byte(`"frames_kept_hours": 72`)) {
		t.Error("manifest without frames_kept_hours")
	}
	h.st.Flush()
	if ev, err := h.st.QueryAudit(testBase.Add(-24*time.Hour), "bundle-export"); err != nil || len(ev) != 1 {
		t.Fatalf("audit: %v %v", ev, err)
	}
}
