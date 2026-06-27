package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned when a host ID is absent from the registry.
var ErrNotFound = errors.New("registry: host not found")

// ErrDuplicate is returned by Add when a host with the same identity exists.
var ErrDuplicate = errors.New("registry: host already registered")

// registryFileName is the on-disk registry filename under DataDir (M2.1).
const registryFileName = "hosts.json"

// fileRegistry is the JSON-file-backed Registry implementation. All reads return
// copies; Host is a value type so a plain assignment suffices (M2.4).
type fileRegistry struct {
	path  string
	mu    sync.RWMutex
	hosts map[HostID]Host
}

// NewFileRegistry builds a registry persisted at dataDir/hosts.json. Call Load
// before first use to populate it from disk.
func NewFileRegistry(dataDir string) *fileRegistry {
	return &fileRegistry{
		path:  filepath.Join(dataDir, registryFileName),
		hosts: make(map[HostID]Host),
	}
}

// Load (re)reads the registry from disk; called on server start. A missing file
// is not an error — it yields an empty registry (M2.2).
func (r *fileRegistry) Load() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	raw, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		r.hosts = make(map[HostID]Host)
		return nil
	}
	if err != nil {
		return fmt.Errorf("registry: read %s: %w", r.path, err)
	}

	var list []Host
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("registry: parse %s: %w", r.path, err)
	}
	hosts := make(map[HostID]Host, len(list))
	for _, h := range list {
		hosts[h.ID] = h
	}
	r.hosts = hosts
	return nil
}

// Save flushes the current registry to disk via temp-file + rename (M2.3).
// Callers already hold no lock; saveLocked is used when the lock is held.
func (r *fileRegistry) Save() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.saveLocked()
}

// saveLocked marshals and atomically writes the registry. Caller must hold at
// least a read lock.
func (r *fileRegistry) saveLocked() error {
	list := make([]Host, 0, len(r.hosts))
	for _, h := range r.hosts {
		list = append(list, h)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })

	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return fmt.Errorf("registry: marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return fmt.Errorf("registry: mkdir: %w", err)
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("registry: write temp: %w", err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return fmt.Errorf("registry: rename: %w", err)
	}
	return nil
}

// List returns a copy of every host, sorted by ID for stable output (M2.4).
func (r *fileRegistry) List() []Host {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := make([]Host, 0, len(r.hosts))
	for _, h := range r.hosts {
		list = append(list, h) // value copy
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list
}

// Get returns a copy of one host (M2.4).
func (r *fileRegistry) Get(id HostID) (Host, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.hosts[id]
	return h, ok // h is a value copy
}

// Add registers a new host. A blank ID is derived from user@addr; a duplicate
// identity is rejected (M2.5).
func (r *fileRegistry) Add(h Host) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if h.ID == "" {
		h.ID = DeriveHostID(h.User, h.Addr)
	}
	if _, exists := r.hosts[h.ID]; exists {
		return fmt.Errorf("%w: %s", ErrDuplicate, h.ID)
	}
	r.hosts[h.ID] = h
	return r.saveLocked()
}

// Remove deletes a host from the registry (M2.6).
func (r *fileRegistry) Remove(id HostID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.hosts[id]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	delete(r.hosts, id)
	return r.saveLocked()
}

// SetHome updates a host's project-indexing config without touching reachability
// fields (M2.7).
func (r *fileRegistry) SetHome(id HostID, cfg HomeConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.hosts[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	h.HomeConfig = cfg
	r.hosts[id] = h
	return r.saveLocked()
}

// SetStatus updates reachability + LastSeen without touching HomeConfig (M2.7).
func (r *fileRegistry) SetStatus(id HostID, s Status) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.hosts[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	h.Status = s
	h.LastSeen = time.Now()
	r.hosts[id] = h
	return r.saveLocked()
}

// DeriveHostID produces a deterministic ID from a host's login identity, so the
// same user@addr always maps to the same ID (natural dedupe) (M2.8).
func DeriveHostID(user, addr string) HostID {
	sum := sha256.Sum256([]byte(user + "@" + addr))
	return HostID(hex.EncodeToString(sum[:6])) // 12 hex chars
}

// Compile-time check that fileRegistry satisfies Registry.
var _ Registry = (*fileRegistry)(nil)
