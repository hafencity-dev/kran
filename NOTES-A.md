# NOTES-A (transport, daemon, client)

## Wiring the lead must do on merge

Nothing: `internal/daemon/run.go` wires `store.Open`, `metrics.New`, `estimate.NewWithOptions` (benchmark
score prior, late-bound to the daemon), `scheduler.New`, `execenv.New`, `workspace.NewScanner`,
`transport.NewDialer`, E's `internal/config` and `ui.Handler()` (mounted at `/`, API at `/api/v1/`).
`execenv.Options.LinuxBinary`, `BlobCap` and `DiskFloor` are left to B's defaults. Run `go mod tidy`
once after all merges (several imports are still marked `// indirect`).

## Proto changes (additive, marked `// added by A`)

* `QueueEvent{Queue}`, `LogEvent{Level, Msg, Time, Node, JobID}` — typed event payloads.
* `OutputChunk{Offset, Data, Stream, B64}` — log-follow messages (`Offset` is after the chunk).
* `JobRef` and `NodeState.Guests` — jobs other origins run on a node.
* `Job.PlacedAt`, `Job.RunningAt`, `Job.Phase` — placement time, command start, last sync/setup phase.
* `Stats.Since` — local midnight of "today".

## Contracts others rely on (as implemented)

### For E (CLI)
* Run session: daemon sends heartbeats every 2 s in every phase. `client.RunSession` sends its own
  heartbeats, swallows incoming ones, and `Recv` fails with `client.ErrSessionTimeout` after 10 s
  (`client.RecvTimeout`) of silence.
* After a pre-start remote failure the daemon sends `notice{kind:"fallback"}` and then a second
  `decision{mode:"local"}` (or another `decision{mode:"remote"}` when a different node is tried).
  `--remote` or `--no-fallback` (when it would fall back) end with `exit{code:125, state:infra_error}`.
* `reattach{job_id, offset}` is accepted as the first frame of any session, for remote and local jobs,
  and for jobs that already ended (output from offset + exit). Unknown job → `FrameError`.
* Local jobs: the CLI's `exit` frame is final; disconnecting right after it is not a cancel. A CLI
  that disconnects without `exit` gets its scope (`kran*.scope` only) or process group killed
  (TERM, KILL after 5 s). Signals for local jobs are ignored by the daemon (the child has the tty).
* `signal` frames before the job runs cancel it (exit 130 for INT, 143 otherwise).
* `client.Dial` errors: `ErrNoDaemon` (missing/stale socket, or no answer within 3 s),
  `ErrPermission`, `ErrIncompatible` (API or protocol major differs). Extra: `client.APIError`,
  `client.IsNotFound`, and `JobsQuery(ctx, client.JobsQuery{Limit, Owner, Node})` via type assertion.
* `daemon.Options.Logf`: when set, the daemon does not additionally log to stderr itself.
* `kran serve --stdio` starts the daemon on demand (lead request): if the peer socket does not
  answer it waits 1.5 s (a service may be restarting it), then spawns `<exe> daemon` detached
  (`setsid`, stdin `/dev/null`, stdout/stderr appended to `<state>/daemon.stdio.log`, env inherited
  incl. `KRAN_*_DIR`), waits up to 5 s for the socket, then bridges. A flock on
  `<runtime>/daemon-start.lock` makes concurrent serve processes spawn one daemon.
  `KRAN_NO_AUTOSTART=1` disables it. Exit 69 = daemon not running, 77 = socket permission denied.
  Caveat: on Linux with logind `KillUserProcesses=yes` such a daemon dies with the SSH session.
