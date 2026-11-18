package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"syscall"
	"time"

	"foxesslocal/internal/update"
)

// DEF-009: the inverter listener must never fail silently. After a stop,
// the previous session's kernel socket can hold 14431 for up to
// tcp_fin_timeout / TIME_WAIT (60 s on the gateway host) — and prox binds with a
// raw syscall.Bind without SO_REUSEADDR, so even TIME_WAIT blocks it.
// The supervisor retries the bind for listenerBindWait, then fails the
// whole process (systemd's Restart=always retries it), and treats any
// other listener exit while the service runs as fatal too.
const (
	listenerBindWait   = 120 * time.Second
	listenerRetryEvery = 5 * time.Second
)

// runListener is the inverter listener's run function — prox.Run in
// production; the runCmd e2e tests substitute it (an unprivileged test
// cannot open the IP_TRANSPARENT socket).
var runListener = func(h *handle) func(context.Context) error { return h.prox.Run }

// superviseListener runs the inverter listener (prox.Run) under the
// DEF-009 policy: EADDRINUSE is retried every listenerRetryEvery until
// listenerBindWait has passed (tested at their real values on synctest
// virtual time). It returns nil on a clean stop (ctx
// cancelled) and an error the caller must treat as fatal otherwise.
func superviseListener(ctx context.Context, run func(context.Context) error) error {
	const bindWait, retryEvery = listenerBindWait, listenerRetryEvery
	deadline := time.Now().Add(bindWait)
	for {
		err := run(ctx)
		if ctx.Err() != nil {
			return nil //nolint:nilerr // the run error is the shutdown itself: a clean stop
		}
		switch {
		case err == nil:
			return errors.New("inverter listener stopped unexpectedly")
		case !errors.Is(err, syscall.EADDRINUSE):
			return fmt.Errorf("inverter listener: %w", err)
		case !time.Now().Before(deadline):
			return fmt.Errorf("inverter listener: port still in use after %s: %w", bindWait, err)
		}
		slog.Warn("inverter listener: port in use (previous session draining) — retrying",
			"retry_in", retryEvery, "gives_up_in", time.Until(deadline).Round(time.Second))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retryEvery):
		}
	}
}

// awaitExit blocks until a shutdown signal (exit 0) or a fatal error
// from a supervised component (exitRunFailed, with the error).
func awaitExit(ctx context.Context, fatal <-chan error) (int, error) {
	select {
	case <-ctx.Done():
		return 0, nil
	case err := <-fatal:
		slog.Error("fatal: shutting down", "err", err)
		return exitRunFailed, err
	}
}

// exitCause names a fatal error the environment caused, for the upgrade
// guard (install review G4): the inverter port still held after the bind
// wait. "" for anything else — those count against a new binary.
func exitCause(err error) string {
	if errors.Is(err, syscall.EADDRINUSE) {
		return update.ExitPortInUse
	}
	return ""
}
