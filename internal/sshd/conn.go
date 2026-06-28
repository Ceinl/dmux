package sshd

import (
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/Ceinl/dmux/internal/attach"
	"github.com/Ceinl/dmux/internal/remote"
)

// ptyReqPayload is the RFC 4254 "pty-req" body.
type ptyReqPayload struct {
	Term     string
	Cols     uint32
	Rows     uint32
	WidthPx  uint32
	HeightPx uint32
	Modes    string
}

// winChPayload is the RFC 4254 "window-change" body.
type winChPayload struct {
	Cols     uint32
	Rows     uint32
	WidthPx  uint32
	HeightPx uint32
}

// sshConn is one accepted inbound interface connection (M7.4).
type sshConn struct {
	ch     ssh.Channel
	id     attach.ClientID
	iface  string
	resize chan remote.Size

	closeOnce sync.Once
}

func newConn(ch ssh.Channel, remoteAddr string) *sshConn {
	return &sshConn{
		ch:     ch,
		id:     newClientID(),
		iface:  remoteAddr,
		resize: make(chan remote.Size, 8),
	}
}

func (c *sshConn) ClientID() attach.ClientID { return c.id }
func (c *sshConn) Interface() string         { return c.iface }

func (c *sshConn) Read(p []byte) (int, error)  { return c.ch.Read(p) }
func (c *sshConn) Write(p []byte) (int, error) { return c.ch.Write(p) }

func (c *sshConn) Resizes() <-chan remote.Size { return c.resize }

func (c *sshConn) Close() error {
	c.closeOnce.Do(func() { close(c.resize) })
	return c.ch.Close()
}

// serviceRequests answers channel requests: it acknowledges pty-req/shell and
// turns window-change into Resizes events. The initial pty-req size is the
// first value pushed on the resize channel (M7.4).
func (c *sshConn) serviceRequests(reqs <-chan *ssh.Request) {
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			var p ptyReqPayload
			if err := ssh.Unmarshal(req.Payload, &p); err == nil {
				c.pushResize(remote.Size{Rows: uint16(p.Rows), Cols: uint16(p.Cols)})
			}
			reply(req, true)
		case "shell":
			// We drive our own TUI rather than a host shell; accept it.
			reply(req, true)
		case "window-change":
			var p winChPayload
			if err := ssh.Unmarshal(req.Payload, &p); err == nil {
				c.pushResize(remote.Size{Rows: uint16(p.Rows), Cols: uint16(p.Cols)})
			}
			reply(req, false)
		case "env":
			reply(req, true)
		default:
			reply(req, false)
		}
	}
}

// pushResize delivers a size without blocking the request loop.
func (c *sshConn) pushResize(s remote.Size) {
	defer func() { _ = recover() }() // resize may be closed during shutdown
	select {
	case c.resize <- s:
	default:
	}
}

func reply(req *ssh.Request, ok bool) {
	if req.WantReply {
		_ = req.Reply(ok, nil)
	}
}

// Compile-time check that sshConn satisfies Conn.
var _ Conn = (*sshConn)(nil)
