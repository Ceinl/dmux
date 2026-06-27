// Command dmux is the single binary. It runs the always-on server and the
// admin subcommands from SPEC.md.
//
//	dmux serve                       start the always-on server (the brain)
//	dmux connect <host-addr>         register a host (verify SSH + key trust)
//	dmux sethome <path> --host <id>  set a host's project root + scan depth
//	ssh dmux@<server>                attach to the TUI (handled by the server's sshd)
//
// v1 NOTE (control transport, see TODO M10.3): `connect` and `sethome` are
// server-side admin commands — run them ON the server box; they mutate the
// on-disk registry directly. The SPEC's "run `dmux connect <server-addr>` from
// the host" flow needs a control channel that does not exist yet, so it is
// deferred. The TUI attach path (`ssh dmux@<server>`) is fully implemented.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"

	"dmux/internal/attach"
	"dmux/internal/config"
	"dmux/internal/project"
	"dmux/internal/registry"
	"dmux/internal/remote"
	"dmux/internal/server"
	"dmux/internal/session"
	"dmux/internal/sshd"
	"dmux/internal/tui"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(ctx, os.Args[2:])
	case "connect":
		err = runConnect(ctx, os.Args[2:])
	case "sethome":
		err = runSetHome(ctx, os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "dmux: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "dmux: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `dmux — device multiplexer

usage:
  dmux serve [--data-dir DIR] [--listen ADDR]
  dmux connect <host-addr> [--user USER] [--key KEYREF] [--data-dir DIR]
  dmux sethome <path> --host <host-id> [--depth N] [--data-dir DIR]
  ssh dmux@<server>        attach to the TUI

run connect/sethome on the server box (v1: server-side admin).
`)
}

// hoistFlags reorders args so flags may appear after the positional argument
// (the SPEC writes `dmux sethome . --depth N`, but Go's flag package stops at the
// first non-flag token). All dmux flags take a value, so a bare `-x` consumes the
// following token unless it uses the `-x=y` form. Flags are returned first,
// positionals appended, ready for flag.Parse.
func hoistFlags(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

// loadConfig resolves config from an optional --data-dir, falling back to the
// default data dir.
func loadConfig(dataDir string) (config.Config, error) {
	if dataDir == "" {
		dataDir = config.Default().DataDir
	}
	return config.Load(dataDir)
}

// runServe starts the server: load config, build collaborators, Run until signal
// (M10.2).
func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "state directory (default: per-user config dir)")
	listen := fs.String("listen", "", "inbound SSH listen address (overrides config)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(*dataDir)
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.ListenAddr = *listen
	}

	signer, err := config.EnsureHostKey(cfg)
	if err != nil {
		return err
	}

	reg := registry.NewFileRegistry(cfg.DataDir)
	dialer := remote.NewDialer(cfg.DataDir, cfg.DialTimeout, signer)
	sessions := session.NewManager(dialer, reg, cfg)
	clients := attach.NewAttachments()
	indexer := project.NewIndexer(dialer)

	authPath := filepath.Join(cfg.DataDir, "authorized_keys")
	authFn, ok := sshd.AuthorizedKeysFile(authPath)
	if !ok {
		fmt.Fprintf(os.Stderr, "dmux: warning: no %s — no interface keys are authorized to attach\n", authPath)
	}
	inbound := sshd.NewServer(cfg.ListenAddr, signer, authFn)

	srv := server.New(cfg, reg, sessions, clients, dialer, indexer, inbound)

	prefix, err := config.ParsePrefix(cfg.PrefixKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dmux: bad prefix_key %q (%v); using Ctrl-Space\n", cfg.PrefixKey, err)
		prefix = 0x00
	}
	ui := tui.New(srv, sessions, clients, reg, cfg, prefix)
	srv.SetHandler(ui)

	fmt.Fprintf(os.Stderr, "dmux: serving on %s (data dir %s)\n", cfg.ListenAddr, cfg.DataDir)
	for _, hint := range attachHints(cfg.ListenAddr) {
		fmt.Fprintf(os.Stderr, "  attach: %s\n", hint)
	}
	return srv.Run(ctx)
}

