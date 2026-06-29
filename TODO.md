# DMUX — Precise Implementation TODO

Exhaustive, step-by-step checklist to take dmux from pure scaffold
(15 × `panic("not implemented")`, zero deps, no TUI, no tests) to a working v1.

Every step is concrete: exact files, struct fields, method signatures, and logic.
Signatures below match the existing interfaces in the code — implementations must
satisfy them verbatim.

Legend: `[ ]` todo · `[~]` partial · `[x]` done · ⚠ decision required before coding

---

## M0 — Build setup & dependencies

### M0.1 Dependencies (`go.mod` / `go.sum`)
- [x] `go get golang.org/x/crypto/ssh` — outbound dialer + signer + PTY requests. (x/crypto v0.53.0)
- [x] Decide inbound server lib: `golang.org/x/crypto/ssh` (raw)
- [x] Decide TUI: hand-rolled ANSI.
- [x] `go get github.com/sahilm/fuzzy` (or similar) for prefix+p fuzzy matching. (v0.1.3)
- [x] Run `go mod tidy`; commit `go.sum`. (done after packages imported crypto/fuzzy)

### M0.2 Tooling
- [x] Add `Makefile`: `build`, `test`, `race`, `vet`, `lint`, `run`, `clean`.
- [x] Add `.golangci.yml`; wire `golangci-lint run` (via `make lint`).
- [x] Add CI workflow (`.github/workflows/ci.yml`: build + vet + `test -race`).
- [x] Create `internal/tui/` directory (now a full package — see M6).

---

## M1 — Config (`internal/config/config.go`)

