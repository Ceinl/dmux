package attach

import (
	"errors"
	"testing"

	"dmux/internal/config"
	"dmux/internal/remote"
	"dmux/internal/session"
)

func sz(r, c uint16) remote.Size { return remote.Size{Rows: r, Cols: c} }

func mustAttach(t *testing.T, a *attachments, id ClientID, view session.ID, s remote.Size) {
	t.Helper()
	if err := a.Attach(Client{ID: id, Viewing: view, Size: s}); err != nil {
		t.Fatalf("Attach %s: %v", id, err)
	}
}

func TestAttachDetachDuplicate(t *testing.T) {
	a := NewAttachments()
	mustAttach(t, a, "c1", "s1", sz(24, 80))
	if err := a.Attach(Client{ID: "c1"}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate Attach err = %v, want ErrDuplicate", err)
	}
	if err := a.Detach("c1"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if err := a.Detach("c1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Detach absent err = %v, want ErrNotFound", err)
	}
}

func TestSmallestWins(t *testing.T) {
	a := NewAttachments()
	mustAttach(t, a, "c1", "s1", sz(40, 120))
	mustAttach(t, a, "c2", "s1", sz(24, 100))
	mustAttach(t, a, "c3", "s1", sz(30, 80))
	mustAttach(t, a, "other", "s2", sz(10, 10)) // different session, ignored

	got, ok := a.NegotiatedSize("s1", config.SmallestWins)
	if !ok {
		t.Fatal("NegotiatedSize ok = false, want true")
	}
	if got != sz(24, 80) {
		t.Errorf("NegotiatedSize = %v, want 24x80", got)
	}
}

func TestNegotiatedNoViewers(t *testing.T) {
	a := NewAttachments()
	mustAttach(t, a, "c1", "s1", sz(24, 80))
	if _, ok := a.NegotiatedSize("s2", config.SmallestWins); ok {
		t.Error("expected ok=false for session with no viewers")
	}
}

func TestDetachRecomputes(t *testing.T) {
	a := NewAttachments()
	mustAttach(t, a, "small", "s1", sz(20, 60))
	mustAttach(t, a, "big", "s1", sz(50, 200))

	if got, _ := a.NegotiatedSize("s1", config.SmallestWins); got != sz(20, 60) {
		t.Fatalf("pre-detach = %v, want 20x60", got)
	}
	_ = a.Detach("small")
	if got, _ := a.NegotiatedSize("s1", config.SmallestWins); got != sz(50, 200) {
		t.Errorf("post-detach = %v, want 50x200", got)
	}
}

func TestDriverPolicy(t *testing.T) {
	a := NewAttachments()
	mustAttach(t, a, "drv", "s1", sz(40, 120))
	mustAttach(t, a, "other", "s1", sz(24, 80))

	// Without a driver set, Driver policy falls back to smallest-wins.
	if got, _ := a.NegotiatedSize("s1", config.Driver); got != sz(24, 80) {
		t.Errorf("driver-unset fallback = %v, want 24x80", got)
	}

	if err := a.SetDriver("s1", "drv"); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.NegotiatedSize("s1", config.Driver); got != sz(40, 120) {
		t.Errorf("driver size = %v, want 40x120 (driver's own size)", got)
	}

	// Detaching the driver drops the designation → fallback again.
	_ = a.Detach("drv")
	if got, _ := a.NegotiatedSize("s1", config.Driver); got != sz(24, 80) {
		t.Errorf("post-driver-detach = %v, want 24x80", got)
	}
}

func TestSetViewingAndSize(t *testing.T) {
	a := NewAttachments()
	mustAttach(t, a, "c1", "s1", sz(24, 80))
	if err := a.SetViewing("c1", "s2"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetSize("c1", sz(30, 90)); err != nil {
		t.Fatal(err)
	}
	c, _ := a.Get("c1")
	if c.Viewing != "s2" || c.Size != sz(30, 90) {
		t.Errorf("client = %+v, want Viewing s2 / 30x90", c)
	}
	if err := a.SetViewing("nope", "s1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetViewing absent err = %v, want ErrNotFound", err)
	}
}

func TestListReturnsCopies(t *testing.T) {
	a := NewAttachments()
	mustAttach(t, a, "c1", "s1", sz(24, 80))
	list := a.List()
	list[0].Viewing = "mutated"
	if c, _ := a.Get("c1"); c.Viewing == "mutated" {
		t.Error("List leaked a mutable reference")
	}
}
