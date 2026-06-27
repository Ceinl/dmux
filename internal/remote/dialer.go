package remote

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"dmux/internal/registry"
)

// keepaliveInterval bounds how long a half-dead host stays undetected (M3.7).
const keepaliveInterval = 15 * time.Second

// sshDialer is the concrete Dialer. One instance is shared by the whole server;
// it presents the server's signer (or a per-host key named by KeyRef) and
// verifies host keys against a TOFU known_hosts file under DataDir (M3.1/M3.2).
type sshDialer struct {
	dataDir  string
	timeout  time.Duration
	signer   ssh.Signer
	hostKeys ssh.HostKeyCallback

	keyMu sync.Mutex // serialises known_hosts file writes (TOFU appends)
}

// NewDialer builds the shared outbound dialer. signer is the server keypair from
// config.EnsureHostKey (M3.1).
func NewDialer(dataDir string, timeout time.Duration, signer ssh.Signer) *sshDialer {
	d := &sshDialer{
		dataDir: dataDir,
		timeout: timeout,
		signer:  signer,
	}
	d.hostKeys = d.tofuHostKeyCallback(filepath.Join(dataDir, "known_hosts"))
	return d
}

// clientConfig assembles the SSH client config for a host (M3.3).
func (d *sshDialer) clientConfig(h registry.Host) (*ssh.ClientConfig, error) {
	signer := d.signer
	// Resolve a per-host key if KeyRef names a file in DataDir other than the
	// shared server key; otherwise fall back to the shared signer.
	if h.KeyRef != "" {
		if s, err := d.loadKeyRef(h.KeyRef); err == nil && s != nil {
			signer = s
		} else if err != nil {
			return nil, err
		}
	}
	return &ssh.ClientConfig{
		User:            h.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: d.hostKeys,
		Timeout:         d.timeout,
	}, nil
}

// loadKeyRef loads a per-host private key by name from DataDir. A KeyRef that
// does not resolve to a readable file returns (nil, nil) so the caller falls
// back to the shared server signer.
func (d *sshDialer) loadKeyRef(ref string) (ssh.Signer, error) {
	// Disallow path traversal; KeyRef is a bare filename within DataDir.
	if strings.ContainsAny(ref, "/\\") || ref == "." || ref == ".." {
		return nil, nil
	}
	path := filepath.Join(d.dataDir, ref)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil // not a file → use shared signer
	}
	s, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("remote: parse key %s: %w", path, err)
	}
	return s, nil
}

// dial establishes the transport + SSH client, honoring ctx for the TCP dial
// (ssh.Dial itself takes no context) (M3.4).
func (d *sshDialer) dial(ctx context.Context, h registry.Host) (*ssh.Client, error) {
	cfg, err := d.clientConfig(h)
	if err != nil {
		return nil, err
	}
	nd := net.Dialer{Timeout: d.timeout}
	conn, err := nd.DialContext(ctx, "tcp", h.Addr)
	if err != nil {
		return nil, fmt.Errorf("remote: dial %s: %w", h.Addr, err)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, h.Addr, cfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("remote: handshake %s: %w", h.Addr, err)
	}
	return ssh.NewClient(c, chans, reqs), nil
}

// Open dials the host and allocates a PTY running the host's login shell (M3.4).
func (d *sshDialer) Open(ctx context.Context, h registry.Host, spec OpenSpec) (PTY, error) {
	client, err := d.dial(ctx, h)
	if err != nil {
		return nil, err
	}

	sess, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("remote: new session: %w", err)
	}

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	rows, cols := int(spec.Size.Rows), int(spec.Size.Cols)
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		sess.Close()
		client.Close()
		return nil, fmt.Errorf("remote: request pty: %w", err)
	}

	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return nil, fmt.Errorf("remote: stdin pipe: %w", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return nil, fmt.Errorf("remote: stdout pipe: %w", err)
	}
	// On a PTY, host stderr is already merged into stdout by the tty, so we
	// leave sess.Stderr at its default and render only stdout.

	// Start the shell, optionally cd'd into the project root (M3.4).
	if spec.Cwd == "" {
		if err := sess.Shell(); err != nil {
			sess.Close()
			client.Close()
			return nil, fmt.Errorf("remote: start shell: %w", err)
		}
	} else {
		cmd := fmt.Sprintf("cd %s && exec $SHELL -l", shellQuote(spec.Cwd))
		if err := sess.Start(cmd); err != nil {
			sess.Close()
			client.Close()
			return nil, fmt.Errorf("remote: start shell in %s: %w", spec.Cwd, err)
		}
	}

	p := &sshPTY{
		sess:   sess,
		client: client,
		stdin:  stdin,
		stdout: stdout,
		done:   make(chan struct{}),
	}
	// Watch for session exit / connection drop → close done (host-down signal).
	go func() {
		_ = sess.Wait()
		p.markDone()
	}()
	// Keepalives so a half-dead host surfaces promptly via Done() (M3.7).
	go p.keepalive(client)

	return p, nil
}

// Run dials the host, runs a single command, and returns its stdout. Used by
// the on-demand project indexer (M8). Nothing is left running afterward.
func (d *sshDialer) Run(ctx context.Context, h registry.Host, cmd string) ([]byte, error) {
	client, err := d.dial(ctx, h)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("remote: run new session: %w", err)
	}
	defer sess.Close()
	out, err := sess.Output(cmd)
	if err != nil {
		return out, fmt.Errorf("remote: run %q: %w", cmd, err)
	}
	return out, nil
}

// Verify checks SSH + key trust without leaving anything running (M3.6).
func (d *sshDialer) Verify(ctx context.Context, h registry.Host) error {
	client, err := d.dial(ctx, h)
	if err != nil {
		return err
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("remote: verify new session: %w", err)
	}
	defer sess.Close()
	if err := sess.Run("true"); err != nil {
		return fmt.Errorf("remote: verify run: %w", err)
	}
	return nil
}

// shellQuote single-quotes s for safe inclusion in a POSIX shell command,
// handling embedded single quotes (M3.4).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// tofuHostKeyCallback returns a HostKeyCallback backed by a simple known_hosts
// file: trust-on-first-use, then pin. A changed key is rejected (M3.2).
func (d *sshDialer) tofuHostKeyCallback(path string) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		want := base64.StdEncoding.EncodeToString(key.Marshal())

		d.keyMu.Lock()
		defer d.keyMu.Unlock()

		known, err := readKnownHosts(path)
		if err != nil {
			return err
		}
		if got, ok := known[hostname]; ok {
			if got != want {
				return fmt.Errorf("remote: host key mismatch for %s (possible MITM)", hostname)
			}
			return nil
		}
		// First contact: pin it.
		return appendKnownHost(path, hostname, key.Type(), want)
	}
}

func readKnownHosts(path string) (map[string]string, error) {
	out := map[string]string{}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("remote: read known_hosts: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		// format: <host> <keytype> <base64>
		out[fields[0]] = fields[2]
	}
	return out, sc.Err()
}

func appendKnownHost(path, host, keyType, b64 string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("remote: mkdir known_hosts dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("remote: open known_hosts: %w", err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "%s %s %s\n", host, keyType, b64); err != nil {
		return fmt.Errorf("remote: write known_hosts: %w", err)
	}
	return nil
}

// Compile-time check that sshDialer satisfies Dialer.
var _ Dialer = (*sshDialer)(nil)
