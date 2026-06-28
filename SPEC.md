# DMUX — Device Multiplexer

> tmux is the analogy, not the implementation. dmux is a standalone app that
> multiplexes **whole devices**, not panes on one machine.

## Concept

A single TUI that shows every machine you own and every session running on it,
and lets you fuzzy-jump to (or create) a session on any of them. One always-on
**server** holds the connections; everything else is either a plain SSH host or
a thin client.

## Roles

| Role | What it is | Runs dmux? |
|------|-----------|------------|
| **Interface** | The device you sit at (e.g. macbook). Attaches to the TUI, holds no state. | client only |
| **Server** | The always-on box. The brain: host registry, SSH connections, sessions, TUI. | yes (the only one) |
| **Host** | Any machine with `sshd` the server is allowed into (mac mini, pc, server). | **no — runs nothing** |

A device can play more than one role (the server can also be a host).

## Key design decisions

1. **Hosts run nothing.** A host is just a box with SSH access. No agent, no
   daemon, no install. If you can SSH into it, you can add it.
2. **A session is one SSH connection.** Each session = a separate
   `ssh server→host -t` with a PTY allocated on the host. The shell runs on the
   host, in the host's real environment.
3. **The server owns the connections.** It holds every session's SSH connection
   open, multiplexes them, and renders the TUI that interfaces attach to.
4. **Networking is the user's problem.** Making a host reachable (NAT, tunnels,
   VPN, port-forwarding) is out of scope. dmux only needs a reachable address.
5. **One terminal per session.** A session is exactly one SSH PTY — no
   splits/panes. Navigating between devices is the point, not tiling one screen.

## Persistence

| Thing | Stored where | Survives interface disconnect | Survives server restart |
|-------|-------------|------------------------------|------------------------|
| **Hosts** | disk (registry) | yes | **yes** |
| **Sessions** | server memory (live SSH connections) | **yes** | no (connections drop) |

- Interface disconnects → server still holds the SSH connections → sessions live on.
- Server restarts → all SSH connections drop → sessions are gone, but the host
  registry is reloaded from disk and hosts reappear.

(Surviving server restart is a later possibility, not a v1 goal.)

### Host becomes unreachable

If a host drops mid-session, the server:
1. closes that host's session(s);
2. marks the host **down** in the sidebar (kept visible, not removed);
3. auto-moves any attached interface to another open session.

## Attaching & sessions

- **Multiple interfaces, shared view.** Several interfaces can attach at once and
  see the same session (tmux-style shared attach). This forces:
  - **shared resize** — the PTY size is negotiated across attached clients
    (smallest-wins, or a designated driver);
  - **input arbitration** — all attached clients' keystrokes go to the same PTY.
- **Server-side scrollback.** The server keeps a per-session scrollback buffer
  even when no interface is attached, so reattaching shows recent history, not
  just new output. (Buffer size is bounded; costs memory per session.)
- **Prefix key.** A single **configurable** prefix with an uncommon default
  (e.g. `Ctrl-Space`) to avoid clashing with tools inside the host shell — most
  importantly a real `tmux` running on a host. Everything else passes through.

## Authentication

- **SSH keys only.** No password auth.
- The server has its own SSH keypair.
- Registering a host establishes key trust (the server's pubkey is authorized on
  the host, `ssh-copy-id`-style).
- The registry stores: host address + which key to use.

## Commands

```
dmux connect <server-addr>     # one-shot registration: tell the server about this
                               # host (address + key trust), verify SSH works, exit.
                               # Nothing keeps running on the host afterward.

dmux sethome . --depth N       # server-side config for a host: project root + how
                               # deep to scan when indexing projects.

ssh dmux@<server>              # attach to the TUI from any interface.
```

### Onboarding a host with `dmux setup`

`connect` assumes a host already has a reachable `sshd` that trusts the server's
key. `dmux setup <platform>` does that preparation **on the host**, then prints
the `dmux connect …` line to run on the server. It is one-shot prep, not a daemon
— nothing keeps running afterward (decision #1 holds).

The whole flow is three steps and copies no keys:

```
1. dmux serve                    # on the server; prints the exact setup command below
2. go run github.com/Ceinl/dmux/cmd/dmux@latest setup wsl --server <server-addr>   # on each host
3. dmux connect <host-addr>      # on the server (printed by step 2)
```

`setup` needs only the server's **address**: it connects to the server and reads
the public key the server presents (the server signs both its inbound sshd and
its outbound host dials with the same keypair, so that key is exactly what the
host must trust — and it's public, so there's no secret to copy). It then
installs+configures sshd (key-only auth), authorizes that key, starts sshd, and
inspects WSL networking:

- mirrored mode → prints the ready-to-run `dmux connect <wsl-ip>:<port>`.
- NAT'd (default WSL2) → the 172.x IP isn't LAN-reachable; it explains the two
  fixes (mirrored mode, or an elevated netsh portproxy) and prints the connect
  line against the Windows host's LAN IP.

Flags: `--server <addr>` (server address; its key is fetched automatically),
`--port N` (host sshd port), `--user U` (login user), `--portproxy` (attempt the
elevated Windows portproxy step via a UAC prompt), `--dry-run` (print every step
without changing the system), `--server-key <key|@path>` (offline fallback when
the host can't reach the server). `wsl` is the only platform implemented today.

## TUI

Two regions:

1. **Main** — the active session's terminal (an SSH PTY proxied from a host).
2. **Sidebar** — list of devices; under each, its running sessions.

Primary motion is **jumping between devices** (the device is the top-level unit —
the "workspace"). Project search lives one level down, inside a device.

### prefix + p — cross-device project switch

Telescope-style fuzzy finder over **projects across all hosts**, built from each
host's `sethome` root + depth. The server walks the host over SSH to index
**on demand** (when the picker is opened), not on a background schedule.

A **project** is identified by its **root directory** — the git root if the path
is inside a repo, otherwise the directory itself (the same rule
[telescope-project.nvim](https://github.com/nvim-telescope/telescope-project.nvim)
uses). So a session's identity for matching is the pair `(host, project_root)`.

The picker mirrors telescope: a fuzzy-filter prompt over all
`(host, project_root)` entries, and selection runs an action.

Selecting a project:
- if a session already exists for that `(host, project_root)` → switch to it;
- otherwise → open a new SSH session on that host, `cd`'d to the project root.

## Data model (sketch)

```
Server
 ├── Registry (disk)
 │    └── Host { addr, key, home_path, scan_depth }
 ├── Sessions (memory)
 │    └── Session { host, pty (ssh conn), cwd/project, title, scrollback, size }
 └── Attachments (memory)
      └── Client { interface, viewing → Session }   # many clients per session
```

## Open questions

- **Shared resize policy** (undecided): smallest-attached-wins vs a designated
  driver client. Default to smallest-wins unless decided otherwise.
