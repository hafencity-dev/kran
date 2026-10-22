# NOTES-B — execenv (target side) and workspace (origin side)

Branch `agent/b-exec`, merged with `main` (A, C, D, E). Everything in SPEC §6.2, §6.5 (target env),
§6.7 and §6.8 that belongs to B is implemented; see "Not done / limits" at the end.

## Changes outside my packages (please review)

* `internal/proto/frames.go`: **added** `Exit.BeforeStart bool` (`before_start`): the job ended before
  the user's command started (setup failed, command not found natively, runtime not ready). Nothing
  of the command ran, so local fallback is safe. The daemon may use it in addition to its own
  "no `JobRunning` phase seen" rule.
* `go.mod`: `go mod tidy` on main had dropped `github.com/creack/pty`; restored (v1.1.24, as before).
* Commit "wiring (A/E files, for lead review)" — drop it if you prefer to do it yourselves:
  * `internal/daemon/run.go`: passes `Container: cfg.ContainerEnabled` and
    `LinuxBinary: paths.BinaryCache(version, "linux", GOARCH)` (where `kran link`/`upgrade` put it) to
    `execenv.New`. Without `LinuxBinary` the container runtime on macOS reports itself unavailable.
  * `internal/cli/daemonctl.go`: hidden `kran internal apply` → `execenv.Apply(args)` (runs inside the
    kran-sync container on macOS; flag parsing disabled like `supervise`).
* `internal/e2e/execenv_test.go` (new, test only): two real daemons in one process over the in-memory
  transport with real execenv/workspace/scheduler/estimator/store/metrics. Covers a remote run
  (sync, exit code, stdout/stderr split, `.env` not synced, blob reuse) and a target-daemon restart in
  the middle of a running job (output continues complete, exit 0).

## Interface additions (additive, my packages)

* `execenv.Options`: `Container` ("auto"|"true"|"false", = `runtime.container.enabled`) and
  `Namespace` (default "kran"; prefixes slice `kran.slice`, scopes `kran-job-<id>.scope`,
  containers `kran-job-<id>`/`kran-sync`, volumes `kran-slots|cache|tools`, image `kran-runner:<ver>`;
  tests use "kran-test").
* `execenv.Apply(args) int` — entry of `kran internal apply`.
* Errors for `errors.Is`: `ErrDiskFloor` (map to peer code `disk_floor`), `ErrUnknownJob`,
  `ErrRuntimeUnavailable` (missing toolchain, Docker not ready …), `ErrBlobMismatch`, `ErrMissingBlob`,
  `ErrBadManifest`.
* workspace: `NormalizeRemote`, `HardExcluded`, `DefaultKeep`, `LinkStaysInside`, `NewMatcher`/`Matcher`
  (gitignore-style), `SetupFor(exists, argv)`, `ProjectConfigName`, `ErrBlobChanged`.

## Contract details for the daemon (A)

* `Process.Output(offset)`: offset = payload bytes of stdout+stderr (same unit as `Reattach.Offset`),
  mid-frame offsets work. The reader is a byte stream of encoded frames: a buffer ≥ 5+32 KiB gets one
  whole frame per Read; `proto.ReadFrame` works with any reader size. EOF after the job ended and all
  output was delivered. `Close` unblocks a waiting Read.
* `OnPhase`: `{setup, "running setup: <cmd>"}` only when a setup runs, then `{running}` exactly when the
  user's command was started (event written by the supervisor after `exec`). Container jobs may also
  get `{setup, "building the kran-runner image …"}` during `Start`/`AcquireSlot` if the image is
  missing — `Run` starts that build in the background at daemon start so it normally never happens on
  the job path (the build takes ~30 s–3 min; your prestart timeout is 2 min).
* `Start` errors (toolchain missing, bad runtime, image not ready, supervisor did not start) happen
  before anything ran.
* Restart: a new Manager re-indexes `jobs/*` with live supervisors (they hold `jobs/<id>/lock`):
  `Active()` lists them, `Attach` works, and **their slots stay busy** until you call `ReleaseSlot`
  with the persisted `SlotRef` (works across restarts; releasing a non-busy slot is a no-op).
  Supervisors found dead get a `lost` status. Safety net: a slot that is busy without a running job
  for 30 min is released by the reaper.
* `Attach` works for finished jobs as long as the job dir exists (7 days).
* `Stdin()` never returns nil (writes for a job without `spec.Stdin` are dropped). Input sent right
  after `Start` is not lost: the pipe/pty exists before the supervisor reports ready.
* `Signal`: `INT`/`HUP`/`QUIT`/`USR1`… go to the job's process group (pty: the terminal's foreground
  group; container: `docker kill -s`). `TERM` = cancel: SIGTERM to the whole tree, SIGKILL after 5 s.
  `KILL` = immediate tree kill. Canceled jobs end with `State canceled`.
* `Live()` reads a small `live.json` the supervisor rewrites every second — cheap, no syscalls beyond
  one read.
* `Renew()` touches `jobs/<id>/lease`; the supervisor kills the tree after `LeaseTTL` without renewal
  → `State lost`, `Code 125`.
* `PutBlob` expects **raw** content (decompress zstd before), reads exactly `size` bytes even if the
  blob already exists (stream stays in sync), verifies sha256, never leaves partial files.
* `Exit`: `Code` 128+n and `Signal` "INT"/"TERM"/"KILL"; OOM: `Code 137`, `Signal KILL`,
  `Reason "job hit its 6 GiB memory cap on devmac — rerun with --mem 12G"`. `Node` is filled with the
  NodeName given in Options.
