package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"foxesslocal/internal/notify"
)

type fakeTester struct{ res []notify.Result }

func (f fakeTester) Test(context.Context) []notify.Result { return f.res }

// TestNotifyTestEndpoint (F2): an installer sends a test notification to
// every configured target and sees each outcome; with none configured
// it is a 409 that says where to set one; below installer it is refused.
func TestNotifyTestEndpoint(t *testing.T) {
	h := newHarness(t)
	h.srv.notifier = fakeTester{res: []notify.Result{{Target: "webhook"}, {Target: "ntfy", Err: "HTTP 403"}}}
	rec := h.do(t, httpPost, "/api/v1/notify/test", RoleInstaller, nil)
	var b struct {
		Results []notify.Result `json:"results"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &b) != nil || len(b.Results) != 2 || b.Results[1].Err != "HTTP 403" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	h.srv.notifier = fakeTester{res: []notify.Result{}}
	checkProblem(t, h.do(t, httpPost, "/api/v1/notify/test", RoleInstaller, nil), http.StatusConflict, "notify_not_configured")
	if rec := h.do(t, httpPost, "/api/v1/notify/test", RoleExpert, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("expert: %d", rec.Code)
	}
}
