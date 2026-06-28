package main

// setup.go implements `dmux setup <platform>` — a one-shot, host-side helper
// that prepares a machine to be a dmux host: install + configure sshd, authorize
// the server's public key, sort out networking, then print the exact
// `dmux connect …` line to run on the server.
//
// This is host-side prep, NOT a daemon (SPEC decision #1: hosts run nothing at
// steady state). The command does its work and exits; the only thing left behind
// is a running sshd and an authorized key.
//
// It is intended to be invoked install-free via:
//
//	go run github.com/Ceinl/dmux/cmd/dmux@latest setup wsl --server-key @key.pub
//
// so it deliberately depends on nothing in internal/ — a host has no server
// config, registry, or data dir.

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// runSetup dispatches `dmux setup <platform> [flags]`.
func runSetup(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("setup: missing <platform> (supported: wsl)")
	}
	platform := strings.ToLower(args[0])
	rest := args[1:]

	switch platform {
	case "wsl", "wsl2":
		return runSetupWSL(ctx, rest)
	default:
		return fmt.Errorf("setup: unsupported platform %q (supported: wsl)", platform)
	}
}

// setupOpts are the shared flags for a setup run.
type setupOpts struct {
	serverKey string // authorized_keys line to install (resolved from --server-key)
	port      int    // sshd port to advertise / configure
	loginUser string // remote login user the server will use
	dryRun    bool   // print commands instead of running them
	portproxy bool   // attempt the elevated Windows netsh portproxy step (WSL NAT)
	pm        pkgMgr // detected package manager + sshd service name

	// discoveredIP is the local source address this host used to reach the
	// server while fetching its key — the most reliable address to advertise
	// back, since it's literally on the path between the two. Empty in the
	// offline (--server-key) path.
	discoveredIP string
}

// pkgMgr describes how to install openssh on a distro and what its sshd service
// is called. WSL ships many distros; the package name and service differ
// (Debian/Ubuntu: openssh-server + service "ssh"; others: openssh + "sshd").
type pkgMgr struct {
	bin     string   // package-manager binary to look for in PATH
	install []string // full install command (incl. the binary)
	update  []string // optional index-refresh command run before install ("" = none)
	service string   // sshd service name for service(8)/systemctl
}

// detectPkgMgr returns the first supported package manager found in PATH.
func detectPkgMgr() (pkgMgr, bool) {
	candidates := []pkgMgr{
		{"apt-get", []string{"apt-get", "install", "-y", "openssh-server"}, []string{"apt-get", "update"}, "ssh"},
		{"dnf", []string{"dnf", "install", "-y", "openssh-server"}, nil, "sshd"},
		{"yum", []string{"yum", "install", "-y", "openssh-server"}, nil, "sshd"},
		{"pacman", []string{"pacman", "-S", "--needed", "--noconfirm", "openssh"}, []string{"pacman", "-Sy", "--noconfirm"}, "sshd"},
		{"zypper", []string{"zypper", "--non-interactive", "install", "openssh"}, nil, "sshd"},
		{"apk", []string{"apk", "add", "openssh"}, []string{"apk", "update"}, "sshd"},
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c.bin); err == nil {
			return c, true
		}
	}
	return pkgMgr{}, false
}

