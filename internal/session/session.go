// Package session manages live sessions in server memory. A session is one SSH
// PTY on a host (SPEC). Sessions survive interface disconnects but not server
// restart (the SSH connections drop). The server keeps a per-session scrollback
// buffer even with no interface attached.
package session

import (
	"time"

	"dmux/internal/project"
	"dmux/internal/registry"
	"dmux/internal/remote"
)

// ID uniquely identifies a session within the server.
type ID string

// State is the lifecycle of a session's underlying SSH PTY.
type State int

const (
	StateStarting State = iota
	StateRunning
	StateClosed // closed cleanly (or because its host went down)
	StateFailed // never reached a running PTY
)

// Spec is the request to create a session.
type Spec struct {
	HostID registry.HostID
	// Root is the project root to cd into; pairs with HostID as the session's
	// (host, project_root) identity. Empty (the default) starts at the device
	// root — only the project picker sets it.
	Root  string
	Title string
	Size  remote.Size
}

// Session is the server-side record of one live SSH PTY.
type Session struct {
	ID      ID
	HostID  registry.HostID
	Project project.Key // (host, project_root) matching identity
	Title   string
	State   State
	Size    remote.Size // current negotiated size
	Created time.Time
}

// Scrollback is a bounded ring buffer of recent session output, retained so a
// reattaching interface sees history, not just new output.
type Scrollback interface {
	// Snapshot returns the buffered bytes for replay on attach.
	Snapshot() []byte
	// Len is the number of bytes currently buffered.
	Len() int
}

// Event notifies subscribers of session output and lifecycle changes.
type Event struct {
	Data    []byte // host output, if any
	State   State  // current state
	Stopped bool   // true once the session has ended
}

// Manager owns all sessions in memory: creation, lookup, the (host, root)
// dedupe used by the project picker, and PTY I/O fan-in/fan-out.
type Manager interface {
	List() []Session
	Get(id ID) (Session, bool)

	// Find returns the existing session for a (host, project_root) pair, if
	// any — the picker switches to it instead of opening a duplicate.
	Find(key project.Key) (ID, bool)

	// Create opens a new SSH PTY for spec and starts buffering scrollback.
	Create(spec Spec) (ID, error)
	// Close ends a session and drops its SSH connection.
	Close(id ID) error
	// CloseHost closes every session on a host that has gone down.
	CloseHost(id registry.HostID) []ID

	// Write delivers arbitrated keystrokes to the session's PTY.
	Write(id ID, p []byte) (int, error)
	// Resize sets the session's negotiated PTY size.
	Resize(id ID, size remote.Size) error

	// Scrollback exposes a session's retained output buffer.
	Scrollback(id ID) (Scrollback, bool)

	// Subscribe streams output + lifecycle events for a session. The returned
	// cancel stops the subscription.
	Subscribe(id ID) (events <-chan Event, cancel func(), err error)
}
