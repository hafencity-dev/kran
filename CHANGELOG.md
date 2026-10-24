# Changelog

All notable changes to kran. Versions follow [semantic versioning](https://semver.org/).

## 0.1.9

### Fixes

- A daemon restart (for example `kran upgrade`) no longer makes queued jobs run on their own: `kran run` reattaches to a queued job (its ID comes with the first "waiting" notice) instead of running the command next to the daemon's accounting, so an upgrade on a busy machine does not start every queued job at once.
- Jobs that keep running across a daemon restart are counted again on both sides: the restarted node showed them as free room ("0 running") until they ended and could take more jobs than it has room for.

## 0.1.8

### Fixes

- A peer that rebooted is noticed as soon as it connects again: the connection to its previous run, which a reboot that cut the network first leaves half open, is closed at once instead of after up to a minute of silence (start times in the hello).
- A job that runs here because no other node could take it says why (`devmac's Linux VM has 409 MiB free`, `devmac: draining`, `devmac (Linux container) cannot hold 40.0 GiB`) instead of "no other node has room", which was also shown for nodes that were offline, draining or missing a toolchain.

## 0.1.7

### Fixes

- Linux container runtime: `> /dev/stderr` works too. 0.1.6 fixed `/dev/stdin` and `/dev/stdout` only: kran-init's shell applied a `2>/dev/null` to itself while it changed the mode of stderr, so it changed `/dev/null` instead.

## 0.1.6

### Fixes

- Linux container runtime: `/dev/stdin`, `/dev/stdout` and `/dev/stderr` can be opened by path (`echo x > /dev/stderr`, `bash /dev/stdin`); they failed with "Permission denied", so scripts using them failed only on the Mac.
- A file that changes while it is synced (a log being written) no longer fails the remote start and runs the job locally: kran scans again and sends the difference (at most 3 scans).
- A local job's history whose output was cut because the daemon fell behind now always ends with the truncation mark; when the queue was full at the end, the mark could be left out.
- The disk floor (below which a node takes no remote jobs) is 5% of the disk capped at 20 GiB, still at least 10 GiB: on a 916 GiB disk 5% kept a node out with 42 GiB free.

### Estimates

- `go test -run …` learns apart from whole-package runs once a repo has a few: agents write a new filter each time, so these used the package's full-run duration (half of all local estimates were 2× too long or more).

## 0.1.5

### Disk shortage is reported

- A remote job that fails while a disk it writes to is full (an ENOSPC message in its output, or less than 1 GiB free on its disk when it ends) keeps its exit code but says so: `kran: disk short: … rerun it` on stderr and `"disk_short": true` in the `exit` event and the job record, so a caller can rerun it instead of counting a red test.

## 0.1.4

### Disk space on busy nodes

- A node that always runs a container job never got to trim its caches (they grew to 33 GiB of a 20 GiB budget on a Mac) while kran deleted the slots of the busiest repository every few minutes instead, so its next job synced the whole workspace again. When the trim is 15 minutes overdue and the Docker disk keeps less than its reserve free, new container jobs now pause (they queue or go elsewhere) until the running ones end and the trim ran, at most 15 minutes.
- Slots used within the last 30 minutes are kept while the disk is above its floor.

### Logs

- Placing a job that waited at least 30 seconds logs why it waited.

## 0.1.3

### Linux container runtime

- The runner image has `busybox` (the binary only; its applets do not replace the GNU tools), for tests that run a script under busybox's shell and tools.

### Fixes

- A local job that ended while its daemon restarted was often recorded as `lost` although `kran run` exited with the command's code: a CLI that reattached, reported the exit and quit before the restored daemon checked for it was not counted as back. The daemon now counts it, and `kran run` waits for the daemon to record a local exit (up to 3 s) and reports it again after reconnecting when the connection dies first.

## 0.1.2

### Linux container runtime

- `docker inspect`, `docker ps` and `docker volume inspect` from a job show the paths the job used for bind mounts (its workspace, `/tmp`, home), not where they live on the Docker host, so tests that check what a container has mounted work.
- The runner image has GNU awk as `awk`, plus GNU parallel, uuidgen, hostname, tput and cpio.
- kran keeps free space on the disks it fills: `accept.disk_reserve` (default max(25 GiB, 20%), at most 50 GiB). Below it, kran trims its container caches and then removes idle slots, least recently used first (never one in use), on this machine's disk and on the Docker disk of a Mac. `kran doctor` shows the reserve.
- After an upgrade the old runner image is removed even when the old version's sync helper or a stopped container still held it; while a job of the old version runs, kran retries every 5 minutes.

### Fixes

- The service tests no longer read the real `/Library/LaunchDaemons` on a Mac where kran runs as a system daemon.

## 0.1.1

### Linux container runtime

- A job's `/tmp` (and `TMPDIR`) is a volume of its own on the Docker host's disk, so files and directories under `mktemp -d` can be bind-mounted into nested containers (`docker run -v "$tmp/ssl:/etc/nginx/sslcerts:ro"`, `-v "$tmp/file:/etc/x.conf"`). It is removed with the job; the reaper removes any a crash left behind.
- The runner image has a real `/usr/bin/python3` (with venv) for scripts that use `env -i`, a narrowed `PATH` or `python3 -I -S`; on the job's `PATH` the origin's or the repo's python still comes first.
- `docker` and `docker-compose` are also in `/usr/bin`, for scripts with a fixed `PATH`; the compose and buildx plugins work from there too.
- Container jobs are refused with reason `disk` while the Docker disk is below the node's floor, and kran trims its caches at once.

### Fixes

- Canceling a job while it syncs over a stalled link ends it at once instead of when the link is declared dead (up to 2 minutes on a loaded machine): peer calls, the manifest exchange and uploads no longer wait for writes stuck on the connection.

- A job sized by an estimate may use at least 8 GiB before kran stops it (the cap only kills, it reserves nothing), so the first `go test` of a large package no longer hits a cap sized from its smaller siblings. An explicit `--mem` keeps its exact cap.
- On macOS, where kran enforces a memory cap by measuring, a capped job is measured every 200 ms instead of every second.

- A remote job's first output byte can no longer overtake its "running" notice, which the origin uses to decide between falling back and exit 125.
- The test suites pass on macOS (path symlinks, process lookup, a root-only test path) and tolerate a heavily loaded CI runner.

## 0.1.0 - first release

kran moves heavy dev jobs to whichever of your machines has room, and never makes a job worse than running it locally.

### Run

- `kran run -- <command>` and `kran run --sh '<shell string>'` place a job on the machine with the earliest expected finish, sync only changed files, stream output back and return the command's exit code. Colors and progress bars survive (a pty is used when you have a terminal).
- Falls back to running on this machine when kran fails before the command starts. After the command started, an infrastructure failure ends with exit 125 and a message; nothing is re-run behind your back.
- Placement considers online state, same OS (a Mac offers a Linux container through Docker), GPU, toolchains, disk, and free CPU and memory after each owner's reserve. Memory is never overcommitted. Estimates learn from earlier runs. Short jobs stay local.
- Queue with fair sharing between owners (you, Claude, Codex, CI); a waiting job says why on one stderr line after two seconds.
- Flags: `--local`, `--remote`, `--node`, `--gpu`, `--cpus`, `--mem`, `--any-os`, `--stdin`, `--quiet`, `--json-events`, `--priority`, `--label`, `--no-fallback`, `--env`, `--sh`.
- Exit codes: the command's own; 125 kran failed after start; 126/127 not executable / not found; 128+n signal; 2 usage error.
- Per-repository hints in `.kran.toml` (rules, sync include/exclude, environment, setup command). Secrets, `.env` files and host-specific variables are never sent.
- Jobs on a remote machine run under a supervisor with a lease: a lost network or crashed daemon leaves no orphan processes, and the origin reattaches after a daemon restart.

### Machines

- `kran link <host>` links a machine over plain SSH (your config, agent and jump hosts), installs kran there, creates a dedicated key restricted to one forced command, pins the host key and checks both directions. `kran unlink` removes exactly what link added, on both sides.
- `kran drain` / `kran resume`, `kran upgrade` (this machine and every linked one), `kran doctor [--fix]`.
- Linux (systemd user unit) and macOS (launchd agent, or `kran service install --system` for a Mac without a GUI login; the daemon also starts on demand when a job or a linked machine connects).

### See

- `kran status` (also `--json`), `kran jobs` (`--why` shows the scheduler's reason, `--json`), `kran logs [-f]`, `kran cancel`, `kran top` (terminal view), `kran ui` (web dashboard on `127.0.0.1:7270`).

### Coding agents

- `kran agents install` adds a short Claude Code skill, a managed block in `~/.claude/CLAUDE.md` and `~/.codex/AGENTS.md`, and a Claude Code `PreToolUse` hook that wraps long tests, checks, lints and builds as `kran run --sh`. Servers, watchers, installs, quick commands and anything that writes a file you want locally are left alone; the hook stays silent when no daemon is running. `kran agents uninstall` removes exactly what was added.

### Install

- `install.sh` (anonymous download; `gh` or `GITHUB_TOKEN` are only a fallback when GitHub rate limits you) verifies checksums, installs to `~/.local/bin` and runs `kran setup`; it asks setup's questions on your terminal even when piped into `sh` (`--yes` to accept the defaults).
- Release binaries for linux and darwin on arm64 and amd64; `make install` also fills the binary cache that `kran link` uses to install kran on a machine of another OS.

### Speed and placement

- Syncing an unchanged workspace takes a fraction of a second, also for monorepos with tens of thousands of files: after the first sync only a manifest delta travels, the target scans its copy once, and a Mac's Docker volume is updated with a single helper call.
- Placement counts memory, I/O and CPU pressure and a full swap, not only free CPU: an overloaded machine sends work elsewhere instead of slowing it down.
- Estimates learn per repository and kind of command (go test, vitest, tsc, …) from measured duration, CPU use and peak memory, so a new command in a known repository is sized from its siblings instead of a fixed default. Explicit `--cpus`/`--mem` keep the learned duration.
- `kran jobs --why` and the dashboard keep why a job ran where it ran after it finished.

### Linux containers (on a Mac, or opted in)

- A job sees the origin's user name, home, time zone and locale, and the same toolchain versions as the origin (go, node, pnpm, yarn, python, bun, deno, rust, uv), installed on first use. Pins nearest the job's directory win (`.nvmrc`, `.node-version`, `package.json` `packageManager`/`engines`, `.python-version`, `.tool-versions`, `mise.toml`, `go.mod`).
- `/dev/shm` grows with the job's memory, the open-files limit is 65536, and git trusts the workspace copy.
- Docker from a job works with bind mounts: `docker run -v "$PWD":/src`, relative bind mounts in compose files and BuildKit builds see the job's workspace (and home) instead of an empty directory. The job's Docker socket is a small proxy that maps those paths to where the slot lives on the Docker host; concurrent jobs and repos keep their own mapping.
- The shared caches of container jobs (Go, pnpm, npm, uv, pip, cargo, mise installs, the persistent home's caches) are trimmed hourly to a budget (`runtime.container.cache_budget`, default min(20 GiB, a quarter of the Docker disk)), least recently used first and harder when the Docker disk runs low; never while a job uses them. `kran doctor` shows their size. Dangling runner images and stopped kran containers are removed too.
- The runner image has the everyday tools scripts expect: envsubst, jq, ripgrep, shellcheck, lsof, strace, sfdisk/fdisk, file, zip, zstd, rsync, netcat, dig, ip/ss, sqlite3, bc, patch, xxd and more, plus `docker buildx`.

### Safety

- A remote job runs exactly once across daemon restarts and lost answers: kran runs a command locally only when the target confirmed it never started.
- `kran service restart` waits only while a job is starting; queued and running jobs carry over.
- Jobs are capped in processes as well as memory (systemd `TasksMax`), and never swap beyond their cap. A test binary is never started as kran, and the test suites run in a capped scope.
- The coding-agent hook keeps a leading `cd DIR &&` outside the wrap, so kran syncs the directory the command runs in.

### Robustness

- Links between machines survive an overloaded peer: any data counts as a sign of life, a calm peer may stay silent for 45 s and a heavily loaded one up to 2 minutes; a dead link is still noticed. Sync and setup in flight are no longer thrown away when a machine is starved.
- The daemon runs with priority over the jobs it manages (systemd: CPU/IO weight, best-effort I/O, memory protection; launchd: no low-priority I/O). `kran upgrade` and `kran doctor --fix` rewrite older service definitions.
- Under load the metrics probes fork `nvidia-smi` and `docker` less often and back off after failures; one failed probe no longer makes the container runtime disappear.
- Reconnects back off up to 20 s and start over when the peer connects or the network changes; the log says what changed instead of repeating itself.
- One daemon per machine: a second one started by launchd or systemd waits instead of restarting every few seconds. The daemon log is written once (`daemon.log`); `daemon.out.log` keeps only crash output.
