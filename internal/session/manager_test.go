package session

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/Ceinl/dmux/internal/config"
	"github.com/Ceinl/dmux/internal/project"
	"github.com/Ceinl/dmux/internal/registry"
	"github.com/Ceinl/dmux/internal/remote"
)

// fakePTY is an in-memory PTY: Write feeds a pipe whose other end Read returns,
// and the test can drop the connection to fire Done.
type fakePTY struct {
	pr   *io.PipeReader
	pw   *io.PipeWriter
	in   chan []byte
	done chan struct{}
	once sync.Once
}

func newFakePTY() *fakePTY {
	pr, pw := io.Pipe()
	return &fakePTY{pr: pr, pw: pw, in: make(chan []byte, 16), done: make(chan struct{})}
}

func (f *fakePTY) Read(p []byte) (int, error)  { return f.pr.Read(p) }
func (f *fakePTY) Write(p []byte) (int, error) { return len(p), nil }
func (f *fakePTY) Resize(remote.Size) error    { return nil }
func (f *fakePTY) Done() <-chan struct{}       { return f.done }
func (f *fakePTY) Close() error {
	f.once.Do(func() {
		close(f.done)
		f.pw.CloseWithError(io.EOF)
	})
	return nil
}

// emit pushes host output to readers.
func (f *fakePTY) emit(s string) { f.pw.Write([]byte(s)) }

// drop simulates the host vanishing.
func (f *fakePTY) drop() { f.Close() }

type fakeDialer struct {
	mu   sync.Mutex
	last *fakePTY
}

func (d *fakeDialer) Open(_ context.Context, _ registry.Host, _ remote.OpenSpec) (remote.PTY, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := newFakePTY()
	d.last = p
	return p, nil
}
func (d *fakeDialer) Verify(context.Context, registry.Host) error { return nil }

func newTestManager(t *testing.T) (*manager, *fakeDialer, registry.HostID) {
	t.Helper()
	reg := registry.NewFileRegistry(t.TempDir())
	if err := reg.Load(); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(registry.Host{Addr: "h:22", User: "u"}); err != nil {
		t.Fatal(err)
	}
	hostID := reg.List()[0].ID
	cfg := config.Default()
	cfg.ScrollbackLines = 10
	d := &fakeDialer{}
	return NewManager(d, reg, cfg), d, hostID
}

func TestCreateAndFindDedupe(t *testing.T) {
	m, _, host := newTestManager(t)
	id, err := m.Create(Spec{HostID: host, Root: "/code/app", Title: "app"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, ok := m.Find(project.Key{HostID: host, Root: "/code/app"})
	if !ok || got != id {
		t.Errorf("Find = (%q,%v), want (%q,true)", got, ok, id)
	}
	if s, ok := m.Get(id); !ok || s.State != StateRunning {
		t.Errorf("Get state = %v ok=%v, want Running", s.State, ok)
	}
}

func TestSubscribeFanout(t *testing.T) {
	m, d, host := newTestManager(t)
	id, _ := m.Create(Spec{HostID: host})

	c1, cancel1, _ := m.Subscribe(id)
	c2, cancel2, _ := m.Subscribe(id)
	defer cancel1()
	defer cancel2()

	d.last.emit("xyz")

	for _, ch := range []<-chan Event{c1, c2} {
		select {
		case ev := <-ch:
			if string(ev.Data) != "xyz" {
				t.Errorf("event data = %q, want xyz", ev.Data)
			}
		case <-time.After(time.Second):
			t.Fatal("no event delivered to a subscriber")
		}
	}
}

func TestScrollbackBuffersAndBounds(t *testing.T) {
	m, d, host := newTestManager(t)
	id, _ := m.Create(Spec{HostID: host})

	d.last.emit("hello ")
	d.last.emit("world")
	// Allow the pump to consume.
	time.Sleep(50 * time.Millisecond)

	sb, ok := m.Scrollback(id)
	if !ok {
		t.Fatal("Scrollback not found")
	}
	snap := string(sb.Snapshot())
	if snap != "hello world" {
		t.Errorf("snapshot = %q, want %q", snap, "hello world")
	}

	// cap = ScrollbackLines(10) * 256 = 2560; write more to force trim.
	big := make([]byte, 4000)
	for i := range big {
		big[i] = 'a'
	}
	d.last.emit(string(big))
	time.Sleep(50 * time.Millisecond)
	if sb.Len() > 2560 {
		t.Errorf("scrollback len = %d, want <= 2560", sb.Len())
	}
}

func TestHostDownCascade(t *testing.T) {
	m, d, host := newTestManager(t)

	var gotHost registry.HostID
	done := make(chan struct{})
	var once sync.Once
	m.SetHostDownHook(func(h registry.HostID) {
		gotHost = h
		once.Do(func() { close(done) })
	})

	id, _ := m.Create(Spec{HostID: host})
	sub, _, _ := m.Subscribe(id)

	d.last.drop() // host vanishes unexpectedly

	// Subscriber must see a Stopped event.
	select {
	case ev := <-sub:
		// Could be a data event first; drain until Stopped.
		for !ev.Stopped {
			ev = <-sub
		}
	case <-time.After(time.Second):
		t.Fatal("no Stopped event after drop")
	}

	select {
	case <-done:
		if gotHost != host {
			t.Errorf("host-down hook host = %q, want %q", gotHost, host)
		}
	case <-time.After(time.Second):
		t.Fatal("host-down hook not fired")
	}

	// Session is gone from the manager.
	if _, ok := m.Get(id); ok {
		t.Error("session still present after host down")
	}
}

func TestCloseIsIntentionalNoHostDown(t *testing.T) {
	m, _, host := newTestManager(t)
	hookFired := make(chan struct{}, 1)
	m.SetHostDownHook(func(registry.HostID) { hookFired <- struct{}{} })

	id, _ := m.Create(Spec{HostID: host})
	if err := m.Close(id); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case <-hookFired:
		t.Error("host-down hook fired on intentional Close")
	case <-time.After(200 * time.Millisecond):
	}

	// Close of a now-absent session is a not-found error (idempotent-ish).
	if err := m.Close(id); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Close err = %v, want ErrNotFound", err)
	}
}

func TestCloseHostReturnsIDs(t *testing.T) {
	m, _, host := newTestManager(t)
	id1, _ := m.Create(Spec{HostID: host, Root: "/a"})
	id2, _ := m.Create(Spec{HostID: host, Root: "/b"})

	ids := m.CloseHost(host)
	if len(ids) != 2 {
		t.Fatalf("CloseHost returned %d ids, want 2", len(ids))
	}
	set := map[ID]bool{ids[0]: true, ids[1]: true}
	if !set[id1] || !set[id2] {
		t.Errorf("CloseHost ids = %v, want %v and %v", ids, id1, id2)
	}
}
