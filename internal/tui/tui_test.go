package tui

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/Ceinl/plumtree/tui-runtime/keyboard"
	"github.com/Ceinl/plumtree/tui-runtime/screen"

	"github.com/Ceinl/dmux/internal/attach"
	"github.com/Ceinl/dmux/internal/config"
	"github.com/Ceinl/dmux/internal/project"
	"github.com/Ceinl/dmux/internal/registry"
	"github.com/Ceinl/dmux/internal/remote"
	"github.com/Ceinl/dmux/internal/session"
)

// --- encode (pure) ----------------------------------------------------------

func TestEncode(t *testing.T) {
	cases := []struct {
		ev   keyboard.Event
		want []byte
	}{
		{keyboard.Event{Type: keyboard.KeyRune, Ch: 'a'}, []byte("a")},
		{keyboard.Event{Type: keyboard.KeyRune, Ch: 'D', Ctrl: true}, []byte{0x04}}, // Ctrl-D
		{keyboard.Event{Type: keyboard.KeyEnter}, []byte{'\r'}},
		{keyboard.Event{Type: keyboard.KeyBackspace}, []byte{127}},
		{keyboard.Event{Type: keyboard.KeyArrowUp}, []byte("\x1b[A")},
		{keyboard.Event{Type: keyboard.KeyCtrlC}, []byte{3}},
	}
	for _, c := range cases {
		if got := encode(c.ev); !bytes.Equal(got, c.want) {
			t.Errorf("encode(%+v) = %v, want %v", c.ev, got, c.want)
		}
	}
}

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
	for _, idx := range ov.filtered {
		if l := ov.entries[idx].label; l == "beta" || l == "gamma" {
			t.Errorf("unexpected match %q for 'al'", l)
		}
	}
	ov.sel = 0
	ov.move(-1)
	if ov.sel != 0 {
		t.Errorf("move(-1) at top = %d, want 0", ov.sel)
	}
	ov.move(100)
	if ov.sel != len(ov.filtered)-1 {
		t.Errorf("move(100) = %d, want %d", ov.sel, len(ov.filtered)-1)
	}
}

// --- sidebar ----------------------------------------------------------------

func TestSidebarToggleWidth(t *testing.T) {
	sb := newSidebar(func() {}, func(session.ID) {})
	if sb.width() != sidebarWidth {
		t.Errorf("expanded width = %d, want %d", sb.width(), sidebarWidth)
	}
	sb.toggle()
	if sb.width() != collapsedWidth {
		t.Errorf("collapsed width = %d, want %d", sb.width(), collapsedWidth)
	}
	if sb.collapse.Label() != ">" {
		t.Errorf("collapsed label = %q, want >", sb.collapse.Label())
	}
	sb.toggle()
	if sb.width() != sidebarWidth {
		t.Errorf("re-expanded width = %d, want %d", sb.width(), sidebarWidth)
	}
}

func TestSidebarRebuildClickSelects(t *testing.T) {
	var selected session.ID
	sb := newSidebar(func() {}, func(id session.ID) { selected = id })
	sb.rebuild([]session.Session{
		{ID: "s1", HostID: "hA", State: session.StateRunning, Title: "app"},
		{ID: "s2", HostID: "hB", State: session.StateRunning, Title: "web"},
	}, "s1")
	if len(sb.rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(sb.rows))
	}
	// Lay out so buttons have rects, then click the second one.
	sb.applyWidth()
	sb.div.Layout(0, 0, sidebarWidth, 10)
	r := sb.rows[1]
	// Click center of its button by hit-testing via HandleMouseDown/Up at a
	// point we know is inside (derive from layout by brute force).
	clicked := false
	for y := 0; y < 10 && !clicked; y++ {
		for x := 0; x < sidebarWidth; x++ {
			if r.btn.HitTest(x, y) {
				r.btn.HandleMouseDown(x, y)
				r.btn.HandleMouseUp(x, y)
				clicked = true
				break
			}
		}
	}
	if !clicked {
		t.Fatal("could not locate second session button")
	}
	if selected != "s2" {
		t.Errorf("selected = %q, want s2", selected)
	}
}

// --- input routing ----------------------------------------------------------

type fakeConn struct {
	mu     sync.Mutex
	closed bool
	resize chan remote.Size
}

func newFakeConn() *fakeConn { return &fakeConn{resize: make(chan remote.Size, 8)} }
func (c *fakeConn) ClientID() attach.ClientID   { return "client-1" }
func (c *fakeConn) Interface() string           { return "test" }
func (c *fakeConn) Read(p []byte) (int, error)  { return 0, errClosed }
func (c *fakeConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *fakeConn) Resizes() <-chan remote.Size { return c.resize }
func (c *fakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}
func (c *fakeConn) isClosed() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.closed }

type errStr string

func (e errStr) Error() string { return string(e) }

const errClosed = errStr("closed")

