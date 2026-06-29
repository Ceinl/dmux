package tui

import (
	"fmt"
	"strings"

	"github.com/sahilm/fuzzy"

	"github.com/Ceinl/plumtree/tui-runtime/components"
	"github.com/Ceinl/plumtree/tui-runtime/layout"

	"github.com/Ceinl/dmux/internal/project"
	"github.com/Ceinl/dmux/internal/registry"
	"github.com/Ceinl/dmux/internal/session"
)

// entry is one selectable row in an overlay.
type entry struct {
	label  string
	action func()
	// swatch, when non-nil, draws a colored block before the label (used by the
	// device color picker) as a real component rather than embedded ANSI.
	swatch *[3]uint8
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

	// preview, when non-nil, turns the overlay into a two-pane picker: the list
	// is rendered on the left and this component — the info card for the
	// highlighted entry (indexed into entries) — on the right. Used by the
	// project picker (M6.6).
	preview func(entryIdx int) layout.Component
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
		ov.entries = append(ov.entries, entry{
			label: p.Name,
			action: func() {
				id, err := u.t.ctrl.OpenProject(u.ctx, u.id, p)
				if err == nil {
					u.switchView(id)
				}
			},
		})
	}
	ov.preview = func(i int) layout.Component {
		if i < 0 || i >= len(projs) {
			return nil
		}
		return u.projectDetail(projs[i])
	}
	u.startOverlay(ov)
}

// projectDetail builds the right-pane info card for the highlighted project: its
// name, the machine it lives on (with that device's color swatch), and the
// absolute path on that host. It is a plumtree component column, not raw ANSI,
// so it composes with the rest of the overlay tree.
func (u *connUI) projectDetail(p project.Project) layout.Component {
	r, g, b := u.t.deviceColor(p.HostID)
	addr := ""
	if h, ok := u.t.hosts.Get(p.HostID); ok {
		if h.User != "" {
			addr = h.User + "@"
		}
		addr += h.Addr
	}

	card := components.NewDiv()
	card.SetDirection(layout.Column)
	card.SetSize(grow(), grow())
	card.SetStyle(ovBgStyle())
	card.SetPadding(layout.Padding{Left: px(2)})

	card.AppendChild(textRow(p.Name, ovNameStyle()))
	card.AppendChild(textRow("", ovBgStyle())) // spacer
	card.AppendChild(machineRow(r, g, b, u.t.deviceName(p.HostID)))
	if addr != "" {
		card.AppendChild(kvRow("Address", addr))
	}
	card.AppendChild(kvRow("Path", p.Root))
	return card
}

// kvRow is one "Label   value" line in the info card.
func kvRow(label, value string) *components.Div {
	row := components.NewDiv()
	row.SetDirection(layout.Row)
	row.SetSize(grow(), px(1))
	row.SetStyle(ovBgStyle())
	row.AppendChild(cell(label, kvLabelW, ovLabelStyle()))
	row.AppendChild(cell(value, 0, ovValueStyle())) // grow
	return row
}

// machineRow is the "Machine ▩ <id>" line: a colored swatch (a 2-wide filled
// Div) standing in for the device's sidebar color, then the host id.
func machineRow(r, g, b uint8, id string) *components.Div {
	row := components.NewDiv()
	row.SetDirection(layout.Row)
	row.SetSize(grow(), px(1))
	row.SetStyle(ovBgStyle())
	row.AppendChild(cell("Machine", kvLabelW, ovLabelStyle()))
	row.AppendChild(swatchBox(r, g, b))
	row.AppendChild(cell(" "+id, 0, ovValueStyle()))
	return row
}

// swatchBox is a 2-column filled block in the given color — the component-tree
// equivalent of the old ANSI swatch.
func swatchBox(r, g, b uint8) *components.Div {
	sw := components.NewDiv()
	sw.SetSize(px(2), px(1))
	var ss layout.Style
	ss.SetBackground(r, g, b)
	sw.SetStyle(ss)
	return sw
}

