// Package registry is the on-disk host registry. Hosts survive both interface
// disconnects and server restarts (SPEC persistence table). A host runs nothing
// itself — the registry only records how to SSH into it and how to index it.
package registry

import "time"

// HostID uniquely identifies a host within the registry.
type HostID string

// Status reflects the server's last-known reachability of a host. A host that
// drops mid-session is marked Down but kept visible in the sidebar.
type Status int

const (
	StatusUnknown Status = iota
	StatusUp
	StatusDown
)

// Host is a single registered machine: an address the server can SSH into,
// the key to use, and the project-indexing config set via `dmux sethome`.
type Host struct {
	ID   HostID
	Addr string // host:port the server dials
	User string // remote login user

	// KeyRef names the private key (in DataDir) the server presents to this
	// host. Trust is established at registration, ssh-copy-id style.
	KeyRef string

	// HomeConfig is the `dmux sethome` project-finder anchor, composed in
	// rather than duplicated.
	HomeConfig

	// Color is an optional user-chosen sidebar color for this device, stored as
	// a "#rrggbb" hex string. Empty means the TUI derives a color automatically
	// from the host ID.
	Color string

	Status   Status
	LastSeen time.Time
}

// HomeConfig is the `dmux sethome` payload. It is the anchor the server indexes
// for the prefix+p fuzzy project finder, and how deep to scan from it. It is
// ONLY a search root — never a session start dir. New sessions always start at
// the device's own root.
type HomeConfig struct {
	Root      string // project-finder index anchor on the host
	ScanDepth int
}

// Registry is the persistent store of hosts. Implementations load from and
// flush to DataDir; all reads return copies so callers can't mutate state.
type Registry interface {
	// Load (re)reads the registry from disk; called on server start.
	Load() error
	// Save flushes the current registry to disk.
	Save() error

	List() []Host
	Get(id HostID) (Host, bool)

	// Add registers a new host (after `dmux connect` verifies SSH works).
	Add(h Host) error
	// Remove deletes a host from the registry.
	Remove(id HostID) error

	// SetHome updates a host's project-indexing config (`dmux sethome`).
	SetHome(id HostID, cfg HomeConfig) error
	// SetColor sets a host's sidebar color ("#rrggbb"); "" clears the override
	// and restores the auto-derived color.
	SetColor(id HostID, color string) error
	// SetStatus updates reachability without touching other fields.
	SetStatus(id HostID, s Status) error
}
