package store

import (
	"log/slog"

	"github.com/nicois/prox/pkg/capture"
)

// AttachRegistry wires the capture registry: every connection record
// gets one persistStream goroutine that blocks on the stream's Wait and
// then persists the connections row (with the evicted-chunk count) and
// all retained chunk rows in stream order. Registry eviction
// (Registry.Evict) stays with the caller — the store runs no timer.
func (s *Store) AttachRegistry(reg *capture.Registry) {
	reg.OnConnection(func(st *capture.Stream) {
		s.streamWG.Add(1)
		go s.persistStream(st)
	})
}

// persistStream blocks until the stream finishes, then enqueues its
// connections row and chunk rows. A stream that finishes after Close is
// dropped, never a panic: a stop must not crash on a flow the drain
// force-closed (2026-11-18 23:37:20, "RecordChunk after Close"). Holding
// persistMu keeps Close from landing between the rows.
func (s *Store) persistStream(st *capture.Stream) {
	defer s.streamWG.Done()
	<-st.Wait()
	info := st.Info()
	s.persistMu.RLock()
	defer s.persistMu.RUnlock()
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		slog.Warn("store: capture stream finished after close; not persisted", "id", info.ID, "chunks", len(st.Chunks()))
		return
	}
	s.recordConn(info, st.Evicted())
	for _, c := range st.Chunks() {
		s.recordChunk(c, info.ID)
	}
}

// WaitStreams blocks until every persistStream has completed — i.e. every
// finished stream's connections row and chunk rows are enqueued. The host
// calls it after quiescing the registry (all flows ended) and before
// Close: Close itself does not wait, because a still-open stream keeps
// its persistStream until the registry evicts it (eviction is the
// caller's job — see AttachRegistry), and Close must never block on a
// live stream.
func (s *Store) WaitStreams() {
	s.checkOpen("WaitStreams")
	s.streamWG.Wait()
}
