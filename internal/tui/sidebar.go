package tui

import (
	"fmt"

	"github.com/Ceinl/plumtree/tui-runtime/components"
	"github.com/Ceinl/plumtree/tui-runtime/keyboard"
	"github.com/Ceinl/plumtree/tui-runtime/layout"

	"dmux/internal/session"
)

const (
	sidebarWidth   = 26 // expanded
	collapsedWidth = 1  // "1 pixel" sliver with an expand handle
)

// sidebar is the right-hand panel: a collapse toggle at the top, then one
// clickable button per session. It rebuilds its buttons when the session set
// changes (M: tmux-style session list, mouse-sensitive).
type sidebar struct {
	div       *components.Div
	collapse  *components.Button
	rows      []*sessButton
	collapsed bool
	sig       string // signature of the last-built session list
	onSelect  func(session.ID)
}

// sessButton pairs a clickable button with the session it selects.
type sessButton struct {
	btn *components.Button
	id  session.ID
}

func newSidebar(onCollapse func(), onSelect func(session.ID)) *sidebar {
	sb := &sidebar{div: components.NewDiv()}
	sb.div.SetDirection(layout.Column)
	sb.div.SetSize(layout.Unit{Type: layout.UnitPx, Value: sidebarWidth}, layout.Unit{Type: layout.UnitGrow})
	sb.div.SetStyle(panelStyle())

	sb.collapse = components.NewButton(collapseLabel(false))
	sb.collapse.OnClick = onCollapse

	sb.onSelect = onSelect
	return sb
}

// width returns the sidebar's current width in columns.
func (sb *sidebar) width() int {
	if sb.collapsed {
		return collapsedWidth
	}
	return sidebarWidth
}

// toggle flips collapsed state and updates the toggle label.
func (sb *sidebar) toggle() {
	sb.collapsed = !sb.collapsed
	sb.collapse.SetLabel(collapseLabel(sb.collapsed))
	sb.applyWidth()
}

func (sb *sidebar) applyWidth() {
	sb.div.SetSize(
		layout.Unit{Type: layout.UnitPx, Value: float64(sb.width())},
		layout.Unit{Type: layout.UnitGrow},
	)
}

// rebuild reconstructs the button list from sessions when the set changes.
// active is the currently-viewed session (highlighted).
func (sb *sidebar) rebuild(sessions []session.Session, active session.ID) {
	sig := listSignature(sessions, active, sb.collapsed)
	if sig == sb.sig {
		return
	}
	sb.sig = sig

	// Reset children: collapse toggle first.
	sb.div = components.NewDiv()
	sb.div.SetDirection(layout.Column)
	sb.div.SetStyle(panelStyle())
	sb.applyWidth()
	sb.div.AppendChild(sb.collapse)
	sb.rows = sb.rows[:0]

	if sb.collapsed {
		return // sliver only: just the toggle handle
	}

	for _, s := range sessions {
		s := s
		label := fmt.Sprintf(" %s", sessionItemLabel(s))
		b := components.NewButton(label)
		if s.ID == active {
			b.SetStyles(activeStyle(), focusStyle(), pressStyle())
			b.SetFocused(true)
		} else {
			b.SetStyles(itemStyle(), focusStyle(), pressStyle())
		}
		id := s.ID
		b.OnClick = func() { sb.onSelect(id) }
		sb.div.AppendChild(b)
		sb.rows = append(sb.rows, &sessButton{btn: b, id: id})
	}
}

// routeMouse dispatches a mouse event to the collapse toggle and session
// buttons. Returns true if a button consumed it.
func (sb *sidebar) routeMouse(ev keyboard.Event) bool {
	x, y := ev.MouseX, ev.MouseY
	switch ev.Type {
	case keyboard.KeyMouseLeftDown:
		if sb.collapse.HandleMouseDown(x, y) {
			return true
		}
		for _, r := range sb.rows {
			if r.btn.HandleMouseDown(x, y) {
				return true
			}
		}
	case keyboard.KeyMouseLeftUp:
		consumed := sb.collapse.HandleMouseUp(x, y)
		for _, r := range sb.rows {
			if r.btn.HandleMouseUp(x, y) {
				consumed = true
			}
		}
		return consumed
	}
	return false
}

func collapseLabel(collapsed bool) string {
	if collapsed {
		return ">"
	}
	return "< sessions"
}

func sessionItemLabel(s session.Session) string {
	name := s.Title
	if name == "" {
		name = string(s.ID)
	}
	host := string(s.HostID)
	if len(host) > 8 {
		host = host[:8]
	}
	return fmt.Sprintf("%s · %s", host, name)
}

func listSignature(sessions []session.Session, active session.ID, collapsed bool) string {
	s := fmt.Sprintf("c=%v|a=%s|", collapsed, active)
	for _, x := range sessions {
		s += string(x.ID) + ":" + x.Title + ";"
	}
	return s
}

// --- styles (RGB) ---

func panelStyle() layout.Style {
	var s layout.Style
	s.SetBackground(30, 28, 36)
	s.SetForeground(200, 200, 200)
	return s
}
func itemStyle() layout.Style {
	var s layout.Style
	s.SetBackground(40, 37, 50)
	s.SetForeground(210, 206, 220)
	return s
}
func activeStyle() layout.Style {
	var s layout.Style
	s.SetBackground(96, 90, 128)
	s.SetForeground(255, 255, 255)
	s.AddTextDecoration(layout.Bold)
	return s
}
func focusStyle() layout.Style {
	var s layout.Style
	s.SetBackground(72, 68, 96)
	s.SetForeground(238, 234, 248)
	return s
}
func pressStyle() layout.Style {
	var s layout.Style
	s.SetBackground(120, 112, 160)
	s.SetForeground(255, 255, 255)
	return s
}
