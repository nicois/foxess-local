// Package scan runs a read-only device scan (F8): every register block of
// a scan plan (compat.ScanPlan) is read once through the gateway, paced
// so the live-value poll keeps its turn, and the results are kept for the
// next problem report. Reads only — nothing is ever written.
package scan

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"foxesslocal/internal/compat"
	"foxesslocal/internal/gateway"
)

// Gap is the pause between two scan reads (the 5 s live poll interleaves).
const Gap = 250 * time.Millisecond

// ErrRunning refuses a second scan while one runs.
var ErrRunning = errors.New("scan: a scan is already running")

// Reader reads one register block (fn 3 holding / 4 input).
type Reader func(ctx context.Context, fn, addr, count int) ([]uint16, error)

// State is the scan's lifecycle.
type State string

// The states.
const (
	StateIdle    State = "idle"
	StateRunning State = "running"
	StateDone    State = "done"
)

// Result is one block's outcome: the values, or the error.
type Result struct {
	compat.ScanBlock
	Values []uint16 `json:"values,omitempty"`
	Err    string   `json:"error,omitempty"`
}

// Status is the progress view.
type Status struct {
	State    State      `json:"state"`
	Model    string     `json:"model,omitempty"`
	Started  *time.Time `json:"started_at,omitempty"`
	Finished *time.Time `json:"finished_at,omitempty"`
	Total    int        `json:"blocks_total"`
	Done     int        `json:"blocks_done"`
	OK       int        `json:"blocks_ok"`
	Failed   int        `json:"blocks_failed"`
}

// Scanner runs one scan at a time and keeps the last results.
type Scanner struct {
	read   Reader
	clock  func() time.Time
	ctx    context.Context //nolint:containedctx // the scanner owns its scans' lifetime; Close cancels it
	cancel context.CancelFunc

	mu      sync.Mutex
	status  Status
	results []Result
}

// New builds a scanner over a block reader.
func New(read Reader, clock func() time.Time) *Scanner {
	ctx, cancel := context.WithCancel(context.Background())
	return &Scanner{read: read, clock: clock, ctx: ctx, cancel: cancel, status: Status{State: StateIdle}}
}

// Start begins a scan of plan for model in the background.
func (s *Scanner) Start(model string, plan []compat.ScanBlock) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.State == StateRunning {
		return ErrRunning
	}
	now := s.clock()
	s.status = Status{State: StateRunning, Model: model, Started: &now, Total: len(plan)}
	s.results = make([]Result, 0, len(plan))
	go s.run(append([]compat.ScanBlock(nil), plan...))
	return nil
}

func (s *Scanner) run(plan []compat.ScanBlock) {
	for i, b := range plan {
		if i > 0 {
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(Gap):
			}
		}
		vals, err := s.read(s.ctx, b.Fn, b.Addr, b.Count)
		r := Result{ScanBlock: b}
		if err != nil {
			r.Err = err.Error()
		} else {
			r.Values = vals
		}
		s.mu.Lock()
		s.results = append(s.results, r)
		s.status.Done++
		if err != nil {
			s.status.Failed++
		} else {
			s.status.OK++
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	now := s.clock()
	s.status.State, s.status.Finished = StateDone, &now
	s.mu.Unlock()
}

// Status returns the progress.
func (s *Scanner) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Results returns the last scan's results (complete once State is done).
func (s *Scanner) Results() []Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Result(nil), s.results...)
}

// Close stops a running scan.
func (s *Scanner) Close() { s.cancel() }

// Submitter is the gateway surface a scan reads through.
type Submitter interface {
	Submit(ctx context.Context, c gateway.Command) (string, error)
	Wait(ctx context.Context, id string) (gateway.Result, error)
}

// readWait bounds one scan read (the gateway's own in-flight bound is
// 10 s; queued behind an operator command it may wait longer).
const readWait = 30 * time.Second

// GatewayReader reads blocks through the gateway as operator-origin
// reads (they queue behind commands; the live poll yields to them).
func GatewayReader(gw Submitter, origin string) Reader {
	return func(ctx context.Context, fn, addr, count int) ([]uint16, error) {
		id, err := gw.Submit(ctx, gateway.Command{
			Kind:   gateway.KindRead,
			Params: map[gateway.Param]int{gateway.ParamFn: fn, gateway.ParamAddr: addr, gateway.ParamCount: count},
			Origin: origin,
		})
		if err != nil {
			return nil, err
		}
		wctx, cancel := context.WithTimeout(ctx, readWait)
		defer cancel()
		res, err := gw.Wait(wctx, id)
		if err != nil {
			return nil, err
		}
		if res.Status != gateway.StatusAcked {
			return nil, fmt.Errorf("%s: %s", res.Status, res.Detail)
		}
		out := make([]uint16, len(res.Registers))
		for i, rv := range res.Registers {
			out[i] = rv.Value
		}
		return out, nil
	}
}
