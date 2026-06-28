package server

import (
	"context"
	"errors"
	"testing"

	"github.com/Ceinl/dmux/internal/attach"
	"github.com/Ceinl/dmux/internal/config"
	"github.com/Ceinl/dmux/internal/project"
	"github.com/Ceinl/dmux/internal/registry"
	"github.com/Ceinl/dmux/internal/remote"
	"github.com/Ceinl/dmux/internal/session"
)

// --- fakes ------------------------------------------------------------------

type fakeSessions struct {
	created      []session.Spec
	byKey        map[project.Key]session.ID
	states       map[session.ID]session.State
	closed       []session.ID
	resizes      map[session.ID]remote.Size
	nextID       int
	createErr    error
	closeHostIDs []session.ID
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{
		byKey:   map[project.Key]session.ID{},
		states:  map[session.ID]session.State{},
		resizes: map[session.ID]remote.Size{},
	}
}

func (f *fakeSessions) List() []session.Session {
	var out []session.Session
	for id, st := range f.states {
		out = append(out, session.Session{ID: id, State: st})
	}
	return out
}
func (f *fakeSessions) Get(id session.ID) (session.Session, bool) {
	st, ok := f.states[id]
	return session.Session{ID: id, State: st}, ok
}
func (f *fakeSessions) Find(k project.Key) (session.ID, bool) { id, ok := f.byKey[k]; return id, ok }
func (f *fakeSessions) Create(spec session.Spec) (session.ID, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	f.created = append(f.created, spec)
	f.nextID++
	id := session.ID(string(rune('a' - 1 + f.nextID)))
	f.states[id] = session.StateRunning
	f.byKey[project.Key{HostID: spec.HostID, Root: spec.Root}] = id
	return id, nil
}
func (f *fakeSessions) Close(id session.ID) error {
	f.closed = append(f.closed, id)
	delete(f.states, id)
	return nil
}
func (f *fakeSessions) CloseHost(registry.HostID) []session.ID {
	for _, id := range f.closeHostIDs {
		delete(f.states, id)
	}
	return f.closeHostIDs
}
func (f *fakeSessions) Write(session.ID, []byte) (int, error) { return 0, nil }
func (f *fakeSessions) Rename(session.ID, string) error       { return nil }
func (f *fakeSessions) Resize(id session.ID, sz remote.Size) error {
	f.resizes[id] = sz
	return nil
}
func (f *fakeSessions) Scrollback(session.ID) (session.Scrollback, bool) { return nil, false }
func (f *fakeSessions) Subscribe(session.ID) (<-chan session.Event, func(), error) {
	return nil, func() {}, nil
}

type fakeDialer struct{ verifyErr error }

func (d *fakeDialer) Open(context.Context, registry.Host, remote.OpenSpec) (remote.PTY, error) {
	return nil, nil
}
func (d *fakeDialer) Verify(context.Context, registry.Host) error { return d.verifyErr }

type fakeIndexer struct {
	out map[registry.HostID][]project.Project
	err error
}

func (i *fakeIndexer) Index(_ context.Context, h registry.Host) ([]project.Project, error) {
	if i.err != nil {
		return nil, i.err
	}
	return i.out[h.ID], nil
}

func newServer(t *testing.T) (*Server, *fakeSessions, registry.Registry, *fakeIndexer, *fakeDialer) {
	t.Helper()
	reg := registry.NewFileRegistry(t.TempDir())
	if err := reg.Load(); err != nil {
		t.Fatal(err)
	}
	sessions := newFakeSessions()
	clients := attach.NewAttachments()
	idx := &fakeIndexer{out: map[registry.HostID][]project.Project{}}
	dialer := &fakeDialer{}
	srv := New(config.Default(), reg, sessions, clients, dialer, idx, nil)
	return srv, sessions, reg, idx, dialer
}

// --- tests ------------------------------------------------------------------

func TestConnectVerifyThenAdd(t *testing.T) {
	srv, _, reg, _, dialer := newServer(t)
	h := registry.Host{Addr: "h:22", User: "u"}

	if err := srv.Connect(context.Background(), h); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	hosts := reg.List()
	if len(hosts) != 1 {
		t.Fatalf("registry has %d hosts, want 1", len(hosts))
	}
	if hosts[0].Status != registry.StatusUp {
		t.Errorf("status = %d, want Up", hosts[0].Status)
	}

	// Verify failure → not added.
	dialer.verifyErr = errors.New("no ssh")
	if err := srv.Connect(context.Background(), registry.Host{Addr: "x:22", User: "u"}); err == nil {
		t.Error("expected Connect error on verify failure")
	}
	if len(reg.List()) != 1 {
		t.Error("host added despite verify failure")
	}
}

