package main

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"foxesslocal/internal/update"
)

// bindBusyUntil is a fake prox.Run: it fails with the wrapped
// EADDRINUSE prox returns (`prox: listen: bind: %w`) until the virtual
// clock reaches clearAt, then serves until ctx is cancelled.
func bindBusyUntil(start time.Time, clearAt time.Duration, attempts *int) func(context.Context) error {
	return func(ctx context.Context) error {
		*attempts++
		if time.Since(start) < clearAt {
			return fmt.Errorf("prox: listen: bind: %w", syscall.EADDRINUSE)
		}
		<-ctx.Done()
		return nil
	}
}

// TestSuperviseListenerWaitsForPort (DEF-009): a port still held by the
// previous session's socket clears within the wait window — the
// supervisor retries and the listener comes up; no fatal.
func TestSuperviseListenerWaitsForPort(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		attempts := 0
		done := make(chan error, 1)
		go func() {
			done <- superviseListener(ctx, bindBusyUntil(time.Now(), 30*time.Second, &attempts))
		}()
		time.Sleep(119 * time.Second)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("supervisor exited while the listener was serving: %v", err)
		default:
		}
		require.Greater(t, attempts, 1, "the busy port was retried")
		cancel()
		require.NoError(t, <-done, "a cancelled serving listener is a clean stop")
	})
}

// TestSuperviseListenerFatalAfterWait (DEF-009): a port that never
// clears is fatal at the end of the window — never a silently degraded
// service.
func TestSuperviseListenerFatalAfterWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		attempts := 0
		err := superviseListener(t.Context(), bindBusyUntil(start, time.Hour, &attempts))
		require.ErrorIs(t, err, syscall.EADDRINUSE)
		require.ErrorContains(t, err, "still in use after 2m0s")
		elapsed := time.Since(start)
		require.GreaterOrEqual(t, elapsed, listenerBindWait)
		require.Less(t, elapsed, listenerBindWait+listenerRetryEvery+time.Second)
	})
}

// TestSuperviseListenerOtherErrorIsFatal: any non-EADDRINUSE failure
// (e.g. a missing CAP_NET_ADMIN) is fatal at once — retrying cannot fix it.
func TestSuperviseListenerOtherErrorIsFatal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		perm := fmt.Errorf("prox: listen: IP_TRANSPARENT: %w", syscall.EPERM)
		err := superviseListener(t.Context(), func(context.Context) error { return perm })
		require.ErrorIs(t, err, syscall.EPERM)
		require.Zero(t, time.Since(start), "no retry delay for a non-busy error")
	})
}

// TestSuperviseListenerUnexpectedStopIsFatal: the listener returning
// while the service is still running (no ctx cancel) is the degraded
// state DEF-009 names — fatal, so systemd restarts the service.
func TestSuperviseListenerUnexpectedStopIsFatal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		err := superviseListener(t.Context(), func(context.Context) error { return nil })
		require.ErrorContains(t, err, "stopped unexpectedly")
	})
}

// TestSuperviseListenerCancelDuringWait: a shutdown while waiting for
// the port is a clean stop, not a fatal.
func TestSuperviseListenerCancelDuringWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		attempts := 0
		done := make(chan error, 1)
		go func() {
			done <- superviseListener(ctx, bindBusyUntil(time.Now(), time.Hour, &attempts))
		}()
		time.Sleep(10 * time.Second)
		cancel()
		require.NoError(t, <-done)
	})
}

// TestAwaitExit: runCmd's main wait — a signal is a clean exit (0); a
// fatal from the listener supervisor is a failed run (non-zero, so
// systemd's Restart=always retries).
func TestAwaitExit(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	fatal := make(chan error, 1)
	cancel()
	code, err := awaitExit(ctx, fatal)
	require.Equal(t, 0, code)
	require.NoError(t, err)

	fatal <- errors.New("boom")
	code, err = awaitExit(t.Context(), fatal)
	require.Equal(t, exitRunFailed, code)
	require.Error(t, err)
}

// TestExitCause (install review G4): only a port still held after the
// bind wait is recorded as an environmental exit; other fatals count.
func TestExitCause(t *testing.T) {
	busy := fmt.Errorf("inverter listener: port still in use after 2m0s: %w", syscall.EADDRINUSE)
	require.Equal(t, update.ExitPortInUse, exitCause(busy))
	require.Empty(t, exitCause(errors.New("inverter listener: IP_TRANSPARENT: operation not permitted")))
	require.Empty(t, exitCause(nil))
}