* Memory cap (SPEC §6.8) = 2 × `Alloc.Mem`, ≥ 2 GiB, ≤ available memory at start; `Alloc.Mem == 0`
  means no cap. Linux: job cgroup `memory.max` + `memory.swap.max=0` + `oom.group`; macOS native:
  1 s RSS watchdog; container: `--memory`/`--memory-swap`.
* `Usage`: `CPUSeconds`, `PeakMem`, `WallSeconds`, `SetupSeconds` filled by execenv; sync fields are
  yours.

## Wiring / deployment notes

* `Options.Self` must be the kran binary (`os.Executable()`); execenv runs
  `<self> internal supervise <jobdir>`, on Linux via
  `systemd-run --user --scope --unit=kran-job-<id> --slice=kran.slice -p Delegate=yes` (probed once;
  falls back to a plain detached process group). The supervisor therefore lives outside
  `kran.service`'s cgroup: restarting the service never kills jobs (E: no KillMode tweaks needed).
* macOS LaunchAgent: supervisors call `setsid`, so launchd's process-group kill does not reach them;
  setting `AbandonProcessGroup` to true in the plist is still a good idea.
* CLI (E): for identical results put the origin's **actual** Go version (`go env GOVERSION`) into
  `Req.Toolchains["go"]`; the target pins it with `GOTOOLCHAIN=go<ver>+auto`. Other pins are checked
  against the target's PATH (npm-style ranges) and run via `mise exec tool@ver --` when mise exists;
  otherwise `Start` fails with `missing toolchain node@24 on <node>`.
* `Spec.Setup` overrides auto-detection; when nil the target auto-detects from the committed manifest
  (`workspace.SetupFor`). The CLI does not need to send `DefaultSetup` (it may; same result).
* Container runtime: slot mounted at `Workspace.Root` (must not be `/`, `/usr`, `/etc`, … → Start
  error). Linux nodes keep slots on the host (bind mount); macOS keeps them in the `kran-slots` volume
  (Docker ≥ 26 / API 1.45 for `volume-subpath`) written by the long-lived `kran-sync` container that
  runs the Linux kran binary. `SlotRef.Path` is an absolute host path for host slots and
  `<repo>/<n>` for volume slots.
* Disk floor: `AcquireSlot` refuses with `ErrDiskFloor` below `Options.DiskFloor` (default
  max(10 GiB, 5 %)) on the state dir's volume.
* Blob cache cap: `Options.BlobCap` (default min(20 GiB, 10 % of disk)), LRU by mtime, trimmed to 90 %.
* Reaper (`Run`: at start and every 30 s): orphan `kran-job-*.scope` units, containers labelled
  `kran.job` of jobs that ended here, processes carrying `KRAN_JOB_ID` of ended jobs (setsid/double-fork
  escapees), job dirs > 7 days, slots unused for 3 days. Only resources whose job dir exists in this
  state dir are touched.

## Workspace (origin)

* Repo ID = sha256(oldest root commit + normalized origin URL); the root commit is cached per git
  common dir in `<cache>/rootcommit/` (Detect on lokalaise: ~12 ms instead of ~70 ms). Submodule
  checkouts are their own workspace (own repo ID). Non-git: sha256(path + host), walk capped at 200k
  files with a clear error.
* Scan: `git ls-files -z --cached --others --exclude-standard`, recursing into submodules and nested
  repos; hard excludes always win; symlinks escaping the root are dropped. Hash cache: one binary file
  per root (`<cacheDir>/hashcache/*.bin`, CRC-checked, published by rename — safe with concurrent
  scanners across processes; files modified < 2 s before the scan are not cached).
  **Re-scan of lokalaise (27.7k files): ~95 ms** (`BenchmarkRescan`). `Open` is safe for concurrent
  use and refuses files that changed since the scan (`ErrBlobChanged`).
* `Manifest.Keep` = `DefaultKeep`; on the target, keep entries match at any depth (monorepos).

## Tests

* `go build ./... && go vet ./...` clean; `GOOS=darwin` and `GOARCH=amd64` vet clean for my packages.
* Linux (gx10), `go test -race`: workspace, execenv (all tests in both pgid mode and systemd scope
  mode: lease expiry kills grandchildren and setsid escapees, OOM cap, reaper of orphan
  scopes/processes), e2e. Container tests with `KRAN_TEST_DOCKER=1` on gx10's Docker 29: host slots and
  forced volume slots + sync helper.
* macOS (devmac, macOS 26, Go 1.27.3, `-race`): workspace, execenv (pgid mode, clonefile, memory
  watchdog, pty, reattach, lease, reaper), container tests on Colima (volume slots, `kran-sync`,
  OOM, setup, lease, job-started containers, reaper), e2e.
* The real `runner.Dockerfile` was built once on devmac (28 s) and checked (mise, docker, compose, git,
  gcc, zsh), then removed.
* Benchmarks: initial commit of 30k files into an empty slot ~1.8 s, no-op diff ~0.2 s (gx10, loaded).
* Leftovers: tests remove every `kran-test-*` container, volume and image. Base images pulled by the
  checks stay (`alpine:3.20` on devmac, `debian:stable-slim` on devmac). An empty transient
  `kran_test.slice` remains on gx10.

## Not done / limits

* Container jobs: a missing command yields exit 127 from tini without `BeforeStart` (cannot tell it
  apart from a script's own 127).
* Container CPU usage is sampled through the Docker API (approximate); native Linux uses cgroup
  `cpu.stat`/`memory.peak`, macOS native samples the tree every second.
* macOS native slots refuse repos with paths that differ only in case (APFS is case-insensitive) with
  a clear error → origin falls back.
* No per-job `cpu.max` for native jobs (only nice); containers get `--cpus` from `Alloc.CPUs`.
