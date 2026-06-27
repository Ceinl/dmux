// Package config holds the server's runtime configuration: where it listens,
// where it stores the host registry, and the tunable policies from SPEC.md
// (prefix key, scrollback size, shared-resize behaviour).
package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// ResizePolicy decides the PTY size when several interfaces share one session.
type ResizePolicy int

const (
	// SmallestWins clamps to the smallest attached interface (SPEC default).
	SmallestWins ResizePolicy = iota
	// Driver follows a single designated client; others just scroll/crop.
	Driver
)

// Config is the fully-resolved server configuration.
type Config struct {
	// ListenAddr is the address the inbound SSH server binds (e.g. ":2222").
	ListenAddr string

	// DataDir is the server's state directory: host registry + its SSH keypair.
	DataDir string

	// HostKeyPath is the server's own SSH private key, used both to host the
	// inbound TUI server and as the identity trusted on registered hosts.
	HostKeyPath string

	// PrefixKey is the configurable TUI prefix (SPEC default: Ctrl-Space).
	// Everything except the prefix passes through to the host shell.
	PrefixKey string

	// ScrollbackLines bounds the per-session server-side scrollback buffer.
	ScrollbackLines int

	// ResizePolicy controls shared-resize negotiation across attached clients.
	ResizePolicy ResizePolicy

	// DialTimeout bounds an individual outbound SSH dial to a host.
	DialTimeout time.Duration
}

// defaultKeyName is the server keypair filename under DataDir.
const defaultKeyName = "id_dmux"

// configFileName is the on-disk config filename under DataDir (M1.2).
const configFileName = "config.json"

// Default returns the baseline configuration described in SPEC.md.
func Default() Config {
	dataDir := defaultDataDir()
	return Config{
		ListenAddr:      ":2222",
		DataDir:         dataDir,
		HostKeyPath:     filepath.Join(dataDir, defaultKeyName),
		PrefixKey:       "Ctrl-D",
		ScrollbackLines: 10000,
		ResizePolicy:    SmallestWins,
		DialTimeout:     10 * time.Second,
	}
}

// defaultDataDir resolves the per-user config dir, never hardcoding "~".
func defaultDataDir() string {
	if base, err := os.UserConfigDir(); err == nil {
		return filepath.Join(base, "dmux")
	}
	// Last-ditch fallback so Default() never returns an empty DataDir.
	return filepath.Join(os.TempDir(), "dmux")
}

// fileConfig mirrors Config on disk. Pointers + omitempty let unset fields fall
// back to Default() during overlay (M1.2).
type fileConfig struct {
	ListenAddr      *string `json:"listen_addr,omitempty"`
	DataDir         *string `json:"data_dir,omitempty"`
	HostKeyPath     *string `json:"host_key_path,omitempty"`
	PrefixKey       *string `json:"prefix_key,omitempty"`
	ScrollbackLines *int    `json:"scrollback_lines,omitempty"`
	ResizePolicy    *string `json:"resize_policy,omitempty"` // "smallest" | "driver"
	DialTimeout     *string `json:"dial_timeout,omitempty"`  // Go duration string
}

// Load reads configuration from disk under dataDir, falling back to Default
// for anything unset.
func Load(dataDir string) (Config, error) {
	c := Default()
	c.DataDir = dataDir
	// Track whether HostKeyPath was left at its default so we can re-anchor it
	// to the supplied dataDir below.
	hostKeyWasDefault := true

	path := filepath.Join(dataDir, configFileName)
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Missing file is not an error: return the Default()/dataDir overlay.
	case err != nil:
		return Config{}, fmt.Errorf("config: read %s: %w", path, err)
	default:
		var fc fileConfig
		if err := json.Unmarshal(raw, &fc); err != nil {
			return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
		}
		if fc.ListenAddr != nil {
			c.ListenAddr = *fc.ListenAddr
		}
		if fc.DataDir != nil {
			c.DataDir = *fc.DataDir
		}
		if fc.HostKeyPath != nil {
			c.HostKeyPath = *fc.HostKeyPath
			hostKeyWasDefault = false
		}
		if fc.PrefixKey != nil {
			c.PrefixKey = *fc.PrefixKey
		}
		if fc.ScrollbackLines != nil {
			c.ScrollbackLines = *fc.ScrollbackLines
		}
		if fc.ResizePolicy != nil {
			p, err := parseResizePolicy(*fc.ResizePolicy)
			if err != nil {
				return Config{}, err
			}
			c.ResizePolicy = p
		}
		if fc.DialTimeout != nil {
			d, err := time.ParseDuration(*fc.DialTimeout)
			if err != nil {
				return Config{}, fmt.Errorf("config: dial_timeout: %w", err)
			}
			c.DialTimeout = d
		}
	}

	// Re-anchor a default key path to the resolved DataDir (M1.3).
	if hostKeyWasDefault {
		c.HostKeyPath = filepath.Join(c.DataDir, defaultKeyName)
	}

	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// validate enforces the invariants from M1.3.
