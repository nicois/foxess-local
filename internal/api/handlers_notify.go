package api

import (
	"context"
	"net/http"

	"foxesslocal/internal/notify"
)

// NotifyTester sends a test notification to each configured target
// (*notify.Notifier).
type NotifyTester interface {
	Test(ctx context.Context) []notify.Result
}

// WithNotifier wires the outbound notifier (F2) for the test endpoint.
func WithNotifier(n NotifyTester) ServerOption {
	return func(s *Server) { s.notifier = n }
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
			Detail: "set a webhook or ntfy URL first (More → Settings → Notifications, or [notify] in the config file)",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": res})
}