func runSetupWSL(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("setup wsl", flag.ContinueOnError)
	server := fs.String("server", "", "dmux server address (host[:port], default port 2222); its key is fetched automatically")
	serverKey := fs.String("server-key", "", "offline fallback: server public key as a string or @path, if --server is unreachable")
	port := fs.Int("port", 22, "sshd port to configure on this host")
	loginUser := fs.String("user", defaultUser(), "remote login user the server will connect as")
	dryRun := fs.Bool("dry-run", false, "print the steps without changing the system")
	portproxy := fs.Bool("portproxy", false, "attempt the elevated Windows netsh portproxy step (triggers a UAC prompt)")
	if err := fs.Parse(hoistFlags(args)); err != nil {
		return err
	}

	key, localIP, err := obtainServerKey(*server, *serverKey)
	if err != nil {
		return err
	}
	if *port < 1 || *port > 65535 {
		return fmt.Errorf("setup wsl: --port out of range: %d", *port)
	}

	opts := setupOpts{
		serverKey:    key,
		port:         *port,
		loginUser:    *loginUser,
		dryRun:       *dryRun,
		portproxy:    *portproxy,
		discoveredIP: localIP,
	}

	if !isWSL() {
		return fmt.Errorf("setup wsl: this does not look like a WSL environment " +
			"(no \"microsoft\" in /proc/version and no $WSL_DISTRO_NAME)")
	}

	pm, ok := detectPkgMgr()
	if !ok {
		return fmt.Errorf("setup wsl: no supported package manager found " +
			"(apt-get/dnf/yum/pacman/zypper/apk); install openssh-server manually")
	}
	opts.pm = pm
	if !isRoot() {
		if _, err := exec.LookPath("sudo"); err != nil {
			return fmt.Errorf("setup wsl: not running as root and no sudo found; " +
				"re-run as root or install sudo")
		}
	}

	fmt.Println("dmux setup wsl — preparing this machine as a dmux host")
	if opts.dryRun {
		fmt.Println("  (dry run: commands are printed, not executed)")
	}

	steps := []struct {
		desc string
		fn   func(setupOpts) error
	}{
		{"install openssh-server", stepInstallSSHD},
		{"generate host keys", stepHostKeys},
		{"write sshd config (key-only auth)", stepSSHDConfig},
		{"authorize server key", stepAuthorizeKey},
		{"start sshd", stepStartSSHD},
	}
	for _, s := range steps {
		fmt.Printf("→ %s\n", s.desc)
		if err := s.fn(opts); err != nil {
			return fmt.Errorf("setup wsl: %s: %w", s.desc, err)
		}
	}

	return stepNetworking(opts)
}

// obtainServerKey gets the authorized_keys line for the server. Preferred path:
// connect to --server and read the host key it presents — the server signs both
// its inbound sshd and its outbound dials with the same keypair, so that key IS
// what this host must trust. No copying, no secret. --server-key is an offline
// fallback for when the host can't reach the server.
// obtainServerKey returns the authorized_keys line and the local source IP this
// host used to reach the server (empty in the offline path).
func obtainServerKey(server, serverKey string) (key, localIP string, err error) {
	if server != "" {
		addr := ensureServerPort(server)
		key, localIP, err := fetchServerKey(addr)
		if err != nil {
			return "", "", fmt.Errorf("setup: fetch key from server %s: %w "+
				"(is the server running? otherwise pass --server-key)", addr, err)
		}
		fmt.Printf("    fetched server key from %s\n", addr)
		return key, localIP, nil
	}
	if serverKey != "" {
		key, err := resolveServerKey(serverKey)
		return key, "", err
	}
	return "", "", fmt.Errorf("setup: pass --server <addr> (recommended) or --server-key <key|@path>")
}

// fetchServerKey opens an SSH handshake to the server and captures the host key
// it presents, without completing auth (the ssh-keyscan technique: the host-key
// callback fires before authentication, so we grab the key and abort). It dials
// the TCP connection itself so it can also report the local source address —
// the address this host should advertise back to the server.
func fetchServerKey(addr string) (key, localIP string, err error) {
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return "", "", err
	}
	defer conn.Close()
	if tcp, ok := conn.LocalAddr().(*net.TCPAddr); ok {
		localIP = tcp.IP.String()
	}

	var captured ssh.PublicKey
	errCaptured := fmt.Errorf("key captured")
	cfg := &ssh.ClientConfig{
		User: "dmux",
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			captured = key
			return errCaptured // stop the handshake; we have what we need
		},
		Timeout: 10 * time.Second,
	}
	// Aborts in the callback, so this returns an error and no usable conn.
	if sc, chans, reqs, herr := ssh.NewClientConn(conn, addr, cfg); herr == nil {
		go ssh.DiscardRequests(reqs)
		go func() {
			for ch := range chans {
				_ = ch.Reject(ssh.Prohibited, "")
			}
		}()
		sc.Close()
	}
	if captured == nil {
		return "", "", fmt.Errorf("server presented no host key")
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(captured))), localIP, nil
}

