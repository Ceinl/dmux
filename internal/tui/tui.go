// Package tui is the dmux terminal UI. It renders into each inbound SSH
// connection (sshd.Conn) — not the local terminal — so it drives the Plumtree
// tui-runtime's layout/screen/components directly instead of using that
// runtime's stdin-bound App loop.
//
// Layout: a Row with a vt10x-emulated main pane (grows) on the left and a
// right-hand sidebar listing every session as a clickable button, plus a
// collapse toggle at the top that shrinks the sidebar to a 1-column sliver.
// The main pane goes through a terminal emulator so the host's escape codes
// stay contained to its rectangle and never stomp the sidebar.
package tui

import (
	"context"
	"sync"

	"github.com/Ceinl/plumtree/tui-runtime/components"
	"github.com/Ceinl/plumtree/tui-runtime/keyboard"
	"github.com/Ceinl/plumtree/tui-runtime/layout"
	"github.com/Ceinl/plumtree/tui-runtime/screen"

	"dmux/internal/attach"
	"dmux/internal/config"
	"dmux/internal/project"
	"dmux/internal/registry"
	"dmux/internal/remote"
	"dmux/internal/session"
	"dmux/internal/sshd"
)

// Controller is the narrow slice of the server the TUI invokes. Declaring it
// here keeps the dependency one-way (tui ← server), avoiding an import cycle.
type Controller interface {
	Jump(c attach.ClientID, target session.ID) error
	Projects(ctx context.Context) ([]project.Project, error)
	OpenProject(ctx context.Context, c attach.ClientID, p project.Project) (session.ID, error)
}

// TUI builds a per-connection handler from shared server collaborators.
type TUI struct {
	ctrl     Controller
	sessions session.Manager
	clients  attach.Attachments
	hosts    registry.Registry
	cfg      config.Config
	prefix   byte
}

// New constructs the TUI handler. prefix is the parsed prefix key.
func New(ctrl Controller, sessions session.Manager, clients attach.Attachments, hosts registry.Registry, cfg config.Config, prefix byte) *TUI {
	return &TUI{ctrl: ctrl, sessions: sessions, clients: clients, hosts: hosts, cfg: cfg, prefix: prefix}
}

// Compile-time check that *TUI satisfies sshd.Handler.
var _ sshd.Handler = (*TUI)(nil)

type mode int

const (
	modeStream  mode = iota // keystrokes pass through to the active session
	modePrefix              // prefix seen; next key is a command
	modeOverlay             // an overlay (picker) owns the screen + input
)

// Terminal setup/teardown sent to the interface: alt screen + SGR mouse.
const (
	enterTerm = "\x1b[?1049h\x1b[?1000h\x1b[?1006h\x1b[2J"
	exitTerm  = "\x1b[?1000l\x1b[?1006l\x1b[?25h\x1b[?1049l"
)

// connUI is the live state for one attached interface.
type connUI struct {
	t    *TUI
	ctx  context.Context
	conn sshd.Conn
	id   attach.ClientID

	scr     *screen.Screen
	root    *components.Div
	pane    *pane
	sidebar *sidebar

	mu        sync.Mutex
	mode      mode
	viewing   session.ID
	cancelSub func()
	size      remote.Size
	overlay   *overlay
	redraw    chan struct{}
}

// Handle attaches the client and drives the UI until the connection closes.
func (t *TUI) Handle(ctx context.Context, conn sshd.Conn) {
	u := &connUI{
		t:      t,
		ctx:    ctx,
		conn:   conn,
		id:     conn.ClientID(),
		size:   remote.Size{Rows: 24, Cols: 80},
		redraw: make(chan struct{}, 1),
	}
	if sz := firstSize(conn); sz.Rows > 0 {
		u.size = sz
	}

	if err := t.clients.Attach(attach.Client{ID: u.id, Interface: conn.Interface(), Size: u.size}); err != nil {
		return
	}
	defer t.clients.Detach(u.id)
	defer u.stopSub()

	u.conn.Write([]byte(enterTerm))
	// Defers run LIFO: write exitTerm (leave alt screen, restore cursor) while
	// the conn is still open, then close it. Closing first would drop exitTerm
	// and leave the client's terminal stuck in the alt buffer.
	defer u.conn.Close()
	defer u.conn.Write([]byte(exitTerm))

	u.buildTree()
	u.autoAttach()

	events := keyboard.ListenReader(ctx, conn)
	u.render()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if u.handleEvent(ev) {
				return // quit/detach
			}
			u.render()
		case sz := <-conn.Resizes():
			u.handleResize(sz)
			u.render()
		case <-u.redraw:
			u.render()
		}
	}
}

