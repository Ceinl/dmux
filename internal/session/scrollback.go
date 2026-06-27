package session

import "sync"

// bytesPerLine is the rough budget used to turn a line-bound (config's
// ScrollbackLines) into a byte cap for the ring buffer (M4.2).
const bytesPerLine = 256

// ringScrollback is a byte-bounded ring buffer of recent session output. It
// keeps at most cap bytes, dropping from the front as new output arrives, so a
// reattaching interface sees recent history (M4.2).
type ringScrollback struct {
	mu  sync.Mutex
	buf []byte
	cap int
}

// newRingScrollback builds a buffer holding ~lines lines of output.
func newRingScrollback(lines int) *ringScrollback {
	c := lines * bytesPerLine
	if c <= 0 {
		c = bytesPerLine
	}
	return &ringScrollback{cap: c}
}

// write appends p, trimming from the front to stay within cap.
func (r *ringScrollback) write(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.cap {
		// Keep only the trailing cap bytes.
		trim := len(r.buf) - r.cap
		r.buf = append(r.buf[:0], r.buf[trim:]...)
	}
}

// Snapshot returns a copy of the buffered bytes for replay on attach.
func (r *ringScrollback) Snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, len(r.buf))
	copy(out, r.buf)
	return out
}

// Len is the number of bytes currently buffered.
func (r *ringScrollback) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.buf)
}

// Compile-time check.
var _ Scrollback = (*ringScrollback)(nil)
