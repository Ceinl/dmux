package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Ceinl/dmux/internal/config"
	"github.com/Ceinl/dmux/internal/project"
	"github.com/Ceinl/dmux/internal/registry"
	"github.com/Ceinl/dmux/internal/remote"
)

// ErrNotFound is returned for unknown session IDs.
var ErrNotFound = errors.New("session: not found")

// subBuffer bounds each subscriber's channel. Output beyond it is dropped for
// that slow subscriber (the pump never blocks); full history stays in
// scrollback (M4.3/M4.4).
const subBuffer = 1024

// liveSession is the manager's per-session state (M4.1).
type liveSession struct {
	rec Session
	pty remote.PTY
	sb  *ringScrollback

	mu          sync.Mutex
	subs        map[int]chan Event
	nextSub     int
	closed      bool
	intentional bool // true once Close/CloseHost asked for teardown
}

// manager owns all sessions in memory (M4.1).
type manager struct {
	mu       sync.RWMutex
	dialer   remote.Dialer
	hosts    registry.Registry
	cfg      config.Config
	sessions map[ID]*liveSession
	byKey    map[project.Key]ID

	onHostDown func(registry.HostID)
}

// NewManager builds the session manager. It resolves spec.HostID → registry.Host
// via hosts to dial (the Manager.Create signature carries neither ctx nor Host,
// so those deps live here) (M4.1, resolves the M4.3 signature gap).
func NewManager(dialer remote.Dialer, hosts registry.Registry, cfg config.Config) *manager {
	return &manager{
		dialer:   dialer,
		hosts:    hosts,
		cfg:      cfg,
		sessions: make(map[ID]*liveSession),
		byKey:    make(map[project.Key]ID),
	}
}

// SetHostDownHook registers the server callback fired when a session's host
// drops unexpectedly (M4.1/M9.1).
func (m *manager) SetHostDownHook(fn func(registry.HostID)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onHostDown = fn
}

func (m *manager) List() []Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Session, 0, len(m.sessions))
	for _, ls := range m.sessions {
		out = append(out, ls.snapshotRec())
	}
	// Stable order: map iteration is randomized, so sort by creation time (then
	// ID) to keep the sidebar list from shuffling between renders/clicks.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (m *manager) Get(id ID) (Session, bool) {
	m.mu.RLock()
	ls, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return Session{}, false
	}
	return ls.snapshotRec(), true
}

func (m *manager) Find(key project.Key) (ID, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.byKey[key]
	return id, ok
}

// Create opens a new SSH PTY for spec and starts buffering scrollback (M4.3).
func (m *manager) Create(spec Spec) (ID, error) {
	host, ok := m.hosts.Get(spec.HostID)
	if !ok {
		return "", fmt.Errorf("session: unknown host %s", spec.HostID)
	}

	id := newID()
	key := project.Key{HostID: spec.HostID, Root: spec.Root}
	rec := Session{
		ID:      id,
		HostID:  spec.HostID,
		Project: key,
		Title:   spec.Title,
		State:   StateStarting,
		Size:    spec.Size,
		Created: time.Now(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.DialTimeout)
	defer cancel()
	pty, err := m.dialer.Open(ctx, host, remote.OpenSpec{Cwd: spec.Root, Size: spec.Size})
	if err != nil {
		rec.State = StateFailed
		return "", fmt.Errorf("session: open: %w", err)
	}
	rec.State = StateRunning

	ls := &liveSession{
		rec:  rec,
		pty:  pty,
		sb:   newRingScrollback(m.cfg.ScrollbackLines),
		subs: make(map[int]chan Event),
	}

	m.mu.Lock()
	m.sessions[id] = ls
	m.byKey[key] = id
	m.mu.Unlock()

	go m.pump(ls)
	return id, nil
}

// pump reads host output into scrollback + subscribers and watches for the PTY
// dropping (M4.4).
func (m *manager) pump(ls *liveSession) {
	buf := make([]byte, 32*1024)
	readErr := make(chan struct{})
	go func() {
		for {
			n, err := ls.pty.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				ls.sb.write(data)
				ls.broadcast(Event{Data: data, State: StateRunning})
			}
			if err != nil {
				close(readErr)
				return
			}
		}
	}()

	select {
	case <-ls.pty.Done():
	case <-readErr:
	}

	ls.mu.Lock()
	intentional := ls.intentional
	ls.mu.Unlock()

	m.finalize(ls, intentional)
}

