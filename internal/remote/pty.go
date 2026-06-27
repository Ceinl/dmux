package remote

import (
	"io"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshPTY is one live SSH PTY on a host (M3.5). It satisfies PTY.
type sshPTY struct {
	sess   *ssh.Session
	client *ssh.Client
	stdin  io.WriteCloser
	stdout io.Reader

	done      chan struct{}
	doneOnce  sync.Once
	closeOnce sync.Once
}

func (p *sshPTY) Read(b []byte) (int, error)  { return p.stdout.Read(b) }
func (p *sshPTY) Write(b []byte) (int, error) { return p.stdin.Write(b) }

// Resize renegotiates the host-side window size.
func (p *sshPTY) Resize(s Size) error {
	return p.sess.WindowChange(int(s.Rows), int(s.Cols))
}

// Done is closed when the underlying SSH connection drops (host down).
func (p *sshPTY) Done() <-chan struct{} { return p.done }

// markDone closes the done channel exactly once.
func (p *sshPTY) markDone() {
	p.doneOnce.Do(func() { close(p.done) })
}

// Close tears down the session and underlying client, and signals Done (M3.5).
func (p *sshPTY) Close() error {
	var err error
	p.closeOnce.Do(func() {
		if p.stdin != nil {
			_ = p.stdin.Close()
		}
		if p.sess != nil {
			_ = p.sess.Close()
		}
		if p.client != nil {
			err = p.client.Close()
		}
		p.markDone()
	})
	return err
}

// keepalive pings the host periodically; on failure it closes the PTY so the
// drop surfaces via Done() (M3.7).
func (p *sshPTY) keepalive(client *ssh.Client) {
	t := time.NewTicker(keepaliveInterval)
	defer t.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-t.C:
			if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				_ = p.Close()
				return
			}
		}
	}
}

// Compile-time check that sshPTY satisfies PTY.
var _ PTY = (*sshPTY)(nil)