// ensureServerPort defaults the dmux server port (2222) when addr omits one.
func ensureServerPort(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return addr + ":2222"
}

// resolveServerKey turns the --server-key value into a single authorized_keys
// line. A leading @ means "read the file at this path"; otherwise the value is
// the key text itself.
func resolveServerKey(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", fmt.Errorf("setup: --server-key is empty")
	}
	if strings.HasPrefix(v, "@") {
		path := expandHome(strings.TrimPrefix(v, "@"))
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("setup: read --server-key file: %w", err)
		}
		v = strings.TrimSpace(string(raw))
	}
	if !strings.HasPrefix(v, "ssh-") && !strings.HasPrefix(v, "ecdsa-") && !strings.HasPrefix(v, "sk-") {
		return "", fmt.Errorf("setup: --server-key does not look like an SSH public key: %q", truncate(v, 32))
	}
	return v, nil
}

// stepInstallSSHD installs openssh using the detected package manager.
func stepInstallSSHD(o setupOpts) error {
	if len(o.pm.update) > 0 {
		if err := shRoot(o, o.pm.update[0], o.pm.update[1:]...); err != nil {
			return err
		}
	}
	return shRoot(o, o.pm.install[0], o.pm.install[1:]...)
}

// stepHostKeys regenerates any missing sshd host keys.
func stepHostKeys(o setupOpts) error {
	return shRoot(o, "ssh-keygen", "-A")
}

// stepSSHDConfig drops a dmux-owned sshd config fragment enabling key-only auth
// on the chosen port. Using a sshd_config.d/ fragment avoids editing the distro
// default in place.
func stepSSHDConfig(o setupOpts) error {
	conf := fmt.Sprintf(`# managed by dmux setup
Port %d
PubkeyAuthentication yes
PasswordAuthentication no
`, o.port)
	// `tee` (as root) so the redirect lands in a root-owned dir.
	return shRootStdin(o, conf, "tee", "/etc/ssh/sshd_config.d/dmux.conf")
}

// stepAuthorizeKey appends the server's public key to the login user's
// authorized_keys, creating ~/.ssh with correct permissions. Idempotent: the
// key is not added twice.
func stepAuthorizeKey(o setupOpts) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	sshDir := filepath.Join(home, ".ssh")
	authPath := filepath.Join(sshDir, "authorized_keys")

	if o.dryRun {
		fmt.Printf("    mkdir -p %s && append to %s:\n      %s\n", sshDir, authPath, o.serverKey)
		return nil
	}
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return err
	}
	if existing, err := os.ReadFile(authPath); err == nil {
		if strings.Contains(string(existing), o.serverKey) {
			fmt.Println("    key already authorized — skipping")
			return nil
		}
	}
	f, err := os.OpenFile(authPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := fmt.Fprintln(f, o.serverKey); err != nil {
		return err
	}
	return os.Chmod(authPath, 0o600)
}

// stepStartSSHD starts/restarts sshd. WSL distros vary wildly in init: try the
// service(8) wrapper first (works without systemd), then systemctl, then run the
// daemon directly as a last resort for minimal distros with neither.
func stepStartSSHD(o setupOpts) error {
	svc := o.pm.service
	if err := shRoot(o, "service", svc, "restart"); err == nil {
		return nil
	}
	if err := shRoot(o, "systemctl", "restart", svc); err == nil {
		return nil
	}
	if path, err := exec.LookPath("sshd"); err == nil {
		return shRoot(o, path)
	}
	return fmt.Errorf("could not start sshd via service, systemctl, or directly")
}

