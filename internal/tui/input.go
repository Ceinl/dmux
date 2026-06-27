package tui

// inputLoop is the single reader for the connection. It routes each byte by the
// current mode: pass-through to the session, prefix-command dispatch, or overlay
// handling (M6.4).
func (u *connUI) inputLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := u.conn.Read(buf)
		if n > 0 {
			u.feed(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// feed dispatches a chunk of input byte-by-byte so prefix detection and overlay
// editing stay correct even when bytes arrive coalesced.
func (u *connUI) feed(p []byte) {
	for _, b := range p {
		u.mu.Lock()
		m := u.mode
		u.mu.Unlock()

		switch m {
		case modeStream:
			u.feedStream(b)
		case modePrefix:
			u.feedPrefix(b)
		case modeOverlay:
			u.feedOverlay(b)
		}
	}
}

// feedStream handles a key in normal pass-through mode.
func (u *connUI) feedStream(b byte) {
	if b == u.t.prefix {
		u.setMode(modePrefix)
		return
	}
	view := u.currentView()
	if view != "" {
		_, _ = u.t.sessions.Write(view, []byte{b})
	}
}

// feedPrefix handles the single command key after the prefix (M6.4).
func (u *connUI) feedPrefix(b byte) {
	// A second prefix press sends a literal prefix byte to the session.
	if b == u.t.prefix {
		u.setMode(modeStream)
		if view := u.currentView(); view != "" {
			_, _ = u.t.sessions.Write(view, []byte{b})
		}
		return
	}

	switch b {
	case 'p', 'P':
		u.openPicker()
	case 'l', 'L', 's', 'S':
		u.openList()
	case 'd', 'D':
		_ = u.conn.Close() // detach
	default:
		// Unknown command: drop back to streaming.
		u.setMode(modeStream)
	}
}
