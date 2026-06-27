package tui

import (
	"dmux/internal/remote"
	"dmux/internal/session"
)

// writeConn writes raw bytes to the interface under the conn-write lock.
func (u *connUI) writeConn(b []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	_, _ = u.conn.Write(b)
}

func (u *connUI) writeString(s string) { u.writeConn([]byte(s)) }

// clear wipes the screen and homes the cursor.
func (u *connUI) clear() { u.writeString("\x1b[2J\x1b[H") }

// stopSub cancels the current session subscription, if any.
func (u *connUI) stopSub() {
	u.mu.Lock()
	cancel := u.cancelSub
	u.cancelSub = nil
	u.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// switchView points the interface at a session: cancel the old subscription,
// replay scrollback, then stream live output (M6.3).
func (u *connUI) switchView(id session.ID) {
	u.stopSub()

	events, cancel, err := u.t.sessions.Subscribe(id)
	if err != nil {
		return
	}

	u.mu.Lock()
	u.viewing = id
	u.cancelSub = cancel
	u.mode = modeStream
	u.mu.Unlock()

	// Replay history so the user sees context, not just new output.
	u.clear()
	if sb, ok := u.t.sessions.Scrollback(id); ok {
		u.writeConn(sb.Snapshot())
	}
	// Size the host PTY to the negotiated size now that we view it.
	u.renegotiate(id)

	go u.streamPump(events)
}

// streamPump copies session output to the interface while in stream mode and
// reacts to the session stopping (M6.3).
func (u *connUI) streamPump(events <-chan session.Event) {
	for ev := range events {
		if ev.Stopped {
			u.onViewStopped()
			return
		}
		if len(ev.Data) == 0 {
			continue
		}
		u.mu.Lock()
		stream := u.mode == modeStream
		u.mu.Unlock()
		if stream {
			u.writeConn(ev.Data)
		}
	}
}

// onViewStopped handles the active session ending (host down or closed): the
// server auto-moves attached clients, so re-read our assignment and follow it.
func (u *connUI) onViewStopped() {
	c, ok := u.t.clients.Get(u.id)
	if ok && c.Viewing != "" && c.Viewing != u.currentView() {
		u.switchView(c.Viewing)
		return
	}
	if id, ok := u.t.firstRunning(); ok {
		u.switchView(id)
		return
	}
	u.clear()
	u.writeString(splash)
}

func (u *connUI) currentView() session.ID {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.viewing
}

// renegotiate recomputes the session PTY size from all its viewers and applies
// it (M6.5 resize hook; mirrors the server but driven by the local size change).
func (u *connUI) renegotiate(id session.ID) {
	if sz, ok := u.t.clients.NegotiatedSize(id, u.t.cfg.ResizePolicy); ok {
		_ = u.t.sessions.Resize(id, sz)
	}
}

// resizeLoop tracks the interface's own window size (M6.5).
func (u *connUI) resizeLoop() {
	for sz := range u.conn.Resizes() {
		u.mu.Lock()
		u.size = sz
		view := u.viewing
		inOverlay := u.mode == modeOverlay
		ov := u.overlay
		u.mu.Unlock()

		_ = u.t.clients.SetSize(u.id, sz)
		if view != "" {
			u.renegotiate(view)
		}
		if inOverlay && ov != nil {
			u.renderOverlay()
		}
	}
}

// setMode updates the input routing mode.
func (u *connUI) setMode(m mode) {
	u.mu.Lock()
	u.mode = m
	u.mu.Unlock()
}

// viewSize returns the interface's last known size.
func (u *connUI) viewSize() remote.Size {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.size
}
