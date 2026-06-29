package tui

import (
	"context"
	"testing"

	"github.com/Ceinl/dmux/internal/attach"
	"github.com/Ceinl/dmux/internal/remote"
	"github.com/Ceinl/dmux/internal/session"
)

// TestSwitchViewResizesPTYToPane guards the tmux misalignment fix: a session may
// be created at the full client width (viewerSize), but the pane only renders the
// area left of the sidebar. Switching to a session must push the pane's real size
// to the remote PTY, otherwise full-screen apps (tmux, vim) draw at the wrong
// width and their status bars/right-aligned content land off-pane.
func TestSwitchViewResizesPTYToPane(t *testing.T) {
	sess := newFakeSessions()
	sess.running = []session.Session{{ID: "s1", State: session.StateRunning}}
	u, _ := newUI(t, sess)

	// Simulate the pane having been laid out to (clientWidth - sidebar) x height.
	const paneW, paneH = 174, 50
	u.pane = newPane(func(int, int) {})
	u.pane.Layout(0, 0, paneW, paneH)

	u.switchView("s1")

	got, ok := sess.resizedTo["s1"]
	if !ok {
		t.Fatal("switchView did not resize the session PTY")
	}
	if int(got.Cols) != paneW || int(got.Rows) != paneH {
		t.Errorf("PTY resized to %dx%d, want %dx%d (pane size)",
			got.Cols, got.Rows, paneW, paneH)
	}
}

func TestSecondViewerUsesNegotiatedPaneSize(t *testing.T) {
	sess := newFakeSessions()
	sess.running = []session.Session{{ID: "s1", State: session.StateRunning}}
	u1, _ := newUI(t, sess)

	u1.pane = newPane(func(int, int) {})
	u1.pane.Layout(0, 0, 80, 24)
	u1.switchView("s1")
	if got := sess.resizedTo["s1"]; got != (remote.Size{Rows: 24, Cols: 80}) {
		t.Fatalf("first viewer resized PTY to %v, want 24x80", got)
	}

	const secondClient attach.ClientID = "client-2"
	if err := u1.t.clients.Attach(attach.Client{ID: secondClient, Size: remote.Size{Rows: 24, Cols: 120}}); err != nil {
		t.Fatal(err)
	}
	u2 := &connUI{
		t:      u1.t,
		ctx:    context.Background(),
		id:     secondClient,
		size:   remote.Size{Rows: 24, Cols: 120},
		redraw: make(chan struct{}, 1),
	}
	u2.pane = newPane(func(int, int) {})
	u2.pane.Layout(0, 0, 120, 24)
	u2.switchView("s1")

	if got := sess.resizedTo["s1"]; got != (remote.Size{Rows: 24, Cols: 80}) {
		t.Errorf("second viewer resized PTY to %v, want negotiated 24x80", got)
	}
}
