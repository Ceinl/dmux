// Package server wires the pieces together: the on-disk registry, the in-memory
// session manager + attachments, the outbound dialer, the project indexer, and
// the inbound SSH server. It owns startup, the host-down reaction, and the
// actions the TUI invokes (jump, project switch).
//
// Data model (SPEC):
//
//	Server
//	 ├── Registry (disk)      hosts
//	 ├── Sessions (memory)    live SSH PTYs
//	 ├── Attachments (memory) clients ──▶ session
//	 ├── Dialer               outbound SSH to hosts
//	 ├── Indexer              prefix+p project discovery
//	 └── sshd.Server          inbound interface attach
package server

import (
	"context"
	"fmt"

	"dmux/internal/attach"
	"dmux/internal/config"
	"dmux/internal/project"
	"dmux/internal/registry"
	"dmux/internal/remote"
	"dmux/internal/session"
	"dmux/internal/sshd"
)

// hostDownHooker is the optional capability (implemented by the concrete session
// manager) the server uses to learn about unexpected host drops without widening
// the session.Manager interface (M9.1, see M4 impl notes).
type hostDownHooker interface {
	SetHostDownHook(func(registry.HostID))
}

// Server is the always-on brain. One per machine; the only thing that runs.
type Server struct {
	cfg config.Config

	hosts    registry.Registry
	sessions session.Manager
	clients  attach.Attachments
	dialer   remote.Dialer
	indexer  project.Indexer
	inbound  sshd.Server

	// handler is the TUI; set via SetHandler before Run. Kept as the narrow
	// sshd.Handler so the server has no dependency on the tui package (the TUI
	// depends on the server, not vice-versa — this breaks the import cycle).
	handler sshd.Handler
}

// New constructs a Server from its config and collaborators. If sessions
// supports SetHostDownHook (the concrete manager does), the host-down cascade is
// wired here.
func New(
	cfg config.Config,
	hosts registry.Registry,
	sessions session.Manager,
	clients attach.Attachments,
	dialer remote.Dialer,
	indexer project.Indexer,
	inbound sshd.Server,
) *Server {
	s := &Server{
		cfg:      cfg,
		hosts:    hosts,
		sessions: sessions,
		clients:  clients,
		dialer:   dialer,
		indexer:  indexer,
		inbound:  inbound,
	}
	if h, ok := sessions.(hostDownHooker); ok {
		h.SetHostDownHook(s.onHostDown)
	}
	return s
}

// SetHandler installs the TUI handler that drives each inbound interface. Call
// before Run. (New can't take it: the TUI needs the constructed Server as its
// controller, so wiring happens after New — see M9 impl notes.)
func (s *Server) SetHandler(h sshd.Handler) { s.handler = h }

// Run loads the registry, starts the inbound SSH server, and blocks until ctx
// is cancelled (M9.2).
func (s *Server) Run(ctx context.Context) error {
	if s.handler == nil {
		return fmt.Errorf("server: no handler set (call SetHandler before Run)")
	}
	if err := s.hosts.Load(); err != nil {
		return fmt.Errorf("server: load registry: %w", err)
	}

	errc := make(chan error, 1)
	go func() { errc <- s.inbound.Serve(ctx, s.handler) }()

	select {
	case <-ctx.Done():
		_ = s.inbound.Close()
		// Close every live session so SSH connections drop cleanly.
		for _, sess := range s.sessions.List() {
			_ = s.sessions.Close(sess.ID)
		}
		<-errc // wait for Serve to return
		return nil
	case err := <-errc:
		return err
	}
}

// --- Host lifecycle ---------------------------------------------------------

// Connect performs the one-shot `dmux connect` registration: verify SSH + key
// trust to a host, then add it to the registry (M9.3).
func (s *Server) Connect(ctx context.Context, h registry.Host) error {
	if err := s.dialer.Verify(ctx, h); err != nil {
		return fmt.Errorf("server: connect verify: %w", err)
	}
	if err := s.hosts.Add(h); err != nil {
		return fmt.Errorf("server: connect add: %w", err)
	}
	// Mark reachable now that SSH + key trust is proven.
	added := s.findByAddr(h.Addr, h.User)
	if added != "" {
		_ = s.hosts.SetStatus(added, registry.StatusUp)
	}
	return nil
}

