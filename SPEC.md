# kran — engineering specification

> kran moves heavy dev jobs (tests, builds, type checks, docker builds, GPU runs)
> to whichever of your machines has room, transparently, and never makes a job
> worse than running it locally.

This document is the contract every package is built against. If code and
spec disagree, fix one of them in the same change.

Module: `github.com/hafencity-dev/kran` · Go 1.25 · pure Go (CGO_ENABLED=0) ·
targets linux/darwin × arm64/amd64.

---

## 1. Product principles

1. **Never worse than local.** Any kran failure *before the user's command
   starts* falls back to running the command locally (with the local memory
   guard). kran is never on the critical path in a way that loses or hangs work.
   The daemon, the UI and the scheduler are optimizations, not dependencies.
2. **Plug and play, KISS.** `curl … | sh` → `kran setup` → `kran link devmac`.
   No config files required. No open ports. No new credentials to manage.
   Every default is the right one for 90% of people.
3. **Same result as local.** A job produces the same outcome it would locally:
   same OS (Linux jobs run in a Linux container on a Mac), pinned toolchains,
   same absolute paths where possible, preserved mtimes, same exit code.
4. **Transparent for agents (AX).** Output is passed through byte-for-byte;
   kran speaks only on stderr, one line, prefixed `kran:`. Exit codes are the
   command's own; kran's own failures use documented codes. A Claude Code hook
   can wrap heavy commands automatically, so agents need to know nothing.
