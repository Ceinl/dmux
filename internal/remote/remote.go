// Package remote owns the outbound SSH to hosts. Each session is exactly one
// `ssh server→host -t` with a PTY allocated on the host (SPEC key decision #2).
// The server holds these connections open and multiplexes them.
package remote

import (
	"context"
	"io"

	"dmux/internal/registry"
)

// Size is a PTY window size in character cells.
type Size struct {
	Rows uint16
	Cols uint16
}

// OpenSpec describes the PTY to allocate on the host.
type OpenSpec struct {
	// Cwd is the directory to cd into before starting the shell. Empty (the
	// default) starts the session at the device root — the host login dir.
	// Only the project picker sets it, to the selected project root.
	Cwd string
	// Size is the initial PTY window size.
	Size Size
}

// PTY is one live SSH PTY on a host: a bidirectional byte stream plus resize
// and lifecycle control. Reads yield host output; writes deliver keystrokes.
type PTY interface {
	io.ReadWriteCloser
	// Resize renegotiates the host-side window size.
	Resize(Size) error
	// Done is closed when the underlying SSH connection drops (host down).
	Done() <-chan struct{}
}

// Dialer establishes and tears down outbound SSH PTYs to hosts. One Dialer is
// shared by the whole server; it presents the server's key (registry KeyRef).
type Dialer interface {
	// Open dials the host and allocates a PTY. Returns an error if SSH fails;
	// the caller marks the host Down on failure.
	Open(ctx context.Context, h registry.Host, spec OpenSpec) (PTY, error)

	// Verify checks that SSH + key trust to a host works, without leaving
	// anything running. Backs `dmux connect`.
	Verify(ctx context.Context, h registry.Host) error
}