// findByAddr returns the ID of the host matching user@addr, or "".
func (s *Server) findByAddr(addr, user string) registry.HostID {
	for _, h := range s.hosts.List() {
		if h.Addr == addr && h.User == user {
			return h.ID
		}
	}
	return ""
}

// SetHome backs `dmux sethome`: stores a host's project root + scan depth (M9.4).
func (s *Server) SetHome(id registry.HostID, cfg registry.HomeConfig) error {
	return s.hosts.SetHome(id, cfg)
}

// onHostDown closes the host's sessions, marks it Down (kept visible), and
// auto-moves any attached interface to another open session (M9.5, SPEC).
func (s *Server) onHostDown(id registry.HostID) {
	closed := s.sessions.CloseHost(id)
	_ = s.hosts.SetStatus(id, registry.StatusDown)

	closedSet := make(map[session.ID]bool, len(closed))
	for _, sid := range closed {
		closedSet[sid] = true
	}

	for _, c := range s.clients.List() {
		if !closedSet[c.Viewing] {
			continue
		}
		if target, ok := s.anyOpenSession(); ok {
			_ = s.clients.SetViewing(c.ID, target)
			s.renegotiate(target)
		}
		// If no session remains, leave the client on its now-dead view; the TUI
		// renders an empty state until the user jumps elsewhere.
	}
}

// anyOpenSession returns some currently-running session, if one exists.
func (s *Server) anyOpenSession() (session.ID, bool) {
	for _, sess := range s.sessions.List() {
		if sess.State == session.StateRunning {
			return sess.ID, true
		}
	}
	return "", false
}

// --- TUI actions ------------------------------------------------------------

// Jump moves a client's view to an existing session (sidebar device jump) (M9.7).
func (s *Server) Jump(c attach.ClientID, target session.ID) error {
	sess, ok := s.sessions.Get(target)
	if !ok {
		return fmt.Errorf("server: jump to unknown session %s", target)
	}
	if sess.State != session.StateRunning {
		return fmt.Errorf("server: jump to non-running session %s", target)
	}
	if err := s.clients.SetViewing(c, target); err != nil {
		return err
	}
	s.renegotiate(target)
	return nil
}

// renegotiate recomputes a session's PTY size from its current viewers and
// applies it (M9.7/M9 resize hook).
func (s *Server) renegotiate(target session.ID) {
	if sz, ok := s.clients.NegotiatedSize(target, s.cfg.ResizePolicy); ok {
		_ = s.sessions.Resize(target, sz)
	}
}

// Projects indexes every up host on demand and returns the picker entries,
// deduped by (host, root) (M9.8). Per-host errors are skipped, not fatal.
func (s *Server) Projects(ctx context.Context) ([]project.Project, error) {
	seen := make(map[project.Key]struct{})
	var out []project.Project
	for _, h := range s.hosts.List() {
		if h.Status == registry.StatusDown {
			continue
		}
		projs, err := s.indexer.Index(ctx, h)
		if err != nil {
			continue // skip this host; don't fail the whole picker
		}
		for _, p := range projs {
			k := project.KeyOf(p)
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, p)
		}
	}
	return out, nil
}

// OpenProject selects a project: switch to the existing session for (host, root)
// if one exists, else open a new SSH session cd'd to root (M9.9).
func (s *Server) OpenProject(ctx context.Context, c attach.ClientID, p project.Project) (session.ID, error) {
	key := project.KeyOf(p)
	if id, ok := s.sessions.Find(key); ok {
		if err := s.Jump(c, id); err != nil {
			return "", err
		}
		return id, nil
	}

	size := s.viewerSize(c)
	id, err := s.sessions.Create(session.Spec{
		HostID: p.HostID,
		Root:   p.Root,
		Title:  p.Name,
		Size:   size,
	})
	if err != nil {
		return "", fmt.Errorf("server: open project: %w", err)
	}
	if err := s.Jump(c, id); err != nil {
		return id, err
	}
	return id, nil
}

// viewerSize returns the requesting client's terminal size, or a sane default.
func (s *Server) viewerSize(c attach.ClientID) remote.Size {
	if cl, ok := s.clients.Get(c); ok && cl.Size.Rows > 0 && cl.Size.Cols > 0 {
		return cl.Size
	}
	return remote.Size{Rows: 24, Cols: 80}
}