// isRoot reports whether we are already uid 0, in which case sudo is neither
// needed nor (on minimal WSL images) present.
func isRoot() bool { return os.Geteuid() == 0 }

// elevate prefixes a command with sudo unless we are already root.
func elevate(name string, args []string) (string, []string) {
	if isRoot() {
		return name, args
	}
	return "sudo", append([]string{name}, args...)
}

// shRoot runs a command as root (directly when uid 0, else via sudo).
func shRoot(o setupOpts, name string, args ...string) error {
	n, a := elevate(name, args)
	return sh(o, n, a...)
}

// shRootStdin runs a command as root with stdin fed from in.
func shRootStdin(o setupOpts, in, name string, args ...string) error {
	n, a := elevate(name, args)
	return shStdin(o, in, n, a...)
}

// stepNetworking decides which address the server should dial. The most reliable
// signal is the source IP this host used to reach the server (discoveredIP): if
// it's a routable LAN address, the server can almost certainly reach it back; if
// it's a WSL NAT address (172.16/12), the server can't, and we print the fixes.
// Without server contact (offline --server-key) we fall back to interface probing.
func stepNetworking(o setupOpts) error {
	fmt.Println("→ networking")

	if o.discoveredIP != "" {
		if !isWSLNATAddr(o.discoveredIP) {
			fmt.Printf("    this host reached the server from %s — using that as its address\n", o.discoveredIP)
			printConnect(o, o.discoveredIP)
			return nil
		}
		printNATGuidance(o, o.discoveredIP)
		return nil
	}

	ips := wslIPv4s()
	wslIP := firstNonLoopback(ips)
	if isWSLNATAddr(wslIP) || classifyNetworking(ips) == netNAT {
		printNATGuidance(o, wslIP)
		return nil
	}
	fmt.Printf("    this host appears reachable directly at %s\n", wslIP)
	printConnect(o, wslIP)
	return nil
}

// printNATGuidance explains how to make a NAT'd WSL2 host reachable and prints
// the connect line. wslIP is the host's NAT address (the portproxy target).
func printNATGuidance(o setupOpts, wslIP string) {
	fmt.Printf("    NAT'd WSL2 network detected (WSL IP %s).\n", wslIP)
	fmt.Println("    The server cannot reach this IP from the LAN. Two options:")
	fmt.Println()
	fmt.Println("    A) Mirrored networking (recommended, persistent across reboots):")
	fmt.Println("       add to C:\\Users\\<you>\\.wslconfig on Windows:")
	fmt.Println("         [wsl2]")
	fmt.Println("         networkingMode=mirrored")
	fmt.Println("       then run `wsl --shutdown` and re-run this command.")
	fmt.Println()
	fmt.Println("    B) Port-proxy (per-reboot; the WSL IP changes on restart). On Windows, elevated:")
	fmt.Printf("         netsh interface portproxy add v4tov4 listenport=%d listenaddress=0.0.0.0 connectport=%d connectaddress=%s\n",
		o.port, o.port, wslIP)
	fmt.Printf("         netsh advfirewall firewall add rule name=\"dmux-ssh\" dir=in action=allow protocol=TCP localport=%d\n", o.port)
	if o.portproxy {
		if err := attemptPortproxy(o, wslIP); err != nil {
			fmt.Printf("    portproxy attempt failed: %v\n", err)
		}
	} else {
		fmt.Println("       (or re-run with --portproxy to attempt this via an elevated UAC prompt)")
	}
	fmt.Println()
	fmt.Println("    After either option, connect to the Windows host's LAN IP:")
	printConnect(o, "<windows-lan-ip>")
}

// isWSLNATAddr reports whether ip is in the 172.16/12 range WSL2 uses for its
// NAT'd virtual network — addresses the server cannot reach from the LAN.
func isWSLNATAddr(ip string) bool {
	p := net.ParseIP(ip)
	if p == nil {
		return false
	}
	p4 := p.To4()
	return p4 != nil && p4[0] == 172 && p4[1] >= 16 && p4[1] <= 31
}