* Node ID: read from `<ConfigDir>/node_id` (E's file); if missing the daemon generates one and keeps
  it in the store (KV `node.id`).
* `peers.toml` `preferred_address` is tried first; when another address wins it is written back with
  `config.UpdatePeers`. Changing it does not reconnect (it is excluded from the change check).

### For C (scheduler, store, estimate, metrics) — implemented per NOTES-C
* Self is pushed every 2 s with gross `Capacity`, `MaxJobs`, `Allocated`/`Running`/`Free` and
  `AllocatedJobs` from `Snapshot()`; peers are pushed with the totals *they* report (their own
  `Snapshot()`), `AllocatedJobs` always non-nil. Every peers.toml entry is registered (offline) before
  the sockets open.
* Target side calls `Sched.Admit` for `PeerAdmit` (codes passed through as `PeerResponse.Code`) and
  `Release` when the reservation expires/is released or the job ends (with usage). Jobs restored after a
  target or origin restart are re-registered with `Admit` so they keep counting.
* `UpdateJobUsage`: local jobs (process group via `metrics.JobUsage`), remote jobs on the target
  (`Process.Live`) and on the origin (from the target's status stream), every 2 s.
* `*UnplaceableError`: with `--remote`/`--no-fallback` → exit 125 with its reason; otherwise the
  session ends with `FrameError` (the CLI runs the command itself) and the job is recorded as
  `canceled`, `Fallback: true`.
* `Reject` is used for every pre-start failure; the daemon additionally marks a node `degraded` for
  60 s after 3 consecutive pre-start failures (SPEC §6.1 wording; shown in the UI).
* `Tick` every 2 s; `PruneJobs(now-7d)` hourly; `Spec.Priority` left empty when no flag was given.
* Request (low priority): `Snapshot.Free` per runtime would let the daemon fill D's per-runtime
  capacity request.

### For B (execenv, workspace)
* `Process.Output(offset)`: offset counts **payload bytes of stdout+stderr frames** (same unit as
  `proto.Reattach.Offset`), not bytes of the framed file.
* `StartRequest.OnPhase` must report `JobRunning` when the user's command starts. Until then any
  failure (Start error, or the process ending with `infra_error`/`lost` before `running`) makes the
  origin fall back; after it, infrastructure failures end with 125.
* After a target daemon restart the daemon calls `Attach(id)` for `Active()` jobs and `ReleaseSlot`
  with the persisted `SlotRef` once `Wait` returns: execenv must keep those slots marked busy across
  the restart. `Attach` must also work for recently finished jobs (Output to EOF, Wait → exit).
* `Process.Stdin()` may return nil when `spec.Stdin` is false. `Live()` should be cheap (called
  every 2 s per running job, outside locks). `Renew` errors are only logged.
* Exit: `Code` 128+n and `Signal` "INT"/"TERM"/"KILL" for signals; the daemon fills `Node`.
* `job.cancel` sends TERM, then KILL after 5 s. `Nice` comes from `accept.nice`.
* The daemon also refuses `admit` below the disk floor max(10 GiB, 5 % of `DiskTotal`) using
  `metrics.Sample().DiskFree`; execenv may still refuse on its own.
* `workspace.Scanner.Open(hash)` is called concurrently from 4 upload workers.

### For D (UI) — per NOTES-D
* `GET /api/v1/events`: `id: <seq>`, `event: <type>`, `data: <proto.Event>`; first event `state` with the
  seq it includes; contiguous seq per stream; slow subscribers are disconnected; `node.metrics` every
  2 s per node (or a `node` event when status changes). `log` events carry notable lines (peer
  connected/lost, breaker, pre-start failures). A `state` event follows peer additions/removals.
* Log follow: unnamed `data: {"offset": <after chunk>, "data": "<utf-8>", "stream": "...", "b64": "..."}`
  (text never splits a UTF-8 sequence; `b64` is exact), then `event: end` / `data: {"offset": N,
  "exit": {...}}`. Without `follow`: `text/plain` from `offset`.
* `GET /api/v1/nodes/{name}/metrics?since=<RFC 3339>`: last 10 min of 2 s samples.
* `GET /api/v1/jobs?state=` filter added (`before`/`q` paging not done).
* `Spec.Env` is never sent by the API (state, events, jobs).
* Auth: loopback listener — reads open, mutations need `X-Kran-Token`, Host must be loopback;
  non-loopback — `/api/` needs the token (header or `?token=` on GETs), Host must be the bind host
  unless a valid token is present; run sessions refused on TCP. 401/403 carry `APIError{error,hint}`.
* Not done: per-runtime capacity (needs C), `ReasonCode` (C), `Estimate.DurationOnNode` (C).

## Design notes

* Peer protocol details are documented at the top of `internal/daemon/peerproto.go` (methods,
  stream phases, private frame types 100–104).
* Origin keeps an append-only per-job spool (`<state>/spool/<job id>`, run-session framing) that run
  sessions tail; the driver never blocks on a slow CLI, reattach is by output offset, logs read the
  same file. Spools are deleted after 7 days.
* Each active job has a daemon-owned driver goroutine; CLI sessions attach/detach. Last detach
  without exit = cancel (SPEC §6.1), except during daemon shutdown.
* Restart recovery: queued/syncing jobs are re-queued; setup/running remote jobs reattach at the spool
  offset (heartbeats resume at once); jobs whose CLI does not reattach within 20 s are canceled
  (local ones are marked `lost`).
* Reattach after a cut retries until lastContact + lease (15 s) + 5 s; then exit 125 "lost contact
  with <node> for 20s". A job stream silent for 10 s also closes the session (half-open detection
  faster than the 3×10 s keepalive).
* Target-side reservations (admit) expire after 30 s without `job.start` and are released at once
  when the admitting session closes; `admit` is serialized so two origins cannot overbook a node.
* `kran.slice`: MemoryMax = current + MemAvailable − reserve (cap MemTotal − reserve, floor 1 GiB),
  MemoryHigh=infinity, refreshed every 20 s and before each local placement, skipped when the change
  is < 64 MiB; disabled without a systemd user session (`systemd-run` + `systemctl --user`).

## Not done / limits

* The chaos suite (`make chaos`) and `internal/e2e` are not written; the daemon package has an
  in-process multi-daemon harness (`harness_test.go`) with fakes that covers the SPEC §12 scenarios
  owned by A.
* Spool size is not capped (a job printing gigabytes uses that much disk on the origin for 7 days).
* x/crypto/ssh's `channel.CloseWrite` is not synchronized with `Write` (data race); the transport
  never calls it concurrently (client side closes the whole channel; the test sshd serializes it).

## Tests

`nice -n 10 go test -race -p 4 ./internal/transport/ ./internal/client/ ./internal/daemon/` green on
linux/arm64 (gx10) and darwin/arm64 (devmac, `GOCACHE` must be private there:
`GOCACHE=$HOME/kran-dev/a-gocache`). `GOOS=darwin go vet` clean. The daemon suite takes ~30 s.
Covered: local/remote runs, stdin, signals, Ctrl-C while queued, fallback + breaker + cool-down,
`--no-fallback`, CLI killed mid-sync, target daemon restart mid-run, short cut / short stall / long
outage (lease), origin restart with and without CLI reattach, reattach after finish, local CLI death
kills the process group, two origins racing for one node, atomic admission, incompatible peer and
origin, reservation expiry, graceful shutdown < 5 s with live jobs, queue notices, peers.toml watch,
SSH end-to-end including a connection drop mid-job, autostart spawning exactly one daemon.