func TestOpenProjectCreatesThenDedupes(t *testing.T) {
	srv, sessions, _, _, _ := newServer(t)
	clients := attach.NewAttachments()
	// Re-wire server with our own clients we can inspect.
	srv.clients = clients
	_ = clients.Attach(attach.Client{ID: "c1", Size: remote.Size{Rows: 30, Cols: 100}})

	p := project.Project{HostID: "h1", Root: "/code/app", Name: "app"}

	id1, err := srv.OpenProject(context.Background(), "c1", p)
	if err != nil {
		t.Fatalf("OpenProject create: %v", err)
	}
	if len(sessions.created) != 1 {
		t.Fatalf("created %d sessions, want 1", len(sessions.created))
	}
	if sessions.created[0].Root != "/code/app" {
		t.Errorf("create root = %q, want /code/app", sessions.created[0].Root)
	}

	// Second open of same (host, root) → no new session, jumps to existing.
	id2, err := srv.OpenProject(context.Background(), "c1", p)
	if err != nil {
		t.Fatalf("OpenProject dedupe: %v", err)
	}
	if id1 != id2 {
		t.Errorf("dedupe returned %q, want existing %q", id2, id1)
	}
	if len(sessions.created) != 1 {
		t.Errorf("created %d sessions on 2nd open, want still 1", len(sessions.created))
	}

	// Client now views the session.
	c, _ := clients.Get("c1")
	if c.Viewing != id1 {
		t.Errorf("client viewing %q, want %q", c.Viewing, id1)
	}
}

func TestJumpValidates(t *testing.T) {
	srv, sessions, _, _, _ := newServer(t)
	clients := attach.NewAttachments()
	srv.clients = clients
	_ = clients.Attach(attach.Client{ID: "c1", Size: remote.Size{Rows: 24, Cols: 80}})

	if err := srv.Jump("c1", "ghost"); err == nil {
		t.Error("expected error jumping to unknown session")
	}

	sessions.states["live"] = session.StateRunning
	if err := srv.Jump("c1", "live"); err != nil {
		t.Fatalf("Jump live: %v", err)
	}
	if sessions.resizes["live"] != (remote.Size{Rows: 24, Cols: 80}) {
		t.Errorf("jump did not renegotiate size: %v", sessions.resizes["live"])
	}
}

func TestProjectsSkipsDownAndDedupes(t *testing.T) {
	srv, _, reg, idx, _ := newServer(t)
	_ = reg.Add(registry.Host{ID: "up1", Addr: "a:22", User: "u", Status: registry.StatusUp})
	_ = reg.Add(registry.Host{ID: "down1", Addr: "b:22", User: "u"})
	_ = reg.SetStatus("down1", registry.StatusDown)

	// Need IDs to match what Add derived; fetch them.
	var upID, downID registry.HostID
	for _, h := range reg.List() {
		if h.Status == registry.StatusDown {
			downID = h.ID
		} else {
			upID = h.ID
		}
	}
	idx.out[upID] = []project.Project{
		{HostID: upID, Root: "/x", Name: "x"},
		{HostID: upID, Root: "/x", Name: "x"}, // dup
	}
	idx.out[downID] = []project.Project{{HostID: downID, Root: "/y", Name: "y"}}

	got, err := srv.Projects(context.Background())
	if err != nil {
		t.Fatalf("Projects: %v", err)
	}
	if len(got) != 1 || got[0].Root != "/x" {
		t.Errorf("Projects = %+v, want single /x (down host skipped, dup removed)", got)
	}
}

func TestOnHostDownMarksDownAndMovesViewers(t *testing.T) {
	reg := registry.NewFileRegistry(t.TempDir())
	_ = reg.Load()
	_ = reg.Add(registry.Host{ID: "hA", Addr: "a:22", User: "u", Status: registry.StatusUp})
	hostID := reg.List()[0].ID

	sessions := newFakeSessions()
	// downed is on the dead host; survivor is elsewhere and stays running.
	sessions.states["downed"] = session.StateRunning
	sessions.states["survivor"] = session.StateRunning
	sessions.closeHostIDs = []session.ID{"downed"}

	clients := attach.NewAttachments()
	_ = clients.Attach(attach.Client{ID: "c1", Viewing: "downed", Size: remote.Size{Rows: 24, Cols: 80}})

	srv := New(config.Default(), reg, sessions, clients, &fakeDialer{}, &fakeIndexer{}, nil)
	srv.onHostDown(hostID)

	if h, _ := reg.Get(hostID); h.Status != registry.StatusDown {
		t.Errorf("host status = %d, want Down", h.Status)
	}
	c, _ := clients.Get("c1")
	if c.Viewing != "survivor" {
		t.Errorf("client moved to %q, want survivor", c.Viewing)
	}
}
