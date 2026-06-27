package attach

import (
	"errors"
	"fmt"
	"sync"

	"dmux/internal/config"
	"dmux/internal/remote"
	"dmux/internal/session"
)

// ErrNotFound is returned for unknown client IDs.
var ErrNotFound = errors.New("attach: client not found")

// ErrDuplicate is returned by Attach when a client ID is already present.
var ErrDuplicate = errors.New("attach: client already attached")

// attachments is the concrete Attachments. It tracks attached clients and the
// per-session designated driver used by the Driver resize policy (M5.1/M5.3).
type attachments struct {
	mu      sync.RWMutex
	clients map[ClientID]Client
	driver  map[session.ID]ClientID
}

// NewAttachments builds an empty attachment set.
func NewAttachments() *attachments {
	return &attachments{
		clients: make(map[ClientID]Client),
		driver:  make(map[session.ID]ClientID),
	}
}

func (a *attachments) List() []Client {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]Client, 0, len(a.clients))
	for _, c := range a.clients {
		out = append(out, c) // value copy
	}
	return out
}

func (a *attachments) Get(id ClientID) (Client, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	c, ok := a.clients[id]
	return c, ok
}

// Attach registers a newly connected interface (M5.2).
func (a *attachments) Attach(c Client) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c.ID == "" {
		return errors.New("attach: empty client ID")
	}
	if _, ok := a.clients[c.ID]; ok {
		return fmt.Errorf("%w: %s", ErrDuplicate, c.ID)
	}
	a.clients[c.ID] = c
	return nil
}

// Detach removes a client; sessions live on if others remain (M5.2, SPEC).
func (a *attachments) Detach(id ClientID) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.clients[id]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	delete(a.clients, id)
	// Drop any driver designations this client held.
	for s, drv := range a.driver {
		if drv == id {
			delete(a.driver, s)
		}
	}
	return nil
}

// SetViewing moves a client to a different session (device jump) (M5.2).
func (a *attachments) SetViewing(id ClientID, s session.ID) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.clients[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	c.Viewing = s
	a.clients[id] = c
	return nil
}

// SetSize records a client's terminal size (feeds resize negotiation) (M5.2).
func (a *attachments) SetSize(id ClientID, size remote.Size) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.clients[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	c.Size = size
	a.clients[id] = c
	return nil
}

// SetDriver designates the driver client for a session under the Driver resize
// policy. Resolves the SPEC open question: the driver is set explicitly (e.g. by
// the TUI), and NegotiatedSize falls back to smallest-wins if none is set or the
// driver isn't currently viewing the session (M5.3).
func (a *attachments) SetDriver(s session.ID, id ClientID) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.clients[id]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	a.driver[s] = id
	return nil
}

// NegotiatedSize computes a session's PTY size from its viewers under policy
// (M5.3). Returns false when no client is viewing the session.
func (a *attachments) NegotiatedSize(s session.ID, policy config.ResizePolicy) (remote.Size, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if policy == config.Driver {
		if drv, ok := a.driver[s]; ok {
			if c, ok := a.clients[drv]; ok && c.Viewing == s && valid(c.Size) {
				return c.Size, true
			}
		}
		// Fall through to smallest-wins when no usable driver.
	}

	var out remote.Size
	found := false
	for _, c := range a.clients {
		if c.Viewing != s || !valid(c.Size) {
			continue
		}
		if !found {
			out = c.Size
			found = true
			continue
		}
		if c.Size.Rows < out.Rows {
			out.Rows = c.Size.Rows
		}
		if c.Size.Cols < out.Cols {
			out.Cols = c.Size.Cols
		}
	}
	return out, found
}

func valid(s remote.Size) bool { return s.Rows > 0 && s.Cols > 0 }

// Compile-time check that attachments satisfies Attachments.
var _ Attachments = (*attachments)(nil)