func (c Config) validate() error {
	if _, _, err := net.SplitHostPort(normalizeListen(c.ListenAddr)); err != nil {
		return fmt.Errorf("config: invalid listen_addr %q: %w", c.ListenAddr, err)
	}
	if c.ScrollbackLines <= 0 {
		return fmt.Errorf("config: scrollback_lines must be > 0, got %d", c.ScrollbackLines)
	}
	if c.DialTimeout <= 0 {
		return fmt.Errorf("config: dial_timeout must be > 0, got %s", c.DialTimeout)
	}
	return nil
}

// normalizeListen lets ":2222" pass net.SplitHostPort (which wants host:port).
func normalizeListen(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "0.0.0.0" + addr
	}
	return addr
}

func parseResizePolicy(s string) (ResizePolicy, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "smallest", "smallest-wins", "smallestwins":
		return SmallestWins, nil
	case "driver":
		return Driver, nil
	default:
		return SmallestWins, fmt.Errorf("config: unknown resize_policy %q", s)
	}
}

// EnsureHostKey loads the server's SSH private key, generating an ed25519
// keypair on first run if HostKeyPath does not yet exist (M1.4).
func EnsureHostKey(c Config) (ssh.Signer, error) {
	if raw, err := os.ReadFile(c.HostKeyPath); err == nil {
		signer, err := ssh.ParsePrivateKey(raw)
		if err != nil {
			return nil, fmt.Errorf("config: parse host key %s: %w", c.HostKeyPath, err)
		}
		return signer, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("config: read host key %s: %w", c.HostKeyPath, err)
	}

	// Generate a fresh ed25519 keypair.
	if err := os.MkdirAll(filepath.Dir(c.HostKeyPath), 0o700); err != nil {
		return nil, fmt.Errorf("config: mkdir data dir: %w", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("config: generate host key: %w", err)
	}

	pemBlock, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, fmt.Errorf("config: marshal host key: %w", err)
	}
	if err := os.WriteFile(c.HostKeyPath, pem.EncodeToMemory(pemBlock), 0o600); err != nil {
		return nil, fmt.Errorf("config: write host key: %w", err)
	}

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("config: derive public key: %w", err)
	}
	pubPath := c.HostKeyPath + ".pub"
	if err := os.WriteFile(pubPath, ssh.MarshalAuthorizedKey(sshPub), 0o644); err != nil {
		return nil, fmt.Errorf("config: write public key: %w", err)
	}

	signer, err := ssh.NewSignerFromSigner(priv)
	if err != nil {
		return nil, fmt.Errorf("config: build signer: %w", err)
	}
	return signer, nil
}

// ParsePrefix maps a human prefix-key string (e.g. "Ctrl-Space", "Ctrl-B") to
// the single control byte the TUI input layer matches against (M1.5).
func ParsePrefix(s string) (byte, error) {
	norm := strings.ToLower(strings.TrimSpace(s))
	norm = strings.ReplaceAll(norm, "_", "-")
	norm = strings.ReplaceAll(norm, "+", "-")
	switch norm {
	case "ctrl-space", "c-space", "^space", "ctrl-@", "c-@":
		return 0x00, nil
	}

	// General "ctrl-<letter>" → control byte (Ctrl-A == 0x01 ... Ctrl-Z == 0x1a).
	for _, pfx := range []string{"ctrl-", "c-", "^"} {
		if rest, ok := strings.CutPrefix(norm, pfx); ok && len(rest) == 1 {
			ch := rest[0]
			if ch >= 'a' && ch <= 'z' {
				return ch - 'a' + 1, nil
			}
		}
	}
	return 0, fmt.Errorf("config: unsupported prefix key %q", s)
}
