# Changelog

All notable changes to kran. Versions follow [semantic versioning](https://semver.org/).

## Unreleased

### Fixes

- A queued job is no longer started outside the daemon's books by a daemon restart (15.11.2026: a queued heavy job was "lost" at a restart: its `kran run` gave up after 20 s, ran the command itself unbooked, and the new daemon canceled its record). Both sides now wait for each other with one set of bounds (`internal/proto/restart.go`): the restarted daemon keeps a restored job for its `kran run` 20 s, and `kran run` redials for 110 s (a restart of up to 90 s plus those 20 s). The daemon names every job to its `kran run` in its first frame, so even a job that was not yet queued reattaches. `kran service restart` and `kran upgrade` hold new jobs while they wait (a job submitted meanwhile prints `kran: waiting: a restart of kran is pending; this job joins the queue after it (at the latest at …)` and is queued by the new daemon), and wait, up to 10 minutes, only for jobs syncing or in setup and for queued jobs of a `kran run` older than this version; queued jobs of a newer `kran run` carry over. Ordinary restarts need no `--force`; `--force` first says what it puts at risk. A wait that runs out changes nothing and exits 1.
- `kran upgrade --version` accepts builds between releases (`0.1.12-5-gf7c1755`), the versions kran runs between tags. They come only from the binary cache, checked against the `SHA256SUMS` that the new `make cache` writes at build time; they are never downloaded or built. `--version` installs exactly that version, also an older one, so the way back is one command per machine: `kran upgrade --version <old> --local-only` here, `kran upgrade --version <old> -n <machine>` for one linked machine (new `-n`: nothing is fetched, installed or restarted here). A peer taken to another version than this machine runs gets that version from the cache, never this machine's own binary (before, an upgrade back would have pushed the running binary as the peer's linux runtime); a build between releases comes from the verified cache also when this machine runs it. The version is checked first against one grammar (`x.y.z`, `x.y.z-tag`, `x.y.z-N-g<hex>`; no separators, `..`, whitespace, leading dash or `-dirty`). A copy pushed to a peer is checked against its digest on the peer before it replaces the binary. Every way back fails loudly (exit 1) instead of reporting success: `-n` succeeds only when the peer's daemon reports the version asked for, and says what is installed and running there otherwise; `--local-only` checks that the restart can happen before it replaces the binary and succeeds only when the restarted daemon reports the version asked for; otherwise it says `the binary on disk is now kran X while the daemon still runs Y (…). Finish it with: kran service restart`, with Y as the daemon reports it. `SHA256SUMS` guards against mix-ups, not against the user. A Mac peer taken to a version with `--version` gets the linux runtime of that version too, from this machine's verified cache: when it cannot be provided nothing is installed on the Mac, and a failed copy is a failure (before, both were notes beside a success, and the Mac's daemon would have had no runtime for its container jobs).

- The first container job after an upgrade no longer pays for the new runner image (15.11.2026, devmac: the first container job after the upgrade to 0.1.12-5 ended with `infra_error`, "the Linux runtime on devmac is still being prepared (first use)", and no other node could take it). A node that ran container jobs before builds the image of the new version when its daemon starts, and its container runtime is unavailable until then (`devmac: no linux runtime (preparing the Linux runtime for this kran version (since 22:38:45); container jobs run elsewhere or wait for it)`): a container job runs elsewhere or waits. A failed build is named in the reason and retried after 5 minutes. A node that never ran a container job still builds lazily, so it pays nothing.
- A learned estimate no longer caps a job that kran.slice bounds (15.11.2026, gx10: five times since 13.11. a local job without `--mem` ran in a scope with `MemoryMax` from its estimate, at least 8 GiB, no swap; a precheck that had started running more tests outgrew it, the kernel killed a compiler, systemd stopped the rest with TERM, and the job ended with no reason). Which job gets which cap now: with `--mem` (or a `.kran.toml` rule's `mem`) every job is capped at twice that, at least 2 GiB; without it a Linux local job in its scope under kran.slice, and a remote native job on a Linux target that runs jobs in scopes under that slice, get no cap (the slice bounds all kran jobs together; the estimate places and books the job); every container job, a native job on macOS, and a native or local job on Linux without a systemd user session keep the node's default cap of twice the estimate, at least 8 GiB, as before, since nothing above them bounds their memory (one runaway job in a Mac's Docker VM could otherwise make its kernel kill another job, a test database or the sync helper). The message for a default cap says so and does not name `--mem` as its source. Never `MemoryHigh`: a throttled unit raises memory pressure for the whole user slice and systemd-oomd then kills its largest cgroup. A local job still runs in its own scope (with `TasksMax`), cap or not.
- kran says how a job ended when its own output does not: `kran: the kernel's out-of-memory killer ended a process of the job on gx10: the machine ran out of memory (the job has no memory cap of its own); journalctl --user -u kran-local-<id>.scope shows more`, `kran: job hit its 40.0 GiB memory cap on gx10 (from --mem 20.0 GiB) — rerun with --mem 80G`, `kran: job ended by signal KILL on gx10`. A local scope's systemd result is read at every ending, not only after a KILL (an OOM kill shows as TERM for the rest of the scope), and a failed scope is reset. The line is `kran run`'s, `kran jobs --why`'s and is repeated by `kran logs` after the output. Signals kran run passed on (Ctrl-C) and `PIPE` stay quiet; uncommon signals are named (`SEGV`, not `segmentation fault` or `11`).
- Small jobs no longer wait behind a job that has waited longer (15.11.2026: with two prechecks running and a third of 20 GiB holding gx10, six tests of 1–3 s waited 30–60 minutes with 13 of 20 CPUs idle). A job may pass a held one when, by the estimates, it ends before the held job can start (only when that start is known: with non-kran load, another origin's jobs or a job larger than any node in the way nothing passes by ending first), or fits in the room left beside it once it starts (CPUs counted as the held job's own admission will: at most free − held / 1.25). The held job cannot starve: once only jobs that passed it stand in its way, nothing passes it any more except into room it does not need, so it starts when those end. Waiting reasons are unchanged; the daemon log names the job a placement went ahead of.
- Running local jobs are measured by their systemd scope's cgroup instead of their process group (15.11.2026: two prechecks that used gigabytes read as 160 MiB each, because their tools run in process groups of their own; the scheduler then took their whole 20 GiB reservations off the 58 GiB gx10 had available again, `kran status` showed 8.5 GiB free, and a 14 GiB job waited). Free memory is what the machine has available minus the part of each running job's reservation it has not used yet; the same measurement stops a local job's own CPU load from counting as load beside its CPUs. Without a scope (macOS, no systemd user session) the process group is measured as before. Page cache the kernel can give back (`memory.stat` `active_file` + `inactive_file`) is not counted as use, locally and for remote jobs in their job cgroup: a build that read many files is not booked as using what `MemAvailable` already shows as available.
- Kill order in a full VM or host: kran's job containers are started with `--oom-score-adj` 1000 (no `--mem`) or 500 (with `--mem`), so when a Mac's Docker VM (which also holds the Mac's own appliance containers, at Docker's 0) or a Linux host runs out of memory, the kernel ends a kran job first, a job without `--mem` before one that asked for its memory; kran's sync helper and cache trim stay at 0. A container a job creates through kran's Docker proxy (a test database, a compose service) gets at least the job's score: a higher one asked for is kept, a lower or negative one is raised, `OomKillDisable` is dropped; updates and execs pass unchanged. With a score the proxy sends its own encoding of every create and refuses with 400 one Docker could read otherwise (another spelling of `HostConfig`, `OomScoreAdj` or `OomKillDisable`, one given twice, a body that is not exactly one JSON object), and sends every request with `Connection: close`. The order is a default against accidents, not a fence: Docker's own socket is in every job, a privileged exec can re-score a container, and `docker build` steps run at BuildKit's own score. The score is set only when kran creates a job container, or in a job's own create request. A container killed far below its cap (measured peak under half of it) is reported as "the kernel's out-of-memory killer ended the job on devmac: devmac's Linux VM ran out of memory and took this job (…)", exit 137, instead of "hit its memory cap".
- `kran doctor` reports `fs.pipe-user-pages-soft` of every kernel kran's jobs run under (a Linux node's own, a Mac's Docker VM's) and warns at the kernel default 16384: all jobs of a node run as one user, and once its pipes hold that many pages every new pipe gets one page and a writer that fills it blocks (a Go test helper hung 45 minutes that way). It says where to raise it to 262144; kran never changes it itself and does not configure the Docker VM.

## 0.1.12

### Fixes

- `kran upgrade` on a macOS node now also fetches the linux binary of the new release into its own cache (`<cache>/bin/<version>/linux-<arch>/kran`), so a Mac that upgrades itself can still run container jobs afterwards (devmac, 10.11.2026). It is the same download, `checksums.txt` check and temp-file, fsync, rename install that fills a peer's cache, and it happens before the native binary is replaced and the daemon restarted: if the linux asset is missing or does not verify, `kran upgrade` stops with a message and changes nothing, so the node is never left worse than before. With `runtime.container.enabled = false` nothing extra is fetched; a Linux node never fetches anything extra. `kran upgrade` prints where each binary went, and running it on a node that already has the target version but lacks the linux binary repairs that too.
- A node whose own Linux binary is missing no longer looks healthy (10.11.2026: devmac's daemon was replaced by hand with a `0.1.10-6-gc9403ef` build, only the darwin binary; the container runtime's `…/kran/bin/<version>/linux-arm64/kran` was not there, the daemon came up online, and for over an hour every container job offered to it waited with "devmac: no linux runtime" and ran on the other machine, unnoticed). The daemon now checks that file (present, a regular file, executable) at start and whenever it re-probes: it logs one line per change with the exact path and the fix, `kran status` prints `! devmac cannot run container jobs: its linux runtime for … is missing at …` (JSON: `info.runtimes[].problem`; the dashboard node card too), and the wait reason of a job says the same instead of only "no linux runtime". The node stays online for native jobs and is simply not eligible for container jobs. It does not refuse to start, because that would also take its native jobs away. The fix names `kran upgrade` only for a release version; a development build has no published asset, so nothing is downloaded and the message gives the path to copy a build to. `kran service restart` and `kran service install` print the line too.

## 0.1.11

### Fixes

- A node that failed to start a job no longer keeps that job for the rest of its life (09.11.2026: a land gate's docs job was refused once by a devmac that was short of memory at that moment, then waited more than 1 h 48 min for a busy gx10 while devmac sat idle with 19.7 GiB free). The node sits out for that job only for a backoff: 1 minute, doubling with each failure on that node, capped at 5 minutes. The waiting job is placed again on every scheduler tick by the rules of a new job and takes any node that has room, the failed one once its backoff ended. The wait reason names the backoff and when the node is tried again (`devmac: failed to start this job (…); backed off for 1m, tried again in 40s`). The two remote attempts a job gets before it falls back to the origin also end with the last backoff, and a job that may run nowhere else (`--remote`, `--no-fallback`) still fails at once.

- A command that runs something only this machine has stays here with the reason, instead of failing on another node with "No such file": a workspace file git ignores (a locally built `bin/lk`, unless `sync.include` names it), a personal tool from `~/.local/bin`, `~/bin` or `~/go/bin`, or a `cd` out of the workspace. A command that builds the program first (`make lk && bin/lk docs check`) may still go. Before, `bin/lk docs check` failed on the Mac every time, so kran also never learned that the docs check runs about 5× faster there.
- Linux: a local job's command line reaches it exactly as given. Newer systemd-run (Ubuntu 26.04) expands `$VAR` and turns `$$` into `$` in the command it starts unless told not to, so `sh -c 'echo $HOME'` or `kill $$` arrived mangled; kran passes `--expand-environment=no` where systemd-run knows it.
- A kran volume removed or recreated behind kran's back (a disk cleanup ran `docker volume rm`) no longer stops every container job on that node until the daemon restarts: Docker recreates a missing volume empty, without the directories jobs mount from it ("cannot access path …/kran-cache/_data/home"). kran checks the volumes it set up before each container job (one API call each) and sets them up again.

## 0.1.10

### Placement

- A busy origin is relieved: while it is under contention (memory, I/O, CPU pressure) or most of its CPUs are taken, a check of 30 s or more (tests, vet, type checks, lints: commands whose result is their output and exit code) goes to another node where it is expected to take up to 25% longer, scaled by how busy the origin is. Commands that may write files the caller needs (builds with an output, `--fix`, snapshot updates, coverage files, redirections into files) never move for this. A relieving job takes only spare room: the node keeps half of its CPUs and memory free for jobs that must run there, such as a land gate's pinned runs. The reason says so (`gx10 is busy (I/O pressure 46%): relieving it is worth up to 12% longer`).

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
