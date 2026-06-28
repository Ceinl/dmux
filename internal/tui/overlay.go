package tui

import (
	"fmt"
	"strings"

	"github.com/sahilm/fuzzy"

	"github.com/Ceinl/dmux/internal/registry"
	"github.com/Ceinl/dmux/internal/session"
)

// entry is one selectable row in an overlay.
type entry struct {
	label  string
	action func()
}

// overlay is a full-screen modal list with a fuzzy-filter prompt (M6.6). When
// onSubmit is set it acts as a single-line text input instead: the list is
// hidden and Enter commits the typed query rather than selecting an entry.
type overlay struct {
	title    string
	entries  []entry
	filtered []int // indices into entries, post-filter
	query    string
	sel      int // index into filtered
	escState int // 0 none, 1 saw ESC, 2 saw ESC[

	onSubmit func(string) // non-nil → text-input overlay (e.g. rename)
}

// openPicker builds the cross-device project picker (prefix+p) (M6.6).
func (u *connUI) openPicker() {
	projs, err := u.t.ctrl.Projects(u.ctx)
	ov := &overlay{title: "Projects (all devices) — type to filter, Enter to open, Esc to cancel"}
	if err != nil {
		ov.title = "Projects — error: " + err.Error()
	}
	for _, p := range projs {
		p := p
		label := fmt.Sprintf("%-12s %s", short(p.HostID), p.Root)
		ov.entries = append(ov.entries, entry{
			label: label,
			action: func() {
				id, err := u.t.ctrl.OpenProject(u.ctx, u.id, p)
				if err == nil {
					u.switchView(id)
				}
			},
		})
	}
	u.startOverlay(ov)
}

// openList builds the running-session / device jump list (prefix+l) (M6.4).
func (u *connUI) openList() {
	ov := &overlay{title: "Sessions — Enter to jump, Esc to cancel"}
	for _, s := range u.t.sessions.List() {
		if s.State != session.StateRunning {
			continue
		}
		s := s
		label := fmt.Sprintf("%-12s %s", short(s.HostID), sessionLabel(s))
		ov.entries = append(ov.entries, entry{
			label: label,
			action: func() {
				if err := u.t.ctrl.Jump(u.id, s.ID); err == nil {
					u.switchView(s.ID)
				}
			},
		})
	}
	u.startOverlay(ov)
}

// openRename prompts for a new title for the current session (prefix+r).
func (u *connUI) openRename() {
	id := u.currentView()
	if id == "" {
		return
	}
	ov := &overlay{title: "Rename session — Enter to save, Esc to cancel"}
	if s, ok := u.t.sessions.Get(id); ok {
		ov.query = s.Title
	}
	ov.onSubmit = func(name string) {
		_ = u.t.sessions.Rename(id, strings.TrimSpace(name))
		u.signalRedraw()
	}
	u.startOverlay(ov)
}

func (u *connUI) startOverlay(ov *overlay) {
	ov.refilter()
	u.mu.Lock()
	u.overlay = ov
	u.mode = modeOverlay
	u.mu.Unlock()
	u.renderOverlay()
}

// selectOverlay runs the highlighted entry's action and returns to streaming.
func (u *connUI) selectOverlay() {
	u.mu.Lock()
	ov := u.overlay
	u.mu.Unlock()
	if ov == nil {
		return
	}
	// Text-input overlay (rename): commit the typed query.
	if ov.onSubmit != nil {
		submit, q := ov.onSubmit, ov.query
		u.closeOverlay()
		submit(q)
		return
	}
	var act func()
	if ov.sel >= 0 && ov.sel < len(ov.filtered) {
		act = ov.entries[ov.filtered[ov.sel]].action
	}
	u.closeOverlay()
	if act != nil {
		act()
	}
}

// closeOverlay tears down the overlay and repaints the component tree over the
// raw overlay output.
func (u *connUI) closeOverlay() {
	u.mu.Lock()
	u.overlay = nil
	u.mode = modeStream
	u.mu.Unlock()
	u.forceFullRepaint()
}

// refilter recomputes the filtered index set from the query.
func (o *overlay) refilter() {
	o.filtered = o.filtered[:0]
	if o.query == "" {
		for i := range o.entries {
			o.filtered = append(o.filtered, i)
		}
	} else {
		labels := make([]string, len(o.entries))
		for i, e := range o.entries {
			labels[i] = e.label
		}
		for _, m := range fuzzy.Find(o.query, labels) {
			o.filtered = append(o.filtered, m.Index)
		}
	}
	if o.sel >= len(o.filtered) {
		o.sel = len(o.filtered) - 1
	}
	if o.sel < 0 {
		o.sel = 0
	}
}

func (o *overlay) move(d int) {
	if len(o.filtered) == 0 {
		return
	}
	o.sel += d
	if o.sel < 0 {
		o.sel = 0
	}
	if o.sel >= len(o.filtered) {
		o.sel = len(o.filtered) - 1
	}
}

// renderOverlay paints the overlay full-screen (M6.6).
func (u *connUI) renderOverlay() {
	u.mu.Lock()
	ov := u.overlay
	rows := int(u.size.Rows)
	cols := int(u.size.Cols)
	u.mu.Unlock()
	if ov == nil {
		return
	}
	if rows <= 0 {
		rows = 24
	}
	if cols <= 0 {
		cols = 80
	}

	var sb strings.Builder
	sb.WriteString("\x1b[2J\x1b[H")
	sb.WriteString(clip(ov.title, cols))
	sb.WriteString("\r\n> ")
	sb.WriteString(clip(ov.query, cols-2))
	sb.WriteString("\r\n")

	if ov.onSubmit != nil {
		u.writeString(sb.String())
		return // text-input overlay: prompt only, no list
	}

	maxRows := rows - 3
	if maxRows < 1 {
		maxRows = 1
	}
	for i, idx := range ov.filtered {
		if i >= maxRows {
			break
		}
		line := clip(ov.entries[idx].label, cols)
		if i == ov.sel {
			sb.WriteString("\x1b[7m" + line + "\x1b[0m\r\n") // reverse video
		} else {
			sb.WriteString(line + "\r\n")
		}
	}
	if len(ov.filtered) == 0 {
		sb.WriteString("  (no matches)\r\n")
	}
	u.writeString(sb.String())
}

func clip(s string, n int) string {
	if n < 0 {
		n = 0
	}
	if len(s) > n {
		return s[:n]
	}
	return s
}

func short(id registry.HostID) string {
	s := string(id)
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func sessionLabel(s session.Session) string {
	if s.Title != "" {
		return s.Title
	}
	if s.Project.Root != "" {
		return s.Project.Root
	}
	return string(s.ID)
}
