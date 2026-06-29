package tui

import (
	"github.com/Ceinl/plumtree/tui-runtime/components"
	"github.com/Ceinl/plumtree/tui-runtime/keyboard"
	"github.com/Ceinl/plumtree/tui-runtime/layout"

	"github.com/Ceinl/dmux/internal/remote"
)

// handleEvent routes one input event. Returns true to quit (detach).
func (u *connUI) handleEvent(ev keyboard.Event) (quit bool) {
	u.mu.Lock()
	m := u.mode
	u.mu.Unlock()

	switch m {
	case modeOverlay:
		u.overlayEvent(ev)
		return false
	case modePrefix:
		return u.prefixEvent(ev)
	default:
		return u.streamEvent(ev)
	}
}

// streamEvent handles input in normal pass-through mode.
func (u *connUI) streamEvent(ev keyboard.Event) (quit bool) {
	if ev.Mouse {
		if u.sidebar.routeMouse(ev) {
			u.signalRedraw()
		}
		return false
	}
	b := encode(ev)
	if len(b) == 1 && b[0] == u.t.prefix {
		u.mu.Lock()
		u.mode = modePrefix
		u.mu.Unlock()
		return false
	}
	if v := u.currentView(); v != "" && len(b) > 0 {
		_, _ = u.t.sessions.Write(v, b)
	}
	return false
}

// prefixEvent handles the command key after the prefix.
func (u *connUI) prefixEvent(ev keyboard.Event) (quit bool) {
	b := encode(ev)
	// Second prefix press → send a literal prefix byte to the session.
	if len(b) == 1 && b[0] == u.t.prefix {
		u.mu.Lock()
		u.mode = modeStream
		u.mu.Unlock()
		if v := u.currentView(); v != "" {
			_, _ = u.t.sessions.Write(v, b)
		}
		return false
	}

	u.mu.Lock()
	u.mode = modeStream
	u.mu.Unlock()

	switch ev.Ch {
	case 'p', 'P':
		u.openPicker()
	case 'l', 'L', 's', 'S':
		u.openList()
	case 'r', 'R':
		u.openRename()
	case 'k', 'K':
		u.openColorDevices()
	case 'c', 'C':
		u.toggleSidebar()
	case 'd', 'D':
		// Detach: return true so Handle's defers send exitTerm (restoring the
		// client's screen) before the connection is closed.
		return true
	}
	return false
}

// overlayEvent drives the picker/list overlay at the Event level.
func (u *connUI) overlayEvent(ev keyboard.Event) {
	u.mu.Lock()
	ov := u.overlay
	u.mu.Unlock()
	if ov == nil {
		u.mu.Lock()
		u.mode = modeStream
		u.mu.Unlock()
		return
	}

	switch ev.Type {
	case keyboard.KeyEscape:
		u.closeOverlay()
	case keyboard.KeyEnter:
		u.selectOverlay()
	case keyboard.KeyArrowUp:
		ov.move(-1)
		u.renderOverlay()
	case keyboard.KeyArrowDown:
		ov.move(1)
		u.renderOverlay()
	case keyboard.KeyBackspace:
		if ov.query != "" {
			ov.query = ov.query[:len(ov.query)-1]
			ov.refilter()
			u.renderOverlay()
		}
	case keyboard.KeyRune:
		if !ev.Ctrl && ev.Ch >= 0x20 {
			ov.query += string(ev.Ch)
			ov.refilter()
			u.renderOverlay()
		}
	}
}

// render lays out and paints the component tree, then places the host cursor.
// It is a no-op while an overlay owns the screen (the overlay draws raw).
func (u *connUI) render() {
	u.mu.Lock()
	if u.mode == modeOverlay {
		u.mu.Unlock()
		return
	}
	view := u.viewing
	u.mu.Unlock()

	u.sidebar.rebuild(u.t.sessions.List(), view)

	root := components.NewDiv()
	root.SetDirection(layout.Row)
	root.AppendChild(u.pane)
	root.AppendChild(u.sidebar.div)

	root.Layout(0, 0, u.scr.Width(), u.scr.Height())
	root.Render(u.scr)
	u.scr.Flush()

	// Place the real cursor at the host cursor inside the main pane.
	if view != "" {
		if cx, cy, vis := u.pane.cursor(); vis {
			u.scr.SetCursor(cx, cy)
			u.scr.ShowCursor()
		}
	}

	// Emit the whole buffered frame to the SSH channel in one write.
	_ = u.out.Flush()
}

// handleResize re-sizes the screen to the interface's new window.
func (u *connUI) handleResize(sz remote.Size) {
	if sz.Rows == 0 || sz.Cols == 0 {
		return
	}
	u.mu.Lock()
	u.size = sz
	u.mu.Unlock()
	_ = u.t.clients.SetSize(u.id, sz)
	u.scr.Resize(int(sz.Cols), int(sz.Rows))
}

// forceFullRepaint discards the diff baseline so the next render repaints every
// cell (used after an overlay or sidebar toggle disturbs the screen).
func (u *connUI) forceFullRepaint() {
	u.scr.Resize(u.scr.Width(), u.scr.Height())
	u.signalRedraw()
}

// writeConn writes raw bytes to the interface (used by overlays). It goes
// through the frame buffer and flushes immediately so the bytes appear at once
// rather than as a flurry of tiny SSH packets.
func (u *connUI) writeConn(b []byte) {
	_, _ = u.out.Write(b)
	_ = u.out.Flush()
}
func (u *connUI) writeString(s string) { u.writeConn([]byte(s)) }
func (u *connUI) clear()                { u.writeString("\x1b[2J\x1b[H") }

// encode turns a decoded keyboard event back into the bytes to send to the
// remote shell (the parser does not retain the raw bytes).
func encode(ev keyboard.Event) []byte {
	switch ev.Type {
	case keyboard.KeyRune:
		if ev.Ctrl {
			// Control byte: parser stored Ch = byte + 0x40.
			return []byte{byte(ev.Ch) - 0x40}
		}
		return []byte(string(ev.Ch))
	case keyboard.KeyEnter:
		return []byte{'\r'}
	case keyboard.KeyBackspace:
		return []byte{127}
	case keyboard.KeyTab:
		return []byte{'\t'}
	case keyboard.KeyEscape:
		return []byte{0x1b}
	case keyboard.KeyCtrlC:
		return []byte{3}
	case keyboard.KeyArrowUp:
		return []byte("\x1b[A")
	case keyboard.KeyArrowDown:
		return []byte("\x1b[B")
	case keyboard.KeyArrowRight:
		return []byte("\x1b[C")
	case keyboard.KeyArrowLeft:
		return []byte("\x1b[D")
	case keyboard.KeyHome:
		return []byte("\x1b[H")
	case keyboard.KeyEnd:
		return []byte("\x1b[F")
	case keyboard.KeyPageUp:
		return []byte("\x1b[5~")
	case keyboard.KeyPageDown:
		return []byte("\x1b[6~")
	case keyboard.KeyDelete:
		return []byte("\x1b[3~")
	case keyboard.KeyPaste:
		return []byte(ev.Text)
	default:
		return nil
	}
}
