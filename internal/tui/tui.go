// Package tui is the hand-rolled ANSI terminal UI that drives each attached
// interface. It implements sshd.Handler: on each inbound connection it attaches
// the client, full-screen proxies the active session's PTY, and overlays a
// device/session list and a telescope-style project picker on the prefix key.
//
// Design note (v1): the SPEC sketches a side-by-side sidebar + main split.
// Faithfully embedding a host's raw PTY byte-stream inside a sub-rectangle would
// require a full vt100 emulator (the stream carries its own cursor/clear escape
// codes addressed to the whole screen). To stay correct without that, v1 renders
// the active session full-screen (pass-through) and surfaces navigation as
// full-screen overlays toggled by the prefix key. The device-as-workspace motion
// the SPEC cares about is preserved; only the simultaneous split is deferred.
package tui

import (
	"context"
	"sync"

	"dmux/internal/attach"
	"dmux/internal/config"
	"dmux/internal/project"
	"dmux/internal/registry"
	"dmux/internal/remote"
	"dmux/internal/session"
	"dmux/internal/sshd"
)

// Controller is the narrow slice of the server the TUI invokes. Declaring it
// here (rather than importing *server.Server) keeps the dependency one-way:
// tui ← server, never the reverse, so there is no import cycle (M6.0).
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

// New constructs the TUI handler. prefix is the parsed prefix key
// (config.ParsePrefix); on error the caller should fall back to Ctrl-Space.
func New(
	ctrl Controller,
	sessions session.Manager,
	clients attach.Attachments,
	hosts registry.Registry,
	cfg config.Config,
	prefix byte,
) *TUI {
	return &TUI{ctrl: ctrl, sessions: sessions, clients: clients, hosts: hosts, cfg: cfg, prefix: prefix}
}

// mode is the per-connection input routing state.
type mode int

const (
	modeStream  mode = iota // keystrokes pass through to the active session
	modePrefix              // prefix seen; next key is a command
	modeOverlay             // an overlay (list/picker) owns the screen + input
)

// connUI is the live state for one attached interface.
type connUI struct {
	t    *TUI
	ctx  context.Context
	conn sshd.Conn
	id   attach.ClientID

	mu        sync.Mutex // guards writes to conn + mode/view/overlay
	mode      mode
	viewing   session.ID
	cancelSub func()
	size      remote.Size

	overlay *overlay // non-nil while mode == modeOverlay
}

// Handle attaches the client and drives the UI until the connection closes
// (M6.1). It satisfies sshd.Handler.
func (t *TUI) Handle(ctx context.Context, conn sshd.Conn) {
	u := &connUI{
		t:    t,
		ctx:  ctx,
		conn: conn,
		id:   conn.ClientID(),
		size: remote.Size{Rows: 24, Cols: 80},
	}

	if err := t.clients.Attach(attach.Client{
		ID:        u.id,
		Interface: conn.Interface(),
		Size:      u.size,
	}); err != nil {
		return
	}
	defer t.clients.Detach(u.id)
	defer u.stopSub()

	go u.resizeLoop()

	u.autoAttach()
	u.inputLoop()
}

// autoAttach drops the interface straight into a session: the first running one
// if any exists, otherwise a fresh session opened on the first reachable host at
// its device root. Only if there is no host to open on do we show the splash.
func (u *connUI) autoAttach() {
	if id, ok := u.t.firstRunning(); ok {
		u.switchView(id)
		return
	}
	if host, ok := u.t.firstUpHost(); ok {
		// Root="" → a login shell at the host's device root (see remote.OpenSpec).
		p := project.Project{HostID: host.ID, Name: host.Addr}
		if id, err := u.t.ctrl.OpenProject(u.ctx, u.id, p); err == nil {
			u.switchView(id)
			return
		}
	}
	u.clear()
	u.writeString(splash)
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

// Compile-time check that *TUI satisfies sshd.Handler.
var _ sshd.Handler = (*TUI)(nil)

const splash = "\x1b[2J\x1b[H" +
	"  dmux — no active session\r\n\r\n" +
	"  press your prefix then:\r\n" +
	"    p  open project picker (across all devices)\r\n" +
	"    l  list sessions / jump\r\n" +
	"    d  detach\r\n"
