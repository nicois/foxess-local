package api

import (
	"net/http"
	"testing"
	"time"

	"foxesslocal/internal/wincap"
)

const pathCaptures = "/api/v1/captures"

// TestMaintenanceCapture (owner, 2026-11-18): a window opened with
// "capture" records its traffic; the captures are listed, downloaded
// (.tar.gz) and deleted by an installer; an unknown id is 404.
func TestMaintenanceCapture(t *testing.T) {
	h := newHarness(t)
	clk := time.Date(2026, 11, 18, 2, 0, 0, 0, time.UTC)
	c := wincap.New(t.TempDir(), 1<<20, func() time.Time { return clk }, "test")
	h.srv.captures = c

	rec := h.do(t, httpPost, pathMaintenance, RoleInstaller, map[string]any{"duration": "30m", "capture": true})
	if rec.Code != http.StatusOK || !c.Active() {
		t.Fatalf("open with capture: %d %s, active %v", rec.Code, rec.Body.String(), c.Active())
	}
	var view struct {
		Capturing bool `json:"capturing"`
	}
	decode(t, h.do(t, httpGet, pathMaintenance, RoleReadOnly, nil), &view)
	if !view.Capturing {
		t.Fatal("the window view does not say it is capturing")
	}
	c.Chunk("c1", true, []byte{0xa5})
	c.Stop()

	var list struct {
		Items []wincap.Info `json:"items"`
	}
	decode(t, h.do(t, httpGet, pathCaptures, RoleInstaller, nil), &list)
	if len(list.Items) != 1 || list.Items[0].Bytes != 1 {
		t.Fatalf("list %+v", list)
	}
	id := list.Items[0].ID
	rec = h.do(t, httpGet, pathCaptures+"/"+id, RoleInstaller, nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != contentTypeGzip || rec.Body.Len() == 0 {
		t.Fatalf("download: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := h.do(t, httpGet, pathCaptures, RoleExpert, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("expert list: %d", rec.Code)
	}
	if rec := h.do(t, httpGet, pathCaptures+"/..%2Fetc", RoleInstaller, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("bad id: %d", rec.Code)
	}
	if rec := h.do(t, httpDelete, pathCaptures+"/"+id, RoleInstaller, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	decode(t, h.do(t, httpGet, pathCaptures, RoleInstaller, nil), &list)
	if len(list.Items) != 0 {
		t.Fatalf("after delete: %+v", list)
	}
}