// cell wraps a text segment in a sized box: a px(w)-wide cell, or a growing one
// when w is 0. The text inherits the cell's style so its background fills.
func cell(text string, w int, st layout.Style) *components.Div {
	d := components.NewDiv()
	if w > 0 {
		d.SetSize(px(w), px(1))
	} else {
		d.SetSize(grow(), px(1))
	}
	d.SetStyle(st)
	d.AppendChild(components.NewText(text))
	return d
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

// openDevices lists every device so the user can manage it (prefix+k). Selecting
// a device opens an action menu (color / rename / home dir) for that device.
func (u *connUI) openDevices() {
	ov := &overlay{title: "Devices — Enter to manage, Esc to cancel"}
	for _, h := range u.t.hosts.List() {
		h := h
		r, g, b := u.t.deviceColor(h.ID)
		label := fmt.Sprintf("%-16s %s", u.t.deviceName(h.ID), h.Addr)
		ov.entries = append(ov.entries, entry{
			label:  label,
			swatch: &[3]uint8{r, g, b},
			action: func() { u.openDeviceActions(h.ID) },
		})
	}
	if len(ov.entries) == 0 {
		ov.title = "Devices — none registered (Esc to cancel)"
	}
	u.startOverlay(ov)
}

// openDeviceActions is the per-device action menu: change its sidebar color,
// rename it, or set its project-finder home dir. Every action persists to the
// registry and repaints immediately — no restart required.
func (u *connUI) openDeviceActions(id registry.HostID) {
	r, g, b := u.t.deviceColor(id)
	ov := &overlay{title: fmt.Sprintf("%s — Enter to choose, Esc to cancel", u.t.deviceName(id))}
	ov.entries = []entry{
		{label: "Set color", swatch: &[3]uint8{r, g, b}, action: func() { u.openColorChoices(id) }},
		{label: "Rename", action: func() { u.openRenameDevice(id) }},
		{label: "Set home dir", action: func() { u.openHomeDevice(id) }},
	}
	u.startOverlay(ov)
}

// openRenameDevice prompts for a new display name for one device.
func (u *connUI) openRenameDevice(id registry.HostID) {
	ov := &overlay{title: fmt.Sprintf("Name for %s — Enter to save (empty clears), Esc to cancel", short(id))}
	if h, ok := u.t.hosts.Get(id); ok {
		ov.query = h.Name
	}
	ov.onSubmit = func(name string) {
		_ = u.t.hosts.SetName(id, strings.TrimSpace(name))
		u.signalRedraw()
	}
	u.startOverlay(ov)
}

// openHomeDevice prompts for a device's project-finder home directory, keeping
// its existing scan depth. The change persists and re-indexes happen on the next
// project scan — no restart required.
func (u *connUI) openHomeDevice(id registry.HostID) {
	ov := &overlay{title: fmt.Sprintf("Home dir for %s — Enter to save, Esc to cancel", short(id))}
	if h, ok := u.t.hosts.Get(id); ok {
		ov.query = h.Root
	}
	ov.onSubmit = func(root string) {
		root = strings.TrimSpace(root)
		cfg := registry.HomeConfig{Root: root}
		if h, ok := u.t.hosts.Get(id); ok {
			cfg.ScanDepth = h.ScanDepth
		}
		_ = u.t.hosts.SetHome(id, cfg)
		u.signalRedraw()
	}
	u.startOverlay(ov)
}

// openColorChoices lists the palette (plus "Auto") for one device, persisting
// the pick to the registry so it survives reconnects and restarts.
func (u *connUI) openColorChoices(id registry.HostID) {
	ov := &overlay{title: fmt.Sprintf("Color for %s — Enter to apply, Esc to cancel", short(id))}
	ov.entries = append(ov.entries, entry{
		label:  "Auto (derive from device id)",
		action: func() { u.applyDeviceColor(id, "") },
	})
	for _, c := range hostPalette {
		c := c
		ov.entries = append(ov.entries, entry{
			label:  c.name,
			swatch: &[3]uint8{c.r, c.g, c.b},
			action: func() { u.applyDeviceColor(id, hexColor(c.r, c.g, c.b)) },
		})
	}
	u.startOverlay(ov)
}

// applyDeviceColor persists a device's color and repaints so the sidebar
// reflects it immediately.
func (u *connUI) applyDeviceColor(id registry.HostID, color string) {
	_ = u.t.hosts.SetColor(id, color)
	u.signalRedraw()
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

// renderOverlay paints the overlay by laying out a plumtree component tree onto
// the shared screen — the same path render() uses for the main UI — rather than
// emitting raw ANSI. The tree is: a title line, a query prompt, then either a
// single-column list or, for the project picker, a two-pane [list │ info card].
func (u *connUI) renderOverlay() {
	u.mu.Lock()
	ov := u.overlay
	u.mu.Unlock()
	if ov == nil {
		return
	}

	w, h := u.scr.Width(), u.scr.Height()
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}

	root := components.NewDiv()
	root.SetDirection(layout.Column)
	root.SetStyle(ovBgStyle())
	root.AppendChild(textRow(ov.title, ovTitleStyle()))
	root.AppendChild(textRow("> "+ov.query, ovBgStyle()))

	if ov.onSubmit == nil { // list / picker overlays carry a body; text input does not
		body := h - 2 // rows below title + prompt
		if body < 1 {
			body = 1
		}
		root.AppendChild(u.overlayBody(ov, body))
	}

	root.Layout(0, 0, w, h)
	root.Render(u.scr)
	u.scr.Flush()

	// Park the cursor at the end of the query prompt (row 1).
	cx := 2 + len([]rune(ov.query))
	if cx >= w {
		cx = w - 1
	}
	u.scr.SetCursor(cx, 1)
	u.scr.ShowCursor()
	_ = u.out.Flush()
}

// overlayBody builds the area beneath the prompt: the two-pane picker when a
// preview is set, otherwise a plain single-column list. height is the number of
// list rows that fit.
func (u *connUI) overlayBody(ov *overlay, height int) layout.Component {
	if ov.preview == nil {
		return u.overlayList(ov, height, true)
	}
	body := components.NewDiv()
	body.SetDirection(layout.Row)
	body.SetSize(grow(), grow())
	body.SetStyle(ovBgStyle())

	leftW := overlayLeftWidth(ov, u.scr.Width())
	left := u.overlayList(ov, height, false)
	left.SetSize(px(leftW), grow())
	body.AppendChild(left)
	body.AppendChild(overlayDivider())

	if ov.sel >= 0 && ov.sel < len(ov.filtered) {
		if card := ov.preview(ov.filtered[ov.sel]); card != nil {
			body.AppendChild(card)
		}
	}
	return body
}

// overlayList builds the scrollable entry column. fullWidth makes it grow to
// fill (single-pane overlays); the picker sizes it explicitly instead. The
// window scrolls to keep the selected row visible.
func (u *connUI) overlayList(ov *overlay, height int, fullWidth bool) *components.Div {
	list := components.NewDiv()
	list.SetDirection(layout.Column)
	list.SetStyle(ovBgStyle())
	if fullWidth {
		list.SetSize(grow(), grow())
	}

	if len(ov.filtered) == 0 {
		list.AppendChild(textRow("  (no matches)", ovBgStyle()))
		return list
	}

	top := 0 // scroll offset that keeps ov.sel on screen
	if ov.sel >= height {
		top = ov.sel - height + 1
	}
	for i := top; i < len(ov.filtered) && i < top+height; i++ {
		e := ov.entries[ov.filtered[i]]
		st := ovItemStyle()
		if i == ov.sel {
			st = ovSelStyle()
		}
		list.AppendChild(overlayRow(e, st))
	}
	return list
}

// overlayRow renders one list entry: an optional color swatch followed by the
// label, the whole row painted in style st (so the selected row reads as a bar).
func overlayRow(e entry, st layout.Style) *components.Div {
	if e.swatch == nil {
		return textRow(" "+e.label, st)
	}
	row := components.NewDiv()
	row.SetDirection(layout.Row)
	row.SetSize(grow(), px(1))
	row.SetStyle(st)
	row.AppendChild(cell(" ", 1, st))
	row.AppendChild(swatchBox(e.swatch[0], e.swatch[1], e.swatch[2]))
	row.AppendChild(cell(" "+e.label, 0, st))
	return row
}

// overlayLeftWidth sizes the picker's list pane to the widest visible label
// (plus a little padding), capped at ~40% of the screen.
func overlayLeftWidth(ov *overlay, screenW int) int {
	leftW := 16
	for _, idx := range ov.filtered {
		if l := len(ov.entries[idx].label) + 2; l > leftW {
			leftW = l
		}
	}
	if max := screenW * 40 / 100; leftW > max {
		leftW = max
	}
	return leftW
}

// overlayDivider is the 1-column vertical rule between the two panes.
func overlayDivider() *components.Div {
	d := components.NewDiv()
	d.SetSize(px(1), grow())
	var s layout.Style
	s.SetBackground(60, 60, 60)
	s.SetForeground(60, 60, 60)
	d.SetStyle(s)
	return d
}

// textRow is a full-width, single-row Div holding one line of text in style st.
// The Div paints the background bar; the text inherits the style.
func textRow(s string, st layout.Style) *components.Div {
	row := components.NewDiv()
	row.SetSize(grow(), px(1))
	row.SetStyle(st)
	row.AppendChild(components.NewText(s))
	return row
}

// px and grow are shorthands for the two layout units the overlay tree uses.
func px(n int) layout.Unit { return layout.Unit{Type: layout.UnitPx, Value: float64(n)} }
func grow() layout.Unit    { return layout.Unit{Type: layout.UnitGrow} }

// kvLabelW is the fixed column width of the info card's "Label" gutter.
const kvLabelW = 9

// --- overlay styles ---

func ovBgStyle() layout.Style {
	var s layout.Style
	s.SetBackground(18, 18, 18)
	s.SetForeground(200, 200, 200)
	return s
}
func ovTitleStyle() layout.Style {
	var s layout.Style
	s.SetBackground(18, 18, 18)
	s.SetForeground(150, 150, 150)
	return s
}
func ovItemStyle() layout.Style {
	var s layout.Style
	s.SetBackground(18, 18, 18)
	s.SetForeground(200, 200, 200)
	return s
}
func ovSelStyle() layout.Style {
	var s layout.Style
	s.SetBackground(168, 168, 168)
	s.SetForeground(20, 20, 20)
	return s
}
func ovLabelStyle() layout.Style {
	var s layout.Style
	s.SetBackground(18, 18, 18)
	s.SetForeground(120, 120, 120)
	return s
}
func ovValueStyle() layout.Style {
	var s layout.Style
	s.SetBackground(18, 18, 18)
	s.SetForeground(205, 205, 205)
	return s
}
func ovNameStyle() layout.Style {
	var s layout.Style
	s.SetBackground(18, 18, 18)
	s.SetForeground(235, 235, 235)
	s.AddTextDecoration(layout.Bold)
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