// buildTree assembles the component tree: [ pane(grow) | sidebar(px) ].
func (u *connUI) buildTree() {
	u.scr = screen.NewScreenWithOutput(int(u.size.Cols), int(u.size.Rows), u.conn)
	u.pane = newPane(func(cols, rows int) {
		// Resize the remote PTY to match the main pane.
		if v := u.currentView(); v != "" {
			_ = u.t.sessions.Resize(v, remote.Size{Rows: uint16(rows), Cols: uint16(cols)})
		}
	})
	u.sidebar = newSidebar(
		func() { u.toggleSidebar() },
		func(id session.ID) { u.selectSession(id) },
	)

	u.root = components.NewDiv()
	u.root.SetDirection(layout.Row)
	u.root.AppendChild(u.pane) // non-Div → grows to fill remaining width
	u.root.AppendChild(u.sidebar.div)
}

// signalRedraw asks the main loop to repaint (coalesced, non-blocking).
func (u *connUI) signalRedraw() {
	select {
	case u.redraw <- struct{}{}:
	default:
	}
}

// autoAttach drops the interface into a session: the first running one, else a
// fresh session on the first reachable host at its device root.
func (u *connUI) autoAttach() {
	if id, ok := u.t.firstRunning(); ok {
		u.switchView(id)
		return
	}
	if host, ok := u.t.firstUpHost(); ok {
		p := project.Project{HostID: host.ID, Name: host.Addr}
		if id, err := u.t.ctrl.OpenProject(u.ctx, u.id, p); err == nil {
			u.switchView(id)
		}
	}
}

// switchView points the interface at a session: cancel the old subscription,
// reset the emulator, replay scrollback, then stream live output into the pane.
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

	u.pane.reset()
	if sb, ok := u.t.sessions.Scrollback(id); ok {
		u.pane.feed(sb.Snapshot())
	}
	go u.streamPump(events)
	u.signalRedraw()
}

// streamPump feeds session output into the emulator and triggers repaints.
func (u *connUI) streamPump(events <-chan session.Event) {
	for ev := range events {
		if ev.Stopped {
			u.onViewStopped()
			return
		}
		if len(ev.Data) > 0 {
			u.pane.feed(ev.Data)
			u.signalRedraw()
		}
	}
}

// onViewStopped follows the server's auto-move when the active session ends.
func (u *connUI) onViewStopped() {
	if c, ok := u.t.clients.Get(u.id); ok && c.Viewing != "" && c.Viewing != u.currentView() {
		u.switchView(c.Viewing)
		return
	}
	if id, ok := u.t.firstRunning(); ok {
		u.switchView(id)
		return
	}
	u.mu.Lock()
	u.viewing = ""
	u.mu.Unlock()
	u.signalRedraw()
}

// selectSession is invoked by a sidebar button click.
func (u *connUI) selectSession(id session.ID) {
	if err := u.t.ctrl.Jump(u.id, id); err == nil {
		u.switchView(id)
	}
}

// toggleSidebar collapses/expands the sidebar and forces a full repaint.
func (u *connUI) toggleSidebar() {
	u.sidebar.toggle()
	u.forceFullRepaint()
}

func (u *connUI) stopSub() {
	u.mu.Lock()
	cancel := u.cancelSub
	u.cancelSub = nil
	u.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (u *connUI) currentView() session.ID {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.viewing
}

// firstRunning returns any currently-running session.
func (t *TUI) firstRunning() (session.ID, bool) {
	for _, s := range t.sessions.List() {
		if s.State == session.StateRunning {
			return s.ID, true
		}
	}
	return "", false
}

// firstUpHost returns the first host not known to be down.
func (t *TUI) firstUpHost() (registry.Host, bool) {
	for _, h := range t.hosts.List() {
		if h.Status != registry.StatusDown {
			return h, true
		}
	}
	return registry.Host{}, false
}

// firstSize drains an initial size from the conn's resize channel if present.
func firstSize(conn sshd.Conn) remote.Size {
	select {
	case sz := <-conn.Resizes():
		return sz
	default:
		return remote.Size{}
	}
}
