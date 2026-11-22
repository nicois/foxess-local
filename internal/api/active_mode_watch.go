package api

import (
	"context"
	"log/slog"
	"time"
)

// activeModeMargin is how long after the inverter's minute turns the
// loop looks (a window starts on the minute; its end is exclusive).
const activeModeMargin = time.Second

// RunActiveMode publishes a config event (section active-mode, the full
// state) whenever the running mode changes with the time alone — a
// schedule window starting or ending on the inverter's clock, or the
// clock becoming known or lost. A schedule or work-mode write already
// publishes its own event; without this one a page would show the old
// mode until it reloaded. It looks once a minute, just after the
// inverter's minute turns.
func (s *Server) RunActiveMode(ctx context.Context) {
	last, known := s.activeModeTick(nil, false)
	for {
		t := time.NewTimer(s.untilNextWallMinute())
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		last, known = s.activeModeTick(last, known)
	}
}

// activeModeTick builds the state and publishes it when its running mode
// differs from last (known: last is a real reading — the first look only
// records). It returns the mode it saw.
func (s *Server) activeModeTick(last *ActiveMode, known bool) (*ActiveMode, bool) {
	st, err := s.st.QueryState()
	if err != nil {
		slog.Warn("active mode: state unreadable", "err", err)
		return last, known
	}
	body, err := s.buildState(st)
	if err != nil {
		slog.Warn("active mode: state unreadable", "err", err)
		return last, known
	}
	if known && !sameActiveMode(last, body.ActiveMode) {
		s.bus.PublishConfig(ConfigEvent{Changed: []string{SectionActiveMode}, State: body})
	}
	return body.ActiveMode, true
}

func sameActiveMode(a, b *ActiveMode) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// untilNextWallMinute is the wait to just past the inverter's next
// minute (a minute when its wall time is unknown).
func (s *Server) untilNextWallMinute() time.Duration {
	w, ok := s.wallNow()
	if !ok {
		return time.Minute
	}
	into := time.Duration(w.Second())*time.Second + time.Duration(w.Nanosecond())
	return time.Minute - into + activeModeMargin
}
