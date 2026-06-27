package sshd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"

	"golang.org/x/crypto/ssh"

	"dmux/internal/attach"
)

// AuthFunc decides whether an interface's public key may attach. SSH keys only;
// no password auth (SPEC) (M7.2).
type AuthFunc func(ssh.PublicKey) bool

// sshServer is the inbound SSH listener for interfaces (M7.1).
type sshServer struct {
	addr       string
	signer     ssh.Signer
	authorized AuthFunc

	mu sync.Mutex
	ln net.Listener
}

// NewServer builds the inbound server. signer is the server keypair; authorized
// gates which interface pubkeys may attach.
func NewServer(addr string, signer ssh.Signer, authorized AuthFunc) *sshServer {
	return &sshServer{addr: addr, signer: signer, authorized: authorized}
}

// Serve accepts connections until ctx is cancelled, dispatching each accepted
// interface session to h (M7.3).
func (s *sshServer) Serve(ctx context.Context, h Handler) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("sshd: listen %s: %w", s.addr, err)
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()

	// Close the listener when ctx is cancelled so Accept unblocks.
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // clean shutdown
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("sshd: accept: %w", err)
		}
		go s.handleConn(ctx, nc, h)
	}
}

// Addr returns the bound listener address, or nil before Serve has bound. Useful
// when listening on ":0".
func (s *sshServer) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Close stops the listener (M7.5).
func (s *sshServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		return s.ln.Close()
	}
	return nil
}

func (s *sshServer) serverConfig() *ssh.ServerConfig {
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if s.authorized != nil && s.authorized(key) {
				return &ssh.Permissions{
					Extensions: map[string]string{"pubkey-fp": ssh.FingerprintSHA256(key)},
				}, nil
			}
			return nil, fmt.Errorf("sshd: unauthorized key")
		},
	}
	cfg.AddHostKey(s.signer)
	return cfg
}

func (s *sshServer) handleConn(ctx context.Context, nc net.Conn, h Handler) {
	conn, chans, reqs, err := ssh.NewServerConn(nc, s.serverConfig())
	if err != nil {
		_ = nc.Close()
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)

	remoteAddr := conn.RemoteAddr().String()
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "only session channels")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		c := newConn(ch, remoteAddr)
		go c.serviceRequests(chReqs)
		// Each accepted session channel is one attached interface.
		go func() {
			defer c.Close()
			h.Handle(ctx, c)
		}()
	}
}

// AuthorizedKeysFile loads an OpenSSH authorized_keys file and returns an
// AuthFunc accepting exactly those keys. A missing file yields a deny-all func
// and the bool false so the caller can warn (M7.2).
func AuthorizedKeysFile(path string) (AuthFunc, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return func(ssh.PublicKey) bool { return false }, false
	}
	var keys [][]byte
	rest := raw
	for len(rest) > 0 {
		pub, _, _, r, err := ssh.ParseAuthorizedKey(rest)
		if err != nil {
			break
		}
		keys = append(keys, pub.Marshal())
		rest = r
	}
	return func(key ssh.PublicKey) bool {
		m := key.Marshal()
		for _, k := range keys {
			if string(k) == string(m) {
				return true
			}
		}
		return false
	}, true
}

// newClientID mints a per-connection attachment identity.
func newClientID() attach.ClientID {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return attach.ClientID(hex.EncodeToString(b[:]))
}

// Compile-time check that sshServer satisfies Server.
var _ Server = (*sshServer)(nil)