// printConnect prints the server-side registration command for this host.
func printConnect(o setupOpts, addr string) {
	fmt.Println()
	fmt.Println("    On the dmux server, run:")
	fmt.Printf("      dmux connect %s:%d --user %s\n", addr, o.port, o.loginUser)
}

// attemptPortproxy runs the Windows netsh portproxy + firewall steps via an
// elevated PowerShell (Start-Process -Verb RunAs triggers a UAC prompt).
func attemptPortproxy(o setupOpts, wslIP string) error {
	inner := fmt.Sprintf(
		"netsh interface portproxy add v4tov4 listenport=%d listenaddress=0.0.0.0 connectport=%d connectaddress=%s; "+
			"netsh advfirewall firewall add rule name=dmux-ssh dir=in action=allow protocol=TCP localport=%d",
		o.port, o.port, wslIP, o.port)
	ps := fmt.Sprintf("Start-Process cmd -Verb RunAs -ArgumentList '/c %s'", inner)
	return sh(o, "powershell.exe", "-NoProfile", "-Command", ps)
}

// networking classifies how WSL is wired to the outside world.
type networking int

const (
	netUnknown  networking = iota
	netNAT                 // default WSL2: 172.x behind a Windows-host NAT
	netMirrored            // networkingMode=mirrored: shares the Windows host network
)

// classifyNetworking is a heuristic: default WSL2 NAT hands out a 172.16/12
// address and nothing else routable; mirrored mode surfaces the Windows host's
// real LAN address (typically 192.168/16 or 10/8).
func classifyNetworking(ips []string) networking {
	if len(ips) == 0 {
		return netUnknown
	}
	for _, ip := range ips {
		if strings.HasPrefix(ip, "192.168.") || strings.HasPrefix(ip, "10.") {
			return netMirrored
		}
	}
	for _, ip := range ips {
		if strings.HasPrefix(ip, "172.") {
			return netNAT
		}
	}
	return netUnknown
}

// wslIPv4s returns this machine's non-loopback IPv4 addresses via `hostname -I`.
func wslIPv4s() []string {
	out, err := exec.Command("hostname", "-I").Output()
	if err != nil {
		return nil
	}
	var ips []string
	for _, f := range strings.Fields(string(out)) {
		if strings.Count(f, ".") == 3 { // IPv4 only
			ips = append(ips, f)
		}
	}
	return ips
}

func firstNonLoopback(ips []string) string {
	for _, ip := range ips {
		if !strings.HasPrefix(ip, "127.") {
			return ip
		}
	}
	if len(ips) > 0 {
		return ips[0]
	}
	return "<host-ip>"
}

// isWSL reports whether we are running inside WSL.
func isWSL() bool {
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	raw, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	v := strings.ToLower(string(raw))
	return strings.Contains(v, "microsoft") || strings.Contains(v, "wsl")
}

// sh runs a command, streaming its output. In dry-run mode it prints the command
// and does nothing.
func sh(o setupOpts, name string, args ...string) error {
	if o.dryRun {
		fmt.Printf("    $ %s %s\n", name, strings.Join(args, " "))
		return nil
	}
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// shStdin runs a command with stdin fed from in. In dry-run mode it prints both.
func shStdin(o setupOpts, in, name string, args ...string) error {
	if o.dryRun {
		fmt.Printf("    $ %s %s <<'EOF'\n%s    EOF\n", name, strings.Join(args, " "), indent(in, "      "))
		return nil
	}
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(in)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// expandHome expands a leading ~ to the user's home directory.
func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if u, err := user.Current(); err == nil {
			return filepath.Join(u.HomeDir, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// indent prefixes every line of s with pre.
func indent(s, pre string) string {
	var b strings.Builder
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		b.WriteString(pre)
		b.WriteString(sc.Text())
		b.WriteByte('\n')
	}
	return b.String()
}
