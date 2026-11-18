package scan

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"foxesslocal/internal/compat"
)

// fakeReader answers every block with its address as the first value
// (and 0 after), except blocks listed as failing.
type fakeReader struct {
	mu    sync.Mutex
	fail  map[int]bool
	reads []int
}

func (f *fakeReader) read(_ context.Context, _, addr, count int) ([]uint16, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, addr)
	if f.fail[addr] {
		return nil, errors.New("modbus exception 2")
	}
	out := make([]uint16, count)
	out[0] = uint16(addr) //nolint:gosec // G115: test addresses fit
	return out, nil
}

var plan = []compat.ScanBlock{
	{Map: "identity", Fn: 3, Addr: 30000, Count: 15},
	{Map: "M", Fn: 3, Addr: 31000, Count: 29},
	{Map: "M", Fn: 4, Addr: 11000, Count: 10},
}

// TestScanRunsThePlan (F8): every block is read once, in order, paced;
// a failing block is recorded with its error and the scan carries on;
// progress and the finished results are visible; a second start while
// running is refused.
func TestScanRunsThePlan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeReader{fail: map[int]bool{11000: true}}
		s := New(f.read, time.Now)
		if err := s.Start("KH10", plan); err != nil {
			t.Fatal(err)
		}
		if err := s.Start("KH10", plan); !errors.Is(err, ErrRunning) {
			t.Fatalf("second start: %v", err)
		}
		synctest.Wait()
		if st := s.Status(); st.State != StateRunning || st.Done != 1 || st.Total != 3 {
			t.Fatalf("after the first read: %+v", st)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		checkFinished(t, s, f)
		s.Close()
	})
}

// checkFinished: all three blocks read once; the failing one recorded.
func checkFinished(t *testing.T, s *Scanner, f *fakeReader) {
	t.Helper()
	st := s.Status()
	if st.State != StateDone || st.Done != 3 || st.OK != 2 || st.Failed != 1 || st.Model != "KH10" {
		t.Fatalf("finished: %+v", st)
	}
	res := s.Results()
	if len(res) != 3 || res[1].Values[0] != 31000 || res[2].Err == "" || res[2].Values != nil {
		t.Fatalf("results %+v", res)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reads) != 3 {
		t.Fatalf("reads %v", f.reads)
	}
}

// TestScanRestartsWhenDone: a finished scan does not block the next one.
func TestScanRestartsWhenDone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New((&fakeReader{}).read, time.Now)
		if err := s.Start("KH10", plan[:1]); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if err := s.Start("KH10", plan[:1]); err != nil {
			t.Fatalf("restart: %v", err)
		}
		synctest.Wait()
		s.Close()
	})
}

// TestScanPaced: reads are spaced by Gap, so the live-value poll keeps
// its turn on the gateway.
func TestScanPaced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeReader{}
		s := New(f.read, time.Now)
		t0 := time.Now()
		if err := s.Start("KH10", plan); err != nil {
			t.Fatal(err)
		}
		for s.Status().State == StateRunning {
			time.Sleep(10 * time.Millisecond)
		}
		if el := time.Since(t0); el < 2*Gap {
			t.Fatalf("3 reads in %v, want at least %v between them", el, Gap)
		}
		s.Close()
	})
}
