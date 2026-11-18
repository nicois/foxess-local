package proxconformance

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nicois/prox/pkg/config"
	"github.com/nicois/prox/pkg/prox"
	"github.com/nicois/prox/pkg/rules"

	"github.com/stretchr/testify/require"
)

// TestR13_ConsumerOwnedCaptureRules: the rule lifecycle is consumer
// controlled (position R13; satisfied in pin 8d68f0e). Two contracts,
// both driven at the facade — Prox.Run is the only seam where rule
// ownership is decided:
//
//   - consumer mode (rules.mode "consumer"): the facade makes no runner
//     calls at all — neither at startup nor on exit (C-007: the consumer
//     owns the egress gate; the tproxy diversion is one rule inside that
//     gate, not a second ruleset the library manages).
//   - managed mode with a consumer-injected runner (SetRulesRunner):
//     install and removal both go through the injected runner — no
//     fallback to the built-in exec runner (clarification doc Q3).
//
// Run is driven to its return: on an unprivileged host the transparent
// listener fails at once; on a privileged host Run blocks in the accept
// loop until the ctx is cancelled. The recorded command sequence is the
// same either way, so the assertions are environment-invariant.
func TestR13_ConsumerOwnedCaptureRules(t *testing.T) {
	const listen = "127.0.0.1:41443"
	cfg := func(mode string) *config.Config {
		ca := newTestCA(t, "r13-test-ca")
		certPEM, keyPEM := ca.caFiles(t, t.TempDir())
		c := config.Default()
		c.DataRoot = "" // embedded: no store, no file capture
		c.Listen = listen
		c.Ports = []int{14431}
		c.CAPaths = []config.CAPath{{CertPEM: certPEM, KeyPEM: keyPEM}}
		c.MITM = config.MITM{ValidityDays: 365}
		c.Rules.Mode = mode
		return c
	}

	t.Run("consumer mode makes no runner calls", func(t *testing.T) {
		r := &recordingRunner{}
		runProxToReturn(t, cfg("consumer"), r)
		require.Empty(t, r.calls, "consumer mode must install and remove nothing")
	})

	t.Run("managed mode routes install and removal through the injected runner", func(t *testing.T) {
		r := &recordingRunner{}
		runProxToReturn(t, cfg("managed"), r)
		params := rules.Params{OnPort: 41443, Ports: []int{14431}, FWMark: 1, RoutingTable: 100}
		want := append([]rules.Cmd{}, rules.BuildRemove(params)...) // startup clear
		want = append(want, rules.BuildRules(params)...)            // install
		want = append(want, rules.BuildRemove(params)...)           // exit removal
		require.Equal(t, want, r.calls)
	})
}

// runProxToReturn builds the facade from cfg, injects the recording
// runner, and drives Run until it returns: the transparent listener
// fails at once on an unprivileged host, and on a privileged host Run
// blocks in the accept loop until the ctx is cancelled.
func runProxToReturn(t *testing.T, cfg *config.Config, r *recordingRunner) {
	t.Helper()
	px, err := prox.New(cfg)
	require.NoError(t, err)
	px.SetRulesRunner(r)
	ctx, cancel := context.WithCancel(context.Background())
	// done is closed, not sent: the wait-then-cancel select below may
	// already have consumed a return, and a close unblocks every later
	// receive (unprivileged host: Run returns before the select times
	// out; privileged host: it blocks in the accept loop until cancel).
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = px.Run(ctx)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second): // privileged host: Run blocks in the accept loop
	}
	cancel()
	<-done
}

// recordingRunner records every command the rule lifecycle asks for; it
// executes nothing (the real iptables/ip state is never touched).
type recordingRunner struct {
	mu    sync.Mutex
	calls []rules.Cmd
}

func (r *recordingRunner) Run(name string, args ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, rules.Cmd{Bin: name, Args: append([]string(nil), args...)})
	return nil
}
