// Package sshd is the inbound SSH server: `ssh dmux@<server>` lands an interface
// directly in the TUI. It accepts SSH-key-authenticated connections, allocates a
// PTY for the interface's own terminal, and hands each one to the TUI as a
// client. SSH keys only — no password auth (SPEC).
package sshd

import (
	"context"

	"github.com/Ceinl/dmux/internal/attach"
	"github.com/Ceinl/dmux/internal/remote"
)

// Conn is one accepted inbound interface connection: its terminal I/O, resize
// notifications, and identity. The TUI renders into it.
type Conn interface {
	// ClientID is the attachment identity assigned to this connection.
	ClientID() attach.ClientID
	// Interface is a human label / source address for the connection.
	Interface() string

	// Read returns keystrokes from the interface; Write renders TUI output.
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)

	// Resizes streams the interface's own window-size changes.
	Resizes() <-chan remote.Size
	// Close ends the inbound connection.
	Close() error
}

// Handler receives each accepted interface connection. The TUI implements this:
// it attaches the client and drives rendering until Conn closes.
type Handler interface {
	Handle(ctx context.Context, c Conn)
}

// Server is the inbound SSH listener for interfaces.
type Server interface {
	// Serve accepts connections until ctx is cancelled, dispatching each to h.
	Serve(ctx context.Context, h Handler) error
	// Close stops the listener.
	Close() error
}