### M1.1 `Default() Config`
- [x] Return:
  - `ListenAddr: ":2222"`
  - `DataDir:` `os.UserConfigDir()/dmux` (resolve, don't hardcode `~`)
  - `HostKeyPath:` `DataDir + "/id_dmux"`
  - `PrefixKey: "Ctrl-Space"`
  - `ScrollbackLines: 10000`
  - `ResizePolicy: SmallestWins`
  - `DialTimeout: 10 * time.Second`

### M1.2 On-disk format
- [x] Pick format: JSON (stdlib, simplest). File: `DataDir/config.json`.
- [x] Define an unexported `fileConfig` struct mirroring `Config` with json tags;
      use pointers / `omitempty` so unset fields fall back to `Default()`.
      (`ResizePolicy`/`DialTimeout` serialized as strings: "smallest"|"driver", Go duration.)

### M1.3 `Load(dataDir string) (Config, error)`
- [x] `c := Default()`, then `c.DataDir = dataDir`.
- [x] If `dataDir/config.json` exists: read, `json.Unmarshal`, overlay set fields
      onto `c`. Missing file is **not** an error → return `Default()` overlay.
- [x] Recompute `HostKeyPath` relative to `dataDir` if it was left default.
- [x] Validate: `ListenAddr` parseable (via `normalizeListen`+`SplitHostPort`),
      `ScrollbackLines > 0`, `DialTimeout > 0`. (ScanDepth lives on registry
      `HomeConfig`, not Config — validated in M2, not here.)

### M1.4 Server keypair bootstrap
- [x] `EnsureHostKey(c Config) (ssh.Signer, error)` (new func): if
      `HostKeyPath` missing, generate ed25519, write private key 0600 + `.pub`,
      `MkdirAll(DataDir, 0700)`. Else load + parse existing.

### M1.5 Prefix-key parsing
- [x] `ParsePrefix(s string) (byte, error)` (new func): map `"Ctrl-Space"`→`0x00`,
      `"Ctrl-B"`→`0x02`, etc. Used by the TUI input layer (M6.4).
      Handles `ctrl-`/`c-`/`^` prefixes + `_`/`+`/`-` separators.

### M1.6 Tests
- [x] `Default()` values; `Load` missing-file returns defaults; overlay merges;
      invalid scrollback rejected; keypair generated once then reused;
      ParsePrefix table (incl. unsupported-key error). All passing.

---

## M2 — Registry (`internal/registry/registry.go`)

Implement concrete `Registry` (only the interface exists).

### M2.1 Type
- [x] `type fileRegistry struct { path string; mu sync.RWMutex; hosts map[HostID]Host }`
- [x] `func NewFileRegistry(dataDir string) *fileRegistry` → path = `dataDir/hosts.json`.

### M2.2 `Load() error`
- [x] Lock(write). Read `path`; missing file → empty map, nil error.
- [x] `json.Unmarshal` into `[]Host`; rebuild `hosts` map keyed by `ID`.

### M2.3 `Save() error`
- [x] Lock(read). Marshal `List()` sorted by `ID`.
- [x] Atomic write: write `path+".tmp"` (0600) then `os.Rename`.

### M2.4 `List() []Host` / `Get(id) (Host, bool)`
- [x] RLock. Return **copies** (contract: reads must not leak mutable state).
      `Host` is a value type → copy is a plain assignment; still don't return the
      map's address.

### M2.5 `Add(h Host) error`
- [x] Lock. If `h.ID == ""` → generate (M2.8). Reject duplicate addr+user.
- [x] Insert, then `Save()`.

### M2.6 `Remove(id) error` — delete from map, `Save()`; error if absent.

### M2.7 Field-scoped updates
- [x] `SetHome(id, cfg HomeConfig)` — load host, set `.HomeConfig = cfg`, store,
      `Save()`. Must **not** touch `Status`/`LastSeen`.
- [x] `SetStatus(id, s Status)` — set `.Status = s`, `.LastSeen = time.Now()`,
      `Save()`. Must **not** touch `HomeConfig`.

### M2.8 ID generation
- [x] ⚠ Choose: deterministic `sha256(user@addr)[:12]` (stable, natural dedupe)
      vs random. Recommend deterministic.

### M2.9 Tests
- [x] Round-trip Load/Save; SetStatus preserves HomeConfig and vice-versa;
      List returns copies (mutating result doesn't change store); duplicate-add
      rejected; Remove (incl. absent → ErrNotFound). All passing.

> **Impl notes (deviations from plan):** concrete impl lives in a new file
> `internal/registry/file_registry.go` (kept `registry.go` as the interface decl).
> Added exported `ErrNotFound`/`ErrDuplicate`, `DeriveHostID(user, addr)`, and a
> compile-time `var _ Registry = (*fileRegistry)(nil)` assertion. `List()` is
> sorted by ID. `Save()`/`saveLocked()` split so locked callers don't re-lock.

---

## M3 — Outbound SSH (`internal/remote/remote.go`)

Implement `Dialer` + `PTY`. Highest-risk package — build against a local sshd early.

### M3.1 Dialer type
- [x] `type sshDialer struct { dataDir string; timeout time.Duration; signer ssh.Signer; hostKeys ssh.HostKeyCallback }`
- [x] `func NewDialer(dataDir string, timeout time.Duration, signer ssh.Signer) *sshDialer`

### M3.2 ⚠ Host-key verification policy
- [x] Decide known-hosts file (`dataDir/known_hosts`) vs TOFU vs InsecureIgnore
      (dev only). Implement chosen `ssh.HostKeyCallback`.

### M3.3 Building `*ssh.ClientConfig`
- [x] `User: h.User`, `Auth: []ssh.AuthMethod{ssh.PublicKeys(d.signer)}`,
      `HostKeyCallback: d.hostKeys`, `Timeout: d.timeout`.
- [x] Resolve `h.KeyRef`: if it names a per-host key in DataDir, load that signer;
      else use the shared server signer.

### M3.4 `Open(ctx, h, spec) (PTY, error)`
- [x] `ssh.Dial("tcp", h.Addr, cfg)` (honor `ctx` via dialer/Deadline).
- [x] `client.NewSession()`.
- [x] `session.RequestPty("xterm-256color", spec.Size.Rows, spec.Size.Cols, modes)`.
- [x] `StdinPipe()` / `StdoutPipe()` (merge stderr into stdout or pipe both).
- [x] Start shell:
  - if `spec.Cwd == ""` → `session.Shell()` (login shell at device root).
  - else → `session.Start("cd <quoted Cwd> && exec $SHELL -l")`.
        ⚠ shell-quote `Cwd` (spaces, special chars) via a quoting helper.
- [x] Construct and return `*sshPTY` (M3.5).
- [x] On any failure: close session/client, return error (caller marks host Down).

### M3.5 `sshPTY` implementing `PTY` (`io.ReadWriteCloser` + `Resize` + `Done`)
- [x] Fields: `sess *ssh.Session`, `client *ssh.Client`, `stdin io.WriteCloser`,
      `stdout io.Reader`, `done chan struct{}`, `closeOnce sync.Once`.
- [x] `Read(p)` → `stdout.Read(p)`.
- [x] `Write(p)` → `stdin.Write(p)`.
- [x] `Resize(s Size)` → `sess.WindowChange(int(s.Rows), int(s.Cols))`.
- [x] `Done() <-chan struct{}` → return `done`.
- [x] Spawn a goroutine: `sess.Wait()` then `close(done)` (signals host-down).
- [x] `Close()` → `closeOnce`: close stdin, `sess.Close()`, `client.Close()`,
      ensure `done` closed.

### M3.6 `Verify(ctx, h) error`
- [x] Dial + auth like Open, `NewSession`, `session.Run("true")`, close
      everything. No PTY, nothing left running. Errors propagate to `dmux connect`.

### M3.7 Keepalives
- [x] In Open, start a ticker goroutine sending `client.SendRequest("keepalive@openssh.com", true, nil)`;
      on error → `Close()` (so half-dead hosts surface via `Done()`).

### M3.8 Tests
- [x] In-process **raw x/crypto/ssh** echo server (not gliderlabs): Open→Read/Write
      echo, Resize no-error, Done closes on Close, Verify ok/reject, shellQuote
      table, TOFU host-key mismatch after key rotation. All passing.

> **Impl notes (deviations from plan):** split across new files
> `internal/remote/dialer.go` (sshDialer + TOFU known_hosts) and
> `internal/remote/pty.go` (sshPTY). Host-key policy = **self-contained TOFU
> known_hosts** (`dataDir/known_hosts`, `<host> <keytype> <base64>` lines; pin on
> first use, reject on change) — no external knownhosts dep. ctx is honored via
> `net.Dialer.DialContext` + `ssh.NewClientConn` (since `ssh.Dial` takes no ctx).
> `KeyRef` resolves to a bare filename in DataDir (path-traversal guarded), else
> falls back to the shared signer. Default PTY size 24×80 when spec.Size is zero.
> stderr left default (PTY merges it into stdout). Added compile-time
> `var _ Dialer`/`var _ PTY` assertions.

---

## M4 — Sessions (`internal/session/session.go`)

Implement `Manager` + `Scrollback`.

### M4.1 Internal types
- [x] `type liveSession struct { rec Session; pty remote.PTY; sb *ringScrollback; subs map[int]chan Event; nextSub int; mu sync.Mutex; closed bool }`
- [x] `type manager struct { mu sync.RWMutex; dialer remote.Dialer; cfg config.Config; sessions map[ID]*liveSession; byKey map[project.Key]ID; onHostDown func(registry.HostID) }`
- [x] `func NewManager(dialer remote.Dialer, cfg config.Config) *manager`
- [x] Hook for host-down notification (set by server): `SetHostDownHook(func(registry.HostID))`.

### M4.2 `ringScrollback` implementing `Scrollback`
- [x] ⚠ Decide bound: bytes (simpler). Cap ≈ `cfg.ScrollbackLines * 256` bytes.
- [x] `type ringScrollback struct { mu sync.Mutex; buf []byte; cap int }`
- [x] `write(p []byte)` — append, trim from front to keep `len ≤ cap`.
- [x] `Snapshot() []byte` — return a copy.
- [x] `Len() int` — current length.

### M4.3 `Create(spec Spec) (ID, error)`
- [x] Build `project.Key{spec.HostID, spec.Root}`; if exists in `byKey` →
      ⚠ decide: return existing ID, or error (let server `Find` handle dedupe).
      Recommend: server checks `Find` first; `Create` always creates.
- [x] Generate `ID` (M4.8).
- [x] `pty, err := dialer.Open(ctx, host, OpenSpec{Cwd: spec.Root, Size: spec.Size})`.
      ⚠ `Create` needs the `registry.Host` and a `ctx` — current signature is
      `Create(spec Spec) (ID, error)` with neither. **Resolve:** either store a
      registry ref + `context.Background()` in the manager, or change the
      signature. Document the choice.
- [x] Set `rec.State = StateStarting` → after Open success `StateRunning`
      (on Open failure `StateFailed`, return err).
- [x] Register in `sessions` + `byKey`.
- [x] Start **pump goroutine** (M4.4).

### M4.4 Pump goroutine (per session)
- [x] Loop `pty.Read(buf)`:
  - on data → `sb.write(data)` + fan-out `Event{Data: copy}` to all subs
    (non-blocking; coalesce/drop for slow subs — never block the pump).
- [x] Also `select` on `pty.Done()`:
  - mark `StateClosed`, set `closed`, emit `Event{State: StateClosed, Stopped: true}`,
    close all sub channels, remove from `byKey`.
  - call `onHostDown(rec.HostID)` so the server cascades (M9.6).

### M4.5 Lookups
- [x] `List()` — RLock, copy each `rec`.
- [x] `Get(id)` — RLock, return `rec` copy + ok.
- [x] `Find(key) (ID, bool)` — RLock `byKey`.

### M4.6 Close paths
- [x] `Close(id)` — `pty.Close()`, mark closed, emit Stopped, close subs, delete
      from `byKey`; idempotent.
- [x] `CloseHost(hostID) []ID` — find all sessions with `rec.HostID == hostID`,
      `Close` each, return their IDs.

### M4.7 I/O + resize + subscribe + scrollback accessors
- [x] `Write(id, p) (int, error)` → `pty.Write(p)`.
- [x] `Resize(id, size)` → set `rec.Size = size`, `pty.Resize(size)`.
- [x] `Scrollback(id) (Scrollback, bool)` → return the `*ringScrollback`.
- [x] `Subscribe(id) (<-chan Event, func(), error)`:
  - register a buffered channel in `subs` under a new sub-id;
  - `cancel` removes it + closes it;
  - if already closed, return a closed channel emitting final state.

### M4.8 ID generation — random hex/uuid, collision-checked under lock.

### M4.9 Tests
- [x] Create→Find dedupe; scrollback buffers + bounds; Subscribe fan-out to N
      subs; Close is intentional (no host-down hook) + ErrNotFound on 2nd call;
      CloseHost returns all IDs; host-down drop → Stopped event + onHostDown
      fired + session removed. All passing, **race-clean** (`go test -race`).

> **Impl notes (deviations/resolutions):** split into new files
> `internal/session/manager.go` + `scrollback.go`. **Resolved the M4.3 signature
> gap:** `Manager.Create(spec)` carries no ctx/Host, so the manager holds a
> `registry.Registry` (passed to `NewManager(dialer, hosts, cfg)`) and dials with
> `context.WithTimeout(Background, cfg.DialTimeout)`. Added `SetHostDownHook` (the
> server wires `onHostDown`). Distinguishes intentional `Close`/`CloseHost` (no
> hook) from unexpected PTY drop (fires hook) via a per-session `intentional`
> flag, so the server's host-down cascade can't loop. `finalize` deletes the
> session from `sessions`+`byKey` and closes all subs once. Scrollback cap =
> `ScrollbackLines * 256` bytes. Slow subscribers drop events (non-blocking
> broadcast); full history stays in scrollback. **New dep for M9:** server must
> call `NewManager` with the registry and `SetHostDownHook(s.onHostDown)`.

---

## M5 — Attachments (`internal/attach/attach.go`)

Implement `Attachments`.

### M5.1 Type
- [x] `type attachments struct { mu sync.RWMutex; clients map[ClientID]Client; driver map[session.ID]ClientID }`
- [x] `func NewAttachments() *attachments`

### M5.2 Basic ops
- [x] `List()` / `Get(id)` — RLock, copy.
- [x] `Attach(c)` — Lock, reject duplicate ID, insert.
- [x] `Detach(id)` — Lock, delete; sessions live on regardless (SPEC).
- [x] `SetViewing(id, s)` — Lock, set `Client.Viewing = s` (validate exists in store).
- [x] `SetSize(id, size)` — Lock, set `Client.Size = size`.

### M5.3 `NegotiatedSize(s session.ID, policy config.ResizePolicy) (remote.Size, bool)`
- [x] Gather all clients with `Viewing == s`. If none → `(Size{}, false)`.
- [x] `SmallestWins` → min `Rows` and min `Cols` independently across viewers.
- [x] `Driver` → ⚠ **SPEC open question.** Need driver selection. Add
      `SetDriver(s session.ID, c ClientID)` + the `driver` map; if no driver set,
      fall back to SmallestWins. Use the driver client's `Size`.

### M5.4 Tests
- [x] Smallest-wins math across 3 viewers; viewer set change recomputes; detach
      drops a client from the min; Driver follows designated client + falls back
      when unset/detached; no-viewer → `false`; List returns copies. Race-clean.

> **Impl notes:** concrete impl in new file `internal/attach/attachments.go`.
> **Resolved the Driver open question:** added `SetDriver(s, id)` + a `driver`
> map; `NegotiatedSize(Driver)` uses the driver's own size only when that client
> is still attached AND currently viewing the session, else falls back to
> smallest-wins. `Detach` also clears any driver designations the client held.
> Zero-sized clients are ignored in negotiation (`valid()` guard). `SetDriver`
> is an extra method on the concrete type (not in the `Attachments` interface) —
> M9/M6 callers that use it need the concrete `*attachments` or an extended iface.

---

## M6 — TUI (NEW `internal/tui/`) — missing entirely

Implements `sshd.Handler`. Drives rendering + input for each attached interface.

### M6.0 Package + model
- [x] Create `internal/tui/tui.go`, package `tui`.
- [x] `type TUI struct { srv *server.Server; sessions session.Manager; clients attach.Attachments; cfg config.Config }`
      ⚠ avoid an import cycle with `server` — TUI may need a narrow interface,
      not the concrete `*server.Server`. Define a `Controller` interface in `tui`
      with just `Jump/Projects/OpenProject/...` and have the server satisfy it.
- [x] `func New(...) *TUI`

### M6.1 `Handle(ctx, c sshd.Conn)` (satisfies `sshd.Handler`)
- [x] Build `attach.Client{ID: c.ClientID(), Interface: c.Interface(), Size: <initial>}`.
- [x] `clients.Attach(client)`; `defer clients.Detach(id)`.
- [x] Pick an initial session to view (first running session, or empty state).
- [x] Start the render loop + input loop + resize loop; return when `c` closes.

### M6.2 Layout
- [x] Split screen: **Sidebar** (left, fixed width ~24 cols) + **Main** (rest).
- [x] Sidebar: list hosts from `srv`/registry; nest each host's sessions under it;
      mark **Down** hosts greyed but visible (SPEC).
- [x] Main: the viewed session's terminal output.

### M6.3 Main render pipeline
- [x] On (re)attach/jump: write `sessions.Scrollback(id).Snapshot()` to `Main`.
- [x] `events, cancel, _ := sessions.Subscribe(id)`; stream `Event.Data` into Main.
- [x] On `Event.Stopped` → host-down/closed: server auto-moves the client; reflect
      the new viewed session.
- [x] `cancel()` the old subscription on every jump.

### M6.4 Input pipeline + prefix key
- [x] Read keystrokes from `c.Read`.
- [x] Maintain a small state machine: normal vs "prefix pressed".
- [x] If byte == parsed prefix (M1.5) → enter prefix mode (consume it).
- [x] In prefix mode, handle commands then return to normal:
  - sidebar nav / device jump → `srv.Jump`
  - `p` → open project picker (M6.6)
  - `d` → detach (close conn)
  - others → configurable
- [x] **Everything else passes through** → `sessions.Write(viewing, bytes)`
      (critical so a real `tmux` on the host still works).

### M6.5 Resize loop
- [x] `for sz := range c.Resizes()`: `clients.SetSize(id, sz)` then ask server to
      renegotiate (`srv` recomputes `NegotiatedSize` + `sessions.Resize`).
- [x] Re-layout sidebar/main on local size change.

### M6.6 prefix+p project picker
- [x] On open: `projs, _ := srv.Projects(ctx)`.
- [x] Telescope-style modal: prompt input + filtered list; fuzzy-match query
      against `host:Root`/`Name` (M0 fuzzy lib).
- [x] On select → `srv.OpenProject(ctx, id, proj)`; jump main to returned session.
- [x] Esc closes the picker without action.

### M6.7 ⚠ Framework decision (M0.1) drives whether this is bubbletea models or a
      hand-rolled ANSI renderer. Keep a first cut **minimal**: proxy one session,
      no sidebar/picker, then layer M6.2/M6.6.
- [x] **Decided: hand-rolled ANSI.** Picker + list overlays implemented (not deferred).
- [x] **v2 REWRITE — tmux-style sidebar (split, not overlay).** Now an always-visible
      right sidebar with the main pane composited so they coexist (see note below).

> **v2 impl notes (sidebar rewrite):** the main pane is now composited through a
> **terminal emulator** (`github.com/hinshun/vt10x`) so the host's PTY stream is
> contained to its rectangle and a sidebar can live beside live output — the
> thing the overlay-only v1 couldn't do. Built on the **Plumtree tui-runtime**
> (`github.com/Ceinl/plumtree/tui-runtime`, local `replace` in go.mod): its
> `screen` diff-renderer targets the `sshd.Conn` via `NewScreenWithOutput`,
> `Div`/`Button` give a flexbox `Row` = [pane(Grow) | sidebar(Px 26)], and
> `keyboard.ListenReader(conn)` parses keys + SGR mouse. Files: `pane.go` (vt10x
> Component), `sidebar.go` (clickable session `Button`s + collapse toggle),
> `render.go` (event routing + frame render + key `encode`), `tui.go` (loop).
> **Mouse:** click a session row → jump; click `< sessions` / `prefix c` →
> collapse to a 1-col sliver. Keys forwarded to the shell are re-encoded from
> decoded events (the parser drops raw bytes). **Live-verified over SSH**: shell
> renders in the pane, sidebar shows the session row, no panic, clean detach.
> **Bug found+fixed during verification:** host `Down` status was persisted and
> survived restart, so auto-attach skipped the host forever — server now resets
> status to Unknown on startup and marks Up on a successful dial.

> **Impl notes (significant design deviation — read this):** new package
> `internal/tui` (files `tui.go`, `stream.go`, `input.go`, `overlay.go`).
> **M6.2 side-by-side sidebar+main split is NOT implemented as a split.** Reason:
> faithfully embedding a host's raw PTY byte-stream inside a sub-rectangle needs a
> full vt100 emulator (the stream carries cursor/clear escapes aimed at the whole
> screen). v1 instead renders the **active session full-screen (pass-through)**
> and surfaces navigation as **full-screen overlays** on the prefix key:
>   - `prefix p` → cross-device project picker (fuzzy via sahilm/fuzzy)  ✔ M6.6
>   - `prefix l`/`s` → running-session list → jump  ✔ (covers M6.2 navigation)
>   - `prefix r` → rename the current session
>   - `prefix k` → pick a device → manage it: color (persisted; "Auto" clears),
>     rename (display name; empty clears), or set project-finder home dir
>   - `prefix d` → detach; `prefix prefix` → send a literal prefix byte to the host
> Down hosts greyed in a side panel (M6.2) is therefore deferred with the split.
> **Import cycle (M6.0):** TUI declares its own `Controller` interface (Jump/
> Projects/OpenProject); the server satisfies it structurally — tui does NOT
> import server. Single input goroutine routes bytes by mode
> (stream/prefix/overlay); a per-conn mutex guards all writes to the connection so
> the output pump and overlay renders don't interleave. Arrow keys parsed via a
> small ESC-`[`-`A/B` state machine; Ctrl-N/Ctrl-P also navigate. Resize →
> `clients.SetSize` + local `NegotiatedSize`→`sessions.Resize` (TUI renegotiates
> directly; no extra controller method). Tests: overlay refilter/move, stream
> passthrough, literal-prefix, detach, list→jump, picker→open. Race-clean.

---

## M7 — Inbound SSH (`internal/sshd/sshd.go`)

Implement `Server` + `Conn`. `ssh dmux@<server>` → TUI.

### M7.1 Server type
- [x] `type sshServer struct { addr string; signer ssh.Signer; authorized func(ssh.PublicKey) bool; ln net.Listener }`
- [x] `func NewServer(addr string, signer ssh.Signer, authorized func(ssh.PublicKey) bool) *sshServer`

### M7.2 ⚠ Inbound auth policy
- [x] Decide which interface pubkeys may attach: `dataDir/authorized_keys` file
      parsed into the `authorized` predicate. **No password auth** (SPEC).

### M7.3 `Serve(ctx, h Handler) error`
- [x] Bind `addr` (store listener for Close).
- [x] Accept loop until ctx cancelled; for each conn, in a goroutine:
  - SSH handshake with `signer` + pubkey auth via `authorized`;
  - accept a `session` channel; require a `pty-req` (capture term + size);
  - track `window-change` requests → feed `Resizes()`;
  - build `sshConn` (M7.4) and call `h.Handle(ctx, conn)`.
- [x] (If using `gliderlabs/ssh`, most of this is the `ssh.Server` handler +
      `PublicKeyHandler` + `s.Pty()`.)

### M7.4 `sshConn` implementing `Conn`
- [x] Fields: channel/`ssh.Session`, assigned `ClientID`, source addr, resize chan.
- [x] `ClientID() attach.ClientID` — generate per connection (uuid).
- [x] `Interface() string` — `RemoteAddr().String()` (+ key comment if available).
- [x] `Read`/`Write` — to the SSH channel (stdin/stdout).
- [x] `Resizes() <-chan remote.Size` — fed from window-change handler.
- [x] `Close()` — close channel/session.

### M7.5 `Close()` — close listener; accept loop exits on ctx/listener error.

### M7.6 Tests
- [x] Authorized key accepts; unauthorized rejected; pty-req initial size parsed;
      echo round-trip; window-change → Resizes emits; Close stops Serve;
      AuthorizedKeysFile(missing) → deny-all + ok=false. Race-clean.

> **Impl notes:** new files `internal/sshd/server.go` + `conn.go`. `AuthFunc =
> func(ssh.PublicKey) bool` gates attach; `AuthorizedKeysFile(path)` parses an
> OpenSSH authorized_keys into an AuthFunc (missing file → deny-all + `ok=false`
> so the CLI can warn). Added `Addr() net.Addr` so `:0` binds are discoverable
> (used by tests + serve logging). pty-req/window-change parsed via `ssh.Unmarshal`
> into `ptyReqPayload`/`winChPayload`; the **initial pty-req size is pushed as the
> first Resizes value**. `pushResize` is non-blocking (drops if buffer full) and
> recovers if the resize chan was closed during shutdown. We accept `shell`/`env`
> requests even though the TUI (not a host shell) renders into the channel.
> ctx cancel closes the listener so Accept unblocks → clean Serve return.

---

## M8 — Project indexer (`internal/project/project.go`)

Implement `Indexer` + `KeyOf`.

### M8.1 `KeyOf(p Project) Key` — `return Key{p.HostID, p.Root}`.

### M8.2 Indexer type
- [x] `type sshIndexer struct { dialer remote.Dialer }` (reuse outbound SSH) or a
      thin exec-over-ssh helper.
- [x] `func NewIndexer(dialer remote.Dialer) *sshIndexer`

### M8.3 `Index(ctx, h registry.Host) ([]Project, error)`
- [x] Run a remote command under `h.HomeConfig.Root` bounded by `ScanDepth`:
  - find git roots: `find <Root> -maxdepth <ScanDepth> -type d -name .git -prune`
    → parent dir is a project root.
  - find non-git leaf dirs at depth (telescope rule: dir itself if not in a repo).
- [x] Dedupe to roots; for each → `Project{HostID: h.ID, Root: abs, Name: base(abs)}`.
- [x] ⚠ Performance: cap results / time; quote `Root`; handle large trees.
- [x] Down host → return `(nil, err)`; server skips it (M9.8), doesn't fail all.

### M8.4 Tests
- [x] Fake `Runner` returning canned `find` output → expected Projects; git-root
      vs plain-dir rule; dedupe (app as both repo + depth-1); excludes dirs inside
      repos; basename naming; HostID propagated; no-root → error; insideAnyRepo
      prefix-safety. All passing.

> **Impl notes (deviations from plan):** new file `internal/project/indexer.go`;
> implemented the existing `KeyOf` stub in `project.go`. **remote.Dialer left
> unchanged** — instead added a `Run(ctx, h, cmd) ([]byte, error)` method to the
> concrete `*remote.sshDialer` and a minimal `project.Runner` interface the
> indexer depends on (NewIndexer takes a Runner, not a Dialer). **Project rule
> (v1, documented):** git repo roots under Root within ScanDepth ∪ depth-1
> subdirs of Root not inside any repo (the "dir itself if not in a repo" case) —
> two `find` commands combined + deduped in Go, rather than one clever find.
> git-roots find uses `maxdepth ScanDepth+1` (so a repo AT depth N is caught via
> its `.git`). Roots sorted for stable output.

---

## M9 — Server wiring (`internal/server/server.go`)

All 9 methods are stubs. Orchestrates everything.

### M9.1 `New(cfg, hosts, sessions, clients, dialer, indexer, inbound) *Server`
- [x] Store all collaborators in fields (struct already declared).
- [x] Register host-down hook: `sessions.SetHostDownHook(s.onHostDown)` (M4.1).

### M9.2 `Run(ctx) error`
- [x] `hosts.Load()`.
- [x] Construct the TUI handler (M6) — ⚠ needs the controller interface to avoid a
      cycle.
- [x] `go inbound.Serve(ctx, tuiHandler)`.
- [x] Block on `<-ctx.Done()`.
- [x] Shutdown: `inbound.Close()`, close all sessions, return ctx err (or nil).

### M9.3 `Connect(ctx, h registry.Host) error`
- [x] `dialer.Verify(ctx, h)`; on success `hosts.Add(h)`; set `StatusUp`.
- [x] ⚠ **Key trust:** the server pubkey must be authorized on the host
      (ssh-copy-id style). Needs one-time host credentials. **Define this flow**
      (the connect CLI collects it, or doc that the user pre-authorizes the key).

### M9.4 `SetHome(id, cfg registry.HomeConfig) error` → `hosts.SetHome(id, cfg)`.

### M9.5 `onHostDown(id registry.HostID)`
- [x] `ids := sessions.CloseHost(id)`.
- [x] `hosts.SetStatus(id, registry.StatusDown)`.
- [x] For each client whose `Viewing ∈ ids`: pick another open session (any
      `StateRunning`) → `clients.SetViewing(client, other)`; renegotiate size.
      If none remain, leave on an empty state.

### M9.6 Wire PTY death → onHostDown
- [x] Already routed via the manager's host-down hook (M4.4/M9.1). Verify the
      cascade fires exactly once per host.

### M9.7 `Jump(c attach.ClientID, target session.ID) error`
- [x] Validate `sessions.Get(target)` exists + running.
- [x] `clients.SetViewing(c, target)`.
- [x] Renegotiate: `sz, ok := clients.NegotiatedSize(target, cfg.ResizePolicy)`;
      if ok → `sessions.Resize(target, sz)`.

### M9.8 `Projects(ctx) ([]project.Project, error)`
- [x] For each host in `hosts.List()` where `Status != StatusDown`:
      `indexer.Index(ctx, h)` (⚠ parallelize with a bounded errgroup).
- [x] Aggregate; dedupe by `project.KeyOf`; skip per-host errors (log, continue).

### M9.9 `OpenProject(ctx, c, p project.Project) (session.ID, error)`
- [x] `key := project.KeyOf(p)`.
- [x] If `id, ok := sessions.Find(key); ok` → `Jump(c, id)`; return `id`.
- [x] Else `id, err := sessions.Create(Spec{HostID: p.HostID, Root: p.Root, Title: p.Name, Size: <viewer size>})`.
- [x] `Jump(c, id)`; return `id`.

### M9.10 Tests
- [x] OpenProject: existing key jumps (no new session) vs new key creates +
      jumps + renegotiates; onHostDown closes + marks Down + moves viewers to a
      survivor; Jump validates target (unknown/non-running) + renegotiates;
      Projects skips Down hosts + dedupes; Connect verify→add, verify-fail→no add.
      All passing (fakes for sessions/dialer/indexer; real registry+attach).

> **Impl notes (deviations/resolutions):** **import-cycle fix (M6.0/M9.2):** the
> server does NOT import `tui`. It holds a `handler sshd.Handler` set via a new
> `SetHandler` method; `main` constructs the TUI with the server as controller,
> then calls `SetHandler` (Go interfaces are structural, so the TUI's Controller
> interface needs no server import either). **Host-down wiring:** `New` does a
> type-assertion `sessions.(interface{ SetHostDownHook(...) })` so the
> `session.Manager` interface stays unwidened. `Run` runs `inbound.Serve` in a
> goroutine, blocks on ctx, then closes inbound + all sessions. `Connect` sets
> the host `StatusUp` after a successful verify+add. Resize renegotiation is a
> shared `renegotiate()` helper used by Jump/OpenProject/onHostDown. **Projects
> is sequential** (not the planned bounded errgroup) for v1 simplicity — per-host
> errors are skipped. **Still TODO (cross-cutting):** `Connect` does not yet push
> the server pubkey into the host's authorized_keys (key-trust establishment is
> still open — see Cross-cutting), and there is no control transport for the CLI
> to call Connect/SetHome remotely (M10.3).

---

## M10 — CLI entrypoint (`cmd/dmux/main.go`)

`main` + `runServe` + `runConnect` + `runSetHome` are stubs. Note: the file's doc
comment lists a `serve` command but no `runServe`-to-`main` dispatch exists yet —
reconcile the command table.

### M10.1 `main()`
- [x] Signal-aware ctx: `signal.NotifyContext(ctx, SIGINT, SIGTERM)`.
- [x] Dispatch `os.Args[1]`: `serve|connect|sethome` → the run funcs; unknown →
      usage + exit 2. On run error → print + exit 1.

### M10.2 `runServe(ctx, args)`
- [x] Flags: `--data-dir`, `--config`, `--listen`.
- [x] `cfg, _ := config.Load(dataDir)`; `signer := config.EnsureHostKey(cfg)`.
- [x] Construct concretes: `registry.NewFileRegistry`, `remote.NewDialer`,
      `session.NewManager`, `attach.NewAttachments`, `project.NewIndexer`,
      `sshd.NewServer`.
- [x] `srv := server.New(...)`; `return srv.Run(ctx)`.

### M10.3 ⚠ Control transport (blocks connect + sethome)
- [x] **Design gap:** `connect`/`sethome` run on a host/interface but mutate
      **server** state, and the inbound sshd only lands you in the TUI. Decide:
      (a) a dedicated SSH subsystem/admin channel, (b) a separate admin listener,
      or (c) a TUI-driven flow. **Resolve before M10.4/M10.5.**

### M10.4 `runConnect(ctx, args)` — `dmux connect <server-addr>`
- [x] Parse `<server-addr>` + this host's addr/user/keyref.
- [x] Send a register request to the server over the chosen control transport →
      server runs `Connect` (`Verify` + `Add`). Print result, exit.

### M10.5 `runSetHome(ctx, args)` — `dmux sethome . --depth N`
- [x] Resolve `.` → abs path; parse `--depth N`.
- [x] Send to server via control transport → `server.SetHome`. Print, exit.

### M10.6 Document `ssh dmux@<server>` needs no subcommand (handled by sshd/TUI).

### M10.7 Tests — arg dispatch table; flag parsing; usage/exit codes.
- [~] Manual smoke tests done (help/usage, unknown-cmd exit 2, serve binds +
      bootstraps keypair, sethome validation, connect missing-addr). **No
      automated `cmd/dmux` tests yet** — main is thin wiring; logic is tested in
      the packages it calls. Worth adding a dispatch/flag-parse test later.

> **Impl notes (deviations/resolutions):** `main` dispatches serve/connect/
> sethome with `signal.NotifyContext`. **M10.3 control transport NOT built (v1
> deviation):** `connect`/`sethome` are **server-side admin commands** — run on
> the server box, mutating the on-disk registry directly (connect builds a server
> with only dialer+registry, nils for the rest, and calls `srv.Connect`;
> sethome calls `reg.SetHome`). The SPEC's host-initiated `dmux connect
> <server-addr>` is deferred until a control channel exists. Added `hoistFlags`
> so flags may follow the positional arg (Go's `flag` stops at the first
> positional, but SPEC writes `sethome . --depth N`). `connect` defaults `--user`
> to the current user and appends `:22` if no port. `sethome` requires an
> absolute host path + an existing `--host` id. serve warns if no
> `authorized_keys` (nobody can attach until one exists).

---

## Cross-cutting decisions

Status after the v1 implementation pass:

- [x] ⚠ **Inbound auth policy** — RESOLVED: `authorized_keys` file in DataDir →
      `sshd.AuthorizedKeysFile` AuthFunc; pubkey-only (M7.2).
- [x] ⚠ **Outbound host-key verification** — RESOLVED: self-contained TOFU
      known_hosts in DataDir (M3.2).
- [x] ⚠ **Resize Driver selection** — RESOLVED: explicit `SetDriver` + smallest-
      wins fallback (M5.3).
- [x] ⚠ **Scrollback bound** — RESOLVED: byte cap = ScrollbackLines×256 (M4.2).
- [x] ⚠ **`session.Create` signature** — RESOLVED: manager holds the registry +
      dials with `ctx=Background+DialTimeout`; signature unchanged (M4.3).
- [x] ⚠ **server↔tui import cycle** — RESOLVED: `tui.Controller` interface +
      server `SetHandler`; tui does not import server (M6.0).
- [x] **Concurrency discipline** — every package mutex-guarded; whole suite is
      **race-clean** under `go test -race ./...`.
- [x] **Server-restart session persistence** — left unbuilt (explicit v1 non-goal).
- [ ] ⚠ **Control transport** for connect/sethome (M10.3) — STILL OPEN. v1 ships
      them as server-side admin commands; remote/host-initiated invocation needs a
      control channel (a dedicated SSH subsystem or admin listener).
- [ ] ⚠ **Key-trust establishment** on `connect` (M9.3) — STILL OPEN. `Connect`
      verifies SSH + key trust and registers, but does NOT yet push the server
      pubkey into the host's `authorized_keys` (that needs one-time host creds,
      ssh-copy-id style). For now the operator authorizes the server key manually.

---

## Suggested build order

1. M1 config + M2 registry (pure, fast, fully testable).
2. M3 dialer/PTY against a local sshd (riskiest — do early).
3. M4 sessions + M5 attachments on top of the dialer.
4. M7 inbound sshd + a **minimal** M6 TUI (proxy one session, no sidebar/picker).
5. M9 server wiring; M10.1/M10.2 `serve` to launch end-to-end.
6. M8 indexer + M6.6 prefix+p picker (cross-device feature) last.
7. Resolve M10.3 control transport before M10.4/M10.5 connect/sethome.
