package sshd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Ceinl/dmux/internal/remote"
)

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// recordingHandler captures the Conn it receives and echoes input→output.
type recordingHandler struct {
	got     chan Conn
	resizes chan remote.Size
}

func newRecordingHandler() *recordingHandler {
	return &recordingHandler{got: make(chan Conn, 1), resizes: make(chan remote.Size, 8)}
}

func (h *recordingHandler) Handle(_ context.Context, c Conn) {
	h.got <- c
	go func() {
		for s := range c.Resizes() {
			h.resizes <- s
		}
	}()
	io.Copy(c, c) // echo until the channel closes
}

func startServer(t *testing.T, authorized AuthFunc) (*sshServer, *recordingHandler) {
	t.Helper()
	srv := NewServer("127.0.0.1:0", newSigner(t), authorized)
	h := newRecordingHandler()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.Serve(ctx, h)

	// Wait for the listener to bind.
	deadline := time.Now().Add(2 * time.Second)
	for srv.Addr() == nil {
		if time.Now().After(deadline) {
			t.Fatal("server did not bind")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return srv, h
}

func dialClient(t *testing.T, addr string, key ssh.Signer) (*ssh.Client, error) {
	t.Helper()
	cfg := &ssh.ClientConfig{
		User:            "interface",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(key)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         2 * time.Second,
	}
	return ssh.Dial("tcp", addr, cfg)
}

func allow(key ssh.PublicKey) AuthFunc {
	want := key.Marshal()
	return func(k ssh.PublicKey) bool { return string(k.Marshal()) == string(want) }
}

func TestAuthorizedKeyAccepted(t *testing.T) {
	clientKey := newSigner(t)
	srv, h := startServer(t, allow(clientKey.PublicKey()))

	client, err := dialClient(t, srv.Addr().String(), clientKey)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()
	if err := sess.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatalf("pty: %v", err)
	}
	stdin, _ := sess.StdinPipe()
	stdout, _ := sess.StdoutPipe()
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}

	// Handler must have received a Conn with the interface label set.
	select {
	case c := <-h.got:
		if c.Interface() == "" || c.ClientID() == "" {
			t.Errorf("conn missing identity: iface=%q id=%q", c.Interface(), c.ClientID())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not receive a conn")
	}

	// Initial pty-req size (24x80) should arrive as a resize event.
	select {
	case s := <-h.resizes:
		if s.Rows != 24 || s.Cols != 80 {
			t.Errorf("initial size = %v, want 24x80", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no initial resize from pty-req")
	}

	// Echo round-trip.
	stdin.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(stdout, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "ping" {
		t.Errorf("echo = %q, want ping", buf)
	}

	// window-change → resize event.
	if err := sess.WindowChange(40, 120); err != nil {
		t.Fatalf("window-change: %v", err)
	}
	select {
	case s := <-h.resizes:
		if s.Rows != 40 || s.Cols != 120 {
			t.Errorf("resize = %v, want 40x120", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no resize after window-change")
	}
}

func TestUnauthorizedKeyRejected(t *testing.T) {
	good := newSigner(t)
	srv, _ := startServer(t, allow(good.PublicKey()))

	bad := newSigner(t)
	if _, err := dialClient(t, srv.Addr().String(), bad); err == nil {
		t.Fatal("expected auth failure for unauthorized key")
	}
}

func TestCloseStopsServer(t *testing.T) {
	srv, _ := startServer(t, func(ssh.PublicKey) bool { return true })
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestAuthorizedKeysFileMissing(t *testing.T) {
	fn, ok := AuthorizedKeysFile("/nonexistent/authorized_keys")
	if ok {
		t.Error("expected ok=false for missing file")
	}
	if fn(newSigner(t).PublicKey()) {
		t.Error("missing file should deny all keys")
	}
}
