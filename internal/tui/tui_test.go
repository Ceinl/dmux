package tui

import (
	"context"
	"sync"
	"testing"

	"dmux/internal/attach"
	"dmux/internal/config"
	"dmux/internal/project"
	"dmux/internal/registry"
	"dmux/internal/remote"
	"dmux/internal/session"
)

// --- overlay (pure) ---------------------------------------------------------

func TestOverlayRefilterAndMove(t *testing.T) {
	ov := &overlay{entries: []entry{
		{label: "alpha"}, {label: "beta"}, {label: "gamma"}, {label: "alabama"},
	}}
	ov.refilter()
	if len(ov.filtered) != 4 {
		t.Fatalf("empty query filtered %d, want 4", len(ov.filtered))
	}

	ov.query = "al"
	ov.refilter()
	// "alpha" and "alabama" contain a→l subsequence; "beta"/"gamma" should not.
	if len(ov.filtered) < 2 {
		t.Errorf("query 'al' matched %d, want >=2", len(ov.filtered))
	}
	for _, idx := range ov.filtered {
		lbl := ov.entries[idx].label
		if lbl == "beta" || lbl == "gamma" {
			t.Errorf("unexpected match %q for query 'al'", lbl)
		}
	}

	// move clamps within bounds.
	ov.sel = 0
	ov.move(-1)
	if ov.sel != 0 {
		t.Errorf("move(-1) at top → %d, want 0", ov.sel)
	}
	ov.move(100)
	if ov.sel != len(ov.filtered)-1 {
		t.Errorf("move(100) → %d, want last %d", ov.sel, len(ov.filtered)-1)
	}
}

// --- fakes ------------------------------------------------------------------

type fakeConn struct {
	in     chan []byte
	mu     sync.Mutex
	out    []byte
	closed bool
	resize chan remote.Size
}

func newFakeConn() *fakeConn {
	return &fakeConn{in: make(chan []byte, 16), resize: make(chan remote.Size, 8)}
}
func (c *fakeConn) ClientID() attach.ClientID { return "client-1" }
func (c *fakeConn) Interface() string         { return "test" }
func (c *fakeConn) Read(p []byte) (int, error) {
	b, ok := <-c.in
	if !ok {
		return 0, errClosed
	}
	return copy(p, b), nil
}
func (c *fakeConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.out = append(c.out, p...)
	return len(p), nil
}
func (c *fakeConn) Resizes() <-chan remote.Size { return c.resize }
func (c *fakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.in)
	}
	return nil
}
func (c *fakeConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

type errStr string

func (e errStr) Error() string { return string(e) }

const errClosed = errStr("closed")

// fakeSessions implements just the session.Manager surface the TUI touches.
type fakeSessions struct {
	mu      sync.Mutex
	written map[session.ID][]byte
	resized map[session.ID]remote.Size
	running []session.Session
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{written: map[session.ID][]byte{}, resized: map[session.ID]remote.Size{}}
}
func (f *fakeSessions) List() []session.Session { return f.running }
func (f *fakeSessions) Get(id session.ID) (session.Session, bool) {
	for _, s := range f.running {
		if s.ID == id {
			return s, true
		}
	}
	return session.Session{}, false
}
func (f *fakeSessions) Find(project.Key) (session.ID, bool)     { return "", false }
func (f *fakeSessions) Create(session.Spec) (session.ID, error) { return "", nil }
func (f *fakeSessions) Close(session.ID) error                  { return nil }
func (f *fakeSessions) CloseHost(registry.HostID) []session.ID  { return nil }
func (f *fakeSessions) Write(id session.ID, p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written[id] = append(f.written[id], p...)
	return len(p), nil
}
func (f *fakeSessions) Resize(id session.ID, sz remote.Size) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resized[id] = sz
	return nil
}
func (f *fakeSessions) Scrollback(session.ID) (session.Scrollback, bool) { return nil, false }
func (f *fakeSessions) Subscribe(session.ID) (<-chan session.Event, func(), error) {
	ch := make(chan session.Event)
	return ch, func() {}, nil
}
func (f *fakeSessions) wrote(id session.ID) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.written[id]...)
}

