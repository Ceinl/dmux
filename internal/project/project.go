// Package project handles cross-device project discovery (SPEC: prefix+p).
// A project is identified by its root directory — the git root if the path is
// inside a repo, else the directory itself. A session's identity for matching
// is the pair (host, project_root).
package project

import (
	"context"

	"dmux/internal/registry"
)

// Project is one entry in the cross-host picker.
type Project struct {
	HostID registry.HostID
	Root   string // absolute path on the host; the matching identity
	Name   string // display label (basename of Root)
}

// Key is the (host, project_root) identity used to dedupe sessions.
type Key struct {
	HostID registry.HostID
	Root   string
}

// KeyOf returns the dedupe identity for a project (M8.1).
func KeyOf(p Project) Key { return Key{HostID: p.HostID, Root: p.Root} }

// Indexer walks a host over SSH to enumerate its projects. Indexing is
// on-demand (when the picker opens), never on a background schedule.
type Indexer interface {
	// Index lists projects under the host's HomeConfig.Root, bounded by
	// ScanDepth. This anchor only scopes the finder; it is not where any
	// resulting session starts.
	Index(ctx context.Context, h registry.Host) ([]Project, error)
}
