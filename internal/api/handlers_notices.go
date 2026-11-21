package api

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"

	"foxesslocal/internal/notify"
	"foxesslocal/internal/store"
)

// Notices from automations (D-030): a sidecar sends a notice with its
// own token; foxess-local delivers it like its own (the outbox, the
// webhook and ntfy, the dedup window) with the token's name as its
// origin, and keeps it in the list the web UI shows.

// NoticeSender queues one notice (*notify.Notifier).
type NoticeSender interface {
	Notify(nt notify.Notice)
}

// Notice limits.
const (
	noticeTitleMax   = 120
	noticeMessageMax = 1000
	// noticesPerHour is each token's budget (a rolling hour).
	noticesPerHour = 20
	// noticeEventPrefix marks an automation's event, so it can never pass
	// for one of foxess-local's own (inverter-offline, grid-lost, …).
	noticeEventPrefix = "sidecar."
	noticesPageMax    = 200
	noticesPageDef    = 50
)

// noticeEventRe is an automation's event name.
var noticeEventRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// noticeRequest is the POST /notices body.
type noticeRequest struct {
	Event    string `json:"event"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Message  string `json:"message"`
}

// NoticeView is the spec's Notice (GET /notices).
type NoticeView struct {
	ID       int64     `json:"id"`
	TS       time.Time `json:"ts"`
	Origin   string    `json:"origin"`
	Event    string    `json:"event"`
	Severity string    `json:"severity"`
	Title    string    `json:"title"`
	Message  string    `json:"message"`
}

// plainText reports whether v is text without control characters (a
// title goes into ntfy's Title header; a newline there would break it).
func plainText(v string, newlines bool) bool {
	if !utf8.ValidString(v) {
		return false
	}
	for _, r := range v {
		if unicode.IsControl(r) && (!newlines || r != '\n') {
			return false
		}
	}
	return true
}

// validate checks the body for tk (critical needs an expert).
func (r noticeRequest) validate(tk Token) *problem {
	if !noticeEventRe.MatchString(r.Event) {
		return badFieldPtr("event", "must be 1–40 of a-z, 0-9 and -, starting with a letter or digit")
	}
	switch notify.Severity(r.Severity) {
	case notify.Info, notify.Warning:
	case notify.Critical:
		if roleLevel[tk.Role] < rankExpert {
			return badFieldPtr("severity", "critical needs an expert or installer token")
		}
	default:
		return badFieldPtr("severity", "must be info, warning or critical")
	}
	if n := utf8.RuneCountInString(r.Title); n == 0 || n > noticeTitleMax || !plainText(r.Title, false) {
		return badFieldPtr("title", fmt.Sprintf("must be 1–%d characters on one line", noticeTitleMax))
	}
	if utf8.RuneCountInString(r.Message) > noticeMessageMax || !plainText(r.Message, true) {
		return badFieldPtr("message", fmt.Sprintf("must be at most %d characters (no control characters but newlines)", noticeMessageMax))
	}
	return nil
}

// noticeAllowed takes one notice from tk's hourly budget; false with
// the wait until the next one when it is spent.
func (s *Server) noticeAllowed(tk Token, now time.Time) (bool, time.Duration) {
	s.noticeMu.Lock()
	defer s.noticeMu.Unlock()
	if s.noticeSent == nil {
		s.noticeSent = map[string][]time.Time{}
	}
	keep := s.noticeSent[tk.ID][:0]
	for _, at := range s.noticeSent[tk.ID] {
		if now.Sub(at) < time.Hour {
			keep = append(keep, at)
		}
	}
	if len(keep) >= noticesPerHour {
		s.noticeSent[tk.ID] = keep
		return false, keep[0].Add(time.Hour).Sub(now)
	}
	s.noticeSent[tk.ID] = append(keep, now)
	return true, 0
}

// handleNoticePost is POST /notices (spec sendNotice): an automation's
// notice, delivered and listed with the token's name as its origin.
func (s *Server) handleNoticePost(w http.ResponseWriter, r *http.Request, tk Token) {
	var req noticeRequest
	if err := readBody(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, badField("body", err.Error()))
		return
	}
	if p := req.validate(tk); p != nil {
		writeProblem(w, http.StatusBadRequest, *p)
		return
	}
	if s.notices == nil {
		writeProblem(w, http.StatusServiceUnavailable, problem{
			Title: "Notices unavailable", Code: "notices_unavailable", Detail: "this server has no notifier",
		})
		return
	}
	ok, wait := s.noticeAllowed(tk, s.clock())
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeProblem(w, http.StatusTooManyRequests, problem{
			Title: "Too many notices", Code: "notice_rate",
			Detail: fmt.Sprintf("at most %d notices an hour per token", noticesPerHour),
		})
		return
	}
	s.notices.Notify(notify.Notice{
		Event: noticeEventPrefix + req.Event, Severity: notify.Severity(req.Severity),
		Title: req.Title, Message: req.Message, Origin: tk.Name,
	})
	writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

// handleNoticesGet is GET /notices (spec listNotices): the kept
// notices, newest first; before (an id) pages back.
func (s *Server) handleNoticesGet(w http.ResponseWriter, r *http.Request, _ Token) {
	q := r.URL.Query()
	limit := noticesPageDef
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > noticesPageMax {
			writeProblem(w, http.StatusBadRequest, badField("limit", "must be an integer in 1..200"))
			return
		}
		limit = n
	}
	var before int64
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			writeProblem(w, http.StatusBadRequest, badField("before", "must be a notice id"))
			return
		}
		before = n
	}
	rows, err := s.st.Notices(before, limit)
	if err != nil {
		s.internal(w, err)
		return
	}
	out := make([]NoticeView, len(rows))
	for i, n := range rows {
		out[i] = noticeView(n)
	}
	writeJSON(w, http.StatusOK, map[string]any{"notices": out})
}

// noticeView is a kept notice in the spec's form ("" origin: foxess-local).
func noticeView(n store.NoticeRow) NoticeView {
	origin := n.Origin
	if origin == "" {
		origin = "foxess-local"
	}
	return NoticeView{ID: n.ID, TS: n.TS.UTC(), Origin: origin, Event: n.Event, Severity: n.Severity, Title: n.Title, Message: n.Message}
}
