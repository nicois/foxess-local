package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"foxesslocal/internal/notify"
)

// noticeHarness is a harness whose notifier is a real one (no targets)
// journalling into the store, as the service wires it.
func noticeHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	n := notify.New(func() notify.Targets { return notify.Targets{} }, h.clk.get, notify.WithJournal(NoticeJournal(h.st, h.bus)))
	WithNotifier(n)(h.srv)
	return h
}

func noticeBody(event, sev, title string) map[string]string {
	return map[string]string{"event": event, "severity": sev, "title": title, "message": "VIC1 at $1500/MWh"}
}

// TestNoticePost (D-030): an automation's notice is accepted, kept with
// its token's name as origin and its event marked as an automation's,
// and listed newest first with foxess-local's own.
func TestNoticePost(t *testing.T) {
	h := noticeHarness(t)
	h.srv.notices.Notify(notify.Notice{Event: SystemGridLost, Severity: notify.Critical, Title: "Grid down", Message: "on backup"})
	h.clk.advance(time.Second)
	rr := h.do(t, httpPost, "/api/v1/notices", RoleStandard, noticeBody("price-spike", "info", "Price spike"))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST: %d %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Notices []NoticeView `json:"notices"`
	}
	decodeJSON(t, h.do(t, httpGet, "/api/v1/notices", RoleReadOnly, nil), &got)
	if len(got.Notices) != 2 {
		t.Fatalf("notices %+v", got.Notices)
	}
	n := got.Notices[0]
	if n.Origin != tokStd || n.Event != "sidecar.price-spike" || n.Title != "Price spike" || n.Severity != "info" {
		t.Fatalf("newest %+v", n)
	}
	if o := got.Notices[1]; o.Origin != "foxess-local" || o.Event != SystemGridLost {
		t.Fatalf("own %+v", o)
	}
	decodeJSON(t, h.do(t, httpGet, "/api/v1/notices?before="+itoa(n.ID)+"&limit=5", RoleReadOnly, nil), &got)
	if len(got.Notices) != 1 || got.Notices[0].Event != SystemGridLost {
		t.Fatalf("page 2 %+v", got.Notices)
	}
}

// TestNoticeRefusals: bad fields, a critical notice below expert, a
// read-only token, and the hourly budget per token (429 with
// Retry-After; another token has its own).
func TestNoticeRefusals(t *testing.T) {
	h := noticeHarness(t)
	for _, b := range []map[string]string{
		noticeBody("Price spike", "info", "x"),
		noticeBody("price-spike", "loud", "x"),
		noticeBody("price-spike", "info", ""),
		noticeBody("price-spike", "info", "two\nlines"),
		noticeBody("price-spike", "info", strings.Repeat("x", noticeTitleMax+1)),
		noticeBody("price-spike", "critical", "x"),
	} {
		checkProblem(t, h.do(t, httpPost, "/api/v1/notices", RoleStandard, b), http.StatusBadRequest, "invalid_value")
	}
	if rr := h.do(t, httpPost, "/api/v1/notices", RoleExpert, noticeBody("price-spike", "critical", "x")); rr.Code != http.StatusAccepted {
		t.Fatalf("expert critical: %d", rr.Code)
	}
	checkProblem(t, h.do(t, httpPost, "/api/v1/notices", RoleReadOnly, noticeBody("a", "info", "x")), http.StatusForbidden, "insufficient_role")
	for i := range noticesPerHour {
		if rr := h.do(t, httpPost, "/api/v1/notices", RoleStandard, noticeBody("n"+itoa(int64(i)), "info", "x")); rr.Code != http.StatusAccepted {
			t.Fatalf("notice %d: %d", i, rr.Code)
		}
	}
	rr := h.do(t, httpPost, "/api/v1/notices", RoleStandard, noticeBody("over", "info", "x"))
	checkProblem(t, rr, http.StatusTooManyRequests, "notice_rate")
	if rr.Header().Get("Retry-After") != "3601" {
		t.Fatalf("Retry-After %q", rr.Header().Get("Retry-After"))
	}
	if rr := h.do(t, httpPost, "/api/v1/notices", RoleInstaller, noticeBody("other", "info", "x")); rr.Code != http.StatusAccepted {
		t.Fatalf("another token: %d", rr.Code)
	}
	h.clk.advance(time.Hour)
	if rr := h.do(t, httpPost, "/api/v1/notices", RoleStandard, noticeBody("later", "info", "x")); rr.Code != http.StatusAccepted {
		t.Fatalf("an hour later: %d", rr.Code)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
