package registry

import (
	"errors"
	"testing"
)

func sampleHost() Host {
	return Host{
		Addr:   "10.0.0.5:22",
		User:   "dima",
		KeyRef: "id_dmux",
		HomeConfig: HomeConfig{
			Root:      "/home/dima/code",
			ScanDepth: 3,
		},
	}
}

func TestAddDerivesIDAndPersists(t *testing.T) {
	dir := t.TempDir()
	r := NewFileRegistry(dir)
	if err := r.Load(); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(sampleHost()); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got := r.List()
	if len(got) != 1 {
		t.Fatalf("List len = %d, want 1", len(got))
	}
	if got[0].ID == "" {
		t.Error("ID not derived on Add")
	}

	// Reload from disk into a fresh registry → round-trip.
	r2 := NewFileRegistry(dir)
	if err := r2.Load(); err != nil {
		t.Fatal(err)
	}
	if len(r2.List()) != 1 {
		t.Fatalf("reloaded List len = %d, want 1", len(r2.List()))
	}
	if _, ok := r2.Get(got[0].ID); !ok {
		t.Error("host missing after reload")
	}
}

func TestAddDuplicateRejected(t *testing.T) {
	dir := t.TempDir()
	r := NewFileRegistry(dir)
	_ = r.Load()
	if err := r.Add(sampleHost()); err != nil {
		t.Fatal(err)
	}
	err := r.Add(sampleHost()) // same user@addr → same derived ID
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("Add duplicate err = %v, want ErrDuplicate", err)
	}
}

func TestSetStatusPreservesHomeConfig(t *testing.T) {
	dir := t.TempDir()
	r := NewFileRegistry(dir)
	_ = r.Load()
	_ = r.Add(sampleHost())
	id := r.List()[0].ID

	if err := r.SetStatus(id, StatusDown); err != nil {
		t.Fatal(err)
	}
	h, _ := r.Get(id)
	if h.Status != StatusDown {
		t.Errorf("Status = %d, want Down", h.Status)
	}
	if h.Root != "/home/dima/code" || h.ScanDepth != 3 {
		t.Errorf("HomeConfig clobbered by SetStatus: %+v", h.HomeConfig)
	}
	if h.LastSeen.IsZero() {
		t.Error("LastSeen not updated by SetStatus")
	}
}

func TestSetHomePreservesStatus(t *testing.T) {
	dir := t.TempDir()
	r := NewFileRegistry(dir)
	_ = r.Load()
	_ = r.Add(sampleHost())
	id := r.List()[0].ID
	_ = r.SetStatus(id, StatusUp)

	if err := r.SetHome(id, HomeConfig{Root: "/srv", ScanDepth: 1}); err != nil {
		t.Fatal(err)
	}
	h, _ := r.Get(id)
	if h.Status != StatusUp {
		t.Errorf("Status clobbered by SetHome: %d", h.Status)
	}
	if h.Root != "/srv" || h.ScanDepth != 1 {
		t.Errorf("HomeConfig not updated: %+v", h.HomeConfig)
	}
}

func TestListReturnsCopies(t *testing.T) {
	dir := t.TempDir()
	r := NewFileRegistry(dir)
	_ = r.Load()
	_ = r.Add(sampleHost())

	list := r.List()
	list[0].Addr = "mutated:22" // mutate the returned copy

	h, _ := r.Get(list[0].ID)
	if h.Addr == "mutated:22" {
		t.Error("List returned a reference into the store; mutation leaked")
	}
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	r := NewFileRegistry(dir)
	_ = r.Load()
	_ = r.Add(sampleHost())
	id := r.List()[0].ID

	if err := r.Remove(id); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := r.Get(id); ok {
		t.Error("host still present after Remove")
	}
	if err := r.Remove(id); !errors.Is(err, ErrNotFound) {
		t.Errorf("Remove absent err = %v, want ErrNotFound", err)
	}
}