type fakeSessions struct {
	mu      sync.Mutex
	written map[session.ID][]byte
	running []session.Session
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{written: map[session.ID][]byte{}}
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

func (f *fakeSessions) Rename(id session.ID, title string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.running {
		if f.running[i].ID == id {
			f.running[i].Title = title
			return nil
		}
	}
	return nil
}
func (f *fakeSessions) Resize(session.ID, remote.Size) error             { return nil }
func (f *fakeSessions) Scrollback(session.ID) (session.Scrollback, bool) { return nil, false }
func (f *fakeSessions) Subscribe(session.ID) (<-chan session.Event, func(), error) {
	return make(chan session.Event), func() {}, nil
}
func (f *fakeSessions) wrote(id session.ID) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.written[id]...)
}

type fakeCtrl struct{ jumped session.ID }

func (c *fakeCtrl) Jump(_ attach.ClientID, t session.ID) error { c.jumped = t; return nil }
func (c *fakeCtrl) Projects(context.Context) ([]project.Project, error) {
	return nil, nil
}
func (c *fakeCtrl) OpenProject(context.Context, attach.ClientID, project.Project) (session.ID, error) {
	return "opened", nil
}

func newUI(t *testing.T, sess *fakeSessions) (*connUI, *fakeConn) {
	t.Helper()
	reg := registry.NewFileRegistry(t.TempDir())
	_ = reg.Load()
	clients := attach.NewAttachments()
	cfg := config.Default()
	prefix, _ := config.ParsePrefix(cfg.PrefixKey)
	tui := New(&fakeCtrl{}, sess, clients, reg, cfg, prefix)
	conn := newFakeConn()
	u := &connUI{
		t: tui, ctx: context.Background(), conn: conn, id: conn.ClientID(),
		size: remote.Size{Rows: 24, Cols: 80}, redraw: make(chan struct{}, 1),
	}
	u.sidebar = newSidebar(func() {}, func(session.ID) {})
	_ = clients.Attach(attach.Client{ID: u.id, Size: u.size})
	return u, conn
}

func TestStreamForwardsToSession(t *testing.T) {
	sess := newFakeSessions()
	u, _ := newUI(t, sess)
	u.viewing = "s1"

	for _, r := range "ls\r" {
		var ev keyboard.Event
		if r == '\r' {
			ev = keyboard.Event{Type: keyboard.KeyEnter}
		} else {
			ev = keyboard.Event{Type: keyboard.KeyRune, Ch: r}
		}
		u.streamEvent(ev)
	}
	if got := string(sess.wrote("s1")); got != "ls\r" {
		t.Errorf("session got %q, want %q", got, "ls\r")
	}
}

func TestPrefixLiteralAndDetach(t *testing.T) {
	sess := newFakeSessions()
	u, _ := newUI(t, sess)
	u.viewing = "s1"
	ctrlD := keyboard.Event{Type: keyboard.KeyRune, Ch: 'D', Ctrl: true} // Ctrl-D = prefix

	// prefix → prefix again sends one literal prefix byte.
	u.streamEvent(ctrlD)
	u.mu.Lock()
	isPrefix := u.mode == modePrefix
	u.mu.Unlock()
	if !isPrefix {
		t.Fatal("prefix key did not enter prefix mode")
	}
	if quit := u.prefixEvent(ctrlD); quit {
		t.Fatal("literal prefix should not quit")
	}
	if got := sess.wrote("s1"); len(got) != 1 || got[0] != u.t.prefix {
		t.Errorf("literal prefix = %v, want [%d]", got, u.t.prefix)
	}

	// prefix + d → detach: signals quit. The conn is closed by Handle's defer
	// chain (after exitTerm is written), not by prefixEvent itself, so that the
	// alt-screen-leave sequence reaches the client before the socket closes.
	u.streamEvent(ctrlD)
	if quit := u.prefixEvent(keyboard.Event{Type: keyboard.KeyRune, Ch: 'd'}); !quit {
		t.Error("prefix+d should quit")
	}
}

func TestPrefixToggleSidebar(t *testing.T) {
	sess := newFakeSessions()
	u, _ := newUI(t, sess)
	u.scr = screen.NewScreenWithOutput(80, 24, new(bytes.Buffer))
	u.pane = newPane(nil)

	ctrlD := keyboard.Event{Type: keyboard.KeyRune, Ch: 'D', Ctrl: true}
	u.streamEvent(ctrlD)
	u.prefixEvent(keyboard.Event{Type: keyboard.KeyRune, Ch: 'c'}) // collapse
	if !u.sidebar.collapsed {
		t.Error("prefix+c should collapse the sidebar")
	}
}

// --- pane emulator ----------------------------------------------------------

func TestPaneRendersHostOutput(t *testing.T) {
	p := newPane(nil)
	p.Layout(0, 0, 10, 3)
	p.feed([]byte("hi"))

	scr := screen.NewScreenWithOutput(10, 3, new(bytes.Buffer))
	p.Render(scr)
	snap := scr.Snapshot()
	if snap[0][0].Ch != 'h' || snap[0][1].Ch != 'i' {
		t.Errorf("pane cells = %q%q, want 'hi'", snap[0][0].Ch, snap[0][1].Ch)
	}
}

func TestVTColorDefaults(t *testing.T) {
	if s := vtColor(0, true); s == "" {
		// palette index 0 (black) should map to a 256-color escape, not default.
		t.Error("vtColor(0) returned empty (should be palette escape)")
	}
}
