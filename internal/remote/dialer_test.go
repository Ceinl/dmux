package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Ceinl/dmux/internal/registry"
)

// newSigner makes a throwaway ed25519 ssh.Signer.
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

// testServer is a minimal in-process SSH server that, for each "shell"/"exec"
// request, echoes whatever the client writes back to it.
type testServer struct {
	ln       net.Listener
	mu       sync.Mutex
	hostKey  ssh.Signer
	clientCA ssh.PublicKey // accepted client key
}

func (ts *testServer) currentHostKey() ssh.Signer {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.hostKey
}

func (ts *testServer) rotateHostKey(s ssh.Signer) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.hostKey = s
}

func startTestServer(t *testing.T, clientKey ssh.PublicKey) *testServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ts := &testServer{ln: ln, hostKey: newSigner(t), clientCA: clientKey}
	go ts.serve(t)
	t.Cleanup(func() { ln.Close() })
	return ts
}

func (ts *testServer) addr() string { return ts.ln.Addr().String() }

func (ts *testServer) serve(t *testing.T) {
	for {
		nc, err := ts.ln.Accept()
		if err != nil {
			return
		}
		go ts.handleConn(nc)
	}
}

func (ts *testServer) handleConn(nc net.Conn) {
	// Build the config per connection so a rotated host key takes effect.
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(ts.clientCA.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, io.EOF
		},
	}
	cfg.AddHostKey(ts.currentHostKey())

	conn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		nc.Close()
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(ssh.UnknownChannelType, "only session")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range chReqs {
				switch req.Type {
				case "pty-req", "shell", "window-change":
					if req.WantReply {
						req.Reply(true, nil)
					}
				case "exec":
					if req.WantReply {
						req.Reply(true, nil)
					}
					// Emulate a successful command: send exit-status 0, close.
					ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
					ch.Close()
				default:
					if req.WantReply {
						req.Reply(false, nil)
					}
				}
			}
		}()
		// Echo loop: pump input back to output.
		go func() {
			io.Copy(ch, ch)
			ch.Close()
		}()
	}
}

func testHost(addr string) registry.Host {
	return registry.Host{ID: "t", Addr: addr, User: "tester"}
}

func TestOpenReadWriteEcho(t *testing.T) {
	signer := newSigner(t)
	ts := startTestServer(t, signer.PublicKey())
	d := NewDialer(t.TempDir(), 3*time.Second, signer)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pty, err := d.Open(ctx, testHost(ts.addr()), OpenSpec{Size: Size{Rows: 24, Cols: 80}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pty.Close()

	msg := []byte("hello dmux\n")
	if _, err := pty.Write(msg); err != nil {
		t.Fatalf("Write: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(pty, buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf) != string(msg) {
		t.Errorf("echo = %q, want %q", buf, msg)
	}

	// Resize should not error.
	if err := pty.Resize(Size{Rows: 40, Cols: 120}); err != nil {
		t.Errorf("Resize: %v", err)
	}
}

func TestDoneClosesOnRemoteExit(t *testing.T) {
	signer := newSigner(t)
	ts := startTestServer(t, signer.PublicKey())
	d := NewDialer(t.TempDir(), 3*time.Second, signer)

	pty, err := d.Open(context.Background(), testHost(ts.addr()), OpenSpec{Size: Size{Rows: 24, Cols: 80}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Closing our side ends the session; Done must fire.
	pty.Close()
	select {
	case <-pty.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done not closed after Close")
	}
}

func TestVerifyOKAndReject(t *testing.T) {
	good := newSigner(t)
	ts := startTestServer(t, good.PublicKey())

	dOK := NewDialer(t.TempDir(), 3*time.Second, good)
	if err := dOK.Verify(context.Background(), testHost(ts.addr())); err != nil {
		t.Errorf("Verify(good) = %v, want nil", err)
	}

	bad := newSigner(t) // not the accepted key
	dBad := NewDialer(t.TempDir(), 3*time.Second, bad)
	if err := dBad.Verify(context.Background(), testHost(ts.addr())); err == nil {
		t.Error("Verify(bad) = nil, want auth error")
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"/home/x":        "'/home/x'",
		"/a b/c":         "'/a b/c'",
		"/it's/here":     `'/it'\''s/here'`,
		"/x;rm -rf/safe": "'/x;rm -rf/safe'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTOFUHostKeyMismatch(t *testing.T) {
	signer := newSigner(t)
	ts := startTestServer(t, signer.PublicKey())
	dir := t.TempDir()
	d := NewDialer(dir, 3*time.Second, signer)

	// First Verify pins the host key.
	if err := d.Verify(context.Background(), testHost(ts.addr())); err != nil {
		t.Fatalf("first Verify: %v", err)
	}
	// Swap the server's host key behind the same address → mismatch must reject.
	ts.rotateHostKey(newSigner(t))
	// New connections from the existing listener use the new host key via a
	// fresh ServerConfig per connection, so re-dial:
	err := d.Verify(context.Background(), testHost(ts.addr()))
	if err == nil {
		t.Error("expected host key mismatch error after key rotation")
	}
}
