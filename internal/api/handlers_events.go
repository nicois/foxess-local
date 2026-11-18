package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// handleEvents is GET /events (spec streamEvents): the SSE stream.
// Auth (including the load-bearing ?token= EventSource fallback) is
// the protect wrapper's; Last-Event-ID drives the replay-or-resync
// decision (an invalid or zero id replays from the start — a
// superset, so the client is never stuck).
// sseWriteFn builds one connection's SSE frame writer (a false return
// is the client going away — the stream ends).
func sseWriteFn(w http.ResponseWriter, flusher http.Flusher) func(Event) bool {
	return func(ev Event) bool {
		data, err := json.Marshal(ev.Data)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Kind, data); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
}

// lastEventID parses the Last-Event-ID header: (replay start, replay
// requested). A present-but-unparseable or non-positive id replays the
// whole window (the spec's Last-Event-ID contract).
func lastEventID(r *http.Request) (int64, bool) {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, true
	}
	return n, true
}

// flushReplay writes the resync marker (the connect missed events) and
// the replay window. false = the client went away.
func flushReplay(write func(Event) bool, resync bool, resyncID int64, replay []Event) bool {
	if resync {
		if !write(Event{ID: resyncID, Kind: EventResync, Data: ResyncEvent{}}) {
			return false
		}
	}
	for i := range replay {
		if !write(replay[i]) {
			return false
		}
	}
	return true
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, _ Token) {
	last, wantReplay := lastEventID(r)
	sub, replay, resync, resyncID := s.bus.Connect(last, wantReplay)
	if sub == nil {
		w.Header().Set("Retry-After", "30")
		writeProblem(w, http.StatusServiceUnavailable, problem{
			Title:  "Too many live streams",
			Code:   "too_many_streams",
			Detail: "the live-update limit is reached; close another tab or retry shortly",
		})
		return
	}
	defer sub.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	// a reverse proxy (nginx, the documented TLS route) must pass events
	// through as they come, not buffer them
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	write := sseWriteFn(w, flusher)
	// The connect-ready comment: it flushes the headers on connect (some
	// proxies hold the stream until the first byte) and marks the
	// subscription registered — events published before this line are not
	// on this live stream (a live connection has no replay).
	if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
		return
	}
	if flusher != nil {
		flusher.Flush()
	}
	if !flushReplay(write, resync, resyncID, replay) {
		return
	}
	for {
		select {
		case ev := <-sub.Events():
			if !write(ev) {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}
