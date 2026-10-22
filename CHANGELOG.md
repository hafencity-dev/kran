# Changelog

All notable changes to kran. Versions follow [semantic versioning](https://semver.org/).

## Unreleased

### Linux container runtime

- A job's `/tmp` (and `TMPDIR`) is a volume of its own on the Docker host's disk, so files and directories under `mktemp -d` can be bind-mounted into nested containers (`docker run -v "$tmp/ssl:/etc/nginx/sslcerts:ro"`, `-v "$tmp/file:/etc/x.conf"`). It is removed with the job; the reaper removes any a crash left behind.
- The runner image has a real `/usr/bin/python3` (with venv) for scripts that use `env -i`, a narrowed `PATH` or `python3 -I -S`; on the job's `PATH` the origin's or the repo's python still comes first.
- `docker` and `docker-compose` are also in `/usr/bin`, for scripts with a fixed `PATH`; the compose and buildx plugins work from there too.
- Container jobs are refused with reason `disk` while the Docker disk is below the node's floor, and kran trims its caches at once.

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
