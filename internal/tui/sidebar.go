package tui

import (
	"fmt"
	"hash/fnv"
	"strconv"

	"github.com/Ceinl/plumtree/tui-runtime/components"
	"github.com/Ceinl/plumtree/tui-runtime/keyboard"
	"github.com/Ceinl/plumtree/tui-runtime/layout"

	"github.com/Ceinl/dmux/internal/session"
)

const (
	sidebarWidth   = 26 // expanded
	collapsedWidth = 1  // "1 pixel" sliver with an expand handle
	rowHeight      = 1  // fixed height of the toggle and each session row
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
	// colorOf resolves a device's sidebar color (user override or auto-derived).
	colorOf func(hostID string) (r, g, b uint8)
}

// sessButton pairs a clickable button with the session it selects.
type sessButton struct {
	btn *components.Button
	id  session.ID
}

func newSidebar(onCollapse func(), onSelect func(session.ID), colorOf func(string) (uint8, uint8, uint8)) *sidebar {
	sb := &sidebar{div: components.NewDiv(), colorOf: colorOf}
	sb.div.SetDirection(layout.Column)
	sb.div.SetSize(layout.Unit{Type: layout.UnitPx, Value: sidebarWidth}, layout.Unit{Type: layout.UnitGrow})
	sb.div.SetStyle(panelStyle())

	sb.collapse = components.NewButton(collapseLabel(false))
	sb.collapse.SetStyles(headerStyle(), headerFocusStyle(), headerPressStyle())
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
	sb.div.AppendChild(fixedRow(sb.collapse))
	sb.rows = sb.rows[:0]

	if sb.collapsed {
		return // sliver only: just the toggle handle
	}

	for _, s := range sessions {
		s := s
		label := fmt.Sprintf(" %s", sessionItemLabel(s))
		b := components.NewButton(label)
		r, g, bl := sb.colorOf(string(s.HostID))
		if s.ID == active {
			// The active row is always focused, and Button shows the focus style
			// when focused — so pass the active style there too, or its device
			// tint would never appear.
			as := activeStyle(r, g, bl)
			b.SetStyles(as, as, pressStyle(r, g, bl))
			b.SetFocused(true)
		} else {
			b.SetStyles(itemStyle(r, g, bl), focusStyle(r, g, bl), pressStyle(r, g, bl))
		}
		id := s.ID
		b.OnClick = func() { sb.onSelect(id) }
		sb.div.AppendChild(fixedRow(b))
		sb.rows = append(sb.rows, &sessButton{btn: b, id: id})
	}
}

// fixedRow wraps a component in a full-width, fixed-height container so it keeps
// a static size in the sidebar's column instead of growing to fill free space.
func fixedRow(child layout.Component) *components.Div {
	row := components.NewDiv()
	row.SetSize(
		layout.Unit{Type: layout.UnitGrow},
		layout.Unit{Type: layout.UnitPx, Value: float64(rowHeight)},
	)
	row.AppendChild(child)
	return row
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
	s.SetBackground(24, 24, 24)
	s.SetForeground(190, 190, 190)
	return s
}
// itemStyle is a resting session row, tinted with its device's color so each
// device reads as a distinct hue in the list.
func itemStyle(r, g, b uint8) layout.Style {
	var s layout.Style
	s.SetBackground(32, 32, 32)
	s.SetForeground(r, g, b)
	return s
}

// activeStyle is the currently-viewed row: same device tint, brightened and
// bold on a lighter background so the selection stands out.
func activeStyle(r, g, b uint8) layout.Style {
	var s layout.Style
	s.SetBackground(58, 58, 58)
	s.SetForeground(brighten(r), brighten(g), brighten(b))
	s.AddTextDecoration(layout.Bold)
	return s
}

// paletteColor is a named, selectable sidebar color.
type paletteColor struct {
	name    string
	r, g, b uint8
}

// hostPalette is a set of distinct colors that stay readable on the dark panel.
// It serves both as the auto-derivation pool (chosen deterministically from a
// HostID) and as the choices offered by the in-app color picker.
var hostPalette = []paletteColor{
	{"blue", 86, 182, 255},
	{"green", 95, 215, 135},
	{"orange", 255, 175, 95},
	{"purple", 215, 135, 255},
	{"red", 255, 135, 135},
	{"cyan", 95, 215, 215},
	{"yellow", 255, 215, 95},
	{"pink", 255, 135, 175},
}

// autoHostColor maps a device's HostID to a stable palette color, used when the
// device has no user-chosen override.
func autoHostColor(hostID string) (r, g, b uint8) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(hostID))
	c := hostPalette[h.Sum32()%uint32(len(hostPalette))]
	return c.r, c.g, c.b
}

// parseHexColor parses a "#rrggbb" string. ok is false for any malformed input,
// letting callers fall back to the auto-derived color.
func parseHexColor(s string) (r, g, b uint8, ok bool) {
	if len(s) != 7 || s[0] != '#' {
		return 0, 0, 0, false
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return uint8(v >> 16), uint8(v >> 8), uint8(v), true
}

// hexColor formats an RGB triple as "#rrggbb" for storage in the registry.
func hexColor(r, g, b uint8) string {
	return fmt.Sprintf("#%02x%02x%02x", r, g, b)
}

// brighten lifts a channel toward white so the active row's tint reads brighter
// than its resting form without losing the device's hue.
func brighten(c uint8) uint8 {
	return c + (255-c)/2
}
// focusStyle is a hovered (non-active) row: lighter background, brightened
// device tint so the hover still reads as that device.
func focusStyle(r, g, b uint8) layout.Style {
	var s layout.Style
	s.SetBackground(44, 44, 44)
	s.SetForeground(brighten(r), brighten(g), brighten(b))
	return s
}

// pressStyle is a row mid-click: brightest background, device tint kept.
func pressStyle(r, g, b uint8) layout.Style {
	var s layout.Style
	s.SetBackground(72, 72, 72)
	s.SetForeground(brighten(r), brighten(g), brighten(b))
	return s
}

// header styles drive the collapse toggle: it reads as part of the panel rather
// than a raised button.
func headerStyle() layout.Style {
	var s layout.Style
	s.SetBackground(24, 24, 24)
	s.SetForeground(150, 150, 150)
	return s
}
func headerFocusStyle() layout.Style {
	var s layout.Style
	s.SetBackground(38, 38, 38)
	s.SetForeground(220, 220, 220)
	return s
}
func headerPressStyle() layout.Style {
	var s layout.Style
	s.SetBackground(58, 58, 58)
	s.SetForeground(255, 255, 255)
	return s
}
