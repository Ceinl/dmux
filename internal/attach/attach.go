// Package attach tracks clients (interfaces) attached to the TUI and which
// session each is viewing. Multiple interfaces can attach at once and share a
// view (SPEC). This is where shared resize is negotiated and where input from
// many clients is arbitrated onto one PTY.
package attach

import (
	"dmux/internal/config"
	"dmux/internal/remote"
	"dmux/internal/session"
)

// ClientID uniquely identifies an attached interface.
type ClientID string

// Client is one attached interface. It holds no session state of its own; it
// just points at the session it's currently viewing and its own window size.
type Client struct {
	ID        ClientID
	Interface string      // human label / source address of the interface
	Viewing   session.ID  // the session this client currently sees
	Size      remote.Size // this client's own terminal size
}

// Attachments tracks all attached clients and drives shared-view behaviour.
type Attachments interface {
	List() []Client
	Get(id ClientID) (Client, bool)

	// Attach registers a newly connected interface.
	Attach(c Client) error
	// Detach removes a client; sessions live on if others remain (SPEC).
	Detach(id ClientID) error

	// SetViewing moves a client to a different session (device jump).
	SetViewing(id ClientID, s session.ID) error
	// SetSize records a client's terminal size (feeds resize negotiation).
	SetSize(id ClientID, size remote.Size) error

	// NegotiatedSize computes a session's PTY size from its viewers under the
	// configured ResizePolicy (smallest-wins by default).
	NegotiatedSize(s session.ID, policy config.ResizePolicy) (remote.Size, bool)
}