// finalize tears a session down once: marks closed, emits Stopped, closes subs,
// removes it from the maps, and (for unexpected drops) fires the host-down hook.
func (m *manager) finalize(ls *liveSession, intentional bool) {
	ls.mu.Lock()
	if ls.closed {
		ls.mu.Unlock()
		return
	}
	ls.closed = true
	ls.rec.State = StateClosed
	hostID := ls.rec.HostID
	subs := ls.subs
	ls.subs = make(map[int]chan Event)
	ls.mu.Unlock()

	for _, ch := range subs {
		ch <- Event{State: StateClosed, Stopped: true}
		close(ch)
	}
	_ = ls.pty.Close()

	m.mu.Lock()
	delete(m.sessions, ls.rec.ID)
	delete(m.byKey, ls.rec.Project)
	hook := m.onHostDown
	m.mu.Unlock()

	if !intentional && hook != nil {
		hook(hostID)
	}
}

func (m *manager) Close(id ID) error {
	m.mu.RLock()
	ls, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	ls.mu.Lock()
	ls.intentional = true
	ls.mu.Unlock()
	_ = ls.pty.Close() // triggers Done → pump → finalize(intentional)
	return nil
}

// CloseHost closes every session on a host that has gone down (M4.6).
func (m *manager) CloseHost(hostID registry.HostID) []ID {
	m.mu.RLock()
	var targets []*liveSession
	for _, ls := range m.sessions {
		if ls.rec.HostID == hostID {
			targets = append(targets, ls)
		}
	}
	m.mu.RUnlock()

	ids := make([]ID, 0, len(targets))
	for _, ls := range targets {
		ls.mu.Lock()
		ls.intentional = true
		id := ls.rec.ID
		ls.mu.Unlock()
		ids = append(ids, id)
		_ = ls.pty.Close()
	}
	return ids
}

func (m *manager) Write(id ID, p []byte) (int, error) {
	ls, err := m.lookup(id)
	if err != nil {
		return 0, err
	}
	return ls.pty.Write(p)
}

func (m *manager) Resize(id ID, size remote.Size) error {
	ls, err := m.lookup(id)
	if err != nil {
		return err
	}
	ls.mu.Lock()
	ls.rec.Size = size
	ls.mu.Unlock()
	return ls.pty.Resize(size)
}

func (m *manager) Scrollback(id ID) (Scrollback, bool) {
	ls, err := m.lookup(id)
	if err != nil {
		return nil, false
	}
	return ls.sb, true
}

// Subscribe streams output + lifecycle events for a session (M4.7).
func (m *manager) Subscribe(id ID) (<-chan Event, func(), error) {
	ls, err := m.lookup(id)
	if err != nil {
		// Already gone: hand back a channel that reports the closed state.
		ch := make(chan Event, 1)
		ch <- Event{State: StateClosed, Stopped: true}
		close(ch)
		return ch, func() {}, nil
	}

	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.closed {
		ch := make(chan Event, 1)
		ch <- Event{State: StateClosed, Stopped: true}
		close(ch)
		return ch, func() {}, nil
	}

	subID := ls.nextSub
	ls.nextSub++
	ch := make(chan Event, subBuffer)
	ls.subs[subID] = ch

	cancel := func() {
		ls.mu.Lock()
		defer ls.mu.Unlock()
		if c, ok := ls.subs[subID]; ok {
			delete(ls.subs, subID)
			close(c)
		}
	}
	return ch, cancel, nil
}

func (m *manager) lookup(id ID) (*liveSession, error) {
	m.mu.RLock()
	ls, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return ls, nil
}

// broadcast fans an event out to every subscriber without blocking the pump:
// a full (slow) subscriber's event is dropped — full history remains in
// scrollback (M4.4).
func (ls *liveSession) broadcast(ev Event) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	for _, ch := range ls.subs {
		select {
		case ch <- ev:
		default: // slow subscriber: drop
		}
	}
}

func (ls *liveSession) snapshotRec() Session {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.rec
}

// Rename sets a session's display title.
func (m *manager) Rename(id ID, title string) error {
	m.mu.RLock()
	ls, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return ErrNotFound
	}
	ls.mu.Lock()
	ls.rec.Title = title
	ls.mu.Unlock()
	return nil
}

func newID() ID {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return ID(hex.EncodeToString(b[:]))
}

// Compile-time check that manager satisfies Manager.
var _ Manager = (*manager)(nil)