5. **Observable.** Every decision has a human-readable reason ("waiting: needs
   6 GiB, devmac has 3.1 GiB free"). `kran status`, `kran top`, `kran ui`.
6. **Bulletproof.** Leases, supervisors and reapers guarantee no orphan
   processes, no leaked slots, no stuck queue, after any crash or network cut.
   Chaos tests prove it.

## 2. Glossary

| Term | Meaning |
|---|---|
| node | a machine running `kran daemon` |
| mesh | the set of nodes linked to each other (stored per node in `peers.toml`) |
| origin | the node where `kran run` was invoked (where the code lives) |
| target | the node that executes a job (may equal origin) |
| runtime | how a target executes: `native` (host OS) or `container` (Linux container via the node's Docker) |
| slot | a workspace directory on a target holding a synced copy of a repo |
| owner | who submitted a job (a Claude/Codex session, a human shell, CI) |
| fingerprint | stable key of "the same job" used to learn estimates |

## 3. Architecture

```
 kran run ──unix socket──▶ kran daemon (origin) ──ssh+yamux──▶ kran daemon (target)
   │  (frames)               │ scheduler, store, peers          │ execenv: slots, sync,
   │                         │ local API + UI (127.0.0.1:7270)  │ runtimes, supervisor
   └─ local exec path        └─ metrics                         └─ metrics
```

* One binary `kran`. Long-running role: `kran daemon` (systemd user unit /
  launchd LaunchAgent). Everything else is a short-lived CLI.
* **CLI ↔ local daemon:** HTTP over unix socket
  `$XDG_RUNTIME_DIR/kran/kran.sock` (fallback `~/.local/state/kran/kran.sock`;
  macOS `~/Library/Application Support/kran/kran.sock`). Same handler also
  served on `127.0.0.1:7270` for the UI (see §9 for auth).
* **daemon ↔ daemon:** one SSH connection per peer (Go `x/crypto/ssh` client),
  authenticated with kran's own key `~/.config/kran/id_ed25519`. On the peer,
  `authorized_keys` pins that key to a forced command:
  `command="<abs path>/kran serve --stdio --peer <name>",restrict ssh-ed25519 … kran@<node>`.
  `--peer` is the name the key was linked as; the bridge passes it to the
  daemon ahead of the yamux bytes and the session is bound to it (the
  peer's own hello cannot claim another linked node's name).
  `kran serve --stdio` connects stdin/stdout to the peer daemon's peer socket
  (`kran-peer.sock`), so all traffic is multiplexed by **yamux** over that one
  stdio stream. The kran key can do nothing except speak kran protocol.
  Host keys are pinned at link time (from the user's `known_hosts`) and stored
  in `peers.toml`.
* Connections are bidirectional when both sides can reach each other; each
  side dials its own outbound connection (no reliance on reverse tunnels).

### Package layout

| Package | Purpose |
|---|---|
| `cmd/kran` | main |
| `internal/proto` | shared wire + API types (this spec, §7–§8) |
| `internal/paths` | all filesystem locations per OS |
| `internal/version` | build info |
| `internal/transport` | ssh dialer, yamux session, `serve --stdio` bridge, peer listener, in-memory pipe transport for tests |
| `internal/daemon` | daemon process: local API server (unix+tcp), event bus, peer manager, job orchestration (origin + target side), lease heartbeats, reattach |
| `internal/client` | Go client for the local API used by CLI and TUI (state, events, run session, cancel, drain…) |
| `internal/execenv` | target-side execution: slots, manifest apply, blob cache, deps setup, runtimes (native/container), supervisor, pty, process-tree kill, resource caps, reaper |
| `internal/workspace` | origin-side: repo root/ID detection, file scan, manifest with hash cache, toolchain detection, project config `.kran.toml` |
| `internal/scheduler` | queue, fair share, admission, placement scoring, reasons |
| `internal/metrics` | node probe (static info) + live metrics, linux + darwin + docker VM + nvidia |
| `internal/estimate` | fingerprints, learned estimates (cpu, mem, duration, per-node speed) |
| `internal/store` | sqlite (modernc.org/sqlite), migrations, job history |
| `internal/ui` + `web/` | embedded web dashboard (Vite + Preact + TS, built to `internal/ui/dist`, committed) |
| `internal/tui` | `kran top` (bubbletea) |
| `internal/cli` | all cobra commands, output style, `run` (spec building, env filter, owner detection, local fallback) |
| `internal/config` | `config.toml`, `peers.toml` load/save (atomic), defaults |
| `internal/link` | `kran link/unlink`: ssh bootstrap, binary push, key exchange, reverse reachability |
| `internal/service` | systemd user unit / launchd agent install, start, stop, status |
| `internal/agents` | `kran agents install`: Claude skill, CLAUDE.md/AGENTS.md block, Claude PreToolUse hook (`kran hook claude`) |
| `internal/doctor` | `kran doctor [--fix]` checks |
| release | goreleaser, `install.sh`, GitHub Actions CI |

Packages talk through the Go interfaces in §10. No package reaches into
another's internals.

## 4. Files and locations (`internal/paths`)

| What | Linux | macOS |
|---|---|
| config dir | `$XDG_CONFIG_HOME/kran` (`~/.config/kran`) | `~/.config/kran` (same, deliberate: one place to look) |
| state dir (db, logs, slots, blobs) | `$XDG_STATE_HOME/kran` (`~/.local/state/kran`) | `~/Library/Application Support/kran` |
| runtime dir (sockets, locks) | `$XDG_RUNTIME_DIR/kran` else state dir | state dir |
| cache dir (binaries for other platforms) | `~/.cache/kran` | `~/Library/Caches/kran` |

Files: `config.toml`, `peers.toml`, `id_ed25519[.pub]`, `kran.db`,
`kran.sock`, `kran-peer.sock`, `daemon.log` (rotated, 10 MB × 3),
`jobs/<id>/` (spool, status), `slots/`, `blobs/`, `bin/<os>-<arch>/kran`.

## 5. Configuration (all optional)

### `~/.config/kran/config.toml`
```toml
name = "gx10"                 # node name; default: short hostname, lowercased
[accept]                      # what this node offers to others
enabled = true
reserve_cpus = 2              # never scheduled; owner's headroom
reserve_mem = "8GiB"
max_jobs = 0                  # 0 = unlimited (bounded by resources)
nice = 10                     # remote jobs run niced
[submit]
prefer = "auto"               # auto | local | remote
[ui]
listen = "127.0.0.1:7270"     # set "" to disable; "tailscale" binds tailscale IP
[runtime.container]
enabled = "auto"              # auto = when docker reachable and needed
```

### `peers.toml` (written by `kran link`, never edited by hand)
```toml
[[peer]]
name = "devmac"
id = "…"                      # node ID (random 16-byte hex, generated at setup)
addresses = ["100.64.0.2:22", "192.168.1.20:22"]  # tried in order, fastest wins and is remembered
user = "acme"
host_key = "ssh-ed25519 AAAA…"
kran_path = "/Users/dev/.local/bin/kran"
direction = "both"            # both | outbound | inbound
linked_at = 2026-10-22T12:00:00Z
```

### `.kran.toml` in a repo (optional; auto-detection covers the common case)
```toml
os = "same"                   # same | any — "any" lets Linux jobs run natively on macOS
docker = true                 # false: container jobs get no Docker socket (SPEC §9)
[sync]
include = ["vendor-bin/"]      # extra untracked/ignored paths to send
exclude = ["testdata/huge/"]
[env]
forward = ["DATABASE_URL"]    # extra env vars to forward (secrets are dropped otherwise)
set = { CI = "1" }
[setup]                       # runs on the target when the key files change
run = "pnpm install --frozen-lockfile"
key = ["pnpm-lock.yaml"]
[[rule]]                      # per-command hints; first match wins
match = "go test*"
cpus = 8
mem = "6GiB"
```

## 6. Job lifecycle

### 6.1 `kran run [flags] -- <cmd…>` (also `kran run --sh '<shell string>'`)

Flags: `-l/--local`, `-r/--remote`, `-n/--node NAME`, `--gpu`, `--cpus N`,
`--mem SIZE`, `--any-os`, `-i/--stdin` (forward stdin to remote),
`-q/--quiet`, `--json-events` (machine-readable progress on stderr),
`--priority high|normal|low`, `--label k=v`, `--no-fallback`.

1. CLI resolves cwd, workspace (git toplevel; else cwd), owner (§6.4),
   env (§6.5), tty info, flags + `.kran.toml` rules → `proto.JobSpec`.
2. CLI dials the local daemon. **If the daemon is unreachable** (or answers
   with an incompatible version), the CLI runs the command locally right away
   (`kran: daemon not running — running locally (start it: kran service start)`),
   unless `--remote`/`--no-fallback` → exit 125.
3. CLI opens a run session (`POST /api/v1/run`, upgraded to a framed stream,
   §8.2) and sends `submit`.
4. The scheduler queues the job, then places it. While queued the daemon
   sends `notice` frames with the wait reason (CLI prints at most one line per
   reason change, only when the wait exceeds 2 s, unless `--quiet`).
5. **Local placement:** daemon replies `decision{mode:"local", envelope}`.
   The CLI executes the command itself as a child (on a pty that mirrors
   the terminal, so colors/progress and interactive programs work), inside a transient systemd scope in `kran.slice`
   on Linux (memory cap from envelope) or its own process group on macOS,
   reports `started{pid, scope}` and later `exit{code, usage}`, and copies
   the output to the daemon for the history (§8.2). If the CLI
   dies, the socket closes, the daemon kills the scope/process group and
   frees the slot.
6. **Remote placement:** daemon replies `decision{mode:"remote", node,
   runtime}`; the origin daemon then:
   a. scans the workspace (`workspace.Scan`, hash cache) → manifest;
   b. asks the target to acquire a slot and diff the manifest → missing blob
      hashes; uploads missing blobs (zstd); target commits the slot
      (writes/deletes files, restores modes + mtimes) — phase `syncing`;
   c. target runs project setup if its key changed — phase `setup`;
   d. target starts the job under a supervisor — phase `running`;
   e. output frames stream target → origin daemon → CLI; stdin/resize/signal
      flow back; origin daemon sends heartbeats every 2 s;
   f. `exit` frame → CLI exits with the same code.
   Any failure in a–c (before `running`) → automatic local fallback (if local
   is eligible), announced on one stderr line. After `running` starts, an
   infrastructure failure ends the job with exit 125 and a clear message;
   no automatic re-run.
7. Ctrl-C in the CLI → `signal{INT}` forwarded; a second Ctrl-C within 2 s
   → `signal{KILL}`. Closing the CLI connection = cancel.

### 6.2 Leases, supervisor, reaper (bulletproofing)

* Every remote job runs under `kran internal supervise` (a detached child of
  the target daemon, own session): it owns the process group (Linux native:
  inside a transient systemd scope `kran-job-<id>.scope` in `kran.slice`;
  container: `docker run --name kran-job-<id> --label kran.job=<id>`), writes
  output to `jobs/<id>/out` (append-only, framed: stream byte + length +
  data), writes `jobs/<id>/status.json` on exit, and watches
  `jobs/<id>/lease` (mtime). Lease not renewed for **15 s** → SIGTERM the
  tree, SIGKILL after 5 s, status `lost`.
* The target daemon renews the lease whenever it receives an origin
  heartbeat. If the target daemon restarts, supervisors keep running; the new
  daemon re-indexes `jobs/*/` and the origin reattaches (`reattach{job,
  offset}`) within the lease window and output continues from the offset.
* The origin daemon persists active remote jobs; on restart it reattaches.
  The CLI, if its daemon connection drops mid-run, retries the connection for
  10 s and resumes via `reattach`; otherwise it exits 125.
* Reaper (every 30 s, and at daemon start): kills scopes/containers/process
  groups labelled kran whose job is not active; deletes job dirs older than
  7 days; trims blob cache (LRU, cap default 20 GiB or 10% of disk) and
  slots unused for 3 days; enforces disk floor (refuse new remote jobs below
  max(10 GiB, 5%) free with reason `disk`).
* Process-tree kill: Linux = scope stop (cgroup kill); macOS native = kill
  process group, then sweep descendants by ppid walk; container =
  `docker kill`. Also: docker containers started *by the job* with label
  `kran.job` are removed (we export `KRAN_JOB_ID` so tools can label).

### 6.3 Placement policy (`internal/scheduler`)

Eligibility filters, each failing filter gives a reason string:
* node online, not draining, accept.enabled;
* `--local` → origin only; `--node` → that node only (a name that is
  neither this machine nor in `peers.toml` is a usage error, exit 2; a
  linked node that is offline makes the job wait for it, and the CLI says
  so right away);
* OS: `same` → target runtime OS == origin OS (a darwin node offers
  `container` runtime = linux when Docker is reachable); `any` → any;
* GPU: `--gpu` → node has an NVIDIA GPU (or Apple GPU if origin is darwin);
* toolchains required by the workspace exist on the runtime, or the runtime
  can provide them (container runtime installs via mise; native via mise if
  present) — else reason `missing toolchain node@24`;
* the job needs things only the origin has: cwd outside the workspace,
  `--sh` strings mentioning `localhost`/`127.0.0.1` ports, or an explicit
  `--local` rule → origin only.
* disk floor ok on target.

Admission (resources): `need = estimate(cpu, mem)` (§6.6). A runtime admits
when `need.mem <= free_mem` and `need.cpu <= free_cpu × 1.25` where
`free_mem = min(capacity_mem - reserve - Σ allocated_mem, live_available - reserve)`
and `free_cpu = capacity_cpu - reserve_cpus - Σ allocated_cpu` adjusted down
by live non-kran CPU busy. Memory is never overcommitted. A job larger than
any node's total schedulable memory is admitted alone on the largest node
(reason shown) rather than queued forever.

Scoring among admissible nodes — minimize expected completion:
`wait + sync_bytes/bandwidth + setup_estimate + duration_estimate × speed(node) × slowdown(node)`
where `speed` is learned per (fingerprint, node) and starts from the node's
single-core benchmark ratio; `bandwidth` is measured on blob uploads (EWMA).
`slowdown` is contention that free CPUs do not show (a node thrashing on
memory runs every job slower with cores idle): `1 / (1 − stall)`, stall
combined from independent sources as `1 − Π(1 − s)` and capped at 0.8 (5×).
Linux: PSI "some" avg10 — memory × 1.0, CPU × 0.5, I/O × 0.5 (with many
cores or one busy disk "some task waited" overstates what a new job waits);
swap ≥ 90% used while memory is tight (memory PSI ≥ 10 or < 10% available)
adds 0.2; without PSI, run-queue oversubscription `1 − cpus/load1`. macOS:
`1 − cpus/load1` and memory pressure (100 − memorystatus level) above 75
scaled to at most 0.6; swap is not used (it grows on demand). A Linux VM's
container runtime takes the host's slowdown plus its own CPU saturation.
Reasons name a slowdown of 1.2× or more: "devmac (Linux container): 15.8
CPUs free, est. 1m3s vs 2m32s here (slowed ~2.5x by memory pressure 40%,
full swap)".
Tie-break: origin first, then lowest load. Jobs with estimated duration under
5 s or sync cost greater than estimated savings stay local.

Queue: per-owner FIFO, owners served round-robin (weighted by priority:
high 4, normal 2, low 1). Interactive (tty) jobs default to `high`. Every
queued job carries a live `reason`. No job waits behind a job that cannot be
admitted anywhere (no head-of-line blocking). Starvation guard: a job queued
> 2 min gets priority boost.

### 6.4 Owner detection (CLI)

Walk the process ancestry: first ancestor whose executable/argv matches
`claude`, `codex`, `2code`, `cursor`, `code` → `{kind, id: pid, name}`; use
`$CLAUDE_SESSION_ID` / `$CODEX_SESSION_ID` / `$KRAN_OWNER` when present.
Otherwise `{kind:"human", name:$USER@tty}`. CI when `$CI` set.

### 6.5 Environment forwarding (CLI)

Forward all env vars **except**: host-specific (`PATH`, `HOME`, `USER`,
`LOGNAME`, `SHELL`, `PWD`, `OLDPWD`, `TMPDIR`, `TMP`, `TEMP`, `XDG_*`,
`SSH_*`, `DISPLAY`, `WAYLAND_*`, `DBUS_*`, `TERM_PROGRAM*`, `GOROOT`,
`GOPATH`, `GOCACHE`, `GOMODCACHE`, `GOTOOLCHAIN`, `NVM_*`, `PNPM_HOME`,
`npm_config_cache`, `*_SOCK`, `*_SOCKET`, `LD_*`, `DYLD_*`, `KRAN_*` except
`KRAN_OWNER`) and secrets (name matches `/(TOKEN|SECRET|PASSWORD|PASSWD|
PASSPHRASE|PRIVATE|CREDENTIAL|API_KEY|APIKEY|ACCESS_KEY|(^|_)AUTH(_|$))/i`
or `AWS_*`, `GH_*`, `GITHUB_*`, `ANTHROPIC_*`, `OPENAI_*`, or the value is a
URL with a password, `scheme://user:pass@…`) unless listed in `.kran.toml
env.forward` or `--env NAME`. `TERM`, `COLORTERM`, `NO_COLOR`,
`FORCE_COLOR`, `CI`, `LANG`, `LC_*` (unless the name looks like a secret),
`TZ` are forwarded. The target sets
`PATH` (runtime default + toolchains), `HOME` (real), `TMPDIR` (per job),
`GOCACHE`/`GOMODCACHE`/`npm_config_store_dir` (node-shared caches),
`KRAN_JOB_ID`, `KRAN_NODE`, `KRAN_ORIGIN`, `KRAN_ORIGIN_ROOT`.

### 6.6 Estimates (`internal/estimate`)

Fingerprint = sha256(repo ID + normalized argv) where normalization drops
values that vary per run (`-count=N` kept, `-run REGEX` kept, temp paths and
numbers > 6 digits replaced). Per fingerprint store EWMA of cpu-seconds,
wall seconds, p90 peak memory (last 20 runs), and per (fingerprint, node)
speed ratio. Unknown job defaults: by command family (`go test`: cpu = min(8,
cores), mem 4 GiB; `go build`/`go vet`: 4/2 GiB; `next build`: 4/6 GiB;
`vitest`/`jest`: 4/4 GiB; `tsc`: 2/3 GiB; `pytest`: 2/2 GiB; `docker build`:
4/4 GiB; `cargo`: 8/4 GiB; other: 1/1 GiB).

Between the two sits a learned **repo-family tier**: agents rarely repeat a
command exactly, but "go test in this repo" has plenty of history. Per (repo
ID, family; `-race` is its own variant of `go test`/`go build`; not for
"other") the last 24 runs are kept (at most 64 groups, least recently
updated dropped, persisted in the store's KV). From 3 runs on, each quantity
replaces the family default, blended with it in log space (the default
counts as 2 runs, 5 for memory):
* duration: mean of the runs with the outer 10% clamped (median below 5);
* cpu: that mean of cpu-seconds/wall × 1.5, at most 1.2 × the most any run
  used, at least 0.5; a container run that used ≥ 85% of its `--cpus` cap
  counts as 1.5 × its use, so a low allocation can grow;
* memory: the largest peak (p90 from 10 runs) × 1.25, and at least 0.6 ×
  the largest peak, so every peak seen stays under the job's cap (§6.8).

Which runs teach what: successful runs everything; failed runs memory, and
duration/cpu only when they ran at least half the expected duration (the
best estimate available: fingerprint, then repo-family, then default) —
and, for the repo-family tier, at most 4 × its usual duration (a hang);
canceled and lost runs only memory, at most.

Explicit flags and rules set only what they give (`--cpus`, `--mem`);
everything else still comes from history and defaults. `Estimate.Source`
says where cpu/mem came from (`learned`, `repo:<family>`,
`family:<family>`, `flag`, `rule`), `duration_source` where the duration
came from, `repo_samples` how many runs a `repo:` estimate rests on.
Usage is measured per job: Linux cgroup (`memory.peak`, `cpu.stat`),
container (`docker stats` sampling / cgroup), macOS (1 s sampling of the
process tree RSS + CPU time).

### 6.7 Workspace sync (`internal/workspace` + `internal/execenv`)

* Repo ID = sha256(first root commit hash + normalized origin remote URL); no
  git → sha256(abs path + hostname). All worktrees of a repo share an ID, so
  targets share blobs and slots across worktrees.
* File set = `git ls-files -z --cached --others --exclude-standard` +
  `sync.include` − `sync.exclude` − hard excludes (`.git/`, `node_modules/`,
  `.env`, `.env.*` except `.example/.sample/.template`, `*.pem`, `*.key`,
  `*.p12`, `id_*` keys, `.kran/`). Submodules: their files included.
  Symlinks preserved as symlinks (never followed out of the workspace).
  Without git: walk with the same hard excludes + `.gitignore` parsing.
* Manifest entry: path, mode, size, mtime (ns), sha256 (hash cache keyed by
  dev+inode+size+mtime+ctime in sqlite, so a re-scan of an unchanged 30k-file
  repo is < 300 ms).
* Target: `blobs/<sha[:2]>/<sha>` content-addressed store (shared by all
  repos), `slots/<repoID>/<n>/` working dirs. A job takes a free slot of its
  repo (preferring the slot last used by the same worktree, then any free,
  else creates one up to 4 per repo, else waits — reason `slot`). Slot update
  is in place: write changed files via temp+rename (from blob store: clone
  `clonefile` on APFS / `FICLONE` where supported / copy), delete files not in
  the manifest, except dependency and cache dirs that setup or the tools
  maintain themselves (`keep`, default: `node_modules/`, `.next/cache/`,
  `target/`, `.venv/`, `.turbo/`, `.pnpm-store/`). Then set modes + mtimes.
* Container runtime: slots live in Docker volume `kran-slots` and are
  mounted into the job container **at the origin's absolute path** (volume
  subpath mount), so absolute paths in tests behave identically. Native
  runtime uses the slot path and exports `KRAN_ORIGIN_ROOT`.
* Setup: key = sha256 of setup key files (default auto-detected:
  `pnpm-lock.yaml` → `pnpm install --frozen-lockfile --prefer-offline`,
  `package-lock.json` → `npm ci`, `yarn.lock` → `yarn install --immutable`,
  `bun.lockb|bun.lock` → `bun install --frozen-lockfile`, `uv.lock` → `uv sync
  --frozen`, `poetry.lock` → `poetry install`, `go.sum` → `go mod download`
  only if the command is a go command, `Cargo.lock` → none). Runs only when
  the command needs it (heuristic on argv: node tools for JS lockfiles, etc.)
  and the key changed for that slot. Output streamed as phase `setup`.

### 6.8 Runtimes and toolchains (`internal/execenv`)

* `native`: host OS. PATH = node default login PATH (captured at daemon
  start via `$SHELL -lc 'printf %s "$PATH"'`) + toolchain dirs. Toolchains:
  Go — use `go` on PATH with `GOTOOLCHAIN=go<ver>+auto` so Go fetches the
  exact version from `go.mod`; Node/Python/others — the version on PATH if it
  satisfies the workspace's pin, else `mise exec tool@ver --` if mise exists,
  else ineligible with reason.
* `container`: image `kran-runner:<kran version>` built on the node from an
  embedded Dockerfile (debian stable-slim, git, build-essential,
  ca-certificates, curl, unzip, xz, pkg-config, mise). Built lazily by the
  first container job (phase `setup`, "preparing the Linux runtime (first
  use, ~30 s)"), one build at a time; until then the runtime reports
  `first_use` but counts as available whenever Docker is reachable. A job
  that gives up waiting falls back as any prestart failure; the build goes
  on. The reaper removes `kran-runner` images of other kran versions. Toolchains via mise
  into volume `kran-tools`; caches in volume `kran-cache`
  (GOCACHE, GOMODCACHE, pnpm store, npm cache, uv cache, mise). Job =
  `docker run --rm --init --network host --name kran-job-<id> --label
  kran.job=<id> --memory <cap> --cpus <n> -v /var/run/docker.sock:… -e …
  -w <origin cwd> --user <uid>:<gid>`. The Docker socket is available so
  testcontainers / `docker compose` in tests work (not with `.kran.toml
  docker = false` or `kran run --no-docker`); host networking makes
  `localhost:<published port>` work as on Linux. The slot sync helper lives
  in its own container's file system, never in a volume jobs mount.
* Resource caps: memory cap = admitted estimate × 2, at least 2 GiB, at most
  free memory at start; on cap breach the job is OOM-killed and the exit
  reason says so (`kran: job hit its 6 GiB memory cap on devmac — rerun with
  --mem 12G`); the estimate learns the peak, so the next run gets more.
  Linux local jobs: same via scope. macOS native: watchdog kills the job
  tree on cap breach.

## 7. Public types (`internal/proto`)

See `internal/proto/*.go` — the source of truth. JSON field names are
`snake_case`. Times are RFC 3339 UTC. Sizes are bytes (int64). IDs are
lowercase base32 (job IDs: 10 chars, sortable by time).

## 8. APIs

### 8.1 Local HTTP API (unix socket and UI listener)

| Method | Path | Purpose |
|---|---|
| GET | `/api/v1/state` | `proto.State` snapshot |
| GET | `/api/v1/events` | SSE: `proto.Event` stream (`state` first, then deltas) |
| GET | `/api/v1/jobs?limit=&owner=&node=` | job history |
| GET | `/api/v1/jobs/{id}` | one job |
| GET | `/api/v1/jobs/{id}/log?follow=1&offset=` | output (SSE when follow) |
| POST | `/api/v1/jobs/{id}/cancel` | cancel |
| POST | `/api/v1/jobs/{id}/priority` | `{priority}` |
| POST | `/api/v1/nodes/{name}/drain` / `resume` | drain control |
| POST | `/api/v1/run` | upgrade to run session (unix socket only) |
| GET | `/api/v1/version` | `proto.VersionInfo` |
| GET | `/` | UI assets |

### 8.2 Run session frames

`HTTP/1.1 Upgrade: kran-run/1`, then frames: `type:u8 | len:u32be | payload`.
Control frame payloads are JSON (`proto.RunFrame*` types); data frames are
raw bytes. Types: `submit`, `notice`, `decision`, `started`, `stdin`,
`stdin_eof`, `stdout`, `stderr`, `resize`, `signal`, `exit`, `heartbeat`,
`reattach`, `error`, `stdin_credit`, `stdin_resume`. See
`internal/proto/frames.go`. Input to a remote job (`-i`) is flow-controlled
end to end: `kran run` forwards stdin only below the credit's limit (the
job's consumed offset + 8 MiB), keeps what was not consumed, and sends it
again from `resume` after a lost connection; signals never wait behind
input. Input is never dropped silently: when it cannot be recovered, the
job's stdin is closed with a warning.

`stdout`/`stderr` also flow client→daemon for a job the CLI runs locally:
a copy of the child's output for the job history (`kran logs`, the UI).
The CLI sits between child and terminal (a pty with the terminal's
settings when the output is a terminal, pipes otherwise) and writes every
read to the real destination first, unbuffered; the copy goes through a
bounded queue (8 MiB) that never blocks the child: when the daemon falls
behind, data is dropped and `[kran: output truncated in history]` is
recorded instead. Frames are sent only after `started` (after `reattach` +
`started` on a new session) and before `exit`. The daemon appends them to
the job's spool (at most 64 MiB per job, then the same mark) and never
echoes them back to run sessions; older daemons ignore them.

### 8.3 Peer protocol

yamux session over the ssh stdio. Each yamux stream begins with a
`proto.PeerRequest` JSON line naming the method; owned by package A; the
target side delegates to `execenv.Manager` (§10). Methods: `hello`,
`metrics.subscribe`, `slot.acquire`, `slot.diff`, `blob.put`, `slot.commit`,
`job.start`, `job.attach`, `job.heartbeat`, `job.signal`, `job.resize`,
`job.cancel`, `slot.release`, `mesh.peers`. Version negotiation in `hello`
(major must match; minor may differ).

## 9. Security

* No listening TCP except the UI on loopback (default). The API on that
  listener always requires the token (loopback TCP is open to every account
  on the machine): `kran ui` opens and `kran ui --print-url` shows
  `http://host:7270/#token=…` (token stored in state dir, 0600); GETs accept
  `?token=` (EventSource), mutating endpoints only the `X-Kran-Token` header.
  The unix socket needs no token. Static UI assets are public.
* Unix sockets 0600 in a 0700 dir. All state files 0600/0700.
* Peer auth = SSH with dedicated key + forced command + pinned host key.
  `restrict` disables forwarding/pty/agent.
* Secrets never synced (§6.7) nor forwarded (§6.5) by default.
* Jobs run as the node's user, never root; the daemon refuses to run as
  root (`kran daemon --allow-root` overrides) and `kran service install`
  never installs one that would.
* **Container jobs get the Docker socket by default**, so testcontainers and
  `docker compose` work. With rootful Docker (Linux without rootless mode)
  that is root-equivalent on the node: a job can start a privileged
  container and own the host or the Docker VM, including other repos'
  slots. Only run code you would run as root there, or opt out per repo
  (`.kran.toml docker = false`) or per run (`kran run --no-docker`). Native
  jobs run as the node user, who can reach Docker anyway if they are in the
  `docker` group.

## 10. Internal Go interfaces

Defined in code (authoritative):
* `internal/execenv/execenv.go` — `Manager` (target side).
* `internal/workspace/workspace.go` — `Scan`, `Detect`, `ProjectConfig`.
* `internal/scheduler/scheduler.go` — `Scheduler`.
* `internal/metrics/metrics.go` — `Prober`.
* `internal/estimate/estimate.go` — `Estimator`.
* `internal/store/store.go` — `Store`.
* `internal/client/client.go` — `Client`.

## 11. CLI surface

```
kran setup                 first-run wizard: service, node name, optional link + agents
kran link [host]           link a machine over SSH (no host → pick from ssh config / tailscale / mDNS)
kran unlink <node>
kran run [flags] -- cmd    run a job where it fits (alias: kran r)
kran status [--json]       mesh at a glance
kran top                   live terminal view
kran ui [--print-url]      open the web dashboard
kran jobs [--json] [-a] [--why]   recent jobs (--why: the scheduler's reason); kran logs <id> [-f]; kran cancel <id>
kran drain <node> / kran resume <node>
kran doctor [--fix] [node]
kran agents install|uninstall|status
kran service install|uninstall|start|stop|restart|status
kran upgrade               self-update all linked nodes
kran version
```

Style: plain sentences, no emoji, color only on a tty and when `NO_COLOR`
unset, every error says what to do next. `--json` everywhere it makes sense.

### Exit codes
| Code | Meaning |
|---|---|
| the command's | normal |
| 125 | kran infrastructure failure after the job started, or `--no-fallback` failure |
| 126/127 | command not executable / not found (as a shell would) |
| 128+n | killed by signal n (as a shell would) |
| 2 | kran usage error |

## 12. Testing

* Unit tests in every package; `go test -race ./...` green on linux and
  darwin.
* `internal/e2e`: spins up 2–3 daemons in one process tree with separate
  state dirs, connected via the in-memory transport and via a real
  `x/crypto/ssh` server (in-process) executing `kran serve --stdio` — no
  external sshd needed. Covers: local run, remote run, fallback, queueing,
  fairness, cancel, reattach after target daemon restart, lease expiry after
  network cut, origin daemon crash, disk floor, OOM cap, env filtering,
  secret exclusion, mtime preservation, exit code/signal passthrough.
* Chaos suite (`make chaos`): randomized kills of CLI/daemons/supervisors and
  connection cuts; invariant checks: no orphan processes, no leaked
  allocations, every job reaches a terminal state, queue drains.
* Real-machine smoke test (`make smoke NODE=devmac`) against a linked node.
