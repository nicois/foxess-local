package framesvc

import "foxesslocal/internal/frame"

// reasm is one direction's frame reassembler: chunks are arbitrary TCP
// boundaries (R1), so it accumulates bytes, splits complete frames with
// frame.Split, and retains at most one trailing partial frame. A bad CRC
// or terminator is never fatal (Split resynchronizes from the next byte);
// the residue is therefore bounded by the largest declared frame (a
// spurious start marker holds at most its declared length before Split
// rejects it and progress resumes).
type reasm struct {
	buf []byte // trailing partial frame (retained between chunks)
	off int64  // bytes fully consumed: the base offset for the next Split
}

// newReasm returns an empty reassembler.
func newReasm() *reasm { return &reasm{} }

// Feed appends one chunk and returns the complete frames it contains, in
// stream order. Off is the start byte's offset in the direction stream.
func (r *reasm) Feed(chunk []byte) []frame.Frame {
	r.buf = append(r.buf, chunk...)
	frames, consumed := frame.Split(r.buf, r.off)
	r.off += int64(consumed)
	// Rebound the buffer to the retained residue so a large chunk (a
	// multi-MiB burst) does not pin its backing array for the flow's
	// lifetime.
	if rem := len(r.buf) - consumed; rem == 0 {
		r.buf = nil
	} else {
		r.buf = append([]byte(nil), r.buf[consumed:]...)
	}
	return frames
}

// Frame start markers (7e7e / 7f7f).
const (
	markE = 0x7e
	markF = 0x7f
)

// Settled is the stream offset before which no retained byte can belong
// to a frame still to complete: the first possible start marker in the
// residue (a marker pair, or a lone marker byte at the very end). The
// bytes before it are not a frame and never will be.
func (r *reasm) Settled() int64 {
	b := r.buf
	for i := range b {
		if b[i] != markE && b[i] != markF {
			continue
		}
		if i == len(b)-1 || b[i+1] == b[i] {
			return r.off + int64(i)
		}
	}
	return r.off + int64(len(b))
}

// Residue returns the trailing partial frame retained after the last
// chunk, and the stream offset where the retained bytes start.
func (r *reasm) Residue() ([]byte, int64) {
	return r.buf, r.off
}
