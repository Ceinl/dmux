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
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
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
}

func runSetupWSL(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("setup wsl", flag.ContinueOnError)
	serverKey := fs.String("server-key", "", "server public key to authorize: a key string, or @path to a .pub file")
	port := fs.Int("port", 22, "sshd port to configure on this host")
	loginUser := fs.String("user", defaultUser(), "remote login user the server will connect as")
	dryRun := fs.Bool("dry-run", false, "print the steps without changing the system")
	portproxy := fs.Bool("portproxy", false, "attempt the elevated Windows netsh portproxy step (triggers a UAC prompt)")
	if err := fs.Parse(hoistFlags(args)); err != nil {
		return err
	}

	key, err := resolveServerKey(*serverKey)
	if err != nil {
		return err
	}
	if *port < 1 || *port > 65535 {
		return fmt.Errorf("setup wsl: --port out of range: %d", *port)
	}

	opts := setupOpts{
		serverKey: key,
		port:      *port,
		loginUser: *loginUser,
		dryRun:    *dryRun,
		portproxy: *portproxy,
	}

	if !isWSL() {
		return fmt.Errorf("setup wsl: this does not look like a WSL environment " +
			"(no \"microsoft\" in /proc/version and no $WSL_DISTRO_NAME)")
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

// resolveServerKey turns the --server-key value into a single authorized_keys
// line. A leading @ means "read the file at this path"; otherwise the value is
// the key text itself.
func resolveServerKey(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", fmt.Errorf("setup: --server-key is required " +
			"(the server's public key, e.g. @~/.config/dmux/dmux_host_key.pub)")
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

// stepInstallSSHD installs the openssh-server package via apt.
func stepInstallSSHD(o setupOpts) error {
	if err := sh(o, "sudo", "apt-get", "update"); err != nil {
		return err
	}
	return sh(o, "sudo", "apt-get", "install", "-y", "openssh-server")
}

// stepHostKeys regenerates any missing sshd host keys.
func stepHostKeys(o setupOpts) error {
	return sh(o, "sudo", "ssh-keygen", "-A")
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
	// `sudo tee` so the redirect runs as root.
	return shStdin(o, conf, "sudo", "tee", "/etc/ssh/sshd_config.d/dmux.conf")
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
		fmt.Printf("    mkdir -p %s && append server key to %s\n", sshDir, authPath)
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

// stepStartSSHD starts/restarts sshd, trying the service wrapper first (works on
// non-systemd WSL) and falling back to systemctl.
func stepStartSSHD(o setupOpts) error {
	if err := sh(o, "sudo", "service", "ssh", "restart"); err == nil {
		return nil
	}
	return sh(o, "sudo", "systemctl", "restart", "ssh")
}

// stepNetworking inspects WSL's networking mode and prints what the server needs
// to reach this host, plus the final `dmux connect` line.
func stepNetworking(o setupOpts) error {
	fmt.Println("→ networking")
	ips := wslIPv4s()
	mode := classifyNetworking(ips)
	wslIP := firstNonLoopback(ips)

	switch mode {
	case netMirrored:
		fmt.Printf("    mirrored networking detected — this host is reachable directly at %s\n", wslIP)
		printConnect(o, wslIP)
	case netNAT:
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
	default:
		fmt.Printf("    could not classify networking; this host's WSL IP is %s\n", wslIP)
		printConnect(o, wslIP)
	}
	return nil
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
