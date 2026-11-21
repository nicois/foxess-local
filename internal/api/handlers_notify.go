package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"foxesslocal/internal/notify"
	"foxesslocal/internal/store"
)

// NotifyTester sends a test notification to each configured target
// (*notify.Notifier).
type NotifyTester interface {
	Test(ctx context.Context) []notify.Result
}

// WithNotifier wires the outbound notifier (F2) for the test endpoint
// and, when it queues notices too, for automations' notices (D-030).
func WithNotifier(n NotifyTester) ServerOption {
	return func(s *Server) {
		s.notifier = n
		if ns, ok := n.(NoticeSender); ok {
			s.notices = ns
		}
	}
}

// SystemNotice is the system event for a notice kept in the list (the
// web UI refreshes it; it is not notified again). Its detail is
// "origin: title" for an automation's notice, "" for foxess-local's.
const SystemNotice = "notice"

// NoticeJournal keeps every notice in st for GET /notices and tells the
// web UI (notify.WithJournal).
func NoticeJournal(st *store.Store, bus *Bus) notify.Journal {
	return func(ts time.Time, nt notify.Notice) {
		if err := st.NoticeAdd(store.NoticeRow{
			TS: ts, Origin: nt.Origin, Event: nt.Event, Severity: string(nt.Severity), Title: nt.Title, Message: nt.Message,
		}); err != nil {
			slog.Error("notify: notice not kept", "event", nt.Event, "err", err)
		}
		// foxess-local's own notice has its own system event (already
		// shown); an automation's is shown from this one
		detail := ""
		if nt.Origin != "" {
			detail = nt.Origin + ": " + nt.Title
		}
		bus.PublishSystem(SystemEvent{Type: SystemNotice, Detail: detail})
	}
}

// handleNotifyTest is POST /notify/test (installer, F2): one test
// notification to every configured target now, with each outcome.
func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request, _ Token) {
	var res []notify.Result
	if s.notifier != nil {
		res = s.notifier.Test(r.Context())
	}
	if len(res) == 0 {
		writeProblem(w, http.StatusConflict, problem{
			Title:  "No notification target",
			Code:   "notify_not_configured",
			Detail: "set a webhook or ntfy URL first (System → Notifications, or [notify] in the config file)",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": res})
}