type fakeCtrl struct {
	jumped   session.ID
	projects []project.Project
}

func (c *fakeCtrl) Jump(_ attach.ClientID, target session.ID) error { c.jumped = target; return nil }
func (c *fakeCtrl) Projects(context.Context) ([]project.Project, error) {
	return c.projects, nil
}
func (c *fakeCtrl) OpenProject(context.Context, attach.ClientID, project.Project) (session.ID, error) {
	return "opened", nil
}

func newUI(t *testing.T, sess *fakeSessions, ctrl Controller) (*connUI, *fakeConn) {
	t.Helper()
	reg := registry.NewFileRegistry(t.TempDir())
	_ = reg.Load()
	clients := attach.NewAttachments()
	cfg := config.Default()
	prefix, _ := config.ParsePrefix(cfg.PrefixKey)
	tui := New(ctrl, sess, clients, reg, cfg, prefix)
	conn := newFakeConn()
	u := &connUI{t: tui, ctx: context.Background(), conn: conn, id: conn.ClientID(), size: remote.Size{Rows: 24, Cols: 80}}
	_ = clients.Attach(attach.Client{ID: u.id, Size: u.size})
	return u, conn
}

// --- input routing ----------------------------------------------------------

func TestStreamPassthroughToSession(t *testing.T) {
	sess := newFakeSessions()
	u, _ := newUI(t, sess, &fakeCtrl{})
	u.viewing = "s1"

	u.feed([]byte("ls\n"))

	if got := string(sess.wrote("s1")); got != "ls\n" {
		t.Errorf("session received %q, want %q", got, "ls\n")
	}
}

func TestPrefixThenLiteralPrefix(t *testing.T) {
	sess := newFakeSessions()
	u, _ := newUI(t, sess, &fakeCtrl{})
	u.viewing = "s1"

	// prefix then prefix again → one literal prefix byte to the session.
	pfx := u.t.prefix
	u.feed([]byte{pfx, pfx})
	if got := sess.wrote("s1"); len(got) != 1 || got[0] != pfx {
		t.Errorf("literal prefix passthrough = %v, want [%d]", got, pfx)
	}
}

func TestPrefixDetach(t *testing.T) {
	sess := newFakeSessions()
	u, conn := newUI(t, sess, &fakeCtrl{})
	u.viewing = "s1"

	u.feed([]byte{u.t.prefix, 'd'}) // prefix + d → detach
	if !conn.isClosed() {
		t.Error("prefix+d did not close the connection")
	}
}

func TestPrefixOpensListOverlay(t *testing.T) {
	sess := newFakeSessions()
	sess.running = []session.Session{
		{ID: "s1", HostID: "hA", State: session.StateRunning, Title: "app"},
	}
	ctrl := &fakeCtrl{}
	u, _ := newUI(t, sess, ctrl)
	u.viewing = "s1"

	u.feed([]byte{u.t.prefix, 'l'}) // prefix + l → list overlay
	u.mu.Lock()
	m := u.mode
	hasOverlay := u.overlay != nil
	u.mu.Unlock()
	if m != modeOverlay || !hasOverlay {
		t.Fatalf("mode=%v overlay=%v, want overlay open", m, hasOverlay)
	}

	// Enter selects the only entry → Jump to s1.
	u.feed([]byte{'\r'})
	if ctrl.jumped != "s1" {
		t.Errorf("jumped to %q, want s1", ctrl.jumped)
	}
}

func TestPickerSelectOpensProject(t *testing.T) {
	sess := newFakeSessions()
	ctrl := &fakeCtrl{projects: []project.Project{{HostID: "hA", Root: "/code/app", Name: "app"}}}
	u, _ := newUI(t, sess, ctrl)

	u.feed([]byte{u.t.prefix, 'p'}) // prefix + p → picker
	u.mu.Lock()
	open := u.mode == modeOverlay && u.overlay != nil
	u.mu.Unlock()
	if !open {
		t.Fatal("picker did not open")
	}
	// Select the project; OpenProject returns "opened" and view switches.
	u.feed([]byte{'\r'})
	if u.currentView() != "opened" {
		t.Errorf("view = %q, want opened", u.currentView())
	}
}