// attachHints turns the listen address into concrete `ssh dmux@…` commands an
// interface can run. When the host part is a wildcard (":2222", "0.0.0.0:…",
// "[::]:…"), it expands to the machine's reachable addresses.
func attachHints(listenAddr string) []string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return []string{fmt.Sprintf("ssh dmux@<server> (listen %s)", listenAddr)}
	}

	cmd := func(h string) string {
		if port == "22" {
			return fmt.Sprintf("ssh dmux@%s", h)
		}
		return fmt.Sprintf("ssh dmux@%s -p %s", h, port)
	}

	if host != "" && host != "0.0.0.0" && host != "::" {
		return []string{cmd(host)}
	}

	// Wildcard bind: list loopback + non-loopback IPv4/IPv6 addresses.
	hints := []string{cmd("localhost")}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
				continue
			}
			ip := ipnet.IP.String()
			if ipnet.IP.To4() == nil {
				ip = "[" + ip + "]" // bracket IPv6 for ssh
			}
			hints = append(hints, cmd(ip))
		}
	}
	return hints
}

// runConnect implements `dmux connect <host-addr>`: verify SSH + key trust to a
// host and register it (M10.4, server-side admin in v1).
func runConnect(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "state directory")
	loginUser := fs.String("user", defaultUser(), "remote login user")
	keyRef := fs.String("key", "", "private key filename in data dir (default: server key)")
	if err := fs.Parse(hoistFlags(args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("connect: missing <host-addr>")
	}
	addr := ensurePort(fs.Arg(0))

	cfg, err := loadConfig(*dataDir)
	if err != nil {
		return err
	}
	signer, err := config.EnsureHostKey(cfg)
	if err != nil {
		return err
	}
	reg := registry.NewFileRegistry(cfg.DataDir)
	if err := reg.Load(); err != nil {
		return err
	}
	dialer := remote.NewDialer(cfg.DataDir, cfg.DialTimeout, signer)

	// Build a server with only the pieces Connect needs (sessions/clients/
	// indexer/inbound are nil; Connect touches only dialer + registry).
	srv := server.New(cfg, reg, nil, nil, dialer, nil, nil)

	host := registry.Host{Addr: addr, User: *loginUser, KeyRef: *keyRef}
	if err := srv.Connect(ctx, host); err != nil {
		return err
	}
	fmt.Printf("connected: %s@%s registered (id %s)\n", host.User, host.Addr,
		registry.DeriveHostID(host.User, host.Addr))
	return nil
}

// runSetHome implements `dmux sethome <path> --host <id> --depth N` (M10.5,
// server-side admin in v1).
func runSetHome(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("sethome", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "state directory")
	hostID := fs.String("host", "", "host ID to configure (see registry)")
	depth := fs.Int("depth", 2, "project scan depth")
	if err := fs.Parse(hoistFlags(args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("sethome: missing <path> (the project root on the host)")
	}
	if *hostID == "" {
		return fmt.Errorf("sethome: --host <id> is required")
	}
	root := fs.Arg(0)
	if root == "." || !strings.HasPrefix(root, "/") {
		return fmt.Errorf("sethome: <path> must be an absolute path on the host, got %q", root)
	}
	if *depth < 0 {
		return fmt.Errorf("sethome: --depth must be >= 0")
	}

	cfg, err := loadConfig(*dataDir)
	if err != nil {
		return err
	}
	reg := registry.NewFileRegistry(cfg.DataDir)
	if err := reg.Load(); err != nil {
		return err
	}
	if _, ok := reg.Get(registry.HostID(*hostID)); !ok {
		return fmt.Errorf("sethome: unknown host %q (run `dmux connect` first)", *hostID)
	}
	if err := reg.SetHome(registry.HostID(*hostID), registry.HomeConfig{Root: root, ScanDepth: *depth}); err != nil {
		return err
	}
	fmt.Printf("sethome: host %s → root %s depth %d\n", *hostID, root, *depth)
	return nil
}

// defaultUser returns the current login user as the default remote user.
func defaultUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if env := os.Getenv("USER"); env != "" {
		return env
	}
	return "root"
}

// ensurePort appends the default SSH port if addr has none.
func ensurePort(addr string) string {
	if strings.Contains(addr, ":") {
		return addr
	}
	return addr + ":22"
}
